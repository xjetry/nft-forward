package server

import (
	"encoding/json"
	"testing"

	"nft-forward/internal/db"
)

// Creating a composite via the API accepts an optional per-hop
// traffic_multiplier; hops without one inherit the child node's rate.
func TestCreateCompositeNodeHopMultipliers(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hop-a", "", "")
	b, _ := db.CreateNode(d, "hop-b", "", "")
	_ = db.UpdateNodeRateMultiplier(d, a.ID, 2.0)
	_ = db.UpdateNodeRateMultiplier(d, b.ID, 3.0)

	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	rec := adminJSON(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "chain", "node_type": "composite",
		"hops": []map[string]any{
			{"node_id": a.ID, "mode": "userspace", "traffic_multiplier": 0.5},
			{"node_id": b.ID, "mode": "userspace"},
		},
	})
	var resp struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	hops, err := db.ListNodeHops(d, resp.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %d, want 2", len(hops))
	}
	if hops[0].TrafficMultiplier != 0.5 {
		t.Errorf("hop0 multiplier = %v, want explicit 0.5", hops[0].TrafficMultiplier)
	}
	if hops[1].TrafficMultiplier != 3.0 {
		t.Errorf("hop1 multiplier = %v, want inherited 3.0", hops[1].TrafficMultiplier)
	}
}
