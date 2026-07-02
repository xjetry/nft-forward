package db

import "database/sql"

// NodeBinding is an edge of the middle-layer graph: downstream may be attached
// behind upstream in a rule's chain. Mode is the junction segment's forwarding
// mode (upstream segment tail -> downstream segment head); it is captured into
// rule_hops when a rule expands, so edits affect only later (re)expansions.
type NodeBinding struct {
	UpstreamNodeID   int64  `json:"upstream_node_id"`
	DownstreamNodeID int64  `json:"downstream_node_id"`
	Mode             string `json:"mode"`
}

func scanNodeBinding(r rowScanner) (*NodeBinding, error) {
	b := &NodeBinding{}
	if err := r.Scan(&b.UpstreamNodeID, &b.DownstreamNodeID, &b.Mode); err != nil {
		return nil, err
	}
	return b, nil
}

const bindingCols = `upstream_node_id, downstream_node_id, mode`

func ListAllNodeBindings(d *sql.DB) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings ORDER BY downstream_node_id, upstream_node_id`, scanNodeBinding)
}

func ListBindingsForDownstream(d *sql.DB, downstreamID int64) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings WHERE downstream_node_id=? ORDER BY upstream_node_id`, scanNodeBinding, downstreamID)
}

func GetNodeBinding(d DBTX, upstreamID, downstreamID int64) (*NodeBinding, error) {
	return scanNodeBinding(d.QueryRow(`SELECT `+bindingCols+` FROM node_bindings WHERE upstream_node_id=? AND downstream_node_id=?`, upstreamID, downstreamID))
}

// ReplaceBindingsForDownstream swaps the downstream node's full upstream edge
// set in one transaction, mirroring how composite hops are replaced whole.
func ReplaceBindingsForDownstream(d *sql.DB, downstreamID int64, bindings []NodeBinding) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_bindings WHERE downstream_node_id=?`, downstreamID); err != nil {
		return err
	}
	for _, b := range bindings {
		mode := NormalizeForwardMode(b.Mode)
		if _, err := tx.Exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?,?)`,
			b.UpstreamNodeID, downstreamID, mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}
