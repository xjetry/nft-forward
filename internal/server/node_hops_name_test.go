package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// A composite node's hops must carry the child node's name, so the detail page
// shows names rather than bare ids.
func TestGetCompositeNodeHopsCarryChildNames(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "香港-A", "", "")
	b, _ := db.CreateNode(d, "日本-B", "", "")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/nodes/%d", comp.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		NodeHops []struct {
			HopNodeID int64  `json:"hop_node_id"`
			NodeName  string `json:"node_name"`
		} `json:"node_hops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.NodeHops) != 2 {
		t.Fatalf("want 2 hops, got %d", len(resp.NodeHops))
	}
	want := map[int64]string{a.ID: "香港-A", b.ID: "日本-B"}
	for _, h := range resp.NodeHops {
		if h.NodeName != want[h.HopNodeID] {
			t.Fatalf("hop %d node_name=%q, want %q", h.HopNodeID, h.NodeName, want[h.HopNodeID])
		}
	}
}
