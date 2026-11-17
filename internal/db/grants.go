package db

import (
	"database/sql"
	"errors"

	"nft-forward/internal/chain"
)

type NodeHop struct {
	NodeID    int64  `json:"node_id"`
	Position  int    `json:"position"`
	HopNodeID int64  `json:"hop_node_id"`
	Mode      string `json:"mode"`
}

type UserNode struct {
	UserID   int64 `json:"user_id"`
	NodeID   int64 `json:"node_id"`
	MaxRules int   `json:"max_forwards"`
	// TrafficQuotaBytes 是这条授权自己的配额，0 表示这条授权不设上限。它与用户的全局
	// 配额各自独立判定（suppressedRulesSQL 里各一条子句）：全局配额用尽，该用户的规则
	// 全部不转发；授权配额用尽，只是他经过这个逻辑节点的规则不转发。
	TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	// TrafficUsedBytes 是这条授权本周期的已用量，折算字节（accounting.Folded）：不乘
	// 倍率与费率；用户每条经过这个逻辑节点的规则，在这个节点那一段的段首跳记一次。
	// 与 users 上同名的列单位不同；授权配额 TrafficQuotaBytes 与它同一单位。
	TrafficUsedBytes int64 `json:"traffic_used_bytes"`
	// RateLimitMBytes is the per-grant shared rate limit in MB/s; all of the
	// user's rules on this node share one bucket. 0 = unlimited (unlike the
	// quota, there is no user-level fallback).
	RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	// Seats 是这条授权对节点入口/中转资格的覆盖：0 表示不覆盖、沿用节点自己的
	// （nodes.seats），非 0 表示整体改用这个值（入口、中转的任意组合）。所以一个
	// 节点可以对某个被授权人是入口，对其他人仍只是中转。JSON 键仍是 roles：接口
	// 给出去的键不随 Go 名与列名改。
	Seats     chain.Seat `json:"roles"`
	GrantedAt int64      `json:"granted_at"`
}

// ErrNotComposite 表示试图给不是组合的节点写成员。
var ErrNotComposite = errors.New("members apply to composite nodes only")

// CreateNodeHops 写入组合节点 nodeID 的有序成员。
//
// 成员的 mode 经 chain.InterNodeLegMode 落值后才入库：没填的按节点间那条腿的默认值
// 记成 userspace，认不出的值返回错误。node_hops 只经本函数写入，所以读侧（展开、
// 链路推导）拿到的成员 mode 一定是两种模式之一；也因此“只有组合有成员”在这里检查：
// nodeID 不是组合时返回 ErrNotComposite，不存在时返回 sql.ErrNoRows。
func CreateNodeHops(d DBTX, nodeID int64, hops []NodeHop) error {
	var nodeType string
	if err := d.QueryRow(`SELECT node_type FROM nodes WHERE id=?`, nodeID).Scan(&nodeType); err != nil {
		return err
	}
	if nodeType != "composite" {
		return ErrNotComposite
	}
	for _, h := range hops {
		mode, err := chain.InterNodeLegMode(h.Mode)
		if err != nil {
			return err
		}
		if _, err := d.Exec(`INSERT INTO node_hops(node_id, position, hop_node_id, mode) VALUES (?,?,?,?)`,
			nodeID, h.Position, h.HopNodeID, mode); err != nil {
			return err
		}
	}
	return nil
}

func scanNodeHop(r rowScanner) (*NodeHop, error) {
	h := &NodeHop{}
	if err := r.Scan(&h.NodeID, &h.Position, &h.HopNodeID, &h.Mode); err != nil {
		return nil, err
	}
	return h, nil
}

func ListNodeHops(d *sql.DB, nodeID int64) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode FROM node_hops WHERE node_id=? ORDER BY position`, scanNodeHop, nodeID)
}

// ListAllNodeHops returns every composite node's hops in one query, ordered so
// callers can group by node_id without an N+1 fan-out.
func ListAllNodeHops(d DBTX) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode FROM node_hops ORDER BY node_id, position`, scanNodeHop)
}

func DeleteNodeHops(d DBTX, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM node_hops WHERE node_id=?`, nodeID)
	return err
}

// CompositeReferrers returns the composite nodes that directly list nodeID as a
// member hop, so a delete can warn which composite definitions it would alter.
// Nesting makes "composite references composite" common, and a silent FK cascade
// would quietly drop the member from an upper composite's chain.
func CompositeReferrers(d *sql.DB, nodeID int64) ([]*Node, error) {
	return queryAll(d,
		`SELECT `+nodeColsQualified+` FROM nodes n
		 WHERE n.node_type='composite'
		   AND EXISTS (SELECT 1 FROM node_hops h WHERE h.node_id=n.id AND h.hop_node_id=?)
		 ORDER BY n.sort_order, n.id`, scanNode, nodeID)
}

// GrantNode grants a user access to a node. If the grant already exists its
// max_rules and traffic quota are overwritten; the rate limit, seats and
// usage counters are kept.
func GrantNode(d DBTX, userID, nodeID int64, maxRules int, trafficQuotaBytes int64) error {
	_, err := d.Exec(`INSERT INTO user_nodes(user_id, node_id, max_rules, traffic_quota_bytes, granted_at) VALUES (?,?,?,?,?)
		ON CONFLICT(user_id, node_id) DO UPDATE SET max_rules=excluded.max_rules, traffic_quota_bytes=excluded.traffic_quota_bytes`,
		userID, nodeID, maxRules, trafficQuotaBytes, now())
	return err
}

// GrantWrite is one (user, node) grant together with the caps to write.
type GrantWrite struct {
	UserID            int64
	NodeID            int64
	MaxRules          int
	TrafficQuotaBytes int64
	RateLimitMBytes   int64
}

// ApplyGrants writes a batch of grants in one transaction: either every row
// is written or none is, so a failure partway through (a user id that does
// not exist fails the user_nodes foreign key) cannot leave some pairs granted
// and the rest not.
//
// A (user, node) pair that already has a grant gets its three caps
// overwritten in place; seats, usage counters and granted_at are kept, and so
// is the row's rowid, which GrantShapes hands out as the shaping group id.
// With keepExisting such a pair is left exactly as it is instead. The returned
// slice is parallel to grants: true where a row was inserted or overwritten,
// false where an existing grant was kept.
func ApplyGrants(d *sql.DB, grants []GrantWrite, keepExisting bool) ([]bool, error) {
	onConflict := `DO UPDATE SET max_rules=excluded.max_rules, traffic_quota_bytes=excluded.traffic_quota_bytes, rate_limit_mbytes=excluded.rate_limit_mbytes`
	if keepExisting {
		onConflict = `DO NOTHING`
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	written := make([]bool, len(grants))
	at := now()
	for i, g := range grants {
		res, err := tx.Exec(`INSERT INTO user_nodes(user_id, node_id, max_rules, traffic_quota_bytes, rate_limit_mbytes, granted_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(user_id, node_id) `+onConflict,
			g.UserID, g.NodeID, g.MaxRules, g.TrafficQuotaBytes, g.RateLimitMBytes, at)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		written[i] = n > 0
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return written, nil
}

func RevokeNode(d DBTX, userID, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

// SetGrantSeats 写一条已有授权对节点入口/中转资格的覆盖（0 表示沿用节点自己的）。
// 它只改已有的 (user, node) 行；用户没有这个节点的授权时返回 sql.ErrNoRows。seats
// 只应含 SettableSeats 里的位，由调用方校验（apiSetGrantSeats）。
func SetGrantSeats(d *sql.DB, userID, nodeID int64, seats chain.Seat) error {
	return UpdateRow(d, `UPDATE user_nodes SET seats=? WHERE user_id=? AND node_id=?`, seats, userID, nodeID)
}

func GetNodeGrant(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	row := d.QueryRow(`SELECT user_id, node_id, max_rules, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, seats, granted_at FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	g := &UserNode{}
	if err := row.Scan(&g.UserID, &g.NodeID, &g.MaxRules, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Seats, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

// ListNodesForUser returns the nodes a user has been granted access to, along
// with the grant metadata. Both slices are aligned by index.
func ListNodesForUser(d *sql.DB, userID int64) ([]*Node, []*UserNode, error) {
	rows, err := d.Query(`
		SELECT `+nodeColsQualified+`,
		       g.max_rules, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.seats, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []*Node
	var grants []*UserNode
	for rows.Next() {
		g := &UserNode{UserID: userID}
		n, err := scanNodeAnd(rows, &g.MaxRules, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Seats, &g.GrantedAt)
		if err != nil {
			return nil, nil, err
		}
		g.NodeID = n.ID
		nodes = append(nodes, n)
		grants = append(grants, g)
	}
	return nodes, grants, rows.Err()
}

// CountRulesForUserNode 数 userID 名下以逻辑节点 nodeID 为入口的规则条数，是授权的
// 条数上限（user_nodes.max_rules）比较的那个数：按规则计，一条经组合展开成多跳
// 的规则也只算一条；只把节点用作中转层的规则不计在这个节点的授权上。
//
// 接受事务：上限检查与插入要在同一个事务里（见 internal/rulechange）。
func CountRulesForUserNode(d DBTX, userID, nodeID int64) (int, error) {
	return count(d, `SELECT COUNT(*) FROM rules WHERE owner_id=? AND node_id=?`, userID, nodeID)
}

// ListUsersForNode returns grants for a specific node with user info.
func ListUsersForNode(d *sql.DB, nodeID int64) ([]struct {
	UserID            int64      `json:"user_id"`
	Username          string     `json:"username"`
	MaxRules          int        `json:"max_forwards"`
	TrafficQuotaBytes int64      `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64      `json:"traffic_used_bytes"`
	RateLimitMBytes   int64      `json:"rate_limit_mbytes"`
	Seats             chain.Seat `json:"roles"`
	GrantedAt         int64      `json:"granted_at"`
}, error) {
	rows, err := d.Query(`SELECT g.user_id, u.username, g.max_rules, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.seats, g.granted_at
		FROM user_nodes g JOIN users u ON u.id = g.user_id
		WHERE g.node_id = ? ORDER BY g.granted_at`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		UserID            int64      `json:"user_id"`
		Username          string     `json:"username"`
		MaxRules          int        `json:"max_forwards"`
		TrafficQuotaBytes int64      `json:"traffic_quota_bytes"`
		TrafficUsedBytes  int64      `json:"traffic_used_bytes"`
		RateLimitMBytes   int64      `json:"rate_limit_mbytes"`
		Seats             chain.Seat `json:"roles"`
		GrantedAt         int64      `json:"granted_at"`
	}
	for rows.Next() {
		var r struct {
			UserID            int64      `json:"user_id"`
			Username          string     `json:"username"`
			MaxRules          int        `json:"max_forwards"`
			TrafficQuotaBytes int64      `json:"traffic_quota_bytes"`
			TrafficUsedBytes  int64      `json:"traffic_used_bytes"`
			RateLimitMBytes   int64      `json:"rate_limit_mbytes"`
			Seats             chain.Seat `json:"roles"`
			GrantedAt         int64      `json:"granted_at"`
		}
		if err := rows.Scan(&r.UserID, &r.Username, &r.MaxRules, &r.TrafficQuotaBytes, &r.TrafficUsedBytes, &r.RateLimitMBytes, &r.Seats, &r.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NodeAccess 回答“用户 u 能否使用逻辑节点 nodeID”——把它选进自己的规则（入口或中转
// 层），或经它向外探测。这个问题只在这里回答，规则写入（internal/rulechange）与探测
// 接口都调它：
//
//   - 管理员能用任何节点，不受授权约束：ok 为真，grant 为 nil；
//   - 其余人必须持有这个节点的授权（user_nodes 的一行）：ok 为真，grant 是那一行，
//     它带着条数上限、流量配额、限速与入口/中转资格的覆盖；
//   - 没有授权：ok 为假。
//
// 节点的属主（nodes.owner_id）不在判定里。属主只表示“这台机器的节点侧命令以谁的名义
// 执行”；条数上限、配额、限速、按授权计流量都挂在授权行上，一条不经授权的使用路径
// 会绕开全部这些约束。属主要用自己的节点，同样由管理员给他授权。
//
// 读库失败时返回 err、ok 为假：调用方必须按无权处理，不能把读失败当成放行。
func NodeAccess(d *sql.DB, u *User, nodeID int64) (grant *UserNode, ok bool, err error) {
	if u.Role == "admin" {
		return nil, true, nil
	}
	g, err := GetNodeGrant(d, u.ID, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return g, true, nil
}

// GrantShape carries the shaping identity and limit of one rate-limited grant.
// GrantID is the user_nodes rowid: stable across upserts, so agents can use it
// as the connmark-backed shaping group id. user_nodes has a composite primary
// key (no INTEGER PRIMARY KEY), so it is an ordinary rowid table — after a
// revoke deletes the highest-numbered row, SQLite may reuse that rowid for
// the next insert. A revoke+regrant is therefore not guaranteed to produce a
// new group id: on that rare reuse, a connection that outlives the revoke
// keeps being classified into the tc class the reused id now names, spending
// bandwidth against the new grant's bucket until the connection ends, after
// which the misclassification self-heals.
type GrantShape struct {
	GrantID         int64
	RateLimitMBytes int64
}

// GrantShapes returns every rate-limited grant keyed by {user_id, node_id}.
// Unlimited grants (rate 0) are omitted so callers can treat absence as "no
// shaping".
func GrantShapes(d *sql.DB) (map[[2]int64]GrantShape, error) {
	rows, err := d.Query(`SELECT rowid, user_id, node_id, rate_limit_mbytes FROM user_nodes WHERE rate_limit_mbytes > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]GrantShape{}
	for rows.Next() {
		var gs GrantShape
		var uid, nid int64
		if err := rows.Scan(&gs.GrantID, &uid, &nid, &gs.RateLimitMBytes); err != nil {
			return nil, err
		}
		out[[2]int64{uid, nid}] = gs
	}
	return out, rows.Err()
}
