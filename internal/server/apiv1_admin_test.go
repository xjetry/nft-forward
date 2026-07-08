package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestIssueUserTokenCreateThenRotate(t *testing.T) {
	d := openDB(t)
	hash, _ := HashPassword("x")
	uid, err := db.CreateUser(d, "u1", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	tok1, rotated1, err := db.IssueUserToken(d, uid, db.TokenScopeRead)
	if err != nil || rotated1 {
		t.Fatalf("first issue: rotated=%v err=%v", rotated1, err)
	}
	tok2, rotated2, err := db.IssueUserToken(d, uid, db.TokenScopeReadWrite)
	if err != nil || !rotated2 {
		t.Fatalf("second issue must rotate: rotated=%v err=%v", rotated2, err)
	}
	if tok1 == tok2 {
		t.Fatal("rotation must change the token value")
	}
	if _, _, err := db.GetUserByAPIToken(d, tok1); err == nil {
		t.Fatal("old token must be invalid after rotation")
	}
	_, tk, err := db.GetUserByAPIToken(d, tok2)
	if err != nil || tk.Scope != db.TokenScopeReadWrite {
		t.Fatalf("new token scope: want readwrite, got %+v err=%v", tk, err)
	}
}

func TestAdminCanMintOwnToken(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	body, _ := json.Marshal(map[string]any{"scope": "readwrite"})
	req := httptest.NewRequest("POST", "/api/my/token", bytes.NewReader(body))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin mint own token: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// SPA /api returns bare JSON (no envelope).
	var out struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token == "" || out.Scope != "readwrite" {
		t.Fatalf("want a readwrite token, got %+v", out)
	}
}

func TestV1AdminCreateUserClosedLoop(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	s, _ := New(d)

	rec := v1Do(t, s, "POST", "/api/v1/users", adminTok, map[string]any{
		"username": "svc-bot", "role": "user", "issue_token": true, "token_scope": "readwrite",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create user: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	tok, _ := data["token"].(string)
	if tok == "" {
		t.Fatal("closed loop must return a one-time token")
	}
	// The issued token authenticates and carries readwrite.
	if rec2 := v1Do(t, s, "GET", "/api/v1/info", tok, nil); rec2.Code != http.StatusOK {
		t.Fatalf("issued token should work: %d body=%s", rec2.Code, rec2.Body.String())
	}
	// Duplicate username is a conflict, not a 500.
	rec3 := v1Do(t, s, "POST", "/api/v1/users", adminTok, map[string]any{"username": "svc-bot", "role": "user"})
	if rec3.Code != http.StatusConflict || v1ErrCode(t, rec3) != codeConflict {
		t.Fatalf("dup username: want 409 conflict, got %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestV1AdminMintRotatesUserToken(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	uid, _ := v1UserToken(t, d, 10, db.TokenScopeRead) // user already holds a read token
	s, _ := New(d)

	rec := v1Do(t, s, "POST", fmt.Sprintf("/api/v1/users/%d/token", uid), adminTok, map[string]any{"scope": "readwrite"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	if data["rotated"] != true {
		t.Fatalf("existing token must rotate, got %v", data["rotated"])
	}
	newTok, _ := data["token"].(string)
	if rec2 := v1Do(t, s, "GET", "/api/v1/info", newTok, nil); rec2.Code != http.StatusOK {
		t.Fatalf("rotated token should authenticate: %d", rec2.Code)
	}

	// read metadata never leaks plaintext
	recg := v1Do(t, s, "GET", fmt.Sprintf("/api/v1/users/%d/token", uid), adminTok, nil)
	gd := v1Data(t, recg)
	if gd["has_token"] != true || gd["token"] != nil {
		t.Fatalf("get token meta must not include plaintext: %v", gd)
	}

	// delete revokes
	if recd := v1Do(t, s, "DELETE", fmt.Sprintf("/api/v1/users/%d/token", uid), adminTok, nil); recd.Code != http.StatusOK {
		t.Fatalf("delete token: %d", recd.Code)
	}
	if recg2 := v1Do(t, s, "GET", fmt.Sprintf("/api/v1/users/%d/token", uid), adminTok, nil); v1Data(t, recg2)["has_token"] != false {
		t.Fatal("token should be gone after delete")
	}
}

func TestV1AdminUserScalarSets(t *testing.T) {
	d := openDB(t)
	adminID, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	target, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	s, _ := New(d)

	// quota
	if rec := v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/quota", target), adminTok, map[string]any{"traffic_quota_bytes": 5000}); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := db.GetUserByID(d, target)
	if u.TrafficQuotaBytes != 5000 {
		t.Fatalf("quota not applied: %d", u.TrafficQuotaBytes)
	}
	// idempotent: same PUT again yields same state
	v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/quota", target), adminTok, map[string]any{"traffic_quota_bytes": 5000})
	u, _ = db.GetUserByID(d, target)
	if u.TrafficQuotaBytes != 5000 {
		t.Fatalf("idempotent quota drifted: %d", u.TrafficQuotaBytes)
	}
	// max-forwards
	v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/max-forwards", target), adminTok, map[string]any{"max_forwards": 7})
	if u, _ = db.GetUserByID(d, target); u.MaxForwards != 7 {
		t.Fatalf("max_forwards not applied: %d", u.MaxForwards)
	}
	// expiry (unix)
	v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/expiry", target), adminTok, map[string]any{"expires_at": 1893456000})
	if u, _ = db.GetUserByID(d, target); !u.ExpiresAt.Valid || u.ExpiresAt.Int64 != 1893456000 {
		t.Fatalf("expiry not applied: %+v", u.ExpiresAt)
	}
	// enabled=false disables
	v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/enabled", target), adminTok, map[string]any{"enabled": false})
	if u, _ = db.GetUserByID(d, target); !u.Disabled {
		t.Fatal("enabled=false must disable")
	}
	// admin cannot disable itself
	rec := v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/enabled", adminID), adminTok, map[string]any{"enabled": false})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-disable must be rejected, got %d", rec.Code)
	}
	// missing user -> 404
	if rec := v1Do(t, s, "PUT", "/api/v1/users/99999/quota", adminTok, map[string]any{"traffic_quota_bytes": 1}); rec.Code != http.StatusNotFound {
		t.Fatalf("missing user: want 404, got %d", rec.Code)
	}
}

func TestV1AdminPerNodeSets(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	target, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	n := grantedNode(t, d, "pernode", target, 5)
	s, _ := New(d)

	if rec := v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/nodes/%d/quota", target, n.ID), adminTok, map[string]any{"traffic_quota_bytes": 4096}); rec.Code != http.StatusOK {
		t.Fatalf("per-node quota: %d %s", rec.Code, rec.Body.String())
	}
	var q int64
	d.QueryRow(`SELECT traffic_quota_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, target, n.ID).Scan(&q)
	if q != 4096 {
		t.Fatalf("per-node quota not applied: %d", q)
	}
	if rec := v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/nodes/%d/rate-limit", target, n.ID), adminTok, map[string]any{"rate_limit_mbytes": 12}); rec.Code != http.StatusOK {
		t.Fatalf("per-node rate: %d %s", rec.Code, rec.Body.String())
	}
	var mb int64
	d.QueryRow(`SELECT rate_limit_mbytes FROM user_nodes WHERE user_id=? AND node_id=?`, target, n.ID).Scan(&mb)
	if mb != 12 {
		t.Fatalf("per-node rate not applied: %d", mb)
	}
}

func TestV1AdminGrantRevoke(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	target, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	n, _ := db.CreateNode(d, "grantable", "https://p", "t-grantable")
	s, _ := New(d)

	// grant (ensure-granted)
	if rec := v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/grants/%d", target, n.ID), adminTok, map[string]any{"max_forwards": 3, "traffic_quota_bytes": 100}); rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	nodes, _, _ := db.ListNodesForUser(d, target)
	if len(nodes) != 1 || nodes[0].ID != n.ID {
		t.Fatalf("grant not applied: %+v", nodes)
	}
	// grant again is idempotent (still exactly one grant)
	v1Do(t, s, "PUT", fmt.Sprintf("/api/v1/users/%d/grants/%d", target, n.ID), adminTok, map[string]any{"max_forwards": 3})
	if nodes, _, _ = db.ListNodesForUser(d, target); len(nodes) != 1 {
		t.Fatalf("re-grant must stay one grant, got %d", len(nodes))
	}
	// revoke, then revoke again (no-op success)
	if rec := v1Do(t, s, "DELETE", fmt.Sprintf("/api/v1/users/%d/grants/%d", target, n.ID), adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if nodes, _, _ = db.ListNodesForUser(d, target); len(nodes) != 0 {
		t.Fatalf("revoke should clear grant, got %d", len(nodes))
	}
	if rec := v1Do(t, s, "DELETE", fmt.Sprintf("/api/v1/users/%d/grants/%d", target, n.ID), adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("second revoke must be a no-op success, got %d", rec.Code)
	}
}

func TestV1AdminBatchApplyAndResync(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	u1, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	u2, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	real, _ := db.CreateNode(d, "batch-real", "https://p", "t-batch")
	s, _ := New(d)

	rec := v1Do(t, s, "POST", "/api/v1/grants/batch-apply", adminTok, map[string]any{
		"user_ids": []int64{u1, u2},
		"grants": []map[string]any{
			{"node_name": "batch-real", "max_forwards": 4, "traffic_quota_bytes": 0, "rate_limit_mbytes": 0},
			{"node_name": "ghost-node"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch-apply: %d %s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	// two users x one real node granted
	if g, _ := data["granted"].(float64); int(g) != 2 {
		t.Fatalf("want granted=2, got %v", data["granted"])
	}
	skipped, _ := data["skipped_nodes"].([]any)
	if len(skipped) != 1 || skipped[0] != "ghost-node" {
		t.Fatalf("want skipped=[ghost-node], got %v", data["skipped_nodes"])
	}
	for _, uid := range []int64{u1, u2} {
		if nodes, _, _ := db.ListNodesForUser(d, uid); len(nodes) != 1 || nodes[0].ID != real.ID {
			t.Fatalf("user %d grant not applied: %+v", uid, nodes)
		}
	}

	// resync-all tolerates disconnected nodes and returns 200
	if rec := v1Do(t, s, "POST", "/api/v1/nodes/resync-all", adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("resync-all: %d %s", rec.Code, rec.Body.String())
	}
}

func TestV1ResyncScopeGate(t *testing.T) {
	d := openDB(t)
	_, readTok := v1AdminToken(t, d, db.TokenScopeRead)
	n, _ := db.CreateNode(d, "r", "https://p", "t-r")
	s, _ := New(d)
	rec := v1Do(t, s, "POST", fmt.Sprintf("/api/v1/nodes/%d/resync", n.ID), readTok, nil)
	if rec.Code != http.StatusForbidden || v1ErrCode(t, rec) != codeScopeRequired {
		t.Fatalf("read-scope admin resync: want 403 scope_required, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestV1Usage(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeRead) // read scope suffices
	uid, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	grantedNode(t, d, "usage-node", uid, 5)
	s, _ := New(d)

	rec := v1Do(t, s, "GET", "/api/v1/usage", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	totals, ok := data["totals"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing totals: %s", rec.Body.String())
	}
	if _, ok := totals["traffic_bytes"]; !ok {
		t.Fatalf("totals missing traffic_bytes: %v", totals)
	}
	if _, ok := data["users"].([]any); !ok {
		t.Fatalf("usage missing users array: %s", rec.Body.String())
	}
	if _, ok := data["nodes"].([]any); !ok {
		t.Fatalf("usage missing nodes array: %s", rec.Body.String())
	}
	// a plain user token must not reach it
	_, userTok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	if rec := v1Do(t, s, "GET", "/api/v1/usage", userTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("user token usage: want 403, got %d", rec.Code)
	}
}
