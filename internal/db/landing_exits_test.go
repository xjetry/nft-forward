package db

import "testing"

func inputs(hosts ...string) []LandingExitInput {
	out := make([]LandingExitInput, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, LandingExitInput{Host: h, Port: 443, Name: "n-" + h, Protocol: "vless", URI: "vless://x@" + h + ":443"})
	}
	return out
}

func TestSyncUserLandingExitsLifecycle(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)

	// initial sync materializes present=1 rows
	_, synced, err := SyncUserLandingExits(d, uid, inputs("a.com", "b.com"), "", "")
	if err != nil || !synced {
		t.Fatalf("sync: synced=%v err=%v", synced, err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if len(exits) != 2 || !exits[0].Present {
		t.Fatalf("want 2 present rows, got %+v", exits)
	}

	// quota/used survive a re-sync and a disappearance
	if _, _, err := SetUserLandingExitQuota(d, uid, "a.com", 443, 1000); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=? AND host='a.com'`, uid)
	_, synced, _ = SyncUserLandingExits(d, uid, inputs("b.com"), "", "")
	if !synced {
		t.Fatal("second sync should apply")
	}
	rows, _ := ListUserLandingExits(d, uid)
	var a *LandingExit
	for _, e := range rows {
		if e.Host == "a.com" {
			a = e
		}
	}
	if a == nil || a.Present || a.QuotaBytes != 1000 || a.UsedBytes != 500 {
		t.Fatalf("a.com should be present=0 with ledger kept, got %+v", a)
	}

	// returning exit resumes the same ledger
	SyncUserLandingExits(d, uid, inputs("a.com", "b.com"), "", "")
	rows, _ = ListUserLandingExits(d, uid)
	for _, e := range rows {
		if e.Host == "a.com" && (!e.Present || e.UsedBytes != 500) {
			t.Fatalf("returning exit lost ledger: %+v", e)
		}
	}
}

func TestSyncSweepsEmptyLedgerResiduals(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)

	// Two exits materialize; neither carries a ledger.
	SyncUserLandingExits(d, uid, inputs("a.com", "b.com"), "", "")

	// b.com drops out of the source with an empty ledger — nothing to resume —
	// so it is deleted outright, not kept as a present=0 "not in source" row.
	if _, synced, err := SyncUserLandingExits(d, uid, inputs("a.com"), "", ""); err != nil || !synced {
		t.Fatalf("sync: synced=%v err=%v", synced, err)
	}
	if exits, _ := ListUserLandingExits(d, uid); len(exits) != 1 || exits[0].Host != "a.com" {
		t.Fatalf("empty-ledger residual should be deleted, got %+v", exits)
	}

	// a.com now carries usage; clearing the whole source keeps it as present=0
	// because its ledger still enforces and its usage must survive.
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=? AND host='a.com'`, uid)
	SyncUserLandingExits(d, uid, nil, "", "")
	exits, _ := ListUserLandingExits(d, uid)
	if len(exits) != 1 || exits[0].Present || exits[0].UsedBytes != 500 {
		t.Fatalf("ledger-bearing exit must be kept present=0, got %+v", exits)
	}

	// once its ledger is emptied, the next sync sweeps the stale residual even
	// though it was already at present=0.
	d.Exec(`UPDATE user_landing_exits SET used_bytes=0 WHERE user_id=?`, uid)
	SyncUserLandingExits(d, uid, nil, "", "")
	if exits, _ = ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatalf("emptied residual should be swept, got %+v", exits)
	}
}

func TestSyncDiscardsStaleSource(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	d.Exec(`UPDATE users SET landing_sub_url='https://new.example/sub' WHERE id=?`, uid)
	_, synced, err := SyncUserLandingExits(d, uid, inputs("a.com"), "https://old.example/sub", "")
	if err != nil {
		t.Fatal(err)
	}
	if synced {
		t.Fatal("sync resolved from a stale source must be discarded")
	}
	if exits, _ := ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatalf("no rows expected, got %d", len(exits))
	}
}

func TestSyncReturnsFlippedOverQuotaKeys(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	SetUserLandingExitQuota(d, uid, "a.com", 443, 100)
	d.Exec(`UPDATE user_landing_exits SET used_bytes=100 WHERE user_id=?`, uid)

	// present 1→0 on an exhausted exit lifts its push exclusion
	flipped, _, _ := SyncUserLandingExits(d, uid, nil, "", "")
	if len(flipped) != 1 || flipped[0].Host != "a.com" {
		t.Fatalf("want a.com flipped, got %+v", flipped)
	}
	// present 0→1 re-imposes it
	flipped, _, _ = SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	if len(flipped) != 1 {
		t.Fatalf("want flip back reported, got %+v", flipped)
	}
	// steady state reports nothing
	flipped, _, _ = SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	if len(flipped) != 0 {
		t.Fatalf("no flip expected, got %+v", flipped)
	}
}

func TestExitQuotaHelpers(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")

	if updated, present, _ := SetUserLandingExitQuota(d, uid, "a.com", 443, 100); !updated || !present {
		t.Fatal("quota update on present row")
	}
	if updated, _, _ := SetUserLandingExitQuota(d, uid, "nope.com", 443, 100); updated {
		t.Fatal("missing row must report updated=false")
	}
	d.Exec(`UPDATE user_landing_exits SET used_bytes=150 WHERE user_id=?`, uid)
	keys, _ := ExitsExceedingQuota(d, uid)
	if len(keys) != 1 || keys[0].Host != "a.com" {
		t.Fatalf("want a.com exceeding, got %+v", keys)
	}
	if _, _, err := ResetUserLandingExitTraffic(d, uid, "a.com", 443); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ExitsExceedingQuota(d, uid); len(keys) != 0 {
		t.Fatal("reset should clear the overrun")
	}

	// delete is restricted to residual rows
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "present" {
		t.Fatalf("present row must refuse delete, got %q", st)
	}
	SyncUserLandingExits(d, uid, nil, "", "")
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "deleted" {
		t.Fatalf("residual row should delete, got %q", st)
	}
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "notfound" {
		t.Fatalf("gone row is notfound, got %q", st)
	}
}

func TestCycleResetClearsExitLedger(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=?`, uid)
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=strftime('%s','now')-31*86400, last_traffic_reset_at=0 WHERE id=?`, uid)
	u, _ := GetUserByID(d, uid)
	if reset, err := CheckAndResetTrafficCycle(d, u); err != nil || !reset {
		t.Fatalf("reset=%v err=%v", reset, err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatalf("cycle reset must clear the exit ledger, got %d", exits[0].UsedBytes)
	}

	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=?`, uid)
	if err := ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}
	exits, _ = ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatal("manual full reset must clear the exit ledger too")
	}
}

func TestOpenStripsLegacyExitNameSuffixes(t *testing.T) {
	path := t.TempDir() + "/test.db"
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	uid := createTestUser(t, d)
	ins := []LandingExitInput{
		{Host: "a.com", Port: 443, Name: "boil-hkt ^~2~^", Protocol: "ss", URI: "ss://x@a.com:443"},
		{Host: "b.com", Port: 443, Name: "plain", Protocol: "vless", URI: "vless://x@b.com:443"},
	}
	if _, _, err := SyncUserLandingExits(d, uid, ins, "", ""); err != nil {
		t.Fatal(err)
	}
	// Simulate a DB written before parsing stripped the suffix.
	if _, err := d.Exec(`UPDATE user_landing_exits SET name='boil-hkt ^~2~^' WHERE host='a.com'`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exits, err := ListUserLandingExits(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string]string{}
	for _, e := range exits {
		byHost[e.Host] = e.Name
	}
	if byHost["a.com"] != "boil-hkt" {
		t.Errorf("a.com name = %q, want %q", byHost["a.com"], "boil-hkt")
	}
	if byHost["b.com"] != "plain" {
		t.Errorf("b.com name = %q, want %q", byHost["b.com"], "plain")
	}
}

func TestLandingExitNameOverride(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	if _, _, err := SyncUserLandingExits(d, uid, inputs("a.com"), "", ""); err != nil {
		t.Fatal(err)
	}

	// unknown row: no update, no error
	updated, err := SetUserLandingExitName(d, uid, "nope.com", 443, "x")
	if err != nil || updated {
		t.Fatalf("unknown row: updated=%v err=%v", updated, err)
	}

	updated, err = SetUserLandingExitName(d, uid, "a.com", 443, "香港 01")
	if err != nil || !updated {
		t.Fatalf("set: updated=%v err=%v", updated, err)
	}

	// the override must survive a re-sync that overwrites the parsed name
	if _, _, err := SyncUserLandingExits(d, uid, inputs("a.com"), "", ""); err != nil {
		t.Fatal(err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if len(exits) != 1 || exits[0].NameOverride != "香港 01" || exits[0].Name != "n-a.com" {
		t.Fatalf("override lost or name corrupted: %+v", exits[0])
	}

	// empty name clears the override
	if _, err := SetUserLandingExitName(d, uid, "a.com", 443, ""); err != nil {
		t.Fatal(err)
	}
	exits, _ = ListUserLandingExits(d, uid)
	if exits[0].NameOverride != "" {
		t.Fatalf("override not cleared: %+v", exits[0])
	}
}
