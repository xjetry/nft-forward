package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// A node's WS secret is the only credential an agent presents, so it must never
// reach a user-facing response. It stays available on the admin node-detail
// endpoint (needed for the install command).
func TestNodeSecretNotLeakedToUser(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)

	const secret = "super-secret-node-token-abc123"
	n, _ := db.CreateNode(d, "leaky", "https://p", secret)
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, n.ID, 5, 0)

	for _, path := range []string{"/api/my", "/api/my/rules"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s leaked node secret in response body", path)
		}
	}

	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/nodes/%d", n.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), secret) {
		t.Errorf("admin node detail should expose secret for the install command, body=%s", rec.Body.String())
	}
}

// probe through a node requires the caller to have access to that node, so it
// can't be used as a scanning proxy. The node-less (panel-dials) branch is
// admin-only.
func TestProbeNodeAuthorization(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)

	n, _ := db.CreateNode(d, "probe-node", "https://p", "sekret")
	granted, grantedCookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, granted, n.ID, 5, 0)
	_, otherCookie := loginAsUser(t, d, 10)
	adminCookie := loginAsAdmin(t, d)

	probe := func(cookie *http.Cookie, node string) int {
		url := "/api/probe?target=1.2.3.4:80"
		if node != "" {
			url += "&node=" + node
		}
		req := httptest.NewRequest("GET", url, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}

	nodeParam := fmt.Sprintf("%d", n.ID)
	if code := probe(otherCookie, nodeParam); code != http.StatusForbidden {
		t.Errorf("non-granted user probing node: want 403, got %d", code)
	}
	if code := probe(grantedCookie, nodeParam); code == http.StatusForbidden {
		t.Errorf("granted user probing node: want non-403, got 403")
	}
	if code := probe(adminCookie, nodeParam); code == http.StatusForbidden {
		t.Errorf("admin probing node: want non-403, got 403")
	}
	// node-less panel dial: admin only.
	if code := probe(grantedCookie, ""); code != http.StatusForbidden {
		t.Errorf("non-admin node-less probe: want 403, got %d", code)
	}
	if code := probe(adminCookie, ""); code == http.StatusForbidden {
		t.Errorf("admin node-less probe: want non-403, got 403")
	}
}

// Session and API tokens are stored as SHA-256 hashes; the plaintext returned to
// the client is never what sits in the DB, yet lookups by plaintext still work.
func TestBearerTokensHashedAtRest(t *testing.T) {
	d := openDB(t)
	uid, err := db.CreateUser(d, "hashme", "x", "user")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	_ = d.QueryRow(`SELECT token FROM sessions WHERE user_id=?`, uid).Scan(&stored)
	if stored == tok {
		t.Error("session token stored in plaintext")
	}
	if stored != db.HashToken(tok) {
		t.Error("session token not stored as its SHA-256 hash")
	}
	if u, err := db.GetSessionUser(d, tok); err != nil || u == nil || u.ID != uid {
		t.Errorf("GetSessionUser by plaintext failed: %v", err)
	}

	apiTok, err := db.CreateAPIToken(d, uid, db.TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	var storedAPI, prefix string
	_ = d.QueryRow(`SELECT token, token_prefix FROM api_tokens WHERE user_id=?`, uid).Scan(&storedAPI, &prefix)
	if storedAPI == apiTok {
		t.Error("api token stored in plaintext")
	}
	if storedAPI != db.HashToken(apiTok) {
		t.Error("api token not stored as its SHA-256 hash")
	}
	if prefix != apiTok[:8] {
		t.Errorf("token_prefix = %q, want %q", prefix, apiTok[:8])
	}
	if u, _, err := db.GetUserByAPIToken(d, apiTok); err != nil || u.ID != uid {
		t.Errorf("GetUserByAPIToken by plaintext failed: %v", err)
	}
}

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	const key = "1.2.3.4\x00admin"
	for i := 0; i < loginMaxFailures-1; i++ {
		l.recordFailure(key)
		if !l.allowed(key) {
			t.Fatalf("locked out too early at failure %d", i+1)
		}
	}
	l.recordFailure(key) // crosses threshold
	if l.allowed(key) {
		t.Fatal("should be locked after reaching threshold")
	}

	// A successful login for a different key must not be affected.
	if !l.allowed("5.6.7.8\x00admin") {
		t.Fatal("unrelated key should be allowed")
	}

	// Lockout expires after the window.
	now = now.Add(loginLockout + time.Second)
	if !l.allowed(key) {
		t.Fatal("should be allowed after lockout expires")
	}

	// A success resets accumulated failures.
	l.recordFailure(key)
	l.recordSuccess(key)
	if _, ok := l.byID[key]; ok {
		t.Fatal("recordSuccess should clear the key")
	}
}
