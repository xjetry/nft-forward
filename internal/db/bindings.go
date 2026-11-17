package db

import (
	"database/sql"

	"nft-forward/internal/chain"
)

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

func ListAllNodeBindings(d DBTX) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings ORDER BY downstream_node_id, upstream_node_id`, scanNodeBinding)
}

func ListBindingsForDownstream(d *sql.DB, downstreamID int64) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings WHERE downstream_node_id=? ORDER BY upstream_node_id`, scanNodeBinding, downstreamID)
}

// GetNodeBinding 取一条绑定边。面板自己按整张表读（链路推导的拓扑、节点详情的
// 上下游列表），不单条取；这个函数是给测试核对写入结果用的。
func GetNodeBinding(d DBTX, upstreamID, downstreamID int64) (*NodeBinding, error) {
	return scanNodeBinding(d.QueryRow(`SELECT `+bindingCols+` FROM node_bindings WHERE upstream_node_id=? AND downstream_node_id=?`, upstreamID, downstreamID))
}

// ReplaceBindingsForDownstream swaps the downstream node's full upstream edge
// set in one transaction, mirroring how composite hops are replaced whole.
//
// 边的 mode 经 chain.InterNodeLegMode 落值后才入库：没填的记成 userspace，认不出的
// 值返回错误且整次替换回滚。node_bindings 只经本函数与 ReplaceBindingsForUpstream
// 写入，所以链路推导读到的边 mode 一定是两种模式之一。
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
		mode, err := chain.InterNodeLegMode(b.Mode)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?,?)`,
			b.UpstreamNodeID, downstreamID, mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ListBindingsForUpstream(d *sql.DB, upstreamID int64) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings WHERE upstream_node_id=? ORDER BY downstream_node_id`, scanNodeBinding, upstreamID)
}

// ReplaceBindingsForUpstream swaps the upstream node's full downstream edge set
// in one transaction. It touches only edges where this node is the upstream, so
// it never disturbs a downstream node's other upstream edges — the mirror image
// of ReplaceBindingsForDownstream.
func ReplaceBindingsForUpstream(d *sql.DB, upstreamID int64, bindings []NodeBinding) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_bindings WHERE upstream_node_id=?`, upstreamID); err != nil {
		return err
	}
	for _, b := range bindings {
		// 与 ReplaceBindingsForDownstream 同一条落值规则，见它的说明。
		mode, err := chain.InterNodeLegMode(b.Mode)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?,?)`,
			upstreamID, b.DownstreamNodeID, mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}
