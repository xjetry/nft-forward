package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

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
	ExpiresAt         sql.NullInt64  `json:"expires_at"`
	// RuleCount is not a users-table column; it is filled by FillUserRuleCounts
	// so the user list can show used/total rule quota.
	RuleCount int `json:"rule_count"`
}

type Node struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	NodeType  string         `json:"node_type"`
	OwnerID   *int64         `json:"owner_id,omitempty"`
	Address   string         `json:"address"`
	Secret    string         `json:"secret"`
	RelayHost string         `json:"relay_host"`
	Online    int            `json:"online"`
	AgentVersion string      `json:"agent_version"`
	LastSeen     *int64      `json:"last_seen,omitempty"`
	LastApplyAt  sql.NullInt64 `json:"last_apply_at"`
	LastError    sql.NullString `json:"last_error"`
	Disabled     bool         `json:"disabled"`
	LocalMigratedAt *int64   `json:"local_migrated_at,omitempty"`
	PortRange    string       `json:"port_range"`
	CreatedAt    int64        `json:"created_at"`
	LastUpgradeAt      sql.NullInt64 `json:"last_upgrade_at"`
	LastUpgradeVersion string        `json:"last_upgrade_version,omitempty"`
	LastUpgradeStatus  string        `json:"last_upgrade_status,omitempty"`
	LastUpgradeError   string        `json:"last_upgrade_error,omitempty"`
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
	Disabled        bool          `json:"disabled"`
	CreatedAt       int64         `json:"created_at"`
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
	Comment    string `json:"comment"`
	LastBytes  int64  `json:"last_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

func RandToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
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
	if err := r.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &disabled, &u.DisableReason, &u.MaxForwards, &u.TrafficQuotaBytes, &u.TrafficUsedBytes, &u.ExpiresAt); err != nil {
		return nil, err
	}
	u.Disabled = disabled == 1
	return u, nil
}

const userCols = `id, username, pw_hash, role, disabled, disable_reason, max_forwards, traffic_quota_bytes, traffic_used_bytes, expires_at`

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
		token, userID, time.Now().Add(ttl).Unix(), now())
	if err != nil {
		return "", err
	}
	return token, nil
}

const userColsQualified = `u.id, u.username, u.pw_hash, u.role, u.disabled, u.disable_reason, u.max_forwards, u.traffic_quota_bytes, u.traffic_used_bytes, u.expires_at`

func GetSessionUser(d *sql.DB, token string) (*User, error) {
	return scanUser(d.QueryRow(`
		SELECT `+userColsQualified+`
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > strftime('%s','now')`, token))
}

func DeleteSession(d *sql.DB, token string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE token = ?`, token)
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

func CreateNode(d *sql.DB, name, address, secret string) (*Node, error) {
	if secret == "" {
		secret = RandToken(32)
	}
	res, err := d.Exec(`INSERT INTO nodes(name, address, secret, created_at) VALUES (?,?,?,?)`,
		name, address, secret, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return GetNode(d, id)
}

const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,online,agent_version,last_seen,last_apply_at,last_error,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error`

func GetNode(d *sql.DB, id int64) (*Node, error) {
	row := d.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

type rowScanner interface{ Scan(...any) error }

func scanNode(r rowScanner) (*Node, error) {
	n := &Node{}
	var disabled int
	var localMigratedAt, lastSeen sql.NullInt64
	var agentVersion sql.NullString
	var ownerID sql.NullInt64
	var luVersion, luStatus, luError sql.NullString
	if err := r.Scan(
		&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
		&n.RelayHost, &n.Online, &agentVersion,
		&lastSeen, &n.LastApplyAt, &n.LastError,
		&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
		&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
	); err != nil {
		return nil, err
	}
	n.Disabled = disabled == 1
	if ownerID.Valid {
		v := ownerID.Int64
		n.OwnerID = &v
	}
	if localMigratedAt.Valid {
		v := localMigratedAt.Int64
		n.LocalMigratedAt = &v
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

func ListNodes(d *sql.DB) ([]*Node, error) {
	return queryAll(d, `SELECT `+nodeCols+` FROM nodes ORDER BY id`, scanNode)
}

// ResolveCompositeOnline derives the Online field of composite nodes from their
// children: a composite is online only when every child is reachable (online
// and not disabled), and offline if any child is offline or it has no children.
// Composite nodes have no agent of their own, so their stored online column is
// always 0 — this aggregation is the only source of truth for their status.
func ResolveCompositeOnline(d *sql.DB, nodes []*Node) {
	hasComposite := false
	for _, n := range nodes {
		if n.NodeType == "composite" {
			hasComposite = true
			break
		}
	}
	if !hasComposite {
		return
	}
	hops, err := ListAllNodeHops(d)
	if err != nil {
		return
	}
	resolveCompositeOnline(nodes, hops)
}

// resolveCompositeOnline is the pure aggregation: a composite is online only
// when it has children and every child is reachable (online and not disabled).
func resolveCompositeOnline(nodes []*Node, hops []*NodeHop) {
	effective := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		effective[n.ID] = n.Online == 1 && !n.Disabled
	}
	children := make(map[int64][]int64)
	for _, h := range hops {
		children[h.NodeID] = append(children[h.NodeID], h.HopNodeID)
	}
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		kids := children[n.ID]
		online := len(kids) > 0
		for _, id := range kids {
			if !effective[id] {
				online = false
				break
			}
		}
		if online {
			n.Online = 1
		} else {
			n.Online = 0
		}
	}
}

func RenameNode(d *sql.DB, id int64, name string) error {
	_, err := d.Exec(`UPDATE nodes SET name=? WHERE id=?`, name, id)
	return err
}

func DeleteNode(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// UpdateNodeRelayHost sets a node's data-plane reachable address (empty clears
// it). Validation of the value (IPv4 / hostname) is the caller's job.
func UpdateNodeRelayHost(d *sql.DB, id int64, relayHost string) error {
	_, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, relayHost, id)
	return err
}

// UpdateNodePortRange sets a node's port_range spec. An empty string resets to
// the default range. Callers must validate with ValidatePortRange first.
func UpdateNodePortRange(d *sql.DB, id int64, portRange string) error {
	if portRange == "" {
		portRange = DefaultPortRange
	}
	_, err := d.Exec(`UPDATE nodes SET port_range=? WHERE id=?`, portRange, id)
	return err
}

// RecordUpgradeResult stores the outcome of the most recent upgrade push to a
// node. status is "acked" (daemon accepted and is restarting) or "error"
// (send/ack failure); errText is empty on acked. It overwrites the previous
// record — only the latest attempt is kept.
func RecordUpgradeResult(d DBTX, nodeID int64, version, status, errText string) error {
	_, err := d.Exec(
		`UPDATE nodes SET last_upgrade_at=?, last_upgrade_version=?, last_upgrade_status=?, last_upgrade_error=? WHERE id=?`,
		now(), version, status, errText, nodeID)
	return err
}

// Rules

const ruleCols = `id,node_id,owner_id,name,proto,exit_host,exit_port,entry_listen_port,comment,disabled,created_at`

func scanRule(r rowScanner) (*Rule, error) {
	rl := &Rule{}
	var disabled int
	if err := r.Scan(&rl.ID, &rl.NodeID, &rl.OwnerID, &rl.Name, &rl.Proto, &rl.ExitHost, &rl.ExitPort, &rl.EntryListenPort, &rl.Comment, &disabled, &rl.CreatedAt); err != nil {
		return nil, err
	}
	rl.Disabled = disabled == 1
	return rl, nil
}

const ruleHopCols = `id,rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment,last_bytes,total_bytes`

func scanRuleHop(r rowScanner) (*RuleHop, error) {
	h := &RuleHop{}
	if err := r.Scan(&h.ID, &h.RuleID, &h.Position, &h.NodeID, &h.Proto, &h.ListenPort, &h.TargetHost, &h.TargetPort, &h.Mode, &h.Comment, &h.LastBytes, &h.TotalBytes); err != nil {
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

func ActiveRuleHopsForPush(d *sql.DB, nodeID int64) ([]*RuleHop, error) {
	q := `SELECT ` + ruleHopCols + ` FROM rule_hops rh
		WHERE rh.node_id=?
		AND NOT EXISTS (
		  SELECT 1 FROM rules r
		  WHERE r.id = rh.rule_id AND r.disabled = 1
		)
		AND NOT EXISTS (
		  SELECT 1 FROM rules r JOIN users u ON u.id = r.owner_id
		  WHERE r.id = rh.rule_id
		  AND (u.disabled = 1 OR (u.expires_at IS NOT NULL AND u.expires_at > 0 AND u.expires_at < strftime('%s','now')))
		)
		ORDER BY rh.listen_port`
	return queryAll(d, q, scanRuleHop, nodeID)
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

// AddUserTraffic increments a user's traffic_used_bytes by delta.
func AddUserTraffic(d *sql.DB, id int64, delta int64) error {
	_, err := d.Exec(`UPDATE users SET traffic_used_bytes = traffic_used_bytes + ? WHERE id=?`, delta, id)
	return err
}

// ResetUserTraffic zeroes a user's traffic counter only. Enabling a user is a
// separate action (toggle) so an admin can clear usage without lifting a
// disable, and vice versa.
func ResetUserTraffic(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE users SET traffic_used_bytes = 0 WHERE id=?`, id)
	return err
}

// Audit

func WriteAudit(d *sql.DB, userID int64, action, target, payload string) {
	_, _ = d.Exec(`INSERT INTO audit_logs(user_id, action, target, payload, at) VALUES (?,?,?,?,?)`,
		userID, action, target, payload, now())
}

// Agent-dialer helpers

// UpsertSelfNode ensures the panel's built-in self-node exists and is marked
// online. The partial unique index idx_nodes_self guarantees there is at most
// one row with node_type='self', so re-running on every boot is safe.
func UpsertSelfNode(d *sql.DB) (*Node, error) {
	_, err := d.Exec(`
		INSERT INTO nodes (name, address, secret, node_type, online, last_seen, created_at)
		VALUES ('self', 'unix:///var/run/nft-forward.sock', '', 'self', 1, ?, ?)
		ON CONFLICT(node_type) WHERE node_type='self'
		DO UPDATE SET last_seen=excluded.last_seen, online=1`,
		now(), now())
	if err != nil {
		return nil, err
	}
	// Set owner_id to first admin if not yet assigned.
	_, _ = d.Exec(`UPDATE nodes SET owner_id = (
		SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1
	) WHERE node_type = 'self' AND owner_id IS NULL`)
	row := d.QueryRow(`SELECT ` + nodeCols + ` FROM nodes WHERE node_type='self'`)
	return scanNode(row)
}

// MarkNodeOnline records a successful hello/heartbeat from an agent and
// refreshes the reported binary version and connect IP.
func MarkNodeOnline(d *sql.DB, id int64, agentVersion, connectIP string) error {
	_, err := d.Exec(
		`UPDATE nodes SET online=1, last_seen=?, agent_version=?, last_error=NULL, address=? WHERE id=?`,
		now(), agentVersion, connectIP, id)
	return err
}

// MarkNodeOffline flips a node back to offline when its websocket drops; we
// keep last_seen as-is so the UI can still show "last seen N minutes ago".
func MarkNodeOffline(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET online=0 WHERE id=?`, id)
	return err
}

// MarkNodeApplied stamps last_apply_at and clears last_error after a
// successful dispatch. Templates render "已同步" when last_apply_at is
// set and last_error is empty.
func MarkNodeApplied(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET last_apply_at=?, last_error=NULL WHERE id=?`, now(), id)
	return err
}

// MarkNodeDispatchError records a dispatch failure so the panel UI can
// flag the node as out-of-sync. last_apply_at is deliberately not touched
// — the admin needs to see both "last successful apply was at T" and
// "but the most recent attempt failed with msg".
func MarkNodeDispatchError(d *sql.DB, id int64, msg string) error {
	_, err := d.Exec(`UPDATE nodes SET last_error=? WHERE id=?`, msg, id)
	return err
}

// MarkLocalMigrated stamps nodes.local_migrated_at on the first call; later
// calls are no-ops by design (idempotency anchor for register_local retries).
// Returns (true, nil) when this call did the stamping, (false, nil) when the
// node was already marked. The boolean lets callers distinguish first
// registration from a retried one without an extra SELECT.
func MarkLocalMigrated(d *sql.DB, id int64) (bool, error) {
	res, err := d.Exec(`UPDATE nodes SET local_migrated_at=? WHERE id=? AND local_migrated_at IS NULL`, now(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func ToggleNode(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET disabled = CASE WHEN disabled = 0 THEN 1 ELSE 0 END WHERE id = ?`, id)
	return err
}

func UpdateNodeOwner(d *sql.DB, id int64, ownerID *int64) error {
	_, err := d.Exec(`UPDATE nodes SET owner_id = ? WHERE id = ?`, ownerID, id)
	return err
}
