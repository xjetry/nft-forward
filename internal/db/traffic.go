package db

import (
	"database/sql"
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

// ResetAllUserTraffic zeroes the global traffic counter and all per-node counters
// for a user. Both must be cleared together so accounting stays consistent.
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
	return tx.Commit()
}

// NodeMultipliers returns a map of node ID to traffic_multiplier for all nodes.
// A multiplier of 1.0 means traffic is charged at face value; values above or
// below scale the effective cost applied by the accounting layer.
func NodeMultipliers(d *sql.DB) (map[int64]float64, error) {
	rows, err := d.Query(`SELECT id, traffic_multiplier FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]float64)
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

// CheckAndResetTrafficCycle checks whether the user's traffic reset window has
// elapsed since the last reset. If so, it zeros all counters and records the
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
// userID that include nodeID as one of their hops. Matching covers both physical
// hops (rh2.node_id = nodeID) and composite nodes declared as the rule's entry
// node (r.node_id = nodeID), since composite quotas are tracked on the composite
// node ID rather than any individual physical hop.
func RulesAffectedByNode(d *sql.DB, userID, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		WHERE r.owner_id = ?
		  AND (rh.rule_id IN (
		          SELECT rh2.rule_id FROM rule_hops rh2 WHERE rh2.node_id = ?
		      )
		      OR r.node_id = ?)`, userID, nodeID, nodeID)
}
