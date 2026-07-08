package db

import "database/sql"

// idempotencyTTLSeconds bounds how long a replayed Idempotency-Key stays honored.
// A day comfortably covers agent retry windows without letting the table grow
// unbounded; entries are pruned by age on every write.
const idempotencyTTLSeconds = 24 * 3600

// LookupIdempotentRule returns the rule id a prior request already created under
// (userID, key), or ok=false when the key is new. Callers replay that rule
// instead of creating a duplicate.
func LookupIdempotentRule(d *sql.DB, userID int64, key string) (int64, bool, error) {
	var rid int64
	err := d.QueryRow(`SELECT rule_id FROM api_idempotency WHERE user_id=? AND idem_key=?`, userID, key).Scan(&rid)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return rid, true, nil
}

// SaveIdempotentRule records that (userID, key) produced ruleID. This makes a
// sequential retry (the common agent case: network blip, at-least-once queue)
// replay the original rule; it does not serialize two *simultaneous* creates —
// INSERT OR IGNORE just keeps the first mapping. It also prunes rows older than
// idempotencyTTLSeconds so the table can't grow unbounded.
func SaveIdempotentRule(d *sql.DB, userID int64, key string, ruleID int64) error {
	if _, err := d.Exec(
		`INSERT OR IGNORE INTO api_idempotency(user_id, idem_key, rule_id, created_at) VALUES (?,?,?,?)`,
		userID, key, ruleID, now(),
	); err != nil {
		return err
	}
	_, _ = d.Exec(`DELETE FROM api_idempotency WHERE created_at < ?`, now()-idempotencyTTLSeconds)
	return nil
}
