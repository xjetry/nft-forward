package db

import (
	"database/sql"
	"testing"
)

// seedRuleWithTraffic 建一条 uid 名下的单跳规则，规则的加权流量
// （rules.weighted_bytes）与这一跳的逐跳流量（rule_hops.total_bytes）都记 totalBytes。
func seedRuleWithTraffic(t *testing.T, d *sql.DB, uid, nodeID int64, listenPort int, totalBytes int64) (hopID int64) {
	t.Helper()
	res, err := d.Exec(`INSERT INTO rules(node_id, owner_id, name, proto, exit_host, exit_port, created_at, weighted_bytes) VALUES (?,?,?,?,?,?,0,?)`,
		nodeID, uid, "r", "tcp", "8.8.8.8", 443, totalBytes)
	if err != nil {
		t.Fatal(err)
	}
	ruleID, _ := res.LastInsertId()
	res, err = d.Exec(`INSERT INTO rule_hops(rule_id, position, node_id, proto, listen_port, target_host, target_port, total_bytes) VALUES (?,0,?,?,?,?,?,?)`,
		ruleID, nodeID, "tcp", listenPort, "8.8.8.8", 443, totalBytes)
	if err != nil {
		t.Fatal(err)
	}
	hopID, _ = res.LastInsertId()
	return hopID
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

// The via_node_id backfill assigns each physical hop the logical segment it
// belongs to. A composite-entry chain is a single entry segment, so every hop
// keeps the composite entry id and the whole chain bills and suppresses on the
// composite grant. An explicit-hops chain (non-composite entry) is one segment
// per hop, so each hop must carry its own node id — otherwise a downstream hop's
// per-node grant would stop metering and quota-suppressing. This pins that
// per-hop rewrite, and that composite-entry chains are left on the entry id.
func TestLegacyChainBackfillPerHopSegments(t *testing.T) {
	d := openTestDB(t)

	plain, _ := CreateNode(d, "plain-entry", "", "") // node_type 'remote'
	relay, _ := CreateNode(d, "relay", "", "")
	comp, _ := CreateNode(d, "comp-entry", "", "")
	if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, comp.ID); err != nil {
		t.Fatal(err)
	}
	child, _ := CreateNode(d, "comp-child", "", "")

	insertRule := func(entryNodeID int64) int64 {
		res, err := d.Exec(`INSERT INTO rules(node_id, proto, exit_host, exit_port, created_at) VALUES (?, 'tcp', '9.9.9.9', 443, ?)`, entryNodeID, now())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	// via = entryNodeID for every hop reproduces the first backfill
	// (via_node_id = rules.node_id) before the per-segment refinement runs.
	port := 20000
	insertHop := func(ruleID, position, hopNode, via int64) {
		port++
		if _, err := d.Exec(`INSERT INTO rule_hops(rule_id, position, node_id, proto, listen_port, target_host, target_port, via_node_id)
			VALUES (?, ?, ?, 'tcp', ?, '127.0.0.1', 443, ?)`, ruleID, position, hopNode, port, via); err != nil {
			t.Fatal(err)
		}
	}

	explicitRule := insertRule(plain.ID)
	insertHop(explicitRule, 0, plain.ID, plain.ID)
	insertHop(explicitRule, 1, relay.ID, plain.ID)

	compRule := insertRule(comp.ID)
	insertHop(compRule, 0, child.ID, comp.ID)
	insertHop(compRule, 1, relay.ID, comp.ID)

	// The refining statement verbatim from migration 0033: explicit-hops chains
	// get each hop's via rewritten to its own node; composite-entry chains are
	// left untouched.
	if _, err := d.Exec(`UPDATE rule_hops SET via_node_id = node_id
		WHERE rule_id IN (SELECT r.id FROM rules r JOIN nodes n ON n.id = r.node_id WHERE n.node_type != 'composite')`); err != nil {
		t.Fatal(err)
	}

	viaOf := func(ruleID, position int64) int64 {
		var via int64
		if err := d.QueryRow(`SELECT via_node_id FROM rule_hops WHERE rule_id=? AND position=?`, ruleID, position).Scan(&via); err != nil {
			t.Fatal(err)
		}
		return via
	}

	if got := viaOf(explicitRule, 0); got != plain.ID {
		t.Fatalf("explicit entry hop via: want %d, got %d", plain.ID, got)
	}
	if got := viaOf(explicitRule, 1); got != relay.ID {
		t.Fatalf("explicit downstream hop via: want own node %d, got %d", relay.ID, got)
	}
	if got := viaOf(compRule, 0); got != comp.ID {
		t.Fatalf("composite hop 0 via: want entry %d, got %d", comp.ID, got)
	}
	if got := viaOf(compRule, 1); got != comp.ID {
		t.Fatalf("composite hop 1 via: want entry %d, got %d", comp.ID, got)
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
