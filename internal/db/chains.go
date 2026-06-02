package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"strconv"
)

// DBTX is satisfied by both *sql.DB and *sql.Tx so chain helpers can run either
// standalone or inside a regeneration transaction.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Admin chains allocate listen ports from this high range, skipping anything
// already occupied on the node. Tenant chains use their tunnel's port range.
const (
	ChainPortMin = 20000
	ChainPortMax = 60000
)

// OccupiedPortsOnNode returns every listen port held on (node, proto), unioning
// the panel forwards table with the node's last-reported tui-segment snapshot.
// The daemon rejects cross-segment port conflicts at apply time, so the tui
// snapshot must be consulted or auto-allocation would pick ports the daemon
// then refuses. excludeChainID>0 drops that chain's own forwards so a chain
// regenerating in place doesn't see itself as occupying its ports.
//
// Invariant: node_tui_snapshot only ever holds the daemon's non-panel TUI-segment
// ports; panel and chain forwards live in the forwards table. So the snapshot can
// never contain a chain's own port, which is why it is unioned in unconditionally
// (excludeChainID applies only to the forwards query) without breaking stable reuse.
func OccupiedPortsOnNode(d DBTX, nodeID int64, proto string, excludeChainID int64) (map[int]bool, error) {
	out := map[int]bool{}
	rows, err := d.Query(
		`SELECT listen_port FROM forwards WHERE node_id=? AND proto=? AND (chain_id IS NULL OR chain_id<>?)`,
		nodeID, proto, excludeChainID)
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
	// tui snapshot is best-effort (may be stale/absent); the daemon's 409 is the
	// ultimate authority, this only avoids the common collisions up front.
	var fj string
	switch err := d.QueryRow(`SELECT forwards_json FROM node_tui_snapshot WHERE node_id=?`, nodeID).Scan(&fj); err {
	case nil:
		var snap []struct {
			Proto      string `json:"proto"`
			ListenPort int    `json:"listen_port"`
		}
		if json.Unmarshal([]byte(fj), &snap) == nil {
			for _, f := range snap {
				if f.Proto == proto {
					out[f.ListenPort] = true
				}
			}
		}
	case sql.ErrNoRows:
		// node never reported a tui segment; nothing to union
	default:
		return nil, err
	}
	return out, nil
}

// hostPort joins a relay host / exit host with a port for display + targets.
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// PickFreePort returns a port in [start,end] not present in used, or 0 when the
// range is exhausted. A random offset keeps assignment unpredictable so two
// near-simultaneous allocations don't keep colliding on the same port.
func PickFreePort(start, end int, used map[int]bool) int {
	span := end - start + 1
	if span <= 0 {
		return 0
	}
	offset := rand.Intn(span)
	for i := 0; i < span; i++ {
		p := start + ((offset + i) % span)
		if !used[p] {
			return p
		}
	}
	return 0
}

type Chain struct {
	ID              int64
	TenantID        sql.NullInt64
	Name            string
	Proto           string
	ExitHost        string
	ExitPort        int
	EntryNodeID     sql.NullInt64
	EntryListenPort int
	CreatedAt       int64
}

type ChainHop struct {
	ChainID    int64
	Position   int
	NodeID     int64
	TunnelID   sql.NullInt64
	ListenPort int
	Mode       string
}

const chainCols = `id,tenant_id,name,proto,exit_host,exit_port,entry_node_id,entry_listen_port,created_at`

func scanChain(r rowScanner) (*Chain, error) {
	c := &Chain{}
	if err := r.Scan(&c.ID, &c.TenantID, &c.Name, &c.Proto, &c.ExitHost, &c.ExitPort,
		&c.EntryNodeID, &c.EntryListenPort, &c.CreatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// CreateChain inserts the chain header; hops + forwards are written by
// RegenerateChain. entry_* start at 0/NULL until the first regeneration.
func CreateChain(d DBTX, c *Chain) (int64, error) {
	res, err := d.Exec(`INSERT INTO chains(tenant_id,name,proto,exit_host,exit_port,created_at) VALUES (?,?,?,?,?,?)`,
		c.TenantID, c.Name, c.Proto, c.ExitHost, c.ExitPort, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetChain(d DBTX, id int64) (*Chain, error) {
	return scanChain(d.QueryRow(`SELECT `+chainCols+` FROM chains WHERE id=?`, id))
}

// UpdateChainHeader persists editable header fields (name/proto/exit). entry_*
// is owned by RegenerateChain and not touched here.
func UpdateChainHeader(d DBTX, c *Chain) error {
	_, err := d.Exec(`UPDATE chains SET name=?,proto=?,exit_host=?,exit_port=? WHERE id=?`,
		c.Name, c.Proto, c.ExitHost, c.ExitPort, c.ID)
	return err
}

func listChainsWhere(d *sql.DB, where string, args ...any) ([]*Chain, error) {
	q := `SELECT ` + chainCols + ` FROM chains`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chain
	for rows.Next() {
		c, err := scanChain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListAdminChains returns chains with no owning tenant (admin-built, unmetered).
func ListAdminChains(d *sql.DB) ([]*Chain, error) {
	return listChainsWhere(d, "tenant_id IS NULL")
}

func ListChainsByTenant(d *sql.DB, tenantID int64) ([]*Chain, error) {
	return listChainsWhere(d, "tenant_id=?", tenantID)
}

func ListChainHops(d DBTX, chainID int64) ([]*ChainHop, error) {
	rows, err := d.Query(`SELECT chain_id,position,node_id,tunnel_id,listen_port,mode FROM chain_hops WHERE chain_id=? ORDER BY position`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChainHop
	for rows.Next() {
		h := &ChainHop{}
		if err := rows.Scan(&h.ChainID, &h.Position, &h.NodeID, &h.TunnelID, &h.ListenPort, &h.Mode); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func ListForwardsByChain(d DBTX, chainID int64) ([]*Forward, error) {
	rows, err := d.Query(`SELECT `+forwardCols+` FROM forwards WHERE chain_id=? ORDER BY node_id, listen_port`, chainID)
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

// DeleteChain removes a chain and returns the nodes whose kernel state must be
// re-dispatched (i.e. the nodes its forwards lived on). The ON DELETE CASCADE on
// chain_hops + forwards.chain_id clears the rows; we collect nodes first so the
// caller can re-push them after the rules are gone.
func DeleteChain(d *sql.DB, id int64) ([]int64, error) {
	rows, err := d.Query(`SELECT DISTINCT node_id FROM forwards WHERE chain_id=?`, id)
	if err != nil {
		return nil, err
	}
	var nodes []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM chains WHERE id=?`, id); err != nil {
		return nil, err
	}
	return nodes, nil
}

// HopInput is one ordered hop the caller wants the chain to have. TunnelID is
// set for tenant chains (the granted tunnel the hop draws its port/range from)
// and invalid for admin chains. Mode is the requested data plane; udp chains
// coerce every hop to kernel.
type HopInput struct {
	NodeID   int64
	TunnelID sql.NullInt64
	Mode     string
}

// RegenerateChain rewrites chain c's hops + generated forwards for the given
// ordered hops and returns the copyable entry endpoint plus the set of nodes
// whose kernel state must be re-dispatched (current hops ∪ previously-touched
// nodes). Ports are kept stable per node across edits; avoid[nodeID]=port forces
// that node off a given port (used by the reallocate-on-conflict flow).
//
// Structural validation only: relay_host present, no repeated node, port-range
// exhaustion, tunnel<->node match + proto_mask, udp=>kernel. Tenant policy
// (grant ownership, exit CIDR, quota) is the caller's responsibility.
func RegenerateChain(tx DBTX, c *Chain, hops []HopInput, avoid map[int64]int) (string, []int64, error) {
	if len(hops) == 0 {
		return "", nil, fmt.Errorf("链路至少需要一跳")
	}

	type resolved struct {
		nodeID    int64
		relayHost string
		tunnelID  sql.NullInt64
		mode      string
		rangeLo   int
		rangeHi   int
	}
	rs := make([]resolved, len(hops))
	seen := map[int64]bool{}
	for i, hop := range hops {
		if seen[hop.NodeID] {
			return "", nil, fmt.Errorf("同一节点不能在链路中重复")
		}
		seen[hop.NodeID] = true

		var name, relay string
		if err := tx.QueryRow(`SELECT name, relay_host FROM nodes WHERE id=?`, hop.NodeID).Scan(&name, &relay); err != nil {
			return "", nil, fmt.Errorf("节点 %d 不存在", hop.NodeID)
		}
		if relay == "" {
			return "", nil, fmt.Errorf("节点 %s 未设置中继地址", name)
		}
		mode := NormalizeForwardMode(hop.Mode)
		if c.Proto == "udp" {
			mode = "kernel" // userspace relay is TCP-only
		}
		lo, hi := ChainPortMin, ChainPortMax
		tunnelID := hop.TunnelID
		if tunnelID.Valid {
			var pm string
			var ps, pe int
			var tNode int64
			if err := tx.QueryRow(`SELECT node_id, proto_mask, port_start, port_end FROM tunnels WHERE id=?`, tunnelID.Int64).Scan(&tNode, &pm, &ps, &pe); err != nil {
				return "", nil, fmt.Errorf("通道 %d 不存在", tunnelID.Int64)
			}
			if tNode != hop.NodeID {
				return "", nil, fmt.Errorf("通道与节点不匹配")
			}
			if pm != "tcp+udp" && pm != c.Proto {
				return "", nil, fmt.Errorf("通道 %d 不允许 %s", tunnelID.Int64, c.Proto)
			}
			lo, hi = ps, pe
		}
		rs[i] = resolved{nodeID: hop.NodeID, relayHost: relay, tunnelID: tunnelID, mode: mode, rangeLo: lo, rangeHi: hi}
	}

	// Read existing ports (keyed by node) BEFORE deleting so unchanged nodes keep
	// their port — entry endpoint + installed rules don't churn on edits.
	prev, err := ListForwardsByChain(tx, c.ID)
	if err != nil {
		return "", nil, err
	}
	prevPort := map[int64]int{}
	affected := map[int64]bool{}
	for _, f := range prev {
		prevPort[f.NodeID] = f.ListenPort
		affected[f.NodeID] = true
	}

	if _, err := tx.Exec(`DELETE FROM forwards WHERE chain_id=?`, c.ID); err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(`DELETE FROM chain_hops WHERE chain_id=?`, c.ID); err != nil {
		return "", nil, err
	}

	ports := make([]int, len(rs))
	for i, h := range rs {
		// This chain's own old forwards were DELETEd just above, so passing c.ID
		// is belt-and-suspenders here; it only matters for callers that query
		// occupancy without pre-deleting the chain's rows.
		occ, err := OccupiedPortsOnNode(tx, h.nodeID, c.Proto, c.ID)
		if err != nil {
			return "", nil, err
		}
		if av, ok := avoid[h.nodeID]; ok {
			occ[av] = true // force this node off its current port
		}
		p := prevPort[h.nodeID]
		if p >= h.rangeLo && p <= h.rangeHi && !occ[p] {
			// keep
		} else {
			p = PickFreePort(h.rangeLo, h.rangeHi, occ)
			if p == 0 {
				var name string
				_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
				return "", nil, fmt.Errorf("节点 %s 端口段(%d-%d)无可用端口", name, h.rangeLo, h.rangeHi)
			}
		}
		ports[i] = p
	}

	for i, h := range rs {
		var targetIP string
		var targetPort int
		if i < len(rs)-1 {
			targetIP = rs[i+1].relayHost
			targetPort = ports[i+1]
		} else {
			targetIP = c.ExitHost
			targetPort = c.ExitPort
		}
		if _, err := tx.Exec(`INSERT INTO chain_hops(chain_id,position,node_id,tunnel_id,listen_port,mode) VALUES (?,?,?,?,?,?)`,
			c.ID, i, h.nodeID, h.tunnelID, ports[i], h.mode); err != nil {
			return "", nil, err
		}
		comment := fmt.Sprintf("链路 %s · 第%d跳", c.Name, i+1)
		if _, err := tx.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode,chain_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			h.nodeID, c.TenantID, h.tunnelID, c.Proto, ports[i], targetIP, targetPort, comment, now(), h.mode, c.ID); err != nil {
			return "", nil, err
		}
		affected[h.nodeID] = true
	}

	entryNodeID := rs[0].nodeID
	if _, err := tx.Exec(`UPDATE chains SET entry_node_id=?, entry_listen_port=? WHERE id=?`, entryNodeID, ports[0], c.ID); err != nil {
		return "", nil, err
	}
	c.EntryNodeID = sql.NullInt64{Int64: entryNodeID, Valid: true}
	c.EntryListenPort = ports[0]

	nodes := make([]int64, 0, len(affected))
	for n := range affected {
		nodes = append(nodes, n)
	}
	return hostPort(rs[0].relayHost, ports[0]), nodes, nil
}
