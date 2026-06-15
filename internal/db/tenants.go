package db

import (
	"database/sql"
)

type Tunnel struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	NodeID          int64  `json:"node_id"`
	ProtoMask       string `json:"proto_mask"`
	PortStart       int    `json:"port_start"`
	PortEnd         int    `json:"port_end"`
	TargetCIDRAllow string `json:"target_cidr_allow"`
	BandwidthMbps   int    `json:"bandwidth_mbps"`
	CreatedAt       int64  `json:"created_at"`
}

type UserTunnel struct {
	UserID      int64 `json:"user_id"`
	TunnelID    int64 `json:"tunnel_id"`
	MaxForwards int   `json:"max_forwards"`
	GrantedAt   int64 `json:"granted_at"`
}

// Tunnels

func CreateTunnel(d *sql.DB, t *Tunnel) (int64, error) {
	res, err := d.Exec(`INSERT INTO tunnels(name,node_id,proto_mask,port_start,port_end,target_cidr_allow,bandwidth_mbps,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		t.Name, t.NodeID, t.ProtoMask, t.PortStart, t.PortEnd, t.TargetCIDRAllow, t.BandwidthMbps, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetTunnel(d *sql.DB, id int64) (*Tunnel, error) {
	row := d.QueryRow(`SELECT id,name,node_id,proto_mask,port_start,port_end,target_cidr_allow,bandwidth_mbps,created_at FROM tunnels WHERE id = ?`, id)
	return scanTunnel(row)
}

func scanTunnel(r rowScanner) (*Tunnel, error) {
	t := &Tunnel{}
	if err := r.Scan(&t.ID, &t.Name, &t.NodeID, &t.ProtoMask, &t.PortStart, &t.PortEnd, &t.TargetCIDRAllow, &t.BandwidthMbps, &t.CreatedAt); err != nil {
		return nil, err
	}
	return t, nil
}

func ListTunnels(d *sql.DB) ([]*Tunnel, error) {
	return queryAll(d, `SELECT id,name,node_id,proto_mask,port_start,port_end,target_cidr_allow,bandwidth_mbps,created_at FROM tunnels ORDER BY id`, scanTunnel)
}

func DeleteTunnel(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM tunnels WHERE id = ?`, id)
	return err
}

// TunnelsByID returns all tunnels keyed by ID in a single query, replacing
// per-forward GetTunnel lookups in the dispatch hot path.
func TunnelsByID(d *sql.DB) (map[int64]*Tunnel, error) {
	all, err := ListTunnels(d)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]*Tunnel, len(all))
	for _, t := range all {
		m[t.ID] = t
	}
	return m, nil
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

// User <-> Tunnel grants

func GrantTunnel(d *sql.DB, userID, tunnelID int64, maxForwards int) error {
	_, err := d.Exec(`INSERT INTO user_tunnels(user_id, tunnel_id, max_forwards, granted_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id, tunnel_id) DO UPDATE SET max_forwards=excluded.max_forwards`,
		userID, tunnelID, maxForwards, now())
	return err
}

func RevokeTunnel(d *sql.DB, userID, tunnelID int64) error {
	_, err := d.Exec(`DELETE FROM user_tunnels WHERE user_id=? AND tunnel_id=?`, userID, tunnelID)
	return err
}

func scanGrant(r rowScanner) (*UserTunnel, error) {
	g := &UserTunnel{}
	if err := r.Scan(&g.UserID, &g.TunnelID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

func ListGrants(d *sql.DB) ([]*UserTunnel, error) {
	return queryAll(d, `SELECT user_id, tunnel_id, max_forwards, granted_at FROM user_tunnels`, scanGrant)
}

func ListTunnelsForUser(d *sql.DB, userID int64) ([]*Tunnel, []*UserTunnel, error) {
	rows, err := d.Query(`
		SELECT t.id,t.name,t.node_id,t.proto_mask,t.port_start,t.port_end,t.target_cidr_allow,t.bandwidth_mbps,t.created_at,
		       g.max_forwards, g.granted_at
		FROM tunnels t JOIN user_tunnels g ON g.tunnel_id = t.id
		WHERE g.user_id = ? ORDER BY t.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var tunnels []*Tunnel
	var grants []*UserTunnel
	for rows.Next() {
		t := &Tunnel{}
		g := &UserTunnel{UserID: userID}
		if err := rows.Scan(&t.ID, &t.Name, &t.NodeID, &t.ProtoMask, &t.PortStart, &t.PortEnd, &t.TargetCIDRAllow, &t.BandwidthMbps, &t.CreatedAt, &g.MaxForwards, &g.GrantedAt); err != nil {
			return nil, nil, err
		}
		g.TunnelID = t.ID
		tunnels = append(tunnels, t)
		grants = append(grants, g)
	}
	return tunnels, grants, rows.Err()
}

func GetGrant(d *sql.DB, userID, tunnelID int64) (*UserTunnel, error) {
	row := d.QueryRow(`SELECT user_id, tunnel_id, max_forwards, granted_at FROM user_tunnels WHERE user_id=? AND tunnel_id=?`, userID, tunnelID)
	g := &UserTunnel{}
	if err := row.Scan(&g.UserID, &g.TunnelID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
