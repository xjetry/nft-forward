package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestRulesListShowsNodeName(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "po0-test", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}

	admin := loginAsAdmin(t, d)
	body, _ := json.Marshal(map[string]any{
		"node_id": n.ID,
		"name":    "test-rule",
		"proto":   "tcp",
		"exit":    "10.0.0.9:443",
	})
	createReq := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(admin)
	createRec := httptest.NewRecorder()
	s.Router().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("POST /api/rules: status %d body=%s", createRec.Code, createRec.Body.String())
	}

	req := httptest.NewRequest("GET", "/api/rules", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/rules: status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rules []json.RawMessage `json:"rules"`
		Nodes []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(resp.Rules) == 0 {
		t.Fatalf("expected at least 1 rule, got 0")
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
