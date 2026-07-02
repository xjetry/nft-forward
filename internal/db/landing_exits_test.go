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
