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

func TestNodeMultipliers(t *testing.T) {
	d := openTestDB(t)
	n1 := createTestNode(t, d, "x")
	n2 := createTestNode(t, d, "y")
	d.Exec(`UPDATE nodes SET traffic_multiplier=0.5 WHERE id=?`, n2)

	m, err := NodeMultipliers(d)
	if err != nil {
		t.Fatal(err)
	}
	if m[n1] != 1.0 {
		t.Fatalf("n1 want 1.0, got %f", m[n1])
	}
	if m[n2] != 0.5 {
		t.Fatalf("n2 want 0.5, got %f", m[n2])
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
// per-grant quota is enforced in ActiveRuleHopsForPush. The quota is tracked
// on the composite node's user_nodes row (keyed by rules.node_id), not on any
// individual physical hop, so a dedicated NOT EXISTS clause is required.
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
	// Insert a rule_hop manually on the physical node.
	if _, err := d.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,'tcp',12345,'1.2.3.4',80,'kernel','')`, ruleID, physID); err != nil {
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
