package db

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
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
// into individual (start, end) segments. A single port becomes (p, p).
// An empty string is treated as DefaultPortRange.
func ParsePortRange(spec string) ([][2]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = DefaultPortRange
	}
	parts := strings.Split(spec, ",")
	segments := make([][2]int, 0, len(parts))
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
			segments = append(segments, [2]int{lo, hi})
		} else {
			p, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", part)
			}
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("port out of 1-65535: %d", p)
			}
			segments = append(segments, [2]int{p, p})
		}
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("empty port range")
	}
	return segments, nil
}

// ValidatePortRange checks format validity and returns a human-readable error.
func ValidatePortRange(spec string) error {
	_, err := ParsePortRange(spec)
	return err
}

// PortInRange returns true if port falls within any of the parsed segments.
func PortInRange(port int, segments [][2]int) bool {
	for _, seg := range segments {
		if port >= seg[0] && port <= seg[1] {
			return true
		}
	}
	return false
}

// PickFreePortFromRange picks a random free port from the given segments.
// Returns 0 when every port is occupied.
func PickFreePortFromRange(segments [][2]int, used map[int]bool) int {
	// Calculate total capacity across all segments
	total := 0
	for _, seg := range segments {
		total += seg[1] - seg[0] + 1
	}
	if total <= 0 {
		return 0
	}

	offset := rand.Intn(total)
	for i := 0; i < total; i++ {
		idx := (offset + i) % total
		// Map linear index to a port within segments
		cur := 0
		for _, seg := range segments {
			span := seg[1] - seg[0] + 1
			if idx < cur+span {
				p := seg[0] + (idx - cur)
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

// NormalizeForwardMode keeps the NOT NULL mode column valid: empty or any
// unknown value means kernel. Centralizing it means the kernel default is
// computed in one place across rule creation and the register_local import.
func NormalizeForwardMode(m string) string {
	if m == "userspace" {
		return "userspace"
	}
	return "kernel"
}

// storedProtos enumerates the proto values rule_hops.proto can hold (enforced by
// the table's CHECK constraint). overlappingProtos walks it to find conflicts.
var storedProtos = []string{"tcp", "udp", "tcp+udp"}

// protoNamespaces returns the L4 transport namespaces a forward proto occupies:
// tcp+udp spans both tcp and udp; tcp and udp each span only their own. This is
// the single source of truth that keeps port-occupancy overlap and counter-key
// fan-out (hopCounterKeys) from drifting apart, and mirrors the split
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
}

// CreateRule inserts the rule header; hops are written by RegenerateRule.
// entry_listen_port starts at 0 until the first regeneration.
func CreateRule(d DBTX, r *Rule) (int64, error) {
	res, err := d.Exec(`INSERT INTO rules(node_id,owner_id,name,proto,exit_host,exit_port,comment,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		r.NodeID, r.OwnerID, r.Name, r.Proto, r.ExitHost, r.ExitPort, r.Comment, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetRule(d DBTX, id int64) (*Rule, error) {
	return scanRule(d.QueryRow(`SELECT `+ruleCols+` FROM rules WHERE id=?`, id))
}

// UpdateRuleHeader persists editable header fields (node/name/proto/exit).
// node_id is the logical entry node the rule belongs to; switching it goes
// hand in hand with RegenerateRule rebuilding the hops for the new node.
// entry_listen_port is owned by RegenerateRule and not touched here.
func UpdateRuleHeader(d DBTX, r *Rule) error {
	_, err := d.Exec(`UPDATE rules SET node_id=?,name=?,proto=?,exit_host=?,exit_port=?,comment=? WHERE id=?`,
		r.NodeID, r.Name, r.Proto, r.ExitHost, r.ExitPort, r.Comment, r.ID)
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

// FillRuleTraffic sets each rule's TotalBytes from its entry hop (position=0).
// A composite chain carries the same bytes through every hop but is billed
// once at the entrance, so the entry hop's counter is the rule's traffic;
// summing all hops would multiply it by the hop count.
func FillRuleTraffic(d DBTX, rules []*Rule) error {
	if len(rules) == 0 {
		return nil
	}
	rows, err := d.Query(`SELECT rule_id, total_bytes FROM rule_hops WHERE position=0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	bytesByRule := map[int64]int64{}
	for rows.Next() {
		var ruleID, bytes int64
		if err := rows.Scan(&ruleID, &bytes); err != nil {
			return err
		}
		bytesByRule[ruleID] = bytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range rules {
		r.TotalBytes = bytesByRule[r.ID]
	}
	return nil
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

// ListRuleHopsByChain is an alias for ListRuleHops (used by hub dispatch).
func ListRuleHopsByChain(d DBTX, ruleID int64) ([]*RuleHop, error) {
	return ListRuleHops(d, ruleID)
}

// DeleteRule removes a rule and returns the node IDs whose kernel state must be
// re-dispatched (i.e. the nodes its hops lived on). The ON DELETE CASCADE on
// rule_hops clears the hop rows; we collect nodes first so the caller can
// re-push them after the rules are gone.
func DeleteRule(d *sql.DB, id int64) ([]int64, error) {
	nodes, err := queryInt64s(d, `SELECT DISTINCT node_id FROM rule_hops WHERE rule_id=?`, id)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM rules WHERE id=?`, id); err != nil {
		return nil, err
	}
	return nodes, nil
}

func CountRulesForUser(d *sql.DB, userID int64) (int, error) {
	return count(d, `SELECT COUNT(*) FROM rules WHERE owner_id=?`, userID)
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

// RegenerateRule rewrites rule r's hops for the given ordered hops and returns
// the copyable entry endpoint plus the set of nodes whose kernel state must be
// re-dispatched (current hops + previously-touched nodes). Ports are kept
// stable per node across edits; avoid[nodeID]=port forces that node off a
// given port (used by the reallocate-on-conflict flow).
//
// Structural validation only: relay_host present, no repeated node,
// port-range exhaustion, udp=>kernel. Policy (grant ownership, exit CIDR,
// quota) is the caller's responsibility.
func RegenerateRule(tx DBTX, r *Rule, hops []HopInput, avoid map[int64]int) (string, []int64, error) {
	if len(hops) == 0 {
		return "", nil, fmt.Errorf("链路至少需要一跳")
	}

	type resolved struct {
		nodeID      int64
		relayHost   string
		relayHostV6 string
		mode        string
		desiredPort int
		comment     string
		portSegs    [][2]int // parsed from the node's port_range column
	}
	rs := make([]resolved, len(hops))
	seen := map[int64]bool{}
	for i, hop := range hops {
		if seen[hop.NodeID] {
			return "", nil, fmt.Errorf("同一节点不能在链路中重复")
		}
		seen[hop.NodeID] = true

		var name, relay, relayV6, portRange string
		if err := tx.QueryRow(`SELECT name, relay_host, relay_host_v6, port_range FROM nodes WHERE id=?`, hop.NodeID).Scan(&name, &relay, &relayV6, &portRange); err != nil {
			return "", nil, fmt.Errorf("节点 %d 不存在", hop.NodeID)
		}
		if relay == "" {
			return "", nil, fmt.Errorf("节点 %s 未设置中继地址", name)
		}
		segs, err := ParsePortRange(portRange)
		if err != nil {
			return "", nil, fmt.Errorf("节点 %s 端口范围格式错误: %v", name, err)
		}
		mode := NormalizeForwardMode(hop.Mode)
		if r.Proto == "udp" {
			mode = "kernel" // userspace relay is TCP-only
		}
		rs[i] = resolved{nodeID: hop.NodeID, relayHost: relay, relayHostV6: relayV6, mode: mode, desiredPort: hop.DesiredPort, comment: hop.Comment, portSegs: segs}
	}

	if exitIsIPv6(r.ExitHost) && rs[len(rs)-1].relayHostV6 == "" {
		var name string
		_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, rs[len(rs)-1].nodeID).Scan(&name)
		return "", nil, fmt.Errorf("节点 %s 未设置 IPv6 中继地址，不能转发 IPv6 目标", name)
	}

	// Read existing ports (keyed by node) BEFORE deleting so unchanged nodes keep
	// their port — entry endpoint + installed rules don't churn on edits.
	prev, err := ListRuleHops(tx, r.ID)
	if err != nil {
		return "", nil, err
	}
	prevPort := map[int64]int{}
	affected := map[int64]bool{}
	prevHopComment := map[int64]string{}
	for _, h := range prev {
		prevPort[h.NodeID] = h.ListenPort
		affected[h.NodeID] = true
		prevHopComment[h.NodeID] = h.Comment
	}

	if _, err := tx.Exec(`DELETE FROM rule_hops WHERE rule_id=?`, r.ID); err != nil {
		return "", nil, err
	}

	ports := make([]int, len(rs))
	for i, h := range rs {
		occ, err := OccupiedPortsOnNode(tx, h.nodeID, r.Proto, r.ID)
		if err != nil {
			return "", nil, err
		}
		if av, ok := avoid[h.nodeID]; ok {
			occ[av] = true
		}
		var p int
		if h.desiredPort > 0 {
			var name string
			_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
			if !PortInRange(h.desiredPort, h.portSegs) {
				return "", nil, fmt.Errorf("端口 %d 超出节点 %s 允许范围", h.desiredPort, name)
			}
			if occ[h.desiredPort] {
				return "", nil, fmt.Errorf("端口 %d 在节点 %s 上已被占用", h.desiredPort, name)
			}
			p = h.desiredPort
		} else {
			p = prevPort[h.nodeID]
			if !(PortInRange(p, h.portSegs) && !occ[p]) {
				p = PickFreePortFromRange(h.portSegs, occ)
				if p == 0 {
					var name string
					_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
					return "", nil, fmt.Errorf("节点 %s 端口范围内无可用端口", name)
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
		if _, err := tx.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,?,?,?,?,?,?,?,?)`,
			r.ID, i, h.nodeID, r.Proto, ports[i], targetHost, targetPort, h.mode, fwdComment); err != nil {
			return "", nil, err
		}
		affected[h.nodeID] = true
	}

	if _, err := tx.Exec(`UPDATE rules SET entry_listen_port=? WHERE id=?`, ports[0], r.ID); err != nil {
		return "", nil, err
	}
	r.EntryListenPort = ports[0]

	nodes := make([]int64, 0, len(affected))
	for n := range affected {
		nodes = append(nodes, n)
	}
	return hostPort(rs[0].relayHost, ports[0]), nodes, nil
}

// RuleHopCounts returns hop count per rule for the given rule IDs.
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
