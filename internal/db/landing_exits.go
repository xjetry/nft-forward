package db

import (
	"database/sql"
	"strings"
)

// LandingExit is one row of a user's materialized landing-exit set plus its
// traffic ledger. Present=false rows are exits that dropped out of the landing
// source; their quota/used are kept so a returning exit resumes seamlessly.
// URI is server-internal (relay-URI rewriting); it never serializes into
// admin-facing JSON.
type LandingExit struct {
	UserID       int64  `json:"user_id"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Name         string `json:"name"`
	NameOverride string `json:"name_override"`
	Protocol     string `json:"protocol"`
	URI          string `json:"-"`
	Present      bool   `json:"present"`
	QuotaBytes   int64  `json:"quota_bytes"`
	UsedBytes    int64  `json:"used_bytes"`
	UpdatedAt    int64  `json:"updated_at"`
}

// LandingExitInput is a deduplicated landing node destined for the
// materialized set (a plain struct so this package stays decoupled from the
// landing parser).
type LandingExitInput struct {
	Host     string
	Port     int
	Name     string
	Protocol string
	URI      string
}

// LandingExitKey addresses one exit within a user's set.
type LandingExitKey struct {
	Host string
	Port int
}

// UserExitKey addresses one exit ledger row across users.
type UserExitKey struct {
	UserID int64
	Host   string
	Port   int
}

// SyncUserLandingExits materializes a successfully resolved landing set.
// Inputs must already be deduplicated by host:port (first wins, manual URIs
// preceding subscription nodes). Rows missing from the input are swept if their
// ledger is empty (quota==0 && used==0), since present=0 retention exists only
// to resume a returning exit's quota/usage and an empty ledger has nothing to
// resume; ledger-bearing rows flip to present=0 instead so their quota keeps
// enforcing and usage survives. quota/used are never touched here. srcSubURL/srcURIs
// are the source values the resolution ran against: if the users row no
// longer matches (the admin changed the source during a slow subscription
// fetch), the stale result is discarded with synced=false. The returned keys
// flipped presence while at/over quota — their push-exclusion state changed,
// so the caller must re-dispatch the rules pointed at them.
func SyncUserLandingExits(d *sql.DB, userID int64, exits []LandingExitInput, srcSubURL, srcURIs string) (flipped []LandingExitKey, synced bool, err error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var curSub, curURIs string
	if err := tx.QueryRow(`SELECT landing_sub_url, landing_uris FROM users WHERE id=?`, userID).Scan(&curSub, &curURIs); err != nil {
		return nil, false, err
	}
	if curSub != srcSubURL || curURIs != srcURIs {
		return nil, false, nil
	}

	type rowState struct {
		present     bool
		overQuota   bool
		emptyLedger bool
	}
	existing := map[LandingExitKey]rowState{}
	rows, err := tx.Query(`SELECT host, port, present, quota_bytes, used_bytes FROM user_landing_exits WHERE user_id=?`, userID)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var k LandingExitKey
		var present int
		var quota, used int64
		if err := rows.Scan(&k.Host, &k.Port, &present, &quota, &used); err != nil {
			rows.Close()
			return nil, false, err
		}
		existing[k] = rowState{present: present == 1, overQuota: quota > 0 && used >= quota, emptyLedger: quota == 0 && used == 0}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()

	nowTs := now()
	inInput := map[LandingExitKey]bool{}
	for _, e := range exits {
		k := LandingExitKey{Host: e.Host, Port: e.Port}
		if inInput[k] {
			continue
		}
		inInput[k] = true
		if _, err := tx.Exec(`INSERT INTO user_landing_exits(user_id, host, port, name, protocol, uri, present, updated_at)
			VALUES (?,?,?,?,?,?,1,?)
			ON CONFLICT(user_id, host, port) DO UPDATE SET name=excluded.name, protocol=excluded.protocol, uri=excluded.uri, present=1, updated_at=excluded.updated_at`,
			userID, e.Host, e.Port, e.Name, e.Protocol, e.URI, nowTs); err != nil {
			return nil, false, err
		}
		if st, ok := existing[k]; ok && !st.present && st.overQuota {
			flipped = append(flipped, k)
		}
	}
	for k, st := range existing {
		if inInput[k] {
			continue
		}
		// Dropped out of the source. An empty ledger has nothing to resume, so
		// sweep it rather than leave a stale "not in source" row — this also
		// reaches rows already at present=0 whose ledger was later cleared.
		if st.emptyLedger {
			if _, err := tx.Exec(`DELETE FROM user_landing_exits WHERE user_id=? AND host=? AND port=?`,
				userID, k.Host, k.Port); err != nil {
				return nil, false, err
			}
			continue
		}
		if !st.present {
			continue
		}
		if _, err := tx.Exec(`UPDATE user_landing_exits SET present=0, updated_at=? WHERE user_id=? AND host=? AND port=?`,
			nowTs, userID, k.Host, k.Port); err != nil {
			return nil, false, err
		}
		if st.overQuota {
			flipped = append(flipped, k)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return flipped, true, nil
}

const landingExitCols = `user_id, host, port, name, name_override, protocol, uri, present, quota_bytes, used_bytes, updated_at`

func scanLandingExit(r rowScanner) (*LandingExit, error) {
	e := &LandingExit{}
	var present int
	if err := r.Scan(&e.UserID, &e.Host, &e.Port, &e.Name, &e.NameOverride, &e.Protocol, &e.URI, &present, &e.QuotaBytes, &e.UsedBytes, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Present = present == 1
	return e, nil
}

// ListUserLandingExits returns the user's full materialized set, present rows
// first, for the admin quota card.
func ListUserLandingExits(d *sql.DB, userID int64) ([]*LandingExit, error) {
	return queryAll(d, `SELECT `+landingExitCols+` FROM user_landing_exits WHERE user_id=? ORDER BY present DESC, name, host, port`,
		scanLandingExit, userID)
}

// PresentLandingExitsForUser returns only the rows that drive classification,
// metering and push exclusion.
func PresentLandingExitsForUser(d *sql.DB, userID int64) ([]*LandingExit, error) {
	return queryAll(d, `SELECT `+landingExitCols+` FROM user_landing_exits WHERE user_id=? AND present=1 ORDER BY name, host, port`,
		scanLandingExit, userID)
}

// PresentLandingExitSet returns the present (user, host, port) triples for the
// given users — the per-batch lookup applyCounters classifies samples against.
func PresentLandingExitSet(d *sql.DB, userIDs []int64) (map[UserExitKey]bool, error) {
	out := map[UserExitKey]bool{}
	if len(userIDs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(userIDs)-1) + "?"
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}
	rows, err := d.Query(`SELECT user_id, host, port FROM user_landing_exits WHERE present=1 AND user_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k UserExitKey
		if err := rows.Scan(&k.UserID, &k.Host, &k.Port); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// MaxHopPositions returns each rule's final hop position. Only the final hop
// meters into the exit ledger: middle hops target system relay addresses,
// which must never be mistaken for the user's destination.
func MaxHopPositions(d *sql.DB, ruleIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(ruleIDs)-1) + "?"
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
	}
	rows, err := d.Query(`SELECT rule_id, MAX(position) FROM rule_hops WHERE rule_id IN (`+ph+`) GROUP BY rule_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			return nil, err
		}
		out[id] = pos
	}
	return out, rows.Err()
}

// exitRowPresent reports whether the row exists and is present. found=false
// means no such row.
func exitRowPresent(d *sql.DB, userID int64, host string, port int) (found, present bool, err error) {
	var p int
	err = d.QueryRow(`SELECT present FROM user_landing_exits WHERE user_id=? AND host=? AND port=?`, userID, host, port).Scan(&p)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, p == 1, nil
}

// SetUserLandingExitQuota updates one exit's quota (0 = unlimited). present
// tells the caller whether a re-dispatch is warranted — present=0 residual
// rows sit outside the push exclusion.
func SetUserLandingExitQuota(d *sql.DB, userID int64, host string, port int, quota int64) (updated, present bool, err error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET quota_bytes=?, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		quota, now(), userID, host, port)
	return err == nil, present, err
}

// ResetUserLandingExitTraffic zeroes one exit's ledger.
func ResetUserLandingExitTraffic(d *sql.DB, userID int64, host string, port int) (updated, present bool, err error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET used_bytes=0, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		now(), userID, host, port)
	return err == nil, present, err
}

// DeleteUserLandingExit removes a residual (present=0) row. In-set rows are
// managed by sync and refuse deletion.
func DeleteUserLandingExit(d *sql.DB, userID int64, host string, port int) (string, error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil {
		return "", err
	}
	if !found {
		return "notfound", nil
	}
	if present {
		return "present", nil
	}
	if _, err := d.Exec(`DELETE FROM user_landing_exits WHERE user_id=? AND host=? AND port=?`, userID, host, port); err != nil {
		return "", err
	}
	return "deleted", nil
}

// ExitsExceedingQuota returns the user's present exits whose ledger reached
// quota. Quota 0 (unlimited) never exceeds.
func ExitsExceedingQuota(d *sql.DB, userID int64) ([]LandingExitKey, error) {
	rows, err := d.Query(`SELECT host, port FROM user_landing_exits
		WHERE user_id=? AND present=1 AND quota_bytes>0 AND used_bytes>=quota_bytes`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LandingExitKey
	for rows.Next() {
		var k LandingExitKey
		if err := rows.Scan(&k.Host, &k.Port); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// NodesForUserExit returns the distinct physical hop nodes of the user's rules
// that exit to host:port. Composite entries are already expanded into physical
// hops in rule_hops; composite virtual nodes have no agent connection and must
// never enter a dispatch set.
func NodesForUserExit(d *sql.DB, userID int64, host string, port int) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		WHERE r.owner_id=? AND r.exit_host=? AND r.exit_port=?`, userID, host, port)
}

// SetUserLandingExitName sets or clears (name == "") one exit's display-name
// override. The override lives outside SyncUserLandingExits so a subscription
// refresh cannot undo an admin rename; the parsed name column stays intact so
// clearing the override restores it. Renames never change push exclusion, so
// no re-dispatch hint is returned.
func SetUserLandingExitName(d *sql.DB, userID int64, host string, port int, name string) (updated bool, err error) {
	found, _, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET name_override=?, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		name, now(), userID, host, port)
	return err == nil, err
}
