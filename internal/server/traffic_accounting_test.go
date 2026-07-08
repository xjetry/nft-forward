package server

import (
	"database/sql"
	"testing"

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

// A unidirectional node bills max(up,down) per sample: whichever direction
// dominates is charged and the quieter one is folded away. Both directions are
// exercised so the max can't degrade into an always-uplink or always-downlink
// pick. A bidirectional node keeps billing up+down (asserted elsewhere).
func TestApplyCountersUnidirectionalMax(t *testing.T) {
	for _, tc := range []struct {
		name           string
		up, down, want int64
	}{
		{"uplink heavier", 900, 200, 900},
		{"downlink heavier", 200, 900, 900},
		{"equal", 500, 500, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDB(t)
			uid, _ := loginAsUser(t, d, 10)
			n, _ := db.CreateNode(d, "u", "", "")
			_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
			_ = db.GrantNode(d, uid, n.ID, 5, 0)
			_ = db.UpdateNodeUnidirectional(d, n.ID, true)
			ruleID := createTestRuleDirectNode(t, d, uid, n.ID)
			port := getHopPort(t, d, ruleID, n.ID)

			h := NewHub(d)
			h.applyCounters(n.ID, []wsproto.CounterSample{
				{Proto: "tcp", ListenPort: port, BytesUp: tc.up, BytesDown: tc.down},
			})

			g, err := db.GetNodeGrant(d, uid, n.ID)
			if err != nil {
				t.Fatal(err)
			}
			if g.TrafficUsedBytes != tc.want {
				t.Fatalf("unidirectional billed want max(%d,%d)=%d, got %d", tc.up, tc.down, tc.want, g.TrafficUsedBytes)
			}
		})
	}
}

// The same bytes flow through every hop, so the global user quota is billed
// exactly once — at the entry hop — with the entry node's own rate_multiplier
// (2.0 here). Per-grant quota charges raw bytes once per logical segment: the
// entry grant and the middle-layer grant each accrue the real 1000 bytes, and
// the layer's second physical hop bills no grant of its own. The layer node's
// own 3.0 multiplier never enters billing — only the entry's factor does.
func TestBillingEntryOnlyAndRawGrantBytes(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "e", "", "")
	m1, _ := db.CreateNode(d, "m1", "", "")
	m2, _ := db.CreateNode(d, "m2", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, m1.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, m2.ID, "3.3.3.3")
	_ = db.UpdateNodeRateMultiplier(d, entry.ID, 2.0)
	_ = db.UpdateNodeRateMultiplier(d, m1.ID, 3.0)
	mid := makeComposite(t, d, "layer", m1.ID, m2.ID)

	uid, _ := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)

	r := &db.Rule{NodeID: entry.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, ViaNodeIDs: []int64{mid.ID}}
	tx, _ := d.Begin()
	id, _ := db.CreateRule(tx, r)
	r.ID = id
	_, _, _, err := db.RegenerateRule(tx, r, []db.HopInput{
		{NodeID: entry.ID, Mode: "userspace", ViaNodeID: entry.ID},
		{NodeID: m1.ID, Mode: "userspace", ViaNodeID: mid.ID},
		{NodeID: m2.ID, Mode: "userspace", ViaNodeID: mid.ID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	hops, _ := db.ListRuleHops(d, id)

	h := NewHub(d)
	// each hop reports the same 1000 bytes (one flow through three hops)
	for _, hp := range hops {
		h.applyCounters(hp.NodeID, []wsproto.CounterSample{
			{Proto: "tcp", ListenPort: hp.ListenPort, BytesUp: 600, BytesDown: 400},
		})
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 2000 { // 1000 × entry multiplier 2.0, billed once
		t.Fatalf("global used want 2000, got %d", u.TrafficUsedBytes)
	}
	ge, _ := db.GetNodeGrant(d, uid, entry.ID)
	gm, _ := db.GetNodeGrant(d, uid, mid.ID)
	if ge.TrafficUsedBytes != 1000 || gm.TrafficUsedBytes != 1000 {
		t.Fatalf("grant raw bytes want 1000/1000, got %d/%d", ge.TrafficUsedBytes, gm.TrafficUsedBytes)
	}
	// hop totals: entry hop stores the billed 2000; every other hop stores raw 1000
	hops, _ = db.ListRuleHops(d, id)
	if hops[0].TotalBytes != 2000 || hops[1].TotalBytes != 1000 || hops[2].TotalBytes != 1000 {
		t.Fatalf("hop totals: %d/%d/%d", hops[0].TotalBytes, hops[1].TotalBytes, hops[2].TotalBytes)
	}
}

// A composite entry's own rate_multiplier is the whole rule's billing factor,
// applied once at the entry hop; the dormant per-hop node_hops multipliers no
// longer enter billing. Per-grant quota charges raw bytes once per logical
// segment: a plain composite rule is one segment (via = the composite), so only
// the composite grant accrues — its physical child nodes carry no grant.
func TestApplyCountersMultiplier(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "entry", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "relay", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", n1.ID, n2.ID)
	// The composite's own column holds the baked billing multiplier (1.0 + 0.5).
	_ = db.UpdateNodeRateMultiplier(d, comp.ID, 1.5)

	db.GrantNode(d, uid, comp.ID, 10, 0)
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)

	// Composite rule: chain comp→n1→n2, every hop on the composite's logical
	// segment (via = comp).
	rl := &db.Rule{
		NodeID:  comp.ID,
		OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name:    "test-comp", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443,
	}
	tx, _ := d.Begin()
	ruleID, _ := db.CreateRule(tx, rl)
	rl.ID = ruleID
	if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{
		{NodeID: n1.ID, Mode: "kernel", ViaNodeID: comp.ID},
		{NodeID: n2.ID, Mode: "kernel", ViaNodeID: comp.ID},
	}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	hops, _ := db.ListRuleHops(d, ruleID)

	s, _ := New(d)
	// Same 1000 bytes flow through both hops.
	for _, hp := range hops {
		s.Hub.applyCounters(hp.NodeID, []wsproto.CounterSample{
			{Proto: "tcp", ListenPort: hp.ListenPort, BytesUp: 500, BytesDown: 500},
		})
	}

	u, _ := db.GetUserByID(d, uid)
	// billed once at the entry hop: 1000 × composite multiplier 1.5
	if u.TrafficUsedBytes != 1500 {
		t.Fatalf("global traffic want 1500, got %d", u.TrafficUsedBytes)
	}

	gc, _ := db.GetNodeGrant(d, uid, comp.ID)
	// raw bytes, charged once for the composite's single logical segment
	if gc.TrafficUsedBytes != 1000 {
		t.Fatalf("composite grant want raw 1000, got %d", gc.TrafficUsedBytes)
	}
	g1, _ := db.GetNodeGrant(d, uid, n1.ID)
	g2, _ := db.GetNodeGrant(d, uid, n2.ID)
	// physical child nodes are not logical segments of the rule → no charge
	if g1.TrafficUsedBytes != 0 || g2.TrafficUsedBytes != 0 {
		t.Fatalf("physical child grants must stay 0, got %d/%d", g1.TrafficUsedBytes, g2.TrafficUsedBytes)
	}
}

// A rate_multiplier of 0 is a deliberate free marker: global usage accrues
// nothing and the entry hop's billed total_bytes is 0, while the per-grant quota
// still charges the raw bytes that actually flowed. Only a negative or absent
// multiplier falls back to 1.0 (see TestApplyCountersNegativeMultiplierBillsAtUnit).
func TestApplyCountersZeroMultiplierIsFree(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "free-relay", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "3.3.3.3")
	comp := makeComposite(t, d, "comp-free", n1.ID)
	_ = db.UpdateNodeRateMultiplier(d, comp.ID, 0.0) // free entry

	db.GrantNode(d, uid, comp.ID, 10, 0)
	db.GrantNode(d, uid, n1.ID, 10, 0)

	rl := &db.Rule{
		NodeID:  comp.ID,
		OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name:    "test-free", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443,
	}
	tx, _ := d.Begin()
	ruleID, _ := db.CreateRule(tx, rl)
	rl.ID = ruleID
	if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{
		{NodeID: n1.ID, Mode: "kernel", ViaNodeID: comp.ID},
	}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	hops, _ := db.ListRuleHops(d, ruleID)

	s, _ := New(d)
	s.Hub.applyCounters(hops[0].NodeID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: hops[0].ListenPort, BytesUp: 2500, BytesDown: 2500},
	})

	u, _ := db.GetUserByID(d, uid)
	// free entry multiplier (0) → global usage accrues nothing
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("free entry multiplier must bill 0 global usage, got %d", u.TrafficUsedBytes)
	}
	gc, _ := db.GetNodeGrant(d, uid, comp.ID)
	// per-grant quota still charges the raw bytes that flowed
	if gc.TrafficUsedBytes != 5000 {
		t.Fatalf("free entry per-grant quota want raw 5000, got %d", gc.TrafficUsedBytes)
	}
	hops, _ = db.ListRuleHops(d, ruleID)
	// entry hop stores the billed amount, which is 0 under a free multiplier
	if hops[0].TotalBytes != 0 {
		t.Fatalf("free entry hop total_bytes (billed) want 0, got %d", hops[0].TotalBytes)
	}
}

// A negative multiplier is invalid input, not a free marker, so billing falls
// back to 1.0. The dedicated endpoint never persists a negative, so set it via
// raw SQL to exercise the guard directly.
func TestApplyCountersNegativeMultiplierBillsAtUnit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "neg-relay", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "4.4.4.4")
	comp := makeComposite(t, d, "comp-neg", n1.ID)
	if _, err := d.Exec(`UPDATE nodes SET rate_multiplier = -2.0 WHERE id=?`, comp.ID); err != nil {
		t.Fatal(err)
	}

	db.GrantNode(d, uid, comp.ID, 10, 0)
	db.GrantNode(d, uid, n1.ID, 10, 0)

	rl := &db.Rule{
		NodeID:  comp.ID,
		OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name:    "test-neg", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443,
	}
	tx, _ := d.Begin()
	ruleID, _ := db.CreateRule(tx, rl)
	rl.ID = ruleID
	if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{
		{NodeID: n1.ID, Mode: "kernel", ViaNodeID: comp.ID},
	}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	hops, _ := db.ListRuleHops(d, ruleID)

	s, _ := New(d)
	s.Hub.applyCounters(hops[0].NodeID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: hops[0].ListenPort, BytesUp: 2500, BytesDown: 2500},
	})

	u, _ := db.GetUserByID(d, uid)
	// negative multiplier coerced to 1.0 → entry billed once at 5000
	if u.TrafficUsedBytes != 5000 {
		t.Fatalf("negative multiplier must bill at unit (5000), got %d", u.TrafficUsedBytes)
	}
	gc, _ := db.GetNodeGrant(d, uid, comp.ID)
	// per-segment grant is raw bytes, independent of the billing multiplier
	if gc.TrafficUsedBytes != 5000 {
		t.Fatalf("composite grant want raw 5000, got %d", gc.TrafficUsedBytes)
	}
}
