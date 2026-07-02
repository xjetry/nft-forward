package server

import (
	"encoding/json"
	"testing"

	"nft-forward/internal/db"
)

// Creating a composite via the API accepts a top-level rate_multiplier, the
// same field the remote-node branch uses, and persists it on the composite's
// own node row. Per-hop traffic_multiplier input is accepted (for backward
// compatibility with older clients) but ignored: hops are always stored with
// a dormant 0, since composite pricing no longer sums hop multipliers.
func TestCompositeHopMultCreateUsesOwnRateMultiplier(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hop-a", "", "")
	b, _ := db.CreateNode(d, "hop-b", "", "")
	_ = db.UpdateNodeRateMultiplier(d, a.ID, 2.0)
	_ = db.UpdateNodeRateMultiplier(d, b.ID, 3.0)

	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	rec := adminJSON(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "chain", "node_type": "composite", "rate_multiplier": 2.5,
		"hops": []map[string]any{
			{"node_id": a.ID, "mode": "userspace", "traffic_multiplier": 0.5},
			{"node_id": b.ID, "mode": "userspace"},
		},
	})
	var resp struct {
		Node struct {
			ID             int64   `json:"id"`
			RateMultiplier float64 `json:"rate_multiplier"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Node.RateMultiplier != 2.5 {
		t.Errorf("create response rate_multiplier = %v, want 2.5", resp.Node.RateMultiplier)
	}

	// composite pricing lives on the composite's own column now
	comp, err := db.GetNode(d, resp.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if comp.RateMultiplier != 2.5 {
		t.Fatalf("composite rate_multiplier want 2.5, got %v", comp.RateMultiplier)
	}

	hops, err := db.ListNodeHops(d, resp.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %d, want 2", len(hops))
	}
	for i, h := range hops {
		if h.TrafficMultiplier != 0 {
			t.Errorf("hop%d multiplier = %v, want dormant 0", i, h.TrafficMultiplier)
		}
	}

	// The node list endpoint reads rate_multiplier straight off the row —
	// no in-memory hop aggregation left to overwrite it.
	listRec := adminJSON(t, s, admin, "GET", "/api/nodes", nil)
	var listResp struct {
		Nodes []struct {
			ID             int64   `json:"id"`
			RateMultiplier float64 `json:"rate_multiplier"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range listResp.Nodes {
		if n.ID == resp.Node.ID {
			found = true
			if n.RateMultiplier != 2.5 {
				t.Errorf("list rate_multiplier = %v, want 2.5", n.RateMultiplier)
			}
		}
	}
	if !found {
		t.Fatalf("composite node %d missing from /api/nodes list", resp.Node.ID)
	}
}

// Editing a composite's hop chain accepts (and ignores) per-hop
// traffic_multiplier input, storing a dormant 0 instead. The composite's own
// rate_multiplier column is untouched by a hop-chain edit.
func TestCompositeHopMultUpdateIgnoresTrafficMultiplier(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hop-a", "", "")
	b, _ := db.CreateNode(d, "hop-b", "", "")
	c, _ := db.CreateNode(d, "hop-c", "", "")

	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	createRec := adminJSON(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "chain", "node_type": "composite", "rate_multiplier": 4.0,
		"hops": []map[string]any{
			{"node_id": a.ID, "mode": "userspace"},
			{"node_id": b.ID, "mode": "userspace"},
		},
	})
	var createResp struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	compID := createResp.Node.ID

	adminJSON(t, s, admin, "POST", "/api/nodes/"+itoa(compID)+"/hops", map[string]any{
		"hops": []map[string]any{
			{"node_id": a.ID, "mode": "userspace", "traffic_multiplier": 9.0},
			{"node_id": b.ID, "mode": "kernel", "traffic_multiplier": 8.0},
			{"node_id": c.ID, "mode": "userspace", "traffic_multiplier": 7.0},
		},
	})

	hops, err := db.ListNodeHops(d, compID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 3 {
		t.Fatalf("hops = %d, want 3", len(hops))
	}
	for i, h := range hops {
		if h.TrafficMultiplier != 0 {
			t.Errorf("hop%d multiplier = %v, want dormant 0", i, h.TrafficMultiplier)
		}
	}

	comp, err := db.GetNode(d, compID)
	if err != nil {
		t.Fatal(err)
	}
	if comp.RateMultiplier != 4.0 {
		t.Fatalf("composite rate_multiplier want unchanged 4.0, got %v", comp.RateMultiplier)
	}
}
