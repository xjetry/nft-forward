package server

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// createTestRuleWithHops inserts a 2-hop rule owned by ownerID.
// n1 is the entry node (position 0), n2 is the relay (position 1).
// Both nodes must have relay_host set before calling.
func createTestRuleWithHops(t *testing.T, d *sql.DB, ownerID, n1, n2 int64) int64 {
	t.Helper()
	rl := &db.Rule{
		NodeID:   n1,
		OwnerID:  sql.NullInt64{Int64: ownerID, Valid: true},
		Name:     "test-two-hop",
		Proto:    "tcp",
		ExitHost: "8.8.8.8",
		ExitPort: 443,
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = id
	if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: n1}, {NodeID: n2}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

// createTestRuleDirectNode inserts a single-hop rule owned by ownerID on nodeID.
// nodeID must have relay_host set before calling.
func createTestRuleDirectNode(t *testing.T, d *sql.DB, ownerID, nodeID int64) int64 {
	t.Helper()
	rl := &db.Rule{
		NodeID:   nodeID,
		OwnerID:  sql.NullInt64{Int64: ownerID, Valid: true},
		Name:     "test-direct",
		Proto:    "tcp",
		ExitHost: "8.8.8.8",
		ExitPort: 443,
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = id
	if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: nodeID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

// getHopPort returns the listen_port allocated for the hop on nodeID in ruleID.
func getHopPort(t *testing.T, d *sql.DB, ruleID, nodeID int64) int {
	t.Helper()
	hops, err := db.ListRuleHops(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hops {
		if h.NodeID == nodeID {
			return h.ListenPort
		}
	}
	t.Fatalf("no hop found for ruleID=%d nodeID=%d", ruleID, nodeID)
	return 0
}

func TestApplyCountersMultiplier(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	// Composite node topology: comp → n1 (mult=1.0), n2 (mult=0.5)
	comp, _ := db.CreateNode(d, "comp", "", "")
	d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, comp.ID)

	n1, _ := db.CreateNode(d, "entry", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")

	n2, _ := db.CreateNode(d, "relay", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")

	db.CreateNodeHops(d, comp.ID, []db.NodeHop{
		{NodeID: comp.ID, Position: 0, HopNodeID: n1.ID, Mode: "kernel", TrafficMultiplier: 1.0},
		{NodeID: comp.ID, Position: 1, HopNodeID: n2.ID, Mode: "kernel", TrafficMultiplier: 0.5},
	})

	db.GrantNode(d, uid, comp.ID, 10, 0)
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)

	// Create a rule on the composite node and manually insert rule_hops on physical nodes.
	rl := &db.Rule{
		NodeID:  comp.ID,
		OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name:    "test-comp", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443,
	}
	ruleID, _ := db.CreateRule(d, rl)
	now := time.Now().Unix()
	_ = now
	d.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,'tcp',10001,'2.2.2.2',10002,'kernel','')`, ruleID, n1.ID)
	d.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,1,?,'tcp',10002,'8.8.8.8',443,'kernel','')`, ruleID, n2.ID)

	s, _ := New(d)

	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: 10001, BytesUp: 500, BytesDown: 500},
	})
	s.Hub.applyCounters(n2.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: 10002, BytesUp: 500, BytesDown: 500},
	})

	u, _ := db.GetUserByID(d, uid)
	// global: 1000*1.0 + 1000*0.5 = 1500
	if u.TrafficUsedBytes != 1500 {
		t.Fatalf("global traffic want 1500, got %d", u.TrafficUsedBytes)
	}

	g1, _ := db.GetNodeGrant(d, uid, n1.ID)
	if g1.TrafficUsedBytes != 1000 {
		t.Fatalf("n1 per-node want 1000, got %d", g1.TrafficUsedBytes)
	}
	g2, _ := db.GetNodeGrant(d, uid, n2.ID)
	// per-node stores weighted bytes: 1000 * 0.5 = 500
	if g2.TrafficUsedBytes != 500 {
		t.Fatalf("n2 per-node want 500, got %d", g2.TrafficUsedBytes)
	}
}

func TestApplyCountersZeroMultiplier(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	// Composite node with a single free hop (multiplier=0, inserted directly).
	comp, _ := db.CreateNode(d, "comp-free", "", "")
	d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, comp.ID)

	n1, _ := db.CreateNode(d, "free-relay", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "3.3.3.3")

	// Bypass CreateNodeHops (which coerces 0→1.0) to exercise the zero-mult path.
	d.Exec(`INSERT INTO node_hops(node_id,position,hop_node_id,mode,traffic_multiplier) VALUES (?,0,?,'kernel',0.0)`, comp.ID, n1.ID)

	db.GrantNode(d, uid, comp.ID, 10, 0)
	db.GrantNode(d, uid, n1.ID, 10, 0)

	rl := &db.Rule{
		NodeID:  comp.ID,
		OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name:    "test-free", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443,
	}
	ruleID, _ := db.CreateRule(d, rl)
	d.Exec(`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,'tcp',10001,'8.8.8.8',443,'kernel','')`, ruleID, n1.ID)

	s, _ := New(d)

	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: 10001, BytesUp: 2500, BytesDown: 2500},
	})

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("multiplier=0 should not add to global, got %d", u.TrafficUsedBytes)
	}
	g, _ := db.GetNodeGrant(d, uid, n1.ID)
	// multiplier=0 → weighted=0 → per-node stays 0 (consistent with global)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("per-node should be 0 when multiplier=0, got %d", g.TrafficUsedBytes)
	}
}
