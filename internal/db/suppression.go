package db

import (
	"database/sql"
	"time"
)

// SuppressReason 说明一条规则为什么此刻不转发。取值同时是接口里的字符串，页面按它
// 显示文案。
type SuppressReason string

const (
	// 属主账号被管理员停用。
	SuppressUserDisabled SuppressReason = "user_disabled"
	// 属主账号已过到期时间。
	SuppressUserExpired SuppressReason = "user_expired"
	// 属主的全局流量配额用尽。
	SuppressUserQuota SuppressReason = "user_quota"
	// 规则经过的某个节点被停用：某一跳所在的物理节点，或某一跳所属的逻辑节点
	// （rule_hops.via_node_id，组合与中转层都记在这里）。
	SuppressNodeDisabled SuppressReason = "node_disabled"
	// 规则经过的某条授权的流量配额用尽。
	SuppressGrantQuota SuppressReason = "grant_quota"
	// 规则指向的落地出口的流量配额用尽。
	SuppressExitQuota SuppressReason = "exit_quota"
)

// userExpiredSQL 是“账号 u 已到期”的 SQL 条件，与 Go 侧的 User.Expired 是同一个
// 判定（TestUserExpiredAgreesBetweenGoAndThePushPredicate 对照）：没有到期时间
// （NULL 或 0）永不到期，否则到期时间早于当前时刻即到期。
const userExpiredSQL = `(u.expires_at IS NOT NULL AND u.expires_at > 0 AND u.expires_at < strftime('%s','now'))`

// Expired 报告账号在 now 这一刻是否已到期。判定见 userExpiredSQL。
func (u *User) Expired(now time.Time) bool {
	return u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < now.Unix()
}

// suppressedRulesSQL 是“哪些规则此刻不转发、因为什么”的唯一定义，产出
// (rule_id, reason, rank) 三列。每个 SELECT 是一条压制条件；一条规则命中任意一条
// 就整条不转发，它在每个节点上的跳都不下发。同一条规则可以命中多条，也可以被同
// 一条命中多次（多跳各命中一次），所以读者要么只问“在不在这个集合里”
// （ActiveRuleHopsForPush），要么按 rank 取最靠前的一条（RuleSuppressions）。
//
// 它有两个读者，二者不得各写一份条件：下发用它决定哪些跳进节点，展示用它告诉
// 页面哪条规则没生效、为什么。新增压制条件只在这里加一个 SELECT。
//
// 条件读的都是库里的当前值，而下发只在被触发时求值。所以每个改动这些输入（停用
// 标志、到期时间、配额、已用量、节点启停）的入口，改完都必须重新下发受影响的节点
// （server 包的 redispatch* 系列），否则变化要等到下一次无关的下发才生效。到期是
// 唯一不由写入触发的输入，由 Server.expiryEnforcer 定时重推承担。
//
// rank 的次序是给人看的优先级：账号层面的原因先于节点与配额层面的，因为前者解除
// 之前后者无从谈起。它不影响下不下发。
const suppressedRulesSQL = `
	SELECT r.id AS rule_id, 'user_disabled' AS reason, 1 AS rank
	  FROM rules r JOIN users u ON u.id = r.owner_id
	 WHERE u.disabled = 1
	UNION ALL
	SELECT r.id, 'user_expired', 2
	  FROM rules r JOIN users u ON u.id = r.owner_id
	 WHERE ` + userExpiredSQL + `
	UNION ALL
	SELECT r.id, 'user_quota', 3
	  FROM rules r JOIN users u ON u.id = r.owner_id
	 WHERE u.traffic_quota_bytes > 0 AND u.traffic_used_bytes >= u.traffic_quota_bytes
	UNION ALL
	SELECT rh.rule_id, 'node_disabled', 4
	  FROM rule_hops rh JOIN nodes n ON n.id IN (rh.node_id, rh.via_node_id)
	 WHERE n.disabled = 1
	UNION ALL
	SELECT rh.rule_id, 'grant_quota', 5
	  FROM rule_hops rh
	  JOIN rules r ON r.id = rh.rule_id
	  JOIN user_nodes un ON un.user_id = r.owner_id AND un.node_id = rh.via_node_id
	 WHERE un.traffic_quota_bytes > 0 AND un.traffic_used_bytes >= un.traffic_quota_bytes
	UNION ALL
	SELECT r.id, 'exit_quota', 6
	  FROM rules r
	  JOIN user_landing_exits ule ON ule.user_id = r.owner_id
	   AND ule.host = r.exit_host AND ule.port = r.exit_port
	 WHERE ule.present = 1 AND ule.quota_bytes > 0 AND ule.used_bytes >= ule.quota_bytes`

// NodesCarryingRulesThrough 返回“经过 nodeID 的规则”的全部跳所在的节点（去重）。
// “经过”与压制条件 node_disabled 用的是同一个含义——某一跳在 nodeID 上，或某一跳
// 属于逻辑节点 nodeID——所以它正是 nodeID 的启停会改变其规则集的那些节点。两处的
// 含义必须一起改。
func NodesCarryingRulesThrough(d *sql.DB, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT node_id FROM rule_hops
		WHERE rule_id IN (SELECT rule_id FROM rule_hops WHERE node_id = ? OR via_node_id = ?)`,
		nodeID, nodeID)
}

// ActiveRuleHopsForPush 返回应当下发到 nodeID 的规则跳：这个节点上的全部跳，去掉
// 被压制的规则（suppressedRulesSQL）的跳。
//
// 结果按（监听端口，协议）排序，同一组跳每次返回的顺序都相同。两个读者依赖这一点：
// 面板给下发载荷算的 rev（顺序变了 rev 就变，重连的节点会被多下发一次），以及节点
// 按下标比较新旧规则集的数据面（nft.SameDataplane，顺序变了就整表重建）。
//
// 只按监听端口排不够：一个节点上 tcp 与 udp 可以各占同一个端口（OccupiedPortsOnNode
// 按协议的命名空间判占用），那两行谁先谁后会跟着行的物理位置走，而 RegenerateRule
// 每次都把跳行删掉重插。同一节点上（监听端口，协议）不重复由分配端口时的占用检查
// 保证，所以加上协议就是全序。
func ActiveRuleHopsForPush(d *sql.DB, nodeID int64) ([]*RuleHop, error) {
	q := `SELECT ` + ruleHopCols + ` FROM rule_hops
		WHERE node_id=?
		  AND rule_id NOT IN (SELECT rule_id FROM (` + suppressedRulesSQL + `))
		ORDER BY listen_port, proto`
	return queryAll(d, q, scanRuleHop, nodeID)
}

// RuleSuppressions 返回每条被压制的规则及其原因；没被压制的规则不在结果里。一条
// 规则命中多条压制条件时取 rank 最靠前的一条。
func RuleSuppressions(d *sql.DB) (map[int64]SuppressReason, error) {
	rows, err := d.Query(`SELECT rule_id, reason FROM (` + suppressedRulesSQL + `) ORDER BY rule_id, rank DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]SuppressReason{}
	for rows.Next() {
		var id int64
		var reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		// 同一条规则的各行按 rank 从大到小到达，后到的覆盖先到的，留下的是 rank
		// 最小的那一条。
		out[id] = SuppressReason(reason)
	}
	return out, rows.Err()
}
