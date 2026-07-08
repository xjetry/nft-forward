package server

import (
	"bytes"
	"encoding/json"
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
