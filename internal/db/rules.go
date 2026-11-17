package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"

	"nft-forward/internal/chain"
)

// DBTX is satisfied by both *sql.DB and *sql.Tx so rule helpers can run either
// standalone or inside a regeneration transaction.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// DefaultPortRange is the fallback chain-hop port range when a node has no
// explicit port_range. The start/end port picker lives in internal/portutil so
// the daemon can allocate ports without importing this sqlite-backed package.
const DefaultPortRange = "10001-20000"

// ParsePortRange parses a composite port spec like "10001-19999,23333,40000-42000"
// into individual (start, end) ranges. A single port becomes (p, p).
// An empty string is treated as DefaultPortRange.
func ParsePortRange(spec string) ([][2]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = DefaultPortRange
	}
	parts := strings.Split(spec, ",")
	ranges := make([][2]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, "-"); idx >= 0 {
			lo, err := strconv.Atoi(strings.TrimSpace(part[:idx]))
			if err != nil {
				return nil, fmt.Errorf("invalid port %q in range", part[:idx])
			}
			hi, err := strconv.Atoi(strings.TrimSpace(part[idx+1:]))
			if err != nil {
				return nil, fmt.Errorf("invalid port %q in range", part[idx+1:])
			}
			if lo < 1 || lo > 65535 || hi < 1 || hi > 65535 {
				return nil, fmt.Errorf("port out of 1-65535: %s", part)
			}
			if lo > hi {
				return nil, fmt.Errorf("range start %d > end %d", lo, hi)
			}
			ranges = append(ranges, [2]int{lo, hi})
		} else {
			p, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", part)
			}
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("port out of 1-65535: %d", p)
			}
			ranges = append(ranges, [2]int{p, p})
		}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("empty port range")
	}
	return ranges, nil
}

// ValidatePortRange checks format validity and returns a human-readable error.
func ValidatePortRange(spec string) error {
	_, err := ParsePortRange(spec)
	return err
}

// PortInRange returns true if port falls within any of the parsed ranges.
func PortInRange(port int, ranges [][2]int) bool {
	for _, rng := range ranges {
		if port >= rng[0] && port <= rng[1] {
			return true
		}
	}
	return false
}

// PickFreePortFromRange picks a random free port from the given ranges.
// Returns 0 when every port is occupied.
func PickFreePortFromRange(ranges [][2]int, used map[int]bool) int {
	// Calculate total capacity across all ranges
	total := 0
	for _, rng := range ranges {
		total += rng[1] - rng[0] + 1
	}
	if total <= 0 {
		return 0
	}

	offset := rand.Intn(total)
	for i := 0; i < total; i++ {
		idx := (offset + i) % total
		// Map linear index to a port within ranges
		cur := 0
		for _, rng := range ranges {
			span := rng[1] - rng[0] + 1
			if idx < cur+span {
				p := rng[0] + (idx - cur)
				if !used[p] {
					return p
				}
				break
			}
			cur += span
		}
	}
	return 0
}

// storedProtos enumerates the proto values rule_hops.proto can hold (enforced by
// the table's CHECK constraint). overlappingProtos walks it to find conflicts.
var storedProtos = []string{"tcp", "udp", "tcp+udp"}

// protoNamespaces returns the L4 transport namespaces a forward proto occupies:
// tcp+udp spans both tcp and udp; tcp and udp each span only their own. This is
// the single source of truth that keeps port-occupancy overlap and counter-key
// fan-out (CounterProtos) from drifting apart, and mirrors the split
// forward.Partition performs on the daemon.
func protoNamespaces(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}

// overlappingProtos returns every stored rule_hops.proto that competes for the
// same listen port as proto: any stored proto whose namespaces intersect this
// one's. So tcp+udp conflicts with tcp, udp, and tcp+udp; a plain tcp hop with
// tcp and tcp+udp; likewise for udp. The server uses this so it never hands out
// a port the daemon's forward.Partition would later reject as overlapping.
func overlappingProtos(proto string) []string {
	want := map[string]bool{}
	for _, ns := range protoNamespaces(proto) {
		want[ns] = true
	}
	var out []string
	for _, cand := range storedProtos {
		for _, ns := range protoNamespaces(cand) {
			if want[ns] {
				out = append(out, cand)
				break
			}
		}
	}
	if len(out) == 0 {
		// Unknown proto (not in storedProtos): fall back to self so the IN clause
		// is never empty.
		out = []string{proto}
	}
	return out
}

// OccupiedPortsOnNode returns every listen port held on (node, proto) in the
// rule_hops table. excludeRuleID>0 drops that rule's own hops so a rule
// regenerating in place doesn't see itself as occupying its ports.
func OccupiedPortsOnNode(d DBTX, nodeID int64, proto string, excludeRuleID int64) (map[int]bool, error) {
	protos := overlappingProtos(proto)
	placeholders := make([]string, len(protos))
	args := make([]any, 0, len(protos)+2)
	args = append(args, nodeID)
	for i, p := range protos {
		placeholders[i] = "?"
		args = append(args, p)
	}
	args = append(args, excludeRuleID)
	q := `SELECT listen_port FROM rule_hops WHERE node_id=? AND proto IN (` +
		strings.Join(placeholders, ",") + `) AND (rule_id IS NULL OR rule_id<>?)`
	out := map[int]bool{}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// hostPort joins a relay host / exit host with a port for display + targets.
func exitIsIPv6(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// EntryAddresses resolves a rule's entry_family against a node's relay
// addresses. entry is the primary address (v4 by default, so existing single
// address consumers like RelayURI rewriting keep working unchanged); entryV6
// is only non-empty for family "both", carrying the second address. A family
// whose relay address is missing yields an empty string for that slot rather
// than a malformed ":port" — the write path validates presence up front, but
// the read path can race a since-cleared node field.
func EntryAddresses(family, relayHost, relayHostV6 string, port int) (entry, entryV6 string) {
	switch family {
	case "v6":
		if relayHostV6 == "" {
			return "", ""
		}
		return hostPort(relayHostV6, port), ""
	case "both":
		if relayHostV6 == "" {
			return hostPort(relayHost, port), ""
		}
		return hostPort(relayHost, port), hostPort(relayHostV6, port)
	default:
		return hostPort(relayHost, port), ""
	}
}

// CountV6EntryRulesOnNode counts rules whose entry hop runs on the node and
// whose entry family requires the node's IPv6 relay address. Guards the
// relay_host_v6 clear: RegenerateRule rejects such rules once the address is
// gone, which would block every subsequent edit of them.
func CountV6EntryRulesOnNode(d DBTX, nodeID int64) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM rules r
		JOIN rule_hops h ON h.rule_id = r.id AND h.position = 0
		WHERE h.node_id = ? AND r.entry_family IN ('v6','both')`, nodeID).Scan(&n)
	return n, err
}

// HopInput is one ordered hop the caller wants the rule to have. Mode is the
// requested data plane (udp coerces every hop to kernel). DesiredPort, when >0,
// pins this hop's listen_port to an explicit value instead of the
// keep-or-reallocate default; it must be in range and free or RegenerateRule
// fails. Comment, when non-empty, is a user override stored on the hop and
// preserved across future regenerations; empty keeps whatever the hop already
// had, falling back to a generated label.
type HopInput struct {
	NodeID      int64
	Mode        string
	DesiredPort int
	Comment     string
	// LogicalNodeID is the logical-segment node this hop belongs to, written to
	// rule_hops.logical_node_id. RegenerateRule falls back to the rule's node_id
	// when a caller leaves this 0, so no code path can write 0 provenance.
	LogicalNodeID int64
}

// encodeViaNodeIDs/decodeViaNodeIDs marshal the rule's middle-layer path for
// the TEXT column; a broken value decodes to an empty path rather than erroring
// (the chain snapshot in rule_hops still drives the data plane).
func encodeViaNodeIDs(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeViaNodeIDs(s string) []int64 {
	var ids []int64
	if s == "" || json.Unmarshal([]byte(s), &ids) != nil {
		return nil
	}
	return ids
}

// CreateRule inserts the rule header; hops are written by RegenerateRule.
// entry_listen_port starts at 0 until the first regeneration. Callers that
// don't set EntryFamily (e.g. the agent WS create path) get the "v4" default
// normalized here rather than relying on the column's DEFAULT, since the
// INSERT always supplies the column explicitly.
func CreateRule(d DBTX, r *Rule) (int64, error) {
	if r.EntryFamily == "" {
		r.EntryFamily = "v4"
	}
	res, err := d.Exec(`INSERT INTO rules(node_id,owner_id,name,proto,exit_host,exit_port,comment,created_at,entry_family,via_node_ids) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.NodeID, r.OwnerID, r.Name, r.Proto, r.ExitHost, r.ExitPort, r.Comment, now(), r.EntryFamily, encodeViaNodeIDs(r.ViaNodeIDs))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetRule(d DBTX, id int64) (*Rule, error) {
	return scanRule(d.QueryRow(`SELECT `+ruleCols+` FROM rules WHERE id=?`, id))
}

// UpdateRuleHeader 写规则表头里由发起方填写的字段：名称、协议、目标、备注。
//
// 表头里由链决定的字段不在这里写：逻辑路径（node_id、via_node_ids）与入口监听端口
// （entry_listen_port）都由 RegenerateRule 随跳行一起写。换入口、换中转层是换一条
// 链，做法是用新的跳调 RegenerateRule，而不是改这两列——只改列不改跳，表头就与
// 实际在转发的那条链说的不是一回事了。
func UpdateRuleHeader(d DBTX, r *Rule) error {
	if r.EntryFamily == "" {
		r.EntryFamily = "v4"
	}
	_, err := d.Exec(`UPDATE rules SET name=?,proto=?,exit_host=?,exit_port=?,comment=?,entry_family=? WHERE id=?`,
		r.Name, r.Proto, r.ExitHost, r.ExitPort, r.Comment, r.EntryFamily, r.ID)
	return err
}

func listRulesWhere(d *sql.DB, where string, args ...any) ([]*Rule, error) {
	q := `SELECT ` + ruleCols + ` FROM rules`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY id`
	return queryAll(d, q, scanRule, args...)
}

func ListAllRules(d *sql.DB) ([]*Rule, error) {
	return listRulesWhere(d, "")
}

func ListRulesByUser(d *sql.DB, userID int64) ([]*Rule, error) {
	return listRulesWhere(d, "owner_id=?", userID)
}

// ListRulesByOwnerIDs returns rules whose owner_id matches any of the given IDs.
// If ids is empty it falls back to returning all rules.
func ListRulesByOwnerIDs(d *sql.DB, ids []int64) ([]*Rule, error) {
	if len(ids) == 0 {
		return ListAllRules(d)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return listRulesWhere(d, "owner_id IN ("+strings.Join(placeholders, ",")+")", args...)
}

// RulesByID returns all rules keyed by ID in a single query.
func RulesByID(d *sql.DB) (map[int64]*Rule, error) {
	all, err := listRulesWhere(d, "")
	if err != nil {
		return nil, err
	}
	m := make(map[int64]*Rule, len(all))
	for _, r := range all {
		m[r.ID] = r
	}
	return m, nil
}

// ListRuleHops returns hops for a rule ordered by position.
func ListRuleHops(d DBTX, ruleID int64) ([]*RuleHop, error) {
	return queryAll(d, `SELECT `+ruleHopCols+` FROM rule_hops WHERE rule_id=? ORDER BY position`, scanRuleHop, ruleID)
}

// DeleteRule removes a rule and returns the node IDs whose kernel state must be
// re-dispatched (i.e. the nodes its hops lived on). The ON DELETE CASCADE on
// rule_hops clears the hop rows; we collect nodes first so the caller can
// re-push them after the rules are gone.
func DeleteRule(d DBTX, id int64) ([]int64, error) {
	nodes, err := queryInt64s(d, `SELECT DISTINCT node_id FROM rule_hops WHERE rule_id=?`, id)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM rules WHERE id=?`, id); err != nil {
		return nil, err
	}
	return nodes, nil
}

// CountRulesForUser 数 userID 名下的规则条数，是账号的条数上限（users.max_forwards）
// 比较的那个数：一条规则占一个名额，与它的链有几跳无关。
//
// 接受事务：上限检查与插入要在同一个事务里（见 internal/rulechange）。
func CountRulesForUser(d DBTX, userID int64) (int, error) {
	return count(d, `SELECT COUNT(*) FROM rules WHERE owner_id=?`, userID)
}

// CountAllRules returns the total number of rules.
func CountAllRules(d *sql.DB) (int, error) {
	return count(d, `SELECT COUNT(*) FROM rules`)
}

// RuleCountByNode returns rule counts grouped by entry node, for the dashboard's
// per-node rule tally without shipping the whole rules list to the client.
func RuleCountByNode(d *sql.DB) (map[int64]int, error) {
	rows, err := d.Query(`SELECT node_id, COUNT(*) FROM rules GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int{}
	for rows.Next() {
		var nodeID int64
		var c int
		if err := rows.Scan(&nodeID, &c); err != nil {
			return nil, err
		}
		m[nodeID] = c
	}
	return m, rows.Err()
}

// TotalRuleTrafficBytes 是全部规则加权流量（rules.weighted_bytes）之和，供仪表盘
// 显示总量。
func TotalRuleTrafficBytes(d *sql.DB) (int64, error) {
	var total int64
	err := d.QueryRow(`SELECT COALESCE(SUM(weighted_bytes),0) FROM rules`).Scan(&total)
	return total, err
}

// DeleteRulesForUserNode removes the rules a user owns that enter at nodeID
// (rule.node_id = nodeID, covering both single-node rules and composite rules
// whose composite node is nodeID) and returns the physical node IDs whose
// kernel state must be re-pushed. Used when a node grant is revoked so the
// user's forwarding stops instead of lingering. rule_hops cascade-delete.
func DeleteRulesForUserNode(d DBTX, userID, nodeID int64) ([]int64, error) {
	// A rule "uses" a granted logical node when the node is its entry OR one of
	// its middle layers — both surface as rule_hops.logical_node_id == nodeID (the
	// entry segment's hops carry logical_node_id = the rule's node_id). Matching on
	// logical_node_id, not rules.node_id, tears down a via-only rule too, so revoking
	// a middle-layer grant stops the still-running (still-billing) chain.
	ruleIDs, err := queryInt64s(d, `SELECT DISTINCT r.id FROM rules r JOIN rule_hops rh ON rh.rule_id=r.id WHERE r.owner_id=? AND rh.logical_node_id=?`, userID, nodeID)
	if err != nil {
		return nil, err
	}
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	ph, args := placeholderList(ruleIDs)
	nodes, err := queryInt64s(d, `SELECT DISTINCT node_id FROM rule_hops WHERE rule_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM rules WHERE id IN (`+ph+`)`, args...); err != nil {
		return nil, err
	}
	return nodes, nil
}

// DeleteRulesUsingNode removes every rule that runs through nodeID — as a
// physical hop, as its entry, or as a middle layer — and returns the OTHER
// physical nodes those rules touched, so their kernel state can be re-pushed
// after the node (and its rules) are gone. Used when a node is deleted: a rule
// can't keep running through a node that no longer exists, and a composite's id
// never appears in rule_hops.node_id, so an FK cascade alone would leave the
// composite's physical children carrying stale kernel rules.
func DeleteRulesUsingNode(d DBTX, nodeID int64) ([]int64, error) {
	ruleIDs, err := queryInt64s(d, `SELECT DISTINCT rule_id FROM rule_hops WHERE node_id=? OR logical_node_id=?`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	ph, args := placeholderList(ruleIDs)
	// Sibling physical nodes to re-push, excluding the node being deleted (its
	// kernel state goes away with it).
	nodes, err := queryInt64s(d, `SELECT DISTINCT node_id FROM rule_hops WHERE rule_id IN (`+ph+`) AND node_id<>?`, append(append([]any{}, args...), nodeID)...)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM rules WHERE id IN (`+ph+`)`, args...); err != nil {
		return nil, err
	}
	return nodes, nil
}

// placeholderList builds an "?,?,..." fragment and the matching args slice for
// an IN clause over int64 ids.
func placeholderList(ids []int64) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

// RulesReferencingNode returns the distinct rule IDs that have a hop on the
// given node. Rule hops bake the next hop's relay_host into their target,
// so when a node's relay_host changes or the node is removed, every rule it
// participates in must be re-materialized.
func RulesReferencingNode(d DBTX, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `SELECT DISTINCT rule_id FROM rule_hops WHERE node_id=?`, nodeID)
}

// FillUserRuleCounts sets each user's RuleCount to the number of rules they own,
// for showing used/total rule quota against MaxForwards.
func FillUserRuleCounts(d DBTX, users []*User) error {
	if len(users) == 0 {
		return nil
	}
	rows, err := d.Query(`SELECT owner_id, COUNT(*) FROM rules WHERE owner_id IS NOT NULL GROUP BY owner_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	countByUser := map[int64]int{}
	for rows.Next() {
		var ownerID int64
		var n int
		if err := rows.Scan(&ownerID, &n); err != nil {
			return err
		}
		countByUser[ownerID] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range users {
		u.RuleCount = countByUser[u.ID]
	}
	return nil
}

// A ChainError says that a rule's chain cannot be built as asked: a hop names
// a node that does not exist or has no relay address, a requested port is
// taken, a node is not allowed in that position. What has to change is the
// request or the nodes' configuration, so callers answer it as the caller's
// error and show the message. Every other error from deriving or generating
// a chain is a statement that failed, which says nothing about the chain.
type ChainError struct{ msg string }

func (e *ChainError) Error() string { return e.msg }

// ChainErrorf builds a ChainError.
func ChainErrorf(format string, a ...any) error {
	return &ChainError{msg: fmt.Sprintf(format, a...)}
}

// IsChainError reports whether err is, or wraps, a ChainError.
func IsChainError(err error) bool {
	var ce *ChainError
	return errors.As(err, &ce)
}

// ErrDuplicateChainNode is the ChainError RegenerateRule returns when the
// resolved chain visits the same physical node twice (e.g. a composite entry
// and a via sharing a machine). It's a configuration conflict between the
// entry and via selections, not a malformed request, so callers surfacing it
// over HTTP should map it to 409 rather than 400.
var ErrDuplicateChainNode error = &ChainError{msg: "同一节点不能在链路中重复"}

// nodeLabel names a node inside an error message. The caller is already
// returning an error of its own, so a name that cannot be read is replaced by
// the node's id: a second error here would only hide the first.
func nodeLabel(tx DBTX, nodeID int64) string {
	var name string
	if err := tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, nodeID).Scan(&name); err != nil {
		return fmt.Sprintf("#%d", nodeID)
	}
	return name
}

// LogicalPath 从一条物理链各跳所属的逻辑节点（segments[i] 是第 i 跳的，与
// rule_hops.logical_node_id 同义）算出规则表头里的逻辑路径：相邻且属于同一个逻辑节点的
// 跳是一段，第一段的逻辑节点是入口（rules.node_id），其余各段依次是中转层
// （rules.via_node_ids）。
//
// 一个逻辑节点只能占一段。它的跳被别的段隔开时，表头里它会出现两次，“这个逻辑节点
// 的段首”——按授权计流量只在段首记一次（accounting 的 bookSQL 取同一逻辑节点里
// 位置最小的那一跳）——也不再对应一段的开头。这样的链返回 ChainError。
func LogicalPath(segments []int64) (entry int64, vias []int64, err error) {
	if len(segments) == 0 {
		return 0, nil, ChainErrorf("链路至少需要一跳")
	}
	seen := map[int64]bool{}
	for i, id := range segments {
		if i > 0 && id == segments[i-1] {
			continue
		}
		if seen[id] {
			return 0, nil, ChainErrorf("节点 %d 的各跳必须相邻：同一个逻辑节点在链上只能占一段", id)
		}
		seen[id] = true
		if i == 0 {
			entry = id
		} else {
			vias = append(vias, id)
		}
	}
	return entry, vias, nil
}

// RegenerateRule rewrites rule r's hops for the given ordered hops and returns
// the copyable entry endpoint, the secondary v6 endpoint (non-empty only for
// entry_family "both"), plus the set of nodes whose kernel state must be
// re-dispatched (current hops + previously-touched nodes). Ports are kept
// stable per node across edits; avoid[nodeID]=port forces that node off a
// given port (used by the reallocate-on-conflict flow).
//
// Structural validation only: relay_host present, no repeated node,
// port-range exhaustion, udp=>kernel. Policy (grant ownership, exit CIDR,
// quota) is the caller's responsibility.
//
// 表头里的逻辑路径（rules.node_id、rules.via_node_ids）也在这里写，由这次的跳算出
// （LogicalPath），r 上的两个字段同步成写进去的值。跳行只在本函数里插入，这两列在
// 规则建成之后也只在这里写（UpdateRuleHeader 不写它们），所以经本函数生成过的规则
// 都满足“表头的逻辑路径 = 跳行按 logical_node_id 分成的段”，不依赖调用方记得去改表头：
// 按入口与中转层推导的链、管理员直接给的物理链、删掉一台节点后在剩下的跳上重接
// （rewireRules），走的都是这一处。此前留下的规则由迁移 0053 按同一算法重算一次，
// 第一跳的逻辑节点已经不存在的除外。
func RegenerateRule(tx DBTX, r *Rule, hops []HopInput, avoid map[int64]int) (string, string, []int64, error) {
	if len(hops) == 0 {
		return "", "", nil, ChainErrorf("链路至少需要一跳")
	}

	type resolved struct {
		nodeID        int64
		relayHost     string
		relayHostV6   string
		mode          string
		desiredPort   int
		comment       string
		portRanges    [][2]int // parsed from the node's port_range column
		logicalNodeID int64
	}
	rs := make([]resolved, len(hops))
	seen := map[int64]bool{}
	for i, hop := range hops {
		if seen[hop.NodeID] {
			return "", "", nil, ErrDuplicateChainNode
		}
		seen[hop.NodeID] = true

		var name, relay, relayV6, portRange string
		err := tx.QueryRow(`SELECT name, relay_host, relay_host_v6, port_range FROM nodes WHERE id=?`, hop.NodeID).Scan(&name, &relay, &relayV6, &portRange)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil, ChainErrorf("节点 %d 不存在", hop.NodeID)
		}
		if err != nil {
			return "", "", nil, err
		}
		if relay == "" {
			return "", "", nil, ChainErrorf("节点 %s 未设置中继地址", name)
		}
		ranges, err := ParsePortRange(portRange)
		if err != nil {
			return "", "", nil, ChainErrorf("节点 %s 端口范围格式错误: %v", name, err)
		}
		// 没写 LogicalNodeID 的跳归到规则现有的入口节点（r.NodeID）那一段。这是缺省，不是
		// 另一种含义：一条全不写 LogicalNodeID 的链算出的入口仍是 r.NodeID、没有中转层。
		// r.NodeID 也是 0 时这一跳没有归属，下面的存在性检查会以“节点 0 不存在”拒绝。
		logicalNodeID := hop.LogicalNodeID
		if logicalNodeID == 0 {
			logicalNodeID = r.NodeID
		}
		rs[i] = resolved{nodeID: hop.NodeID, relayHost: relay, relayHostV6: relayV6, desiredPort: hop.DesiredPort, comment: hop.Comment, portRanges: ranges, logicalNodeID: logicalNodeID}
	}

	segments := make([]int64, len(rs))
	for i, h := range rs {
		segments[i] = h.logicalNodeID
	}
	entryNode, viaNodes, err := LogicalPath(segments)
	if err != nil {
		return "", "", nil, err
	}
	// 逻辑节点要真的存在：入口会写进 rules.node_id。物理跳自己的节点上面已经查过，
	// 这里只需再查与它不同的那些（跳属于一个组合时）。rules.node_id 上有外键，但那
	// 只会把这种请求变成一条语句失败；这里把它作为“搭不成的链”说清楚。
	for _, id := range append([]int64{entryNode}, viaNodes...) {
		if seen[id] {
			continue
		}
		var one int
		err := tx.QueryRow(`SELECT 1 FROM nodes WHERE id=?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil, ChainErrorf("节点 %d 不存在", id)
		}
		if err != nil {
			return "", "", nil, err
		}
	}

	if exitIsIPv6(r.ExitHost) && rs[len(rs)-1].relayHostV6 == "" {
		return "", "", nil, ChainErrorf("节点 %s 未设置 IPv6 中继地址，不能转发 IPv6 目标", nodeLabel(tx, rs[len(rs)-1].nodeID))
	}

	// The entry family is derived from what the entry node can serve, not chosen
	// by the caller: a rule exposes an entry endpoint for every IP family its
	// entry node supports. relay_host (v4) is mandatory (checked above), so the
	// only question is whether a v6 entry is also offered. v6 ingress rides the
	// userspace relay (kernel DNAT can't cross address families, and there's no
	// NAT64), which is TCP-only — so a v6 entry is offered only for a plain TCP
	// rule; a udp / tcp+udp rule stays v4-only.
	entryOffersV6 := rs[0].relayHostV6 != "" && r.Proto == "tcp"
	if entryOffersV6 {
		r.EntryFamily = "both"
	} else {
		r.EntryFamily = "v4"
	}

	// 调用方给的各跳模式在这里落成写进 rule_hops 的值：空值的默认、udp 只走内核、
	// 入口对外提供 v6 时入口那一跳走用户态，三条都在 chain.EnforceModes 里。rule_hops
	// 的行只在本函数里插入，所以调用方不需要、也不应该自己先补默认值。
	requested := make([]string, len(hops))
	for i, hop := range hops {
		requested[i] = hop.Mode
	}
	for i, mode := range chain.EnforceModes(r.Proto, entryOffersV6, requested) {
		rs[i].mode = mode
	}

	// Read existing ports (keyed by node) BEFORE deleting so unchanged nodes keep
	// their port — entry endpoint + installed rules don't churn on edits.
	prev, err := ListRuleHops(tx, r.ID)
	if err != nil {
		return "", "", nil, err
	}
	prevPort := map[int64]int{}
	affected := map[int64]bool{}
	prevHopComment := map[int64]string{}
	// keptBytes[i] 是新链第 i 跳从旧行带过来的逐跳流量（rule_hops.total_bytes）。
	//
	// 这个数记在“规则的第几跳、哪台物理节点”这一格上，而下面是把这条规则的跳行整体
	// 删除再插入：不显式带过去，改一次规则名每一跳都会从 0 起。旧行与新行的位置、节点
	// 都相同才带——位置上换了节点、或节点挪了位置，都是另一格，从 0 起。端口、模式、
	// 下一跳地址、所属逻辑段变了不算换格。
	//
	// 一个位置上至多一行旧行：跳行只在本函数里插入，位置取 0..len-1，插入前先删光
	// 这条规则的旧行。
	keptBytes := make([]int64, len(rs))
	for _, h := range prev {
		prevPort[h.NodeID] = h.ListenPort
		affected[h.NodeID] = true
		prevHopComment[h.NodeID] = h.Comment
		if h.Position >= 0 && h.Position < len(rs) && rs[h.Position].nodeID == h.NodeID {
			keptBytes[h.Position] = h.FoldedBytes
		}
	}

	if _, err := tx.Exec(`DELETE FROM rule_hops WHERE rule_id=?`, r.ID); err != nil {
		return "", "", nil, err
	}

	ports := make([]int, len(rs))
	for i, h := range rs {
		occ, err := OccupiedPortsOnNode(tx, h.nodeID, r.Proto, r.ID)
		if err != nil {
			return "", "", nil, err
		}
		if av, ok := avoid[h.nodeID]; ok {
			occ[av] = true
		}
		var p int
		if h.desiredPort > 0 {
			if !PortInRange(h.desiredPort, h.portRanges) {
				return "", "", nil, ChainErrorf("端口 %d 超出节点 %s 允许范围", h.desiredPort, nodeLabel(tx, h.nodeID))
			}
			if occ[h.desiredPort] {
				return "", "", nil, ChainErrorf("端口 %d 在节点 %s 上已被占用", h.desiredPort, nodeLabel(tx, h.nodeID))
			}
			p = h.desiredPort
		} else {
			p = prevPort[h.nodeID]
			if !(PortInRange(p, h.portRanges) && !occ[p]) {
				p = PickFreePortFromRange(h.portRanges, occ)
				if p == 0 {
					return "", "", nil, ChainErrorf("节点 %s 端口范围内无可用端口", nodeLabel(tx, h.nodeID))
				}
			}
		}
		ports[i] = p
	}

	for i, h := range rs {
		var targetHost string
		var targetPort int
		if i < len(rs)-1 {
			targetHost = rs[i+1].relayHost
			targetPort = ports[i+1]
		} else {
			targetHost = r.ExitHost
			targetPort = r.ExitPort
		}
		// Custom comment precedence: explicit edit > preserved from the prior
		// hop row > none. rule_hops.comment stores the custom value (empty =
		// none); a generated label carrying the live position is used as fallback.
		hopComment := h.comment
		if hopComment == "" {
			hopComment = prevHopComment[h.nodeID]
		}
		fwdComment := hopComment
		if fwdComment == "" {
			fwdComment = fmt.Sprintf("链路 %s · 第%d跳", r.Name, i+1)
		}
		if _, err := tx.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment,logical_node_id,total_bytes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, i, h.nodeID, r.Proto, ports[i], targetHost, targetPort, h.mode, fwdComment, h.logicalNodeID, keptBytes[i]); err != nil {
			return "", "", nil, err
		}
		affected[h.nodeID] = true
	}

	// Persist the derived entry_family alongside the port so list/detail reads
	// reflect the family this rule actually serves, not whatever the caller sent.
	// 逻辑路径与它们一起写：见本函数文档。
	if _, err := tx.Exec(`UPDATE rules SET node_id=?, via_node_ids=?, entry_listen_port=?, entry_family=? WHERE id=?`,
		entryNode, encodeViaNodeIDs(viaNodes), ports[0], r.EntryFamily, r.ID); err != nil {
		return "", "", nil, err
	}
	r.NodeID, r.ViaNodeIDs = entryNode, viaNodes
	r.EntryListenPort = ports[0]

	nodes := make([]int64, 0, len(affected))
	for n := range affected {
		nodes = append(nodes, n)
	}
	entry, entryV6 := EntryAddresses(r.EntryFamily, rs[0].relayHost, rs[0].relayHostV6, ports[0])
	return entry, entryV6, nodes, nil
}

// RuleChainNodeIDs returns, per rule, the ordered physical node ids of its
// hops (position order): the flattened chain from the entry to the hop that
// dials the target. A composite segment already appears expanded into its child
// nodes here — rule_hops stores the physical chain captured when the rule was
// built, so this reflects what is actually deployed rather than the composite's
// current (possibly since-edited) definition.
func RuleChainNodeIDs(d DBTX, ruleIDs []int64) (map[int64][]int64, error) {
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ruleIDs))
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT rule_id, node_id FROM rule_hops WHERE rule_id IN (` +
		strings.Join(placeholders, ",") + `) ORDER BY rule_id, position`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64][]int64, len(ruleIDs))
	for rows.Next() {
		var rid, nid int64
		if err := rows.Scan(&rid, &nid); err != nil {
			return nil, err
		}
		m[rid] = append(m[rid], nid)
	}
	return m, rows.Err()
}

func RuleHopCounts(d DBTX, ruleIDs []int64) (map[int64]int, error) {
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ruleIDs))
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT rule_id, COUNT(*) FROM rule_hops WHERE rule_id IN (` +
		strings.Join(placeholders, ",") + `) GROUP BY rule_id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]int, len(ruleIDs))
	for rows.Next() {
		var rid int64
		var cnt int
		if err := rows.Scan(&rid, &cnt); err != nil {
			return nil, err
		}
		m[rid] = cnt
	}
	return m, rows.Err()
}
