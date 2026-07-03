package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// The raw ledger counts every byte the physical node actually forwarded: both
// directions even on unidirectional-billing nodes, and samples whose rule was
// deleted mid-batch. It is an operator metric, not a billing counter, so user
// traffic resets must never touch it.
func TestApplyCountersNodeRawTraffic(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "e", "", "")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
	_ = db.GrantNode(d, uid, n.ID, 5, 0)
	// Unidirectional billing takes uplink only; raw must still take both.
	_ = db.UpdateNodeUnidirectional(d, n.ID, true)
	// A non-1.0 multiplier pins "raw is unweighted": with the default 1.0 a
	// regression that folds the entry multiplier into raw would be invisible.
	_ = db.UpdateNodeRateMultiplier(d, n.ID, 3.0)
	ruleID := createTestRuleDirectNode(t, d, uid, n.ID)
	port := getHopPort(t, d, ruleID, n.ID)

	hub := NewHub(d)
	hub.applyCounters(n.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: port, BytesUp: 300, BytesDown: 700},
		// No rule_hop matches this port (rule deleted between apply and
		// sample): the bytes were still forwarded, so raw counts them.
		{Proto: "tcp", ListenPort: port + 1, BytesUp: 11, BytesDown: 9},
	})

	raw, err := db.NodeRawTraffic(d)
	if err != nil {
		t.Fatal(err)
	}
	if raw[n.ID] != 1020 {
		t.Fatalf("raw bytes want 1020, got %d", raw[n.ID])
	}

	g, err := db.GetNodeGrant(d, uid, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.TrafficUsedBytes != 300 {
		t.Fatalf("billed grant bytes want uplink-only 300, got %d", g.TrafficUsedBytes)
	}
	u, err := db.GetUserByID(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	// Global usage takes the multiplier (300 uplink × 3.0); raw above must not.
	if u.TrafficUsedBytes != 900 {
		t.Fatalf("global billed bytes want 900, got %d", u.TrafficUsedBytes)
	}

	if err := db.ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}
	raw, err = db.NodeRawTraffic(d)
	if err != nil {
		t.Fatal(err)
	}
	if raw[n.ID] != 1020 {
		t.Fatalf("raw bytes must survive user reset: want 1020, got %d", raw[n.ID])
	}
}

// Deleting a node drops its raw ledger row via the FK cascade, so a recreated
// node id can never inherit a dead node's byte count.
func TestNodeRawTrafficCascadeOnNodeDelete(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "gone", "", "")
	if err := db.AddNodeRawTraffic(d, n.ID, 4096); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNode(d, n.ID); err != nil {
		t.Fatal(err)
	}
	raw, err := db.NodeRawTraffic(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw[n.ID]; ok {
		t.Fatalf("raw row must cascade away with the node, got %d", raw[n.ID])
	}
}
