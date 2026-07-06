package server

import (
	"fmt"
	"testing"

	"nft-forward/internal/db"
)

// Billing groups rule_hops by (rule_id, via_node_id) and charges each logical
// segment once at its via (db.SegmentFirstHops). A composite is the grant unit:
// all its flattened physical children must share via = the composite's id so the
// traffic bills to the composite's grant, not to the (ungranted) children. The
// admin explicit-hops escape hatch must therefore let the caller tag each hop
// with the logical segment it belongs to.
func TestExplicitHopsCreateHonorsViaNodeID(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	rid := adminCreateRuleID(t, s, admin, map[string]any{
		"name": "r-explicit", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{
			{"node_id": c1.ID, "via_node_id": comp.ID, "mode": "kernel"},
			{"node_id": c2.ID, "via_node_id": comp.ID, "mode": "kernel"},
		},
	})
	hops, _ := db.ListRuleHops(d, rid)
	if len(hops) != 2 {
		t.Fatalf("want 2 hops, got %d", len(hops))
	}
	for i, h := range hops {
		if h.ViaNodeID != comp.ID {
			t.Errorf("hop %d (node %d) via = %d, want composite %d", i, h.NodeID, h.ViaNodeID, comp.ID)
		}
	}
}

// Absent via_node_id keeps the escape hatch's raw semantics: each physical hop
// is its own logical segment (via = node_id), correct for an arbitrary physical
// chain that isn't a composite. Backward compatible with old callers.
func TestExplicitHopsCreateViaDefaultsToNode(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	rid := adminCreateRuleID(t, s, admin, map[string]any{
		"name": "r-plain", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{
			{"node_id": a.ID, "mode": "kernel"},
			{"node_id": b.ID, "mode": "kernel"},
		},
	})
	hops, _ := db.ListRuleHops(d, rid)
	if len(hops) != 2 || hops[0].ViaNodeID != a.ID || hops[1].ViaNodeID != b.ID {
		t.Fatalf("plain explicit hops via should default to node id, got %+v", hops)
	}
}

// The update path's explicit-hops branch tags via the same way, so editing a
// rule into a composite's flattened chain also bills to the composite.
func TestExplicitHopsUpdateHonorsViaNodeID(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	rid := adminCreateRuleID(t, s, admin, map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{{"node_id": c1.ID, "mode": "kernel"}, {"node_id": c2.ID, "mode": "kernel"}},
	})
	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", rid), map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{
			{"node_id": c1.ID, "via_node_id": comp.ID, "mode": "kernel"},
			{"node_id": c2.ID, "via_node_id": comp.ID, "mode": "kernel"},
		},
	})
	hops, _ := db.ListRuleHops(d, rid)
	if len(hops) != 2 {
		t.Fatalf("want 2 hops, got %d", len(hops))
	}
	for i, h := range hops {
		if h.ViaNodeID != comp.ID {
			t.Errorf("updated hop %d (node %d) via = %d, want composite %d", i, h.NodeID, h.ViaNodeID, comp.ID)
		}
	}
}
