package db

import (
	"database/sql"
	"testing"
)

func TestAddUserNodeTraffic(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "n1")
	grantNode(t, d, uid, nid)

	if err := AddUserNodeTraffic(d, uid, nid, 1000); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.TrafficUsedBytes != 1000 {
		t.Fatalf("want 1000, got %d", g.TrafficUsedBytes)
	}

	// accumulate
	if err := AddUserNodeTraffic(d, uid, nid, 500); err != nil {
		t.Fatal(err)
	}
	g, _ = GetNodeGrant(d, uid, nid)
	if g.TrafficUsedBytes != 1500 {
		t.Fatalf("want 1500, got %d", g.TrafficUsedBytes)
	}
}

func TestResetAllUserTraffic(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	n1 := createTestNode(t, d, "a")
	n2 := createTestNode(t, d, "b")
	grantNode(t, d, uid, n1)
	grantNode(t, d, uid, n2)

	_ = AddUserTraffic(d, uid, 5000)
	_ = AddUserNodeTraffic(d, uid, n1, 2000)
	_ = AddUserNodeTraffic(d, uid, n2, 3000)

	if err := ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}
	u, _ := GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global not reset: %d", u.TrafficUsedBytes)
	}
	g1, _ := GetNodeGrant(d, uid, n1)
	g2, _ := GetNodeGrant(d, uid, n2)
	if g1.TrafficUsedBytes != 0 || g2.TrafficUsedBytes != 0 {
		t.Fatalf("per-node not reset: %d, %d", g1.TrafficUsedBytes, g2.TrafficUsedBytes)
	}
}

// seedRuleWithTraffic creates one single-hop rule owned by uid with the given
// accumulated display counter and a nonzero last_bytes snapshot.
func seedRuleWithTraffic(t *testing.T, d *sql.DB, uid, nodeID int64, listenPort int, totalBytes int64) (hopID int64) {
	t.Helper()
	res, err := d.Exec(`INSERT INTO rules(node_id, owner_id, name, proto, exit_host, exit_port, created_at) VALUES (?,?,?,?,?,?,0)`,
		nodeID, uid, "r", "tcp", "8.8.8.8", 443)
	if err != nil {
		t.Fatal(err)
	}
	ruleID, _ := res.LastInsertId()
	res, err = d.Exec(`INSERT INTO rule_hops(rule_id, position, node_id, proto, listen_port, target_host, target_port, last_bytes, total_bytes) VALUES (?,0,?,?,?,?,?,777,?)`,
		ruleID, nodeID, "tcp", listenPort, "8.8.8.8", 443, totalBytes)
	if err != nil {
		t.Fatal(err)
	}
	hopID, _ = res.LastInsertId()
	return hopID
}

func TestResetAllUserTrafficClearsRuleCounters(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	other := createTestUser(t, d)
	n1 := createTestNode(t, d, "rc")
	mine := seedRuleWithTraffic(t, d, uid, n1, 20001, 5000)
	theirs := seedRuleWithTraffic(t, d, other, n1, 20002, 6000)

	if err := ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}

	var total, last int64
	if err := d.QueryRow(`SELECT total_bytes, last_bytes FROM rule_hops WHERE id=?`, mine).Scan(&total, &last); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("user's rule counter should reset with the user ledgers, got %d", total)
	}
	if last != 777 {
		t.Fatalf("last_bytes is the agent counter snapshot for delta computation and must survive a reset, got %d", last)
	}
	if err := d.QueryRow(`SELECT total_bytes FROM rule_hops WHERE id=?`, theirs).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 6000 {
		t.Fatalf("another user's rule counter must be untouched, got %d", total)
	}
}

// SegmentFirstHops maps each logical segment's first hop position to the
// segment's logical node. Per-grant accounting charges a segment once, at that
// first hop; every hop of a segment carries the same bytes.
func TestSegmentFirstHops(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "e", "", "")
	b, _ := CreateNode(d, "m1", "", "")
	c, _ := CreateNode(d, "m2", "", "")
	_ = UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = UpdateNodeRelayHost(d, c.ID, "3.3.3.3")
	r := &Rule{NodeID: a.ID, Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	tx, _ := d.Begin()
	id, _ := CreateRule(tx, r)
	r.ID = id
	// entry segment a; a middle-layer segment carrying both b and c (both hops
	// share the layer's logical node id b.ID as their via).
	_, _, _, err := RegenerateRule(tx, r, []HopInput{
		{NodeID: a.ID, Mode: "userspace", ViaNodeID: a.ID},
		{NodeID: b.ID, Mode: "userspace", ViaNodeID: b.ID},
		{NodeID: c.ID, Mode: "userspace", ViaNodeID: b.ID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	m, err := SegmentFirstHops(d, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{0: a.ID, 1: b.ID}
	if len(m[id]) != 2 || m[id][0] != want[0] || m[id][1] != want[1] {
		t.Fatalf("segment firsts want %v, got %v", want, m[id])
	}
}

func TestCheckAndResetTrafficCycle(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "n")
	grantNode(t, d, uid, nid)

	// reset_days=0 means never reset
	u, _ := GetUserByID(d, uid)
	reset, _ := CheckAndResetTrafficCycle(d, u)
	if reset {
		t.Fatal("reset_days=0 should not trigger reset")
	}

	// set reset_days=30, created_at=31 days ago, add traffic
	past := now() - 31*86400
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=? WHERE id=?`, past, uid)
	_ = AddUserTraffic(d, uid, 9999)
	_ = AddUserNodeTraffic(d, uid, nid, 8888)

	u, _ = GetUserByID(d, uid)
	reset, _ = CheckAndResetTrafficCycle(d, u)
	if !reset {
		t.Fatal("should have reset after 31 days with 30-day cycle")
	}
	u, _ = GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global not reset: %d", u.TrafficUsedBytes)
	}
	g, _ := GetNodeGrant(d, uid, nid)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("per-node not reset: %d", g.TrafficUsedBytes)
	}

	// calling again in the same cycle should not reset
	_ = AddUserTraffic(d, uid, 100)
	u, _ = GetUserByID(d, uid)
	reset, _ = CheckAndResetTrafficCycle(d, u)
	if reset {
		t.Fatal("should not reset again in same cycle")
	}
	u, _ = GetUserByID(d, uid)
	if u.TrafficUsedBytes != 100 {
		t.Fatalf("traffic should remain at 100, got %d", u.TrafficUsedBytes)
	}
}

func TestNodesExceedingQuota(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	n1 := createTestNode(t, d, "q1")
	n2 := createTestNode(t, d, "q2")
	grantNode(t, d, uid, n1)
	grantNode(t, d, uid, n2)

	// set n1 quota=1000, used=1000 (exactly at limit = exceeded)
	d.Exec(`UPDATE user_nodes SET traffic_quota_bytes=1000, traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1)
	// n2 no quota (0)
	exceeded, err := NodesExceedingQuota(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(exceeded) != 1 || exceeded[0] != n1 {
		t.Fatalf("want [%d], got %v", n1, exceeded)
	}
}

// TestActiveRuleHopsForPushCompositeQuota verifies that a composite node's
// per-grant quota is enforced in ActiveRuleHopsForPush. The quota is tracked on
// the composite's user_nodes row (its logical node); every physical hop of the
// composite carries the composite id as its via_node_id, so the logical-segment
// match suppresses the whole chain when that grant is exhausted.
func TestActiveRuleHopsForPushCompositeQuota(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)

	// Create a composite node and one physical hop node.
	compID := createTestNode(t, d, "composite")
	physID := createTestNode(t, d, "phys")
	d.Exec(`UPDATE nodes SET node_type='composite', relay_host='10.0.0.1' WHERE id=?`, compID)
	d.Exec(`UPDATE nodes SET relay_host='10.0.0.2' WHERE id=?`, physID)

	// Grant both; set a quota on the composite node only.
	if err := GrantNode(d, uid, compID, 10, 1000); err != nil {
		t.Fatal(err)
	}
	if err := GrantNode(d, uid, physID, 10, 0); err != nil {
		t.Fatal(err)
	}

	// Create a rule on the composite node with the physical node as its hop.
	ownerID := sql.NullInt64{Valid: true, Int64: uid}
	r := &Rule{NodeID: compID, OwnerID: ownerID, Name: "r", Proto: "tcp", ExitHost: "1.2.3.4", ExitPort: 80}
	ruleID, err := CreateRule(d, r)
	if err != nil {
		t.Fatal(err)
	}
	// Insert a rule_hop manually on the physical node. Its via is the composite
	// (the entry segment's via is rules.node_id), which is how RegenerateRule
	// records a composite chain's hops.
	if _, err := d.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment,via_node_id) VALUES (?,0,?,'tcp',12345,'1.2.3.4',80,'kernel','',?)`, ruleID, physID, compID); err != nil {
		t.Fatal(err)
	}

	// Before quota is hit: hop should be included.
	hops, err := ActiveRuleHopsForPush(d, physID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 {
		t.Fatalf("before quota: want 1 hop, got %d", len(hops))
	}

	// Exceed the composite node's quota.
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, compID)

	// After quota is hit: hop must be excluded.
	hops, err = ActiveRuleHopsForPush(d, physID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 0 {
		t.Fatalf("after quota exceeded: want 0 hops, got %d", len(hops))
	}
}

// --- test helpers ---

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func createTestUser(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	id, err := CreateUser(d, "testuser-"+RandToken(4), "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createTestNode(t *testing.T, d *sql.DB, name string) int64 {
	t.Helper()
	n, err := CreateNode(d, name+"-"+RandToken(4), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func grantNode(t *testing.T, d *sql.DB, uid, nid int64) {
	t.Helper()
	if err := GrantNode(d, uid, nid, 10, 0); err != nil {
		t.Fatal(err)
	}
}
