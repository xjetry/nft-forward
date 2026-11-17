package db

import (
	"database/sql"
	"errors"
	"testing"
)

func applyGrantsFixture(t *testing.T) (d *sql.DB, uid1, uid2, nid1, nid2 int64) {
	t.Helper()
	d = openTestDB(t)
	var err error
	if uid1, err = CreateUser(d, "apply-a", "h", "user"); err != nil {
		t.Fatal(err)
	}
	if uid2, err = CreateUser(d, "apply-b", "h", "user"); err != nil {
		t.Fatal(err)
	}
	n1, err := CreateNode(d, "apply-n1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := CreateNode(d, "apply-n2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return d, uid1, uid2, n1.ID, n2.ID
}

func grantCount(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM user_nodes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApplyGrantsOverwritesCapsAndKeepsTheRest(t *testing.T) {
	d, uid, other, nid, _ := applyGrantsFixture(t)
	if err := GrantNode(d, uid, nid, 3, 100); err != nil {
		t.Fatal(err)
	}
	// A later row, so that a delete-and-reinsert of the first grant would be
	// handed a different rowid instead of reusing the freed one.
	if err := GrantNode(d, other, nid, 3, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes=42, roles=2, rate_limit_mbytes=9 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}
	before, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	written, err := ApplyGrants(d, []GrantWrite{{UserID: uid, NodeID: nid, MaxForwards: 7, TrafficQuotaBytes: 500, RateLimitMBytes: 4}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || !written[0] {
		t.Fatalf("want the existing grant reported as written, got %v", written)
	}
	after, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	key := [2]int64{uid, nid}
	if before[key].GrantID <= 0 || after[key].GrantID != before[key].GrantID {
		t.Fatalf("overwrite must keep the grant's rowid (the shaping group id): before %+v after %+v", before[key], after[key])
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxForwards != 7 || g.TrafficQuotaBytes != 500 || g.RateLimitMBytes != 4 {
		t.Fatalf("caps not overwritten: %+v", g)
	}
	if g.TrafficUsedBytes != 42 || g.Roles != 2 {
		t.Fatalf("usage and roles must survive an overwrite: %+v", g)
	}
}

func TestApplyGrantsKeepExistingLeavesGrantUntouched(t *testing.T) {
	d, uid, _, nid1, nid2 := applyGrantsFixture(t)
	if err := GrantNode(d, uid, nid1, 3, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=9 WHERE user_id=? AND node_id=?`, uid, nid1); err != nil {
		t.Fatal(err)
	}
	written, err := ApplyGrants(d, []GrantWrite{
		{UserID: uid, NodeID: nid1, MaxForwards: 10, TrafficQuotaBytes: 0, RateLimitMBytes: 0},
		{UserID: uid, NodeID: nid2, MaxForwards: 10, TrafficQuotaBytes: 0, RateLimitMBytes: 5},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || written[0] || !written[1] {
		t.Fatalf("want [kept, written], got %v", written)
	}
	kept, err := GetNodeGrant(d, uid, nid1)
	if err != nil {
		t.Fatal(err)
	}
	if kept.MaxForwards != 3 || kept.TrafficQuotaBytes != 100 || kept.RateLimitMBytes != 9 {
		t.Fatalf("existing grant was modified: %+v", kept)
	}
	added, err := GetNodeGrant(d, uid, nid2)
	if err != nil {
		t.Fatal(err)
	}
	if added.MaxForwards != 10 || added.RateLimitMBytes != 5 {
		t.Fatalf("missing grant not written as given: %+v", added)
	}
}

func TestApplyGrantsIsAllOrNothing(t *testing.T) {
	d, uid, _, nid1, nid2 := applyGrantsFixture(t)
	const noSuchUser = 987654
	_, err := ApplyGrants(d, []GrantWrite{
		{UserID: uid, NodeID: nid1, MaxForwards: 10},
		{UserID: noSuchUser, NodeID: nid2, MaxForwards: 10},
	}, false)
	if err == nil {
		t.Fatal("want an error for a user id that does not exist")
	}
	if n := grantCount(t, d); n != 0 {
		t.Fatalf("a failed batch must write nothing, found %d grants", n)
	}
	if _, err := GetNodeGrant(d, uid, nid1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the row before the failing one must be rolled back, got err=%v", err)
	}
}
