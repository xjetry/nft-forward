package server

import (
	"encoding/json"
	"testing"

	"nft-forward/internal/db"
)

// 建组合时顶层的 rate_multiplier 落在组合自己那一行上，组合的价格只有这一个出处。
// 成员上的 traffic_multiplier 是早先“组合价格 = 成员倍率之和”时的字段，已经没有
// 对应的列；请求里仍带着它的调用方不会被拒绝（请求体里不认识的键一律忽略），
// 成员照常建出来。
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
	if len(hops) != 2 || hops[0].HopNodeID != a.ID || hops[1].HopNodeID != b.ID {
		t.Fatalf("成员应为 [a b]，实际 %+v", hops)
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

// 改组合的成员时，成员上带着 traffic_multiplier 的请求同样照常生效、那个键被忽略；
// 组合自己的 rate_multiplier 不受改成员影响。
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
	if len(hops) != 3 || hops[0].HopNodeID != a.ID || hops[1].HopNodeID != b.ID || hops[2].HopNodeID != c.ID {
		t.Fatalf("成员应为 [a b c]，实际 %+v", hops)
	}
	if hops[1].Mode != "kernel" {
		t.Fatalf("第二个成员的模式应为 kernel，实际 %q", hops[1].Mode)
	}

	comp, err := db.GetNode(d, compID)
	if err != nil {
		t.Fatal(err)
	}
	if comp.RateMultiplier != 4.0 {
		t.Fatalf("composite rate_multiplier want unchanged 4.0, got %v", comp.RateMultiplier)
	}

	// Direct rate-multiplier edits work on composites like any other node —
	// the column is the single source of truth for composite pricing.
	adminJSON(t, s, admin, "POST", "/api/nodes/"+itoa(compID)+"/rate-multiplier", map[string]any{
		"rate_multiplier": 6.5,
	})
	comp, err = db.GetNode(d, compID)
	if err != nil {
		t.Fatal(err)
	}
	if comp.RateMultiplier != 6.5 {
		t.Fatalf("composite rate_multiplier after direct edit want 6.5, got %v", comp.RateMultiplier)
	}
}
