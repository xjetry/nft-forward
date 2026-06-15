package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// The admin forwards API includes a nodes list that maps node IDs to names.
// This verifies the node name is present in the response (not just a raw ID).
func TestForwardsPageShowsNodeName(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "po0-test", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID:     n.ID,
		Proto:      "tcp",
		ListenPort: 12345,
		TargetIP:   "10.0.0.9",
		TargetPort: 443,
		Mode:       "userspace",
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/forwards", nil)
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/forwards: status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Forwards []json.RawMessage `json:"forwards"`
		Nodes    []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(resp.Forwards) == 0 {
		t.Fatalf("expected at least 1 forward, got 0")
	}
	found := false
	for _, nd := range resp.Nodes {
		if nd.ID == n.ID && nd.Name == "po0-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("nodes list should contain node %d with name 'po0-test', got %+v", n.ID, resp.Nodes)
	}
}
