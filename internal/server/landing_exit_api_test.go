package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func adminPost(t *testing.T, s *Server, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAPILandingExitQuotaLifecycle(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless"}}, "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	// list
	req := httptest.NewRequest("GET", "/api/users/"+itoa(uid)+"/landing-exits", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Exits []db.LandingExit `json:"exits"`
	}
	json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Exits) != 1 || listResp.Exits[0].Host != "1.2.3.4" {
		t.Fatalf("exits = %+v", listResp.Exits)
	}

	// set quota
	rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "1.2.3.4", "port": 443, "quota_bytes": 1073741824})
	if rec.Code != http.StatusOK {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body.String())
	}
	exits, _ := db.ListUserLandingExits(d, uid)
	if exits[0].QuotaBytes != 1073741824 {
		t.Fatalf("quota not stored: %+v", exits[0])
	}

	// negative quota rejected; unknown exit 404
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "1.2.3.4", "port": 443, "quota_bytes": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative quota: %d", rec.Code)
	}
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "nope", "port": 1, "quota_bytes": 1}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown exit: %d", rec.Code)
	}

	// reset
	d.Exec(`UPDATE user_landing_exits SET used_bytes=999 WHERE user_id=?`, uid)
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/reset",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	exits, _ = db.ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatalf("reset did not zero: %+v", exits[0])
	}

	// delete refuses present rows, accepts residual ones
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/delete",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusConflict {
		t.Fatalf("delete present: %d", rec.Code)
	}
	db.SyncUserLandingExits(d, uid, nil, "", "")
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/delete",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusOK {
		t.Fatalf("delete residual: %d %s", rec.Code, rec.Body.String())
	}
	if exits, _ = db.ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatalf("row should be gone, got %+v", exits)
	}
}

func TestAPILandingExitsRequireAdmin(t *testing.T) {
	d := openDB(t)
	uid, userCookie := loginAsUser(t, d, 10)
	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/users/"+itoa(uid)+"/landing-exits", nil)
	req.AddCookie(userCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("non-admin must be rejected")
	}
}
