package db

import (
	"database/sql"
	"strings"
	"time"
)

// AddUserNodeTraffic increments the per-grant traffic counter for a user/node pair.
func AddUserNodeTraffic(d *sql.DB, userID, nodeID, delta int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + ? WHERE user_id=? AND node_id=?`,
		delta, userID, nodeID)
	return err
}

// ResetUserNodeTraffic zeroes the traffic counter for a single user/node grant.
func ResetUserNodeTraffic(d *sql.DB, userID, nodeID int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

// ResetAllUserTraffic zeroes the global traffic counter, all per-node counters,
// the landing-exit ledger and the displayed per-rule hop totals for a user —
// an admin reset promises a clean slate, so every number shown for the user
// must drop to zero together. rule_hops.last_bytes* are deliberately kept:
// they snapshot the agent's cumulative counters for delta computation, and
// zeroing them would re-bill the full counter value on the next sample.
func ResetAllUserTraffic(d *sql.DB, userID int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET traffic_used_bytes = 0 WHERE id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = 0 WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE rule_hops SET total_bytes = 0 WHERE rule_id IN (SELECT id FROM rules WHERE owner_id = ?)`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// NodeTrafficSums returns total traffic_used_bytes per node across all users.
func NodeTrafficSums(d *sql.DB) (map[int64]int64, error) {
	rows, err := d.Query(`SELECT node_id, SUM(traffic_used_bytes) FROM user_nodes GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]int64)
	for rows.Next() {
		var nodeID, total int64
		if err := rows.Scan(&nodeID, &total); err != nil {
			return nil, err
		}
		m[nodeID] = total
	}
	return m, rows.Err()
}

// NodeRateMultipliers returns every node's rate_multiplier keyed by id. The
// entry node's value is the whole rule's billing multiplier — middle-layer
// and composite-child hops don't stack their own factors (a composite entry
// carries the baked composite factor on its own column).
func NodeRateMultipliers(d *sql.DB) (map[int64]float64, error) {
	rows, err := d.Query(`SELECT id, rate_multiplier FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]float64{}
	for rows.Next() {
		var id int64
		var mult float64
		if err := rows.Scan(&id, &mult); err != nil {
			return nil, err
		}
		m[id] = mult
	}
	return m, rows.Err()
}

// SegmentFirstHops maps each rule's segment-first hop positions to the
// segment's logical node id. Per-grant byte accounting charges a segment's
// grant exactly once per counter batch — at its first hop — since every hop
// of a segment carries the same bytes.
func SegmentFirstHops(d *sql.DB, ruleIDs []int64) (map[int64]map[int]int64, error) {
	if len(ruleIDs) == 0 {
		return map[int64]map[int]int64{}, nil
	}
	args := make([]any, len(ruleIDs))
	ph := make([]string, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
		ph[i] = "?"
	}
	rows, err := d.Query(`SELECT rule_id, MIN(position), via_node_id FROM rule_hops
		WHERE rule_id IN (`+strings.Join(ph, ",")+`) GROUP BY rule_id, via_node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]map[int]int64{}
	for rows.Next() {
		var ruleID, via int64
		var pos int
		if err := rows.Scan(&ruleID, &pos, &via); err != nil {
			return nil, err
		}
		if m[ruleID] == nil {
			m[ruleID] = map[int]int64{}
		}
		m[ruleID][pos] = via
	}
	return m, rows.Err()
}

// CheckAndResetTrafficCycle checks whether the user's traffic reset window has
// elapsed since the last reset. If so, it zeros the global counter, all
// per-node counters and the landing-exit ledger together and records the
// reset timestamp. Returns true if a reset occurred.
//
// traffic_reset_days == 0 means the user is never auto-reset.
// The window is anchored to the account creation date so the cycle boundary is
// predictable (e.g. "every 30 days from account open date").
func CheckAndResetTrafficCycle(d *sql.DB, u *User) (bool, error) {
	if u.TrafficResetDays <= 0 {
		return false, nil
	}
	nowTs := time.Now().Unix()
	period := int64(u.TrafficResetDays) * 86400
	var createdAt int64
	if err := d.QueryRow(`SELECT created_at FROM users WHERE id=?`, u.ID).Scan(&createdAt); err != nil {
		return false, err
	}
	elapsed := nowTs - createdAt
	if elapsed < 0 {
		return false, nil
	}
	cycleStart := createdAt + (elapsed/period)*period
	if u.LastTrafficResetAt >= cycleStart {
		return false, nil
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET traffic_used_bytes = 0, last_traffic_reset_at = ? WHERE id=?`, nowTs, u.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=?`, u.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = 0 WHERE user_id=?`, u.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
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
