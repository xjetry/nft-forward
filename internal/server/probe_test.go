package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// createOwnedRule grants uid a fresh node and creates a rule owned by uid via
// the user API, returning the new rule's ID.
func createOwnedRule(t *testing.T, s *Server, d *sql.DB, uid int64, cookie *http.Cookie) int64 {
	t.Helper()
	g, _ := db.CreateNode(d, fmt.Sprintf("node-%d", uid), "https://p", fmt.Sprintf("tok-%d", uid))
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.GrantNode(d, uid, g.ID, 5)
	body, _ := json.Marshal(map[string]any{
		"node_id": g.ID, "name": "r", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule status=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) == 0 {
		t.Fatal("no rule created")
	}
	return rules[0].ID
}

func probeChainStatus(t *testing.T, s *Server, ruleID int64, cookie *http.Cookie) int {
	t.Helper()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/probe-chain?rule_id=%d", ruleID), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code
}

// A regular user may probe only rules they own; other users get 403 while the
// owner and any admin pass the ownership gate. Guards against leaking another
// user's hop node names and targets via the chain probe.
func TestProbeChainOwnershipGate(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)

	owner, ownerCookie := loginAsUser(t, d, 10)
	ruleID := createOwnedRule(t, s, d, owner, ownerCookie)

	_, otherCookie := loginAsUser(t, d, 10)
	adminCookie := loginAsAdmin(t, d)

	if code := probeChainStatus(t, s, ruleID, otherCookie); code != http.StatusForbidden {
		t.Errorf("non-owner: want 403, got %d", code)
	}
	if code := probeChainStatus(t, s, ruleID, ownerCookie); code == http.StatusForbidden {
		t.Errorf("owner: want non-403, got 403")
	}
	if code := probeChainStatus(t, s, ruleID, adminCookie); code == http.StatusForbidden {
		t.Errorf("admin: want non-403, got 403")
	}
}
