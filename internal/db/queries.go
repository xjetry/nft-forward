package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	LastSeenAt  sql.NullInt64
	LastApplyAt sql.NullInt64
	LastError   sql.NullString
	Dirty       bool
	Disabled    bool
	CreatedAt   int64
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

func GetNode(d *sql.DB, id int64) (*Node, error) {
	row := d.QueryRow(`SELECT id,name,address,secret,last_seen_at,last_apply_at,last_error,dirty,disabled,created_at FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

type rowScanner interface{ Scan(...any) error }

func scanNode(r rowScanner) (*Node, error) {
	n := &Node{}
	var dirty, disabled int
	if err := r.Scan(&n.ID, &n.Name, &n.Address, &n.Secret, &n.LastSeenAt, &n.LastApplyAt, &n.LastError, &dirty, &disabled, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.Dirty = dirty == 1
	n.Disabled = disabled == 1
	return n, nil
}

func ListNodes(d *sql.DB) ([]*Node, error) {
	rows, err := d.Query(`SELECT id,name,address,secret,last_seen_at,last_apply_at,last_error,dirty,disabled,created_at FROM nodes ORDER BY id`)
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

func MarkNodeApplied(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET last_apply_at=?, last_seen_at=?, last_error=NULL, dirty=0 WHERE id=?`,
		now(), now(), id)
	return err
}

func MarkNodeError(d *sql.DB, id int64, msg string) error {
	_, err := d.Exec(`UPDATE nodes SET last_error=?, dirty=1 WHERE id=?`, msg, id)
	return err
}

func MarkNodeSeen(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET last_seen_at=? WHERE id=?`, now(), id)
	return err
}

// Forwards

const forwardCols = `id,node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,disabled,last_bytes,total_bytes,created_at`

func scanForward(r rowScanner) (*Forward, error) {
	f := &Forward{}
	var disabled int
	if err := r.Scan(&f.ID, &f.NodeID, &f.TenantID, &f.TunnelID, &f.Proto, &f.ListenPort, &f.TargetIP, &f.TargetPort, &f.Comment, &disabled, &f.LastBytes, &f.TotalBytes, &f.CreatedAt); err != nil {
		return nil, err
	}
	f.Disabled = disabled == 1
	return f, nil
}

func CreateForward(d *sql.DB, f *Forward) (int64, error) {
	res, err := d.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		f.NodeID, f.TenantID, f.TunnelID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, now())
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

func ListForwardsByTunnel(d *sql.DB, tunnelID int64) ([]*Forward, error) {
	return listForwardsWhere(d, "tunnel_id=?", tunnelID)
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

// AddForwardTraffic adds a delta to (last_bytes, total_bytes) for a forward,
// returning the previous last_bytes so the caller can detect counter resets.
func UpdateForwardBytes(d *sql.DB, id int64, currentBytes int64) (delta int64, err error) {
	var prev int64
	if err := d.QueryRow(`SELECT last_bytes FROM forwards WHERE id=?`, id).Scan(&prev); err != nil {
		return 0, err
	}
	if currentBytes < prev {
		// counter reset (agent reboot); treat the current value as the delta
		delta = currentBytes
	} else {
		delta = currentBytes - prev
	}
	if _, err := d.Exec(`UPDATE forwards SET last_bytes=?, total_bytes=total_bytes+? WHERE id=?`,
		currentBytes, delta, id); err != nil {
		return 0, err
	}
	return delta, nil
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
