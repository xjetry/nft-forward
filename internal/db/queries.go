package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type User struct {
	ID       int64
	Username string
	PwHash   string
	Role     string
	TenantID sql.NullInt64
	Disabled bool
}

type Node struct {
	ID          int64
	Name        string
	Address     string
	Secret      string
	LastSeenAt  sql.NullInt64  // legacy push-era field
	LastApplyAt sql.NullInt64  // legacy push-era field
	LastError   sql.NullString // legacy push-era field
	Disabled    bool
	CreatedAt   int64

	// Agent-dialer model: replaces the periodic-poller liveness view.
	// LocalMigratedAt anchors register_local idempotency; without it a retried
	// register would duplicate-INSERT instead of becoming a no-op. NodeKind
	// distinguishes the panel's built-in self-node so dispatch can short-
	// circuit to the local unix socket without a token round-trip.
	//
	// New nullable columns use *T (not sql.NullT) since they are not surfaced
	// in HTML templates; the legacy fields above must stay sql.Null* because
	// existing templates call .Valid / nullstr on them.
	LocalMigratedAt *int64
	LastSeen        *int64
	Online          int
	AgentVersion    string
	NodeKind        string
}

type Forward struct {
	ID         int64
	NodeID     int64
	TenantID   sql.NullInt64
	TunnelID   sql.NullInt64
	Proto      string
	ListenPort int
	TargetIP   string
	TargetPort int
	Comment    string
	Mode       string
	Disabled   bool
	LastBytes  int64
	TotalBytes int64
	CreatedAt  int64
}

func RandToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
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

func CreateTenantUser(d *sql.DB, tenantID int64, username, pwHash string) (int64, error) {
	res, err := d.Exec(`INSERT INTO users(username, pw_hash, role, tenant_id, created_at) VALUES (?,?,?,?,?)`,
		username, pwHash, "tenant", tenantID, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ListUsers(d *sql.DB) ([]*User, error) {
	rows, err := d.Query(`SELECT id, username, pw_hash, role, tenant_id, disabled FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		var disabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &u.TenantID, &disabled); err != nil {
			return nil, err
		}
		u.Disabled = disabled == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

func SetUserDisabled(d *sql.DB, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := d.Exec(`UPDATE users SET disabled=? WHERE id=?`, v, id)
	return err
}

func DeleteUser(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func CountUsersByTenant(d *sql.DB, tenantID int64) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE tenant_id=?`, tenantID).Scan(&n)
	return n, err
}

// DeleteForwardsForTenant removes all forwards owned by a tenant and returns
// the list of node IDs whose ruleset needs to be re-pushed.
func DeleteForwardsForTenant(d *sql.DB, tenantID int64) ([]int64, error) {
	nodes, err := DistinctTenantNodes(d, tenantID)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM forwards WHERE tenant_id=?`, tenantID); err != nil {
		return nil, err
	}
	return nodes, nil
}

func GetUserByUsername(d *sql.DB, username string) (*User, error) {
	row := d.QueryRow(`SELECT id, username, pw_hash, role, tenant_id, disabled FROM users WHERE username = ?`, username)
	u := &User{}
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &u.TenantID, &disabled); err != nil {
		return nil, err
	}
	u.Disabled = disabled == 1
	return u, nil
}

func GetUserByID(d *sql.DB, id int64) (*User, error) {
	row := d.QueryRow(`SELECT id, username, pw_hash, role, tenant_id, disabled FROM users WHERE id = ?`, id)
	u := &User{}
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &u.TenantID, &disabled); err != nil {
		return nil, err
	}
	u.Disabled = disabled == 1
	return u, nil
}

func CountUsers(d *sql.DB) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
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

func GetSessionUser(d *sql.DB, token string) (*User, error) {
	row := d.QueryRow(`
		SELECT u.id, u.username, u.pw_hash, u.role, u.tenant_id, u.disabled
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > strftime('%s','now')`, token)
	u := &User{}
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &u.TenantID, &disabled); err != nil {
		return nil, err
	}
	u.Disabled = disabled == 1
	return u, nil
}

func DeleteSession(d *sql.DB, token string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE token = ?`, token)
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

const nodeCols = `id,name,address,secret,last_seen_at,last_apply_at,last_error,disabled,created_at,local_migrated_at,last_seen,online,agent_version,node_kind`

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
	if err := r.Scan(
		&n.ID, &n.Name, &n.Address, &n.Secret,
		&n.LastSeenAt, &n.LastApplyAt, &n.LastError,
		&disabled, &n.CreatedAt,
		&localMigratedAt, &lastSeen, &n.Online, &agentVersion, &n.NodeKind,
	); err != nil {
		return nil, err
	}
	n.Disabled = disabled == 1
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
	return n, nil
}

func ListNodes(d *sql.DB) ([]*Node, error) {
	rows, err := d.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func DeleteNode(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// Forwards

const forwardCols = `id,node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,disabled,last_bytes,total_bytes,created_at,mode`

func scanForward(r rowScanner) (*Forward, error) {
	f := &Forward{}
	var disabled int
	if err := r.Scan(&f.ID, &f.NodeID, &f.TenantID, &f.TunnelID, &f.Proto, &f.ListenPort, &f.TargetIP, &f.TargetPort, &f.Comment, &disabled, &f.LastBytes, &f.TotalBytes, &f.CreatedAt, &f.Mode); err != nil {
		return nil, err
	}
	f.Disabled = disabled == 1
	return f, nil
}

// NormalizeForwardMode keeps the NOT NULL mode column valid: empty or any
// unknown value means kernel. Centralizing it means the kernel default is
// computed in one place across CreateForward and the register_local import.
func NormalizeForwardMode(m string) string {
	if m == "userspace" {
		return "userspace"
	}
	return "kernel"
}

func CreateForward(d *sql.DB, f *Forward) (int64, error) {
	res, err := d.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		f.NodeID, f.TenantID, f.TunnelID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, now(), NormalizeForwardMode(f.Mode))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetForward(d *sql.DB, id int64) (*Forward, error) {
	row := d.QueryRow(`SELECT `+forwardCols+` FROM forwards WHERE id=?`, id)
	return scanForward(row)
}

func GetForwardByNodeProtoPort(d *sql.DB, nodeID int64, proto string, port int) (*Forward, error) {
	row := d.QueryRow(`SELECT `+forwardCols+` FROM forwards WHERE node_id=? AND proto=? AND listen_port=?`,
		nodeID, proto, port)
	return scanForward(row)
}

func DeleteForward(d *sql.DB, id int64) (int64, error) {
	var nodeID int64
	if err := d.QueryRow(`SELECT node_id FROM forwards WHERE id=?`, id).Scan(&nodeID); err != nil {
		return 0, err
	}
	if _, err := d.Exec(`DELETE FROM forwards WHERE id=?`, id); err != nil {
		return 0, err
	}
	return nodeID, nil
}

func listForwardsWhere(d *sql.DB, where string, args ...any) ([]*Forward, error) {
	q := `SELECT ` + forwardCols + ` FROM forwards`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY node_id, listen_port`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Forward
	for rows.Next() {
		f, err := scanForward(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func ListForwards(d *sql.DB) ([]*Forward, error) {
	return listForwardsWhere(d, "")
}

func ListForwardsByNode(d *sql.DB, nodeID int64) ([]*Forward, error) {
	return listForwardsWhere(d, "node_id=? AND disabled=0", nodeID)
}

// ActiveForwardsForPush returns forwards eligible to be installed on a node.
// Forwards belonging to a disabled or expired tenant are excluded so the
// kernel state reflects current quota/expiry decisions.
func ActiveForwardsForPush(d *sql.DB, nodeID int64) ([]*Forward, error) {
	q := `SELECT ` + forwardCols + ` FROM forwards f
		WHERE f.node_id=? AND f.disabled=0
		AND NOT EXISTS (
		  SELECT 1 FROM tenants t
		  WHERE t.id = f.tenant_id
		  AND (t.disabled = 1 OR (t.expires_at IS NOT NULL AND t.expires_at > 0 AND t.expires_at < strftime('%s','now')))
		)
		ORDER BY listen_port`
	rows, err := d.Query(q, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Forward
	for rows.Next() {
		f, err := scanForward(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func ListForwardsForTenant(d *sql.DB, tenantID int64) ([]*Forward, error) {
	return listForwardsWhere(d, "tenant_id=?", tenantID)
}

func CountForwardsForTenant(d *sql.DB, tenantID int64) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM forwards WHERE tenant_id=?`, tenantID).Scan(&n)
	return n, err
}

func CountForwardsForTenantTunnel(d *sql.DB, tenantID, tunnelID int64) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM forwards WHERE tenant_id=? AND tunnel_id=?`, tenantID, tunnelID).Scan(&n)
	return n, err
}

// UsedPortsOnNode returns the set of (proto, listen_port) currently held on
// a node within a given port range. Used by the random port allocator so it
// can pick a port that won't collide with the unique constraint.
func UsedPortsOnNode(d *sql.DB, nodeID int64, proto string, start, end int) (map[int]bool, error) {
	rows, err := d.Query(`SELECT listen_port FROM forwards WHERE node_id=? AND proto=? AND listen_port BETWEEN ? AND ?`,
		nodeID, proto, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err == nil {
			out[p] = true
		}
	}
	return out, rows.Err()
}

func DistinctTenantNodes(d *sql.DB, tenantID int64) ([]int64, error) {
	rows, err := d.Query(`SELECT DISTINCT node_id FROM forwards WHERE tenant_id=?`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Audit

func WriteAudit(d *sql.DB, userID int64, action, target, payload string) {
	_, _ = d.Exec(`INSERT INTO audit_logs(user_id, action, target, payload, at) VALUES (?,?,?,?,?)`,
		userID, action, target, payload, now())
}

// Agent-dialer helpers

// UpsertSelfNode ensures the panel's built-in self-node exists and is marked
// online. The partial unique index idx_nodes_self guarantees there is at most
// one row with node_kind='self', so re-running on every boot is safe.
func UpsertSelfNode(d *sql.DB) (*Node, error) {
	_, err := d.Exec(`
		INSERT INTO nodes (name, address, secret, node_kind, online, last_seen, created_at)
		VALUES ('self', 'unix:///var/run/nft-forward.sock', '', 'self', 1, ?, ?)
		ON CONFLICT(node_kind) WHERE node_kind='self'
		DO UPDATE SET last_seen=excluded.last_seen, online=1`,
		now(), now())
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE node_kind='self'`)
	return scanNode(row)
}

// MarkNodeOnline records a successful hello/heartbeat from an agent and
// refreshes the reported binary version.
func MarkNodeOnline(d *sql.DB, id int64, agentVersion string) error {
	_, err := d.Exec(
		`UPDATE nodes SET online=1, last_seen=?, agent_version=? WHERE id=?`,
		now(), agentVersion, id)
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

// UpsertTuiSnapshot stores the daemon-side TUI view of forwards for a node so
// the panel UI can render it on demand without an extra round-trip to the agent.
func UpsertTuiSnapshot(d *sql.DB, nodeID int64, forwardsJSON string) error {
	_, err := d.Exec(`
		INSERT INTO node_tui_snapshot (node_id, forwards_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE
		  SET forwards_json=excluded.forwards_json, updated_at=excluded.updated_at`,
		nodeID, forwardsJSON, now())
	return err
}

// GetTuiSnapshot returns the last TUI snapshot for a node, or ("", nil, nil)
// if none has been recorded yet.
func GetTuiSnapshot(d *sql.DB, nodeID int64) (string, *int64, error) {
	var fj string
	var ts int64
	err := d.QueryRow(`SELECT forwards_json, updated_at FROM node_tui_snapshot WHERE node_id=?`, nodeID).Scan(&fj, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return fj, &ts, nil
}
