package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// The panel-wide overview lists every node, its traffic and the user count.
// Only an admin may read it; a user has /api/my for their own.
func TestPanelDashboardIsAdminOnly(t *testing.T) {
	d := openDB(t)
	if _, err := db.CreateNode(d, "edge-private", "https://p", "t1"); err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	_, user := loginAsUser(t, d, 10)
	s, _ := New(d)

	rec := cookieDo(t, s, admin, "GET", "/api/dashboard", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: want 200, got %d %s", rec.Code, rec.Body)
	}
	var overview struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil || len(overview.Nodes) == 0 {
		t.Fatalf("admin: expected the node list, got %s (err=%v)", rec.Body, err)
	}

	rec = cookieDo(t, s, user, "GET", "/api/dashboard", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user: want 403, got %d %s", rec.Code, rec.Body)
	}
	// 用户自己的概览在 /api/my。只看状态码不够：/api 下不存在的路径由前端页面兜底，
	// 同样是 200（返回 index.html），所以这里要求回的是带 user 字段的 JSON。
	rec = cookieDo(t, s, user, "GET", "/api/my", nil)
	var own struct {
		User map[string]any `json:"user"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &own) != nil || own.User == nil {
		t.Fatalf("user's own dashboard: want 200 with the user's JSON, got %d %s", rec.Code, rec.Body)
	}
}
