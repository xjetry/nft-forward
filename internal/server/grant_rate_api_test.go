package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestAPISetPerNodeRateLimit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rlnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"rate_limit_mbytes": 10})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/rate-limit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.RateLimitMBytes != 10 {
		t.Fatalf("want 10, got %d", g.RateLimitMBytes)
	}

	// negative is rejected
	body, _ = json.Marshal(map[string]any{"rate_limit_mbytes": -1})
	req = httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/rate-limit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative rate: want 400, got %d", rec.Code)
	}
}

func TestAPIRuleBandwidthEndpointRemoved(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/rules/1/bandwidth", bytes.NewReader([]byte(`{"bandwidth_mbps":5}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	// The router's catch-all serves the SPA index for any unmatched path with
	// status 200, so an absent JSON handler cannot be told apart from a live
	// one by status code alone; the response Content-Type is the reliable
	// signal that no API handler processed the request.
	if ct := rec.Header().Get("Content-Type"); ct == "application/json" {
		t.Fatalf("want removed endpoint to fall through to non-JSON response, got Content-Type %q body %s", ct, rec.Body.String())
	}
}

func TestAPIBatchApplyGrantsRateLimit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rlbatch", "", "")
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	payload := map[string]any{
		"user_ids": []int64{uid},
		"grants": []map[string]any{
			{"node_name": "rlbatch", "max_forwards": 5, "traffic_quota_bytes": 0, "rate_limit_mbytes": 7},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/api/grants/batch-apply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	g, err := db.GetNodeGrant(d, uid, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.RateLimitMBytes != 7 {
		t.Fatalf("want 7, got %d", g.RateLimitMBytes)
	}
}
