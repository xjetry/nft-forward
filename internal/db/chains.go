package db

import (
	"database/sql"
	"encoding/json"
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
		if err := rows.Scan(&p); err == nil {
			out[p] = true
		}
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
