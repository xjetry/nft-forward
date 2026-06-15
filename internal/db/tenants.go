package db

import (
	"database/sql"
)

type Tenant struct {
	ID                int64          `json:"id"`
	Name              string         `json:"name"`
	MaxForwards       int            `json:"max_forwards"`
	TrafficQuotaBytes int64          `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64          `json:"traffic_used_bytes"`
	ExpiresAt         sql.NullInt64  `json:"expires_at"`
	Disabled          bool           `json:"disabled"`
	DisableReason     sql.NullString `json:"disable_reason"`
	CreatedAt         int64          `json:"created_at"`
}

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

type TenantTunnel struct {
	TenantID    int64 `json:"tenant_id"`
	TunnelID    int64 `json:"tunnel_id"`
	MaxForwards int   `json:"max_forwards"`
	GrantedAt   int64 `json:"granted_at"`
}

// Tenants

func CreateTenant(d *sql.DB, t *Tenant) (int64, error) {
	res, err := d.Exec(`INSERT INTO tenants(name, max_forwards, traffic_quota_bytes, expires_at, created_at) VALUES (?,?,?,?,?)`,
		t.Name, t.MaxForwards, t.TrafficQuotaBytes, t.ExpiresAt, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetTenant(d *sql.DB, id int64) (*Tenant, error) {
	row := d.QueryRow(`SELECT id,name,max_forwards,traffic_quota_bytes,traffic_used_bytes,expires_at,disabled,disable_reason,created_at FROM tenants WHERE id = ?`, id)
	return scanTenant(row)
}

func scanTenant(r rowScanner) (*Tenant, error) {
	t := &Tenant{}
	var disabled int
	if err := r.Scan(&t.ID, &t.Name, &t.MaxForwards, &t.TrafficQuotaBytes, &t.TrafficUsedBytes, &t.ExpiresAt, &disabled, &t.DisableReason, &t.CreatedAt); err != nil {
		return nil, err
	}
	t.Disabled = disabled == 1
	return t, nil
}

func ListTenants(d *sql.DB) ([]*Tenant, error) {
	return queryAll(d, `SELECT id,name,max_forwards,traffic_quota_bytes,traffic_used_bytes,expires_at,disabled,disable_reason,created_at FROM tenants ORDER BY id`, scanTenant)
}

func DeleteTenant(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM tenants WHERE id = ?`, id)
	return err
}

func SetTenantDisabled(d *sql.DB, id int64, disabled bool, reason string) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := d.Exec(`UPDATE tenants SET disabled=?, disable_reason=? WHERE id=?`, v, reason, id)
	return err
}

func AddTenantTraffic(d *sql.DB, id int64, delta int64) error {
	_, err := d.Exec(`UPDATE tenants SET traffic_used_bytes = traffic_used_bytes + ? WHERE id=?`, delta, id)
	return err
}

func ResetTenantTraffic(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE tenants SET traffic_used_bytes = 0, disabled=0, disable_reason=NULL WHERE id=?`, id)
	return err
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

// TenantsByID returns all tenants keyed by ID in a single query.
func TenantsByID(d *sql.DB) (map[int64]*Tenant, error) {
	all, err := ListTenants(d)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]*Tenant, len(all))
	for _, t := range all {
		m[t.ID] = t
	}
	return m, nil
}

// Tenant <-> Tunnel grants

func GrantTunnel(d *sql.DB, tenantID, tunnelID int64, maxForwards int) error {
	_, err := d.Exec(`INSERT INTO tenant_tunnels(tenant_id, tunnel_id, max_forwards, granted_at) VALUES (?,?,?,?)
		ON CONFLICT(tenant_id, tunnel_id) DO UPDATE SET max_forwards=excluded.max_forwards`,
		tenantID, tunnelID, maxForwards, now())
	return err
}

func RevokeTunnel(d *sql.DB, tenantID, tunnelID int64) error {
	_, err := d.Exec(`DELETE FROM tenant_tunnels WHERE tenant_id=? AND tunnel_id=?`, tenantID, tunnelID)
	return err
}

func scanGrant(r rowScanner) (*TenantTunnel, error) {
	g := &TenantTunnel{}
	if err := r.Scan(&g.TenantID, &g.TunnelID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

func ListGrants(d *sql.DB) ([]*TenantTunnel, error) {
	return queryAll(d, `SELECT tenant_id, tunnel_id, max_forwards, granted_at FROM tenant_tunnels`, scanGrant)
}

func ListTunnelsForTenant(d *sql.DB, tenantID int64) ([]*Tunnel, []*TenantTunnel, error) {
	rows, err := d.Query(`
		SELECT t.id,t.name,t.node_id,t.proto_mask,t.port_start,t.port_end,t.target_cidr_allow,t.bandwidth_mbps,t.created_at,
		       g.max_forwards, g.granted_at
		FROM tunnels t JOIN tenant_tunnels g ON g.tunnel_id = t.id
		WHERE g.tenant_id = ? ORDER BY t.id`, tenantID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var tunnels []*Tunnel
	var grants []*TenantTunnel
	for rows.Next() {
		t := &Tunnel{}
		g := &TenantTunnel{TenantID: tenantID}
		if err := rows.Scan(&t.ID, &t.Name, &t.NodeID, &t.ProtoMask, &t.PortStart, &t.PortEnd, &t.TargetCIDRAllow, &t.BandwidthMbps, &t.CreatedAt, &g.MaxForwards, &g.GrantedAt); err != nil {
			return nil, nil, err
		}
		g.TunnelID = t.ID
		tunnels = append(tunnels, t)
		grants = append(grants, g)
	}
	return tunnels, grants, rows.Err()
}

func GetGrant(d *sql.DB, tenantID, tunnelID int64) (*TenantTunnel, error) {
	row := d.QueryRow(`SELECT tenant_id, tunnel_id, max_forwards, granted_at FROM tenant_tunnels WHERE tenant_id=? AND tunnel_id=?`, tenantID, tunnelID)
	g := &TenantTunnel{}
	if err := row.Scan(&g.TenantID, &g.TunnelID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
