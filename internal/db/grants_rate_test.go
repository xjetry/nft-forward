package db

import "testing"

func TestGrantRateLimitRoundTrip(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "rl")
	grantNode(t, d, uid, nid)

	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=10 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.RateLimitMBytes != 10 {
		t.Fatalf("rate = %d, want 10", g.RateLimitMBytes)
	}

	shapes, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := shapes[[2]int64{uid, nid}]
	if !ok || s.RateLimitMBytes != 10 || s.GrantID <= 0 {
		t.Fatalf("shape = %+v ok=%v, want rate 10 with positive grant id", s, ok)
	}

	// GrantNode upsert must not touch the rate and must keep the rowid stable
	// (the rowid is the shaping group id; churning it would orphan connmarks).
	if err := GrantNode(d, uid, nid, 5, 0); err != nil {
		t.Fatal(err)
	}
	shapes2, _ := GrantShapes(d)
	s2 := shapes2[[2]int64{uid, nid}]
	if s2.GrantID != s.GrantID || s2.RateLimitMBytes != 10 {
		t.Fatalf("after upsert shape = %+v, want unchanged %+v", s2, s)
	}
}

func TestGrantShapesSkipsUnlimited(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "rl0")
	grantNode(t, d, uid, nid)

	shapes, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shapes[[2]int64{uid, nid}]; ok {
		t.Fatal("rate 0 grant must not appear in GrantShapes")
	}
}
