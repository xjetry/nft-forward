package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	NodeRoleEntry int64 = 1 << 0
	NodeRoleVia   int64 = 1 << 1
)

// EffectiveNodeRoles resolves how a specific grant may use a node: the grant's
// own mask when set, otherwise the node's mask. grantRoles == 0 means inherit;
// any other value overrides the node mask entirely — it may add or drop bits,
// the two masks are independent, not subset-constrained.
func EffectiveNodeRoles(nodeRoles, grantRoles int64) int64 {
	if grantRoles == 0 {
		return nodeRoles
	}
	return grantRoles
}

type User struct {
	ID                int64          `json:"id"`
	Username          string         `json:"username"`
	PwHash            string         `json:"-"`
	Role              string         `json:"role"`
	Disabled          bool           `json:"disabled"`
	DisableReason     sql.NullString `json:"disable_reason"`
	MaxForwards       int            `json:"max_forwards"`
	TrafficQuotaBytes int64          `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64          `json:"traffic_used_bytes"`
	// OverQuota 不是 users 表的列：scanUser 在读出这一行时用 QuotaReached 从上面两个
	// 字段算出，供页面显示“已超额”。全局配额用尽只停转发（见 ActiveRuleHopsForPush），
	// 与 Disabled 无关：超额的账号仍可登录。
	OverQuota bool `json:"over_quota"`
	// TrafficResetDays is the rolling window length in days; 0 means never auto-reset.
	TrafficResetDays   int           `json:"traffic_reset_days"`
	LastTrafficResetAt int64         `json:"last_traffic_reset_at"`
	ExpiresAt          sql.NullInt64 `json:"expires_at"`
	// LandingSubURL is an optional subscription URL; LandingURIs is an optional
	// newline-separated list of proxy URIs. They combine into the user's set of
	// landing nodes (see internal/landing). Both empty means no landing source.
	LandingSubURL string  `json:"landing_sub_url"`
	LandingURIs   string  `json:"landing_uris"`
	AdminNote     string  `json:"admin_note"`
	BillingRate   float64 `json:"billing_rate"`
	// RuleCount is not a users-table column; it is filled by FillUserRuleCounts
	// so the user list can show used/total rule quota.
	RuleCount int `json:"rule_count"`
}

// Node 是 nodes 表的一行，字段与列一一对应，没有内存里补出来的派生值。组合的在线、
// 出入口地址、成员链、原始流量不是列，由 NodeCatalog.Derive 从全量节点算出；对外
// 的 JSON 形状由 server 包里各受众的视图定义，行本身不序列化（见 MarshalJSON）。
//
// 一行有两种身份。一台机器（remote、self）：跑 agent 的那一台，它的事实在 Machine
// 里。一条线路：可授权、可计价、可在链里占位置的单元，它的属性在 Line 里；一台机器
// 就是一条一段的线路，组合（composite）是由几台机器按序组成的线路，自己不是机器。
// 所以组合行上的 Machine 恒为零值：机器列只经 updateMachine 写入，它排除组合；建
// 组合时（CreateCompositeNode）不写机器列；此前留在组合行上的机器列由迁移
// 0050_composite_rows_carry_no_machine 清空。Name、NodeType、Disabled、SortOrder、
// CreatedAt 属于这一行本身，两种身份都用。
type Node struct {
	ID       int64
	Name     string
	NodeType string
	// Disabled：停用的节点不承载转发。压制判定（suppressedRulesSQL）对物理跳与
	// 逻辑段一视同仁，所以停用机器与停用线路都会让经过它的规则整条撤下。
	Disabled  bool
	SortOrder int64
	CreatedAt int64
	Machine
	Line
}

// Machine 是一行作为机器时的事实：怎么接入（凭据、连接地址、中继地址）、在哪段端口上
// 分配监听、此刻的连接与同步状态、升级记录、以谁的名义执行节点侧命令（属主），以及
// 只能按物理节点判定的两个开关。
type Machine struct {
	// OwnerID 是属主：这台机器的节点侧命令（TUI 建规则、接管本地规则）以谁的名义执行。
	OwnerID *int64
	Address string
	// Secret 是机器接入 agent 通道时出示的凭据。只有管理员的节点详情页带它。
	Secret              string
	RelayHost           string
	RelayHostV6         string
	RelayHostDeclared   bool
	RelayHostV6Declared bool
	// Online 是 online 列：连接登记时置 1、注销时置 0。组合的在线状态不读这一列，看
	// NodeCatalog.Derive。
	Online             int
	AgentVersion       string
	AgentSHA           string
	LastSeen           *int64
	LastApplyAt        sql.NullInt64
	LastError          sql.NullString
	LastWarning        string
	PortRange          string
	LastUpgradeAt      sql.NullInt64
	LastUpgradeVersion string
	LastUpgradeStatus  string
	LastUpgradeError   string
	// Unidirectional：这台机器上报的计数按单向计费。读者是 Hub 入账，读的是上报计数
	// 的那台机器。
	Unidirectional bool
	// NoDirectExit forbids the machine from terminating a rule chain: it cannot
	// launch the exit segment, so a rule whose physical tail is this machine
	// must cascade into at least one more middle layer.
	NoDirectExit bool
}

// Line 是一行作为线路时的属性。授权（user_nodes）、绑定边（node_bindings）、组合的
// 成员（node_hops）也挂在线路上，它们在各自的表里。
type Line struct {
	// Roles is a bitmask of what the line can be used as: NodeRoleEntry means
	// it can be picked as a rule's entry, NodeRoleVia means it can be attached
	// behind an upstream node as a middle-layer segment. A line may hold both.
	Roles int64
	// RateMultiplier 是计价倍率，取规则入口那条线路的值。
	RateMultiplier float64
}

// errNodeRowJSON 是直接序列化节点行时的错误。
var errNodeRowJSON = errors.New("db.Node is a table row; serialize it through an audience view")

// MarshalJSON 拒绝把节点行直接写进响应。行上没有组合的派生属性（组合的 online 列
// 恒为 0），也带着只给管理员看的机器字段；直接序列化一行，读者既分不清“没有”与
// “没算”，用户面还会拿到整行。所以对外一律经 server 包里的受众视图，这里让绕开
// 视图的写法在第一次序列化时就失败，而不是悄悄发出一份残缺的形状。
func (Node) MarshalJSON() ([]byte, error) { return nil, errNodeRowJSON }

// NodeMember 是组合展开后的一个物理成员：节点行里的名字与类型，加上组合给这一跳配
// 的转发模式。
type NodeMember struct {
	NodeID   int64
	Name     string
	NodeType string
	Mode     string
}

type Rule struct {
	ID              int64         `json:"id"`
	NodeID          int64         `json:"node_id"`
	OwnerID         sql.NullInt64 `json:"owner_id"`
	Name            string        `json:"name"`
	Proto           string        `json:"proto"`
	ExitHost        string        `json:"exit_host"`
	ExitPort        int           `json:"exit_port"`
	EntryListenPort int           `json:"entry_listen_port"`
	Comment         string        `json:"comment"`
	CreatedAt       int64         `json:"created_at"`
	// EntryFamily selects which of the entry node's relay addresses the entry
	// endpoint advertises: "v4" (default), "v6", or "both". Validated and
	// resolved against the entry node's relay_host/relay_host_v6 in RegenerateRule.
	EntryFamily string `json:"entry_family"`
	// ViaNodeIDs is the ordered middle-layer path the rule's chain runs
	// through (entry excluded). Persisted so node_id edits re-derive the same
	// chain; empty for plain single/composite rules.
	ViaNodeIDs []int64 `json:"via_node_ids"`
	// TotalBytes is not a rules-table column; it is filled by FillRuleTraffic
	// from the entry hop so list/detail responses can show per-rule traffic.
	TotalBytes int64 `json:"total_bytes"`
}

type RuleHop struct {
	ID         int64  `json:"id"`
	RuleID     int64  `json:"rule_id"`
	Position   int    `json:"position"`
	NodeID     int64  `json:"node_id"`
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Mode       string `json:"mode"`
	// ViaNodeID is the logical-segment node this physical hop was expanded
	// from (entry segment = the rule's node_id; explicit-hops path = the
	// hop's own node). Quota suppression, per-grant accounting and shaping
	// group hops by it rather than by physical node.
	ViaNodeID     int64  `json:"via_node_id"`
	Comment       string `json:"comment"`
	LastBytes     int64  `json:"last_bytes"`
	LastBytesUp   int64  `json:"last_bytes_up"`
	LastBytesDown int64  `json:"last_bytes_down"`
	TotalBytes    int64  `json:"total_bytes"`
}

func RandToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// HashToken derives the at-rest form of a bearer credential (session cookie or
// API token). These tokens are already high-entropy random values, so a plain
// SHA-256 is sufficient — no salt/bcrypt needed — and lets lookups stay a single
// indexed equality match on the hash. Only the hash is ever stored, so a DB
// leak no longer exposes replayable credentials.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenPrefix returns the first 8 chars of a token for display, or the whole
// token if shorter. It lets the UI show a recognizable prefix without keeping
// the full plaintext at rest.
func tokenPrefix(token string) string {
	if len(token) < 8 {
		return token
	}
	return token[:8]
}

func now() int64 { return time.Now().Unix() }

// Users

func CreateUser(d *sql.DB, username, pwHash, role string) (int64, error) {
	res, err := d.Exec(`INSERT INTO users(username, pw_hash, role, created_at) VALUES (?,?,?,?)`,
		username, pwHash, role, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanUser(r rowScanner) (*User, error) {
	u := &User{}
	var disabled int
	if err := r.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &disabled, &u.DisableReason, &u.MaxForwards, &u.TrafficQuotaBytes, &u.TrafficUsedBytes, &u.TrafficResetDays, &u.LastTrafficResetAt, &u.ExpiresAt, &u.LandingSubURL, &u.LandingURIs, &u.AdminNote, &u.BillingRate); err != nil {
		return nil, err
	}
	u.Disabled = disabled == 1
	u.OverQuota = QuotaReached(u.TrafficQuotaBytes, u.TrafficUsedBytes)
	return u, nil
}

// QuotaReached 是“流量配额用尽”的判定：配额为 0 表示不限量，永不用尽；否则已用
// 达到配额即用尽（恰好相等也算）。ActiveRuleHopsForPush 的 SQL 里对全局配额写的
// 是同一个条件，两处由 TestGlobalQuotaPushPredicateMatchesQuotaReached 对照。
func QuotaReached(quotaBytes, usedBytes int64) bool {
	return quotaBytes > 0 && usedBytes >= quotaBytes
}

const userCols = `id, username, pw_hash, role, disabled, disable_reason, max_forwards, traffic_quota_bytes, traffic_used_bytes, traffic_reset_days, last_traffic_reset_at, expires_at, landing_sub_url, landing_uris, admin_note, billing_rate`

func ListUsers(d *sql.DB) ([]*User, error) {
	return queryAll(d, `SELECT `+userCols+` FROM users ORDER BY id`, scanUser)
}

func SetUserDisabled(d *sql.DB, id int64, disabled bool, reason string) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := d.Exec(`UPDATE users SET disabled=?, disable_reason=? WHERE id=?`, v, reason, id)
	return err
}

func DeleteUser(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func GetUserByUsername(d *sql.DB, username string) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func GetUserByID(d *sql.DB, id int64) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func CountUsers(d *sql.DB) (int, error) {
	return count(d, `SELECT COUNT(*) FROM users`)
}

// SetUserLandingSource stores a user's landing-node source. Either argument may
// be empty; both empty means the user has no landing source configured.
func SetUserLandingSource(d *sql.DB, id int64, subURL, uris string) error {
	_, err := d.Exec(`UPDATE users SET landing_sub_url=?, landing_uris=? WHERE id=?`, subURL, uris, id)
	return err
}

// RenameUser changes a user's username.
func RenameUser(d *sql.DB, id int64, newUsername string) error {
	_, err := d.Exec(`UPDATE users SET username=? WHERE id=?`, newUsername, id)
	return err
}

// UsersByID returns all users keyed by ID in a single query.
func UsersByID(d *sql.DB) (map[int64]*User, error) {
	all, err := ListUsers(d)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]*User, len(all))
	for _, u := range all {
		m[u.ID] = u
	}
	return m, nil
}

// Sessions

func CreateSession(d *sql.DB, userID int64, ttl time.Duration) (string, error) {
	token := RandToken(24)
	_, err := d.Exec(`INSERT INTO sessions(token, user_id, expires_at, created_at) VALUES (?,?,?,?)`,
		HashToken(token), userID, time.Now().Add(ttl).Unix(), now())
	if err != nil {
		return "", err
	}
	return token, nil
}

const userColsQualified = `u.id, u.username, u.pw_hash, u.role, u.disabled, u.disable_reason, u.max_forwards, u.traffic_quota_bytes, u.traffic_used_bytes, u.traffic_reset_days, u.last_traffic_reset_at, u.expires_at, u.landing_sub_url, u.landing_uris, u.admin_note, u.billing_rate`

func GetSessionUser(d *sql.DB, token string) (*User, error) {
	return scanUser(d.QueryRow(`
		SELECT `+userColsQualified+`
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > strftime('%s','now')`, HashToken(token)))
}

func DeleteSession(d *sql.DB, token string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE token = ?`, HashToken(token))
	return err
}

// Settings

// GetSetting returns the value for a global setting key, or "" if unset (an
// empty string is a valid "not configured" state, not an error).
func GetSetting(d *sql.DB, key string) (string, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a global setting key.
func SetSetting(d *sql.DB, key, value string) error {
	_, err := d.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// Nodes

// MachineSpec 是建一台机器时给的机器属性。
type MachineSpec struct {
	// Secret 为空时随机生成。
	Secret string
	// OwnerID 是属主：建它的管理员。为空表示没有属主，节点侧命令会被拒绝。
	OwnerID *int64
	// Address 是连接地址的初值；正常为空，由机器 hello 时写入。
	Address string
	// PortRange 为空时取 DefaultPortRange；调用方先用 ValidatePortRange 校验。
	PortRange      string
	Unidirectional bool
}

// LineSpec 是建一条线路（机器或组合）时给的线路属性。角色一律是入口加中转
// （NodeRoleEntry|NodeRoleVia），建好就能被选作入口或挂成中转层，不必再编辑一次。
type LineSpec struct {
	// RateMultiplier ≤ 0 时取 1.0：建节点的请求体分不清没填与填了 0，而 0 表示免费，
	// 免费只经专门的倍率接口（UpdateNodeRateMultiplier）设置。
	RateMultiplier float64
}

func (l LineSpec) rate() float64 {
	if l.RateMultiplier <= 0 {
		return 1.0
	}
	return l.RateMultiplier
}

const newLineRoles = NodeRoleEntry | NodeRoleVia

// CreateNode 建一台机器，只给名字、连接地址初值与凭据，其余取默认值。
func CreateNode(d *sql.DB, name, address, secret string) (*Node, error) {
	return CreateMachineNode(d, name, MachineSpec{Address: address, Secret: secret}, LineSpec{})
}

// CreateMachineNode 用一条 INSERT 建一台机器（node_type remote），机器属性与线路属性
// 一次写全，不会留下只写了一半的行。
func CreateMachineNode(d *sql.DB, name string, m MachineSpec, l LineSpec) (*Node, error) {
	secret := m.Secret
	if secret == "" {
		secret = RandToken(32)
	}
	portRange := m.PortRange
	if portRange == "" {
		portRange = DefaultPortRange
	}
	res, err := d.Exec(`INSERT INTO nodes(name, node_type, owner_id, address, secret, port_range, unidirectional,
			rate_multiplier, roles, sort_order, created_at)
		VALUES (?, 'remote', ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(sort_order),0)+1 FROM nodes), ?)`,
		name, m.OwnerID, m.Address, secret, portRange, boolInt(m.Unidirectional), l.rate(), newLineRoles, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return GetNode(d, id)
}

// CreateCompositeNode 建一个组合：节点行（node_type composite）与它的成员在同一个事务
// 里写入，任何一步失败都不留下东西。组合行上不写任何机器列：凭据为空串、没有端口段、
// 没有属主。空凭据不会让谁以组合的身份接入——server 包的 lookupNodeBySecret 拒绝空
// 凭据，也不匹配组合行。成员按 members 的次序编号，mode 经 CreateNodeHops 落值。
func CreateCompositeNode(d *sql.DB, name string, l LineSpec, members []NodeHop) (*Node, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO nodes(name, node_type, secret, port_range, rate_multiplier, roles, sort_order, created_at)
		VALUES (?, 'composite', '', '', ?, ?, (SELECT COALESCE(MAX(sort_order),0)+1 FROM nodes), ?)`,
		name, l.rate(), newLineRoles, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	hops := make([]NodeHop, len(members))
	for i, m := range members {
		hops[i] = NodeHop{NodeID: id, Position: i, HopNodeID: m.HopNodeID, Mode: m.Mode}
	}
	if err := CreateNodeHops(tx, id, hops); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetNode(d, id)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nodeCols 是读节点行的列清单，scanNodeAnd 按同样的次序扫描。凡是读节点行的查询都
// 拼这一份清单、经 scanNodeAnd 扫描（ListNodesForUser 把授权的列接在后面，通过
// extra 传入），所以加列只改这两处。
const nodeCols =`id,name,node_type,owner_id,address,secret,relay_host,relay_host_v6,online,agent_version,agent_sha,last_seen,last_apply_at,last_error,last_warning,disabled,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error,sort_order,rate_multiplier,unidirectional,relay_host_declared,relay_host_v6_declared,roles,no_direct_exit`

// nodeColsQualified is nodeCols with every column prefixed by the "n." alias.
// user_nodes has its own roles column (the grant-level override), so a query
// that joins nodes against user_nodes must qualify nodes.roles or the two
// same-named columns make the SELECT ambiguous.
var nodeColsQualified = "n." + strings.Join(strings.Split(nodeCols, ","), ",n.")

func GetNode(d *sql.DB, id int64) (*Node, error) {
	row := d.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

type rowScanner interface{ Scan(...any) error }

func scanNode(r rowScanner) (*Node, error) { return scanNodeAnd(r) }

// scanNodeAnd 扫描以 nodeCols 开头的一行；查询在 nodeCols 之后多选的列，按次序扫进
// extra。
func scanNodeAnd(r rowScanner, extra ...any) (*Node, error) {
	n := &Node{}
	var disabled, unidirectional, relayHostDeclared, relayHostV6Declared, noDirectExit int
	var lastSeen sql.NullInt64
	var agentVersion sql.NullString
	var ownerID sql.NullInt64
	var luVersion, luStatus, luError sql.NullString
	dest := []any{
		&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
		&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
		&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
		&disabled, &n.PortRange, &n.CreatedAt,
		&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
		&n.SortOrder, &n.RateMultiplier, &unidirectional,
		&relayHostDeclared, &relayHostV6Declared, &n.Roles, &noDirectExit,
	}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	n.Disabled = disabled == 1
	n.Unidirectional = unidirectional == 1
	n.NoDirectExit = noDirectExit == 1
	n.RelayHostDeclared = relayHostDeclared == 1
	n.RelayHostV6Declared = relayHostV6Declared == 1
	if ownerID.Valid {
		v := ownerID.Int64
		n.OwnerID = &v
	}
	if lastSeen.Valid {
		v := lastSeen.Int64
		n.LastSeen = &v
	}
	if agentVersion.Valid {
		n.AgentVersion = agentVersion.String
	}
	n.LastUpgradeVersion = luVersion.String
	n.LastUpgradeStatus = luStatus.String
	n.LastUpgradeError = luError.String
	return n, nil
}

func ListNodes(d DBTX) ([]*Node, error) {
	return queryAll(d, `SELECT `+nodeCols+` FROM nodes ORDER BY sort_order, id`, scanNode)
}

// ReorderNodes assigns sort_order to match the given id sequence (1-based).
// IDs absent from the list keep their previous order value, so a partial list
// still places the listed nodes ahead in the given order.
func ReorderNodes(d *sql.DB, ids []int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE nodes SET sort_order=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func RenameNode(d *sql.DB, id int64, name string) error {
	_, err := d.Exec(`UPDATE nodes SET name=? WHERE id=?`, name, id)
	return err
}

func DeleteNode(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// ErrPhysicalNodeSetting 表示试图在组合上写一个机器属性（Machine 里的列）。
var ErrPhysicalNodeSetting = errors.New("setting applies to physical nodes only")

// updateMachine 写节点 id 行上的机器列；set 是 "列=?" 形式的赋值，只由本文件的写函数
// 以字面量传入。下面所有写 Machine 列的函数都经过这里（MarkAllNodesOffline 除外：它只把
// online 清回 0，不会让组合行上出现机器属性）。
//
// 机器列只对跑 agent 的机器有意义：组合不接入、不上报、不会成为物理跳，写在它身上的值
// 没有读者，还会让页面与判定读到不属于它的状态。所以“组合行上 Machine 恒为零值”由这条
// UPDATE 的 WHERE 直接保证，不依赖调用方先判断节点类型；node_type 只在建行时写一次，
// 之后不变，已有的机器不会带着机器属性变成组合。没有改到行时分清两种失败：节点不存在
// 返回 sql.ErrNoRows，节点是组合返回 ErrPhysicalNodeSetting。
func updateMachine(d DBTX, id int64, set string, args ...any) error {
	res, err := d.Exec(`UPDATE nodes SET `+set+` WHERE id=? AND node_type<>'composite'`, append(args, id)...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var nodeType string
	if err := d.QueryRow(`SELECT node_type FROM nodes WHERE id=?`, id).Scan(&nodeType); err != nil {
		return err
	}
	return ErrPhysicalNodeSetting
}

// UpdateNodeRelayHost sets a machine's data-plane reachable address (empty clears
// it). Validation of the value (IPv4 / hostname) is the caller's job.
func UpdateNodeRelayHost(d *sql.DB, id int64, relayHost string) error {
	return updateMachine(d, id, `relay_host=?`, relayHost)
}

func UpdateNodeRelayHostV6(d *sql.DB, id int64, relayHostV6 string) error {
	return updateMachine(d, id, `relay_host_v6=?`, relayHostV6)
}

func SetNodeRelayHostDeclared(d *sql.DB, id int64, declared bool) error {
	return updateMachine(d, id, `relay_host_declared=?`, boolInt(declared))
}

func SetNodeRelayHostV6Declared(d *sql.DB, id int64, declared bool) error {
	return updateMachine(d, id, `relay_host_v6_declared=?`, boolInt(declared))
}

// UpdateNodeUnidirectional 设置机器的计费方向。
func UpdateNodeUnidirectional(d *sql.DB, id int64, uni bool) error {
	return updateMachine(d, id, `unidirectional=?`, boolInt(uni))
}

// UpdateNodeNoDirectExit 设置机器能否作为链尾直连目标。
func UpdateNodeNoDirectExit(d *sql.DB, id int64, v bool) error {
	return updateMachine(d, id, `no_direct_exit=?`, boolInt(v))
}

// UpdateNodePortRange sets a machine's port_range spec. An empty string resets
// to the default range. Callers must validate with ValidatePortRange first.
func UpdateNodePortRange(d *sql.DB, id int64, portRange string) error {
	if portRange == "" {
		portRange = DefaultPortRange
	}
	return updateMachine(d, id, `port_range=?`, portRange)
}

// UpdateNodeOwner 设置机器的属主；ownerID 为 nil 表示清除。
func UpdateNodeOwner(d *sql.DB, id int64, ownerID *int64) error {
	return updateMachine(d, id, `owner_id=?`, ownerID)
}

// RecordUpgradeResult stores the outcome of the most recent upgrade push to a
// machine. status is "acked" (daemon accepted and is restarting) or "error"
// (send/ack failure); errText is empty on acked. It overwrites the previous
// record — only the latest attempt is kept.
func RecordUpgradeResult(d DBTX, nodeID int64, version, status, errText string) error {
	return updateMachine(d, nodeID,
		`last_upgrade_at=?, last_upgrade_version=?, last_upgrade_status=?, last_upgrade_error=?`,
		now(), version, status, errText)
}

// 线路属性的写函数：组合与机器都有这些属性，不按节点类型区分。

func UpdateNodeRateMultiplier(d *sql.DB, id int64, mult float64) error {
	_, err := d.Exec(`UPDATE nodes SET rate_multiplier=? WHERE id=?`, mult, id)
	return err
}

func UpdateNodeRoles(d *sql.DB, id, roles int64) error {
	_, err := d.Exec(`UPDATE nodes SET roles=? WHERE id=?`, roles, id)
	return err
}

// Rules

const ruleCols = `id,node_id,owner_id,name,proto,exit_host,exit_port,entry_listen_port,comment,created_at,entry_family,via_node_ids`

func scanRule(r rowScanner) (*Rule, error) {
	rl := &Rule{}
	var viaJSON string
	if err := r.Scan(&rl.ID, &rl.NodeID, &rl.OwnerID, &rl.Name, &rl.Proto, &rl.ExitHost, &rl.ExitPort, &rl.EntryListenPort, &rl.Comment, &rl.CreatedAt, &rl.EntryFamily, &viaJSON); err != nil {
		return nil, err
	}
	rl.ViaNodeIDs = decodeViaNodeIDs(viaJSON)
	return rl, nil
}

const ruleHopCols = `id,rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment,last_bytes,last_bytes_up,last_bytes_down,total_bytes,via_node_id`

func scanRuleHop(r rowScanner) (*RuleHop, error) {
	h := &RuleHop{}
	if err := r.Scan(&h.ID, &h.RuleID, &h.Position, &h.NodeID, &h.Proto, &h.ListenPort, &h.TargetHost, &h.TargetPort, &h.Mode, &h.Comment, &h.LastBytes, &h.LastBytesUp, &h.LastBytesDown, &h.TotalBytes, &h.ViaNodeID); err != nil {
		return nil, err
	}
	return h, nil
}

func DeleteRulesForUser(d *sql.DB, userID int64) ([]int64, error) {
	nodes, err := DistinctUserNodes(d, userID)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM rules WHERE owner_id=?`, userID); err != nil {
		return nil, err
	}
	return nodes, nil
}

func DistinctUserNodes(d *sql.DB, userID int64) ([]int64, error) {
	return queryInt64s(d, `SELECT DISTINCT rh.node_id FROM rule_hops rh JOIN rules r ON r.id = rh.rule_id WHERE r.owner_id=?`, userID)
}

// ExpiredUserNodeIDs 返回承载着“已到期且未被停用的用户”的规则跳的节点。已停用的
// 用户不算在内：他们的规则在停用那一刻就由停用入口重推撤掉了，到期与否不再改变
// 节点上的规则集。
func ExpiredUserNodeIDs(d *sql.DB) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		JOIN users u ON u.id = r.owner_id
		WHERE u.disabled = 0 AND `+userExpiredSQL)
}

func RuleHopMapByNode(d *sql.DB, nodeID int64) (map[string]*RuleHop, error) {
	hops, err := queryAll(d, `SELECT `+ruleHopCols+` FROM rule_hops WHERE node_id=? ORDER BY listen_port`, scanRuleHop, nodeID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]*RuleHop, len(hops))
	for _, h := range hops {
		// A tcp+udp hop is reported by the daemon either as one tcp+udp sample
		// (kernel mode) or as separate tcp and udp samples (userspace mode, where
		// Partition splits it into a udp kernel DNAT + a tcp userspace relay).
		// Register every key that can carry this hop's bytes so applyCounters
		// sums them into the same row. Cross-proto port occupancy guarantees no
		// two hops on a node share an overlapping (proto, port), so keys stay unique.
		for _, key := range hopCounterKeys(h.Proto, h.ListenPort) {
			m[key] = h
		}
	}
	return m, nil
}

// hopCounterKeys returns the proto/port counter keys a hop may receive samples
// under. The daemon reports a kernel aggregate sample under the literal proto
// (e.g. "tcp+udp/port" via th dport) and, when forward.Partition splits the
// hop, separate per-namespace samples (tcp userspace + udp kernel). So a
// tcp+udp hop fans in to tcp+udp, tcp, and udp; anything else uses its own
// proto only. Derived from protoNamespaces so its key set stays a subset of
// overlappingProtos — cross-proto port occupancy then guarantees no two hops on
// a node ever produce the same key.
func hopCounterKeys(proto string, port int) []string {
	seen := map[string]bool{}
	var protos []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			protos = append(protos, p)
		}
	}
	add(proto)
	for _, ns := range protoNamespaces(proto) {
		add(ns)
	}
	out := make([]string, len(protos))
	for i, p := range protos {
		out[i] = fmt.Sprintf("%s/%d", p, port)
	}
	return out
}

func ListRuleHopsByNode(d *sql.DB, nodeID int64) ([]*RuleHop, error) {
	return queryAll(d, `SELECT `+ruleHopCols+` FROM rule_hops WHERE node_id=? ORDER BY rule_id, position`, scanRuleHop, nodeID)
}

func ListRuleHopsByCompositeNode(d *sql.DB, compositeNodeID int64) ([]*RuleHop, error) {
	return queryAll(d, `SELECT `+ruleHopCols+` FROM rule_hops WHERE rule_id IN (SELECT id FROM rules WHERE node_id=?) ORDER BY rule_id, position`, scanRuleHop, compositeNodeID)
}

// Audit

func WriteAudit(d *sql.DB, userID int64, action, target, payload string) {
	_, _ = d.Exec(`INSERT INTO audit_logs(user_id, action, target, payload, at) VALUES (?,?,?,?,?)`,
		userID, action, target, payload, now())
}

// NodeIDsByNames resolves a slice of node names to their database IDs. Names
// that do not exist in the nodes table are silently omitted from the result so
// the caller can report them as skipped rather than treating them as errors.
func NodeIDsByNames(d *sql.DB, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}
	for _, name := range names {
		var id int64
		err := d.QueryRow(`SELECT id FROM nodes WHERE name = ?`, name).Scan(&id)
		if err != nil {
			continue
		}
		out[name] = id
	}
	return out, nil
}

// Agent-dialer helpers

// UpsertSelfNode ensures the panel's built-in self-node row exists. The partial
// unique index idx_nodes_self guarantees there is at most one row with
// node_type='self', so re-running on every boot is safe.
//
// self 节点没有 token（secret 为空）：它不从对外入口接入，而是由本机 daemon 经面板的
// 本机入口接入，在那里按入口认定。它的在线状态与别的节点一样来自连接
// （MarkNodeOnline / MarkNodeOffline），这里不写。
func UpsertSelfNode(d *sql.DB) (*Node, error) {
	_, err := d.Exec(`
		INSERT INTO nodes (name, address, secret, node_type, online, last_seen, created_at)
		VALUES ('self', '', '', 'self', 0, 0, ?)
		ON CONFLICT(node_type) WHERE node_type='self' DO NOTHING`,
		now())
	if err != nil {
		return nil, err
	}
	// Set owner_id to first admin if not yet assigned.
	_, _ = d.Exec(`UPDATE nodes SET owner_id = (
		SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1
	) WHERE node_type = 'self' AND owner_id IS NULL`)
	return GetSelfNode(d)
}

// GetSelfNode returns the panel's built-in self-node row.
func GetSelfNode(d *sql.DB) (*Node, error) {
	return scanNode(d.QueryRow(`SELECT ` + nodeCols + ` FROM nodes WHERE node_type='self'`))
}

// MarkAllNodesOffline 把每个节点的 online 置 0。nodes.online 是面板进程里连接表的
// 镜像（连接登记时置 1，注销时置 0），而连接表在进程启动时是空的；上一个进程若没
// 走到注销就退出（崩溃、被杀），它留下的 online=1 在新进程里就是假的，所以启动时
// 整体清一次。组合的在线状态由 NodeCatalog.Derive 按成员算，不读组合自己的这一列，
// 清零对它们没有影响。
func MarkAllNodesOffline(d *sql.DB) error {
	_, err := d.Exec(`UPDATE nodes SET online=0 WHERE online<>0`)
	return err
}

// MarkNodeOnline records a successful hello/heartbeat from an agent and
// refreshes the reported binary version and connect IP.
func MarkNodeOnline(d *sql.DB, id int64, agentVersion, agentSHA, connectIP string) error {
	return updateMachine(d, id, `online=1, last_seen=?, agent_version=?, agent_sha=?, last_error=NULL, address=?`,
		now(), agentVersion, agentSHA, connectIP)
}

// MarkNodeOffline flips a machine back to offline when its websocket drops; we
// keep last_seen as-is so the UI can still show "last seen N minutes ago".
func MarkNodeOffline(d *sql.DB, id int64) error {
	return updateMachine(d, id, `online=0`)
}

// MarkNodeApplied stamps last_apply_at and clears last_error after a
// successful dispatch. Templates render "已同步" when last_apply_at is
// set and last_error is empty. warning carries a non-fatal, dispatch-level
// caveat (e.g. some rules skipped) so the UI can flag "applied, but..." —
// pass "" when the apply had nothing to warn about.
func MarkNodeApplied(d *sql.DB, id int64, warning string) error {
	return updateMachine(d, id, `last_apply_at=?, last_error=NULL, last_warning=?`, now(), warning)
}

// MarkNodeDispatchError records a dispatch failure so the panel UI can
// flag the machine as out-of-sync. last_apply_at is deliberately not touched
// — the admin needs to see both "last successful apply was at T" and
// "but the most recent attempt failed with msg". A newer failed attempt
// supersedes any prior success's skip-state, so last_warning is cleared
// and only the error (red) is shown.
func MarkNodeDispatchError(d *sql.DB, id int64, msg string) error {
	return updateMachine(d, id, `last_error=?, last_warning=''`, msg)
}

func ToggleNode(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET disabled = CASE WHEN disabled = 0 THEN 1 ELSE 0 END WHERE id = ?`, id)
	return err
}
