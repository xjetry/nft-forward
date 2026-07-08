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
