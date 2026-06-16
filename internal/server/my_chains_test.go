package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// loginAsUser creates a user with role "user" and the given max_forwards quota,
// returns the user ID and a session cookie.
func loginAsUser(t *testing.T, d *sql.DB, maxForwards int) (int64, *http.Cookie) {
	t.Helper()
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, fmt.Sprintf("user-%d", time.Now().UnixNano()), hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET max_forwards=? WHERE id=?`, maxForwards, uid); err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return uid, &http.Cookie{Name: sessionCookie, Value: tok}
}

func TestUserCreateRuleOnGrantedNode(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"node_id": g.ID,
		"name":    "vless",
		"proto":   "tcp",
		"exit":    "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("want 1 user rule, got %d", len(rules))
	}
}

func TestUserCreateRuleRejectsUngrantedNode(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	// not granted

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"node_id": g.ID,
		"name":    "x",
		"proto":   "tcp",
		"exit":    "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 0 {
		t.Fatalf("ungranted node must be rejected; got %d rules", len(rules))
	}
}

func TestUserCreateRuleRejectsOverQuota(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 0) // max_forwards = 0
	_ = db.GrantNode(d, uid, g.ID, 5)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"node_id": g.ID,
		"name":    "x",
		"proto":   "tcp",
		"exit":    "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 0 {
		t.Fatalf("rule exceeding max_forwards must be rejected; got %d rules", len(rules))
	}
}

func TestUserDeleteRuleBlocksCrossUser(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uidA, cookieA := loginAsUser(t, d, 10)
	_, cookieB := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uidA, g.ID, 5)

	s, _ := New(d)

	body, _ := json.Marshal(map[string]any{
		"node_id": g.ID,
		"name":    "a-rule",
		"proto":   "tcp",
		"exit":    "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("user A create status=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uidA)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule for user A, got %d", len(rules))
	}
	ruleID := rules[0].ID

	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/api/my/rules/%d", ruleID), nil)
	delReq.AddCookie(cookieB)
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusForbidden {
		t.Fatalf("cross-user delete must be 403, got %d", delRec.Code)
	}
	if _, err := db.GetRule(d, ruleID); err != nil {
		t.Fatalf("user A's rule must survive cross-user delete attempt: %v", err)
	}
}
