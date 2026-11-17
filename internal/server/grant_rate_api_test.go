package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
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
	// 这个接口已经删除：路由表里没有它，按“没有这个接口”回 404。
	if rec.Code != http.StatusNotFound {
		t.Fatalf("已删除的接口应回 404，实际 %d %s", rec.Code, rec.Body.String())
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

func postBatchApply(t *testing.T, s *Server, admin *http.Cookie, payload map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/api/grants/batch-apply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAPIBatchApplyGrantsOnlyMissingKeepsExistingGrants(t *testing.T) {
	d := openDB(t)
	has, _ := loginAsUser(t, d, 10)
	lacks, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "keepbatch", "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	if err := db.GrantNode(d, has, n.ID, 3, 4096); err != nil {
		t.Fatal(err)
	}

	code, out := postBatchApply(t, s, admin, map[string]any{
		"user_ids":     []int64{has, lacks},
		"only_missing": true,
		"grants":       []map[string]any{{"node_name": "keepbatch", "max_forwards": 10, "traffic_quota_bytes": 0, "rate_limit_mbytes": 0}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if out["granted"] != float64(1) || out["kept"] != float64(1) {
		t.Fatalf("want granted=1 kept=1, got granted=%v kept=%v", out["granted"], out["kept"])
	}
	kept, err := db.GetNodeGrant(d, has, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.MaxRules != 3 || kept.TrafficQuotaBytes != 4096 {
		t.Fatalf("existing grant must be left as it was, got %+v", kept)
	}
	added, err := db.GetNodeGrant(d, lacks, n.ID)
	if err != nil {
		t.Fatalf("the user without the grant must receive it: %v", err)
	}
	if added.MaxRules != 10 {
		t.Fatalf("want max_rules 10 on the new grant, got %d", added.MaxRules)
	}
}

func TestAPIBatchApplyGrantsOverwritesByDefault(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "overbatch", "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	if err := db.GrantNode(d, uid, n.ID, 3, 4096); err != nil {
		t.Fatal(err)
	}

	code, out := postBatchApply(t, s, admin, map[string]any{
		"user_ids": []int64{uid},
		"grants":   []map[string]any{{"node_name": "overbatch", "max_forwards": 8, "traffic_quota_bytes": 0, "rate_limit_mbytes": 2}},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if out["granted"] != float64(1) || out["kept"] != float64(0) {
		t.Fatalf("want granted=1 kept=0, got granted=%v kept=%v", out["granted"], out["kept"])
	}
	g, err := db.GetNodeGrant(d, uid, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxRules != 8 || g.TrafficQuotaBytes != 0 || g.RateLimitMBytes != 2 {
		t.Fatalf("existing grant must be overwritten, got %+v", g)
	}
}

func TestAPIBatchApplyGrantsUnknownUserWritesNothing(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "atomicbatch", "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	code, out := postBatchApply(t, s, admin, map[string]any{
		"user_ids": []int64{uid, 987654},
		"grants":   []map[string]any{{"node_name": "atomicbatch", "max_forwards": 5}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for a user id that does not exist, got %d: %v", code, out)
	}
	if _, err := db.GetNodeGrant(d, uid, n.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("nothing may be written when the request is rejected, got err=%v", err)
	}
}

func TestAPIBatchApplyGrantsRepeatedNodeKeepsLastSpec(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "dupbatch", "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	code, out := postBatchApply(t, s, admin, map[string]any{
		"user_ids": []int64{uid, uid},
		"grants": []map[string]any{
			{"node_name": "dupbatch", "max_forwards": 4},
			{"node_name": "dupbatch", "max_forwards": 9},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if out["granted"] != float64(1) {
		t.Fatalf("one (user, node) pair was written, want granted=1, got %v", out["granted"])
	}
	g, err := db.GetNodeGrant(d, uid, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxRules != 9 {
		t.Fatalf("want the last spec for the node to win (9), got %d", g.MaxRules)
	}
}
