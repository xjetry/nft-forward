package db

import (
	"database/sql"
)

// 流量账本的累加与清零都在 internal/accounting 里；这里只有读侧。

// NodeRawTraffic returns every node's cumulative raw bytes keyed by id. Nodes
// that never reported a counter batch have no row and are absent from the map.
func NodeRawTraffic(d DBTX) (map[int64]int64, error) {
	rows, err := d.Query(`SELECT node_id, raw_bytes FROM node_raw_traffic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]int64)
	for rows.Next() {
		var nodeID, raw int64
		if err := rows.Scan(&nodeID, &raw); err != nil {
			return nil, err
		}
		m[nodeID] = raw
	}
	return m, rows.Err()
}

// NodesExceedingQuota returns the IDs of nodes where the user's per-grant
// traffic counter has reached or exceeded the configured quota. Grants with
// quota == 0 (unlimited) are excluded.
func NodesExceedingQuota(d *sql.DB, userID int64) ([]int64, error) {
	return queryInt64s(d,
		`SELECT node_id FROM user_nodes WHERE user_id=? AND traffic_quota_bytes > 0 AND traffic_used_bytes >= traffic_quota_bytes`,
		userID)
}

// RulesAffectedByNode returns the distinct hop-node IDs of all rules owned by
// userID whose chain runs a segment of the given logical node (entry or via).
// Grants — and thus quotas — live on logical nodes, so a rule is affected when
// any of its hops was expanded from nodeID's segment (rh2.via_node_id = nodeID);
// the entry segment's via is rules.node_id, so this covers composite/entry
// grants without a separate clause.
func RulesAffectedByNode(d *sql.DB, userID, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		WHERE r.owner_id = ?
		  AND rh.rule_id IN (
		      SELECT rh2.rule_id FROM rule_hops rh2 WHERE rh2.via_node_id = ?)`,
		userID, nodeID)
}
