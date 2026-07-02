package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// /me carries the panel build version so the web sidebar can display it
// without a separate endpoint; a non-release build reports "dev".
func TestMeIncludesServerVersion(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("me status=%d", rec.Code)
	}
	var resp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Version != serverVersion() {
		t.Fatalf("version = %q, want %q", resp.Version, serverVersion())
	}
	if resp.Version == "" {
		t.Fatal("version must not be empty")
	}
}
