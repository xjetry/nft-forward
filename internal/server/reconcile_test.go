package server

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// On (re)connect the hub re-pushes the node's ruleset when the agent's reported
// rev doesn't match the current one, so changes made while the node was offline
// aren't lost. A matching rev skips the redundant push.
func TestReconcileOnConnect(t *testing.T) {
	_, hub, n := newHubTestServer(t)
	createStandaloneRuleHop(t, hub.DB, n.ID, "tcp", 0, "10.0.0.50", 9300, sql.NullInt64{})

	// Redispatch runs off-goroutine, so capture via a channel.
	dispatched := make(chan []int64, 4)
	hub.Redispatch = func(ids []int64) { dispatched <- ids }
	expectDispatch := func(want bool, msg string) {
		t.Helper()
		select {
		case <-dispatched:
			if !want {
				t.Fatalf("%s: unexpected redispatch", msg)
			}
		case <-time.After(time.Second):
			if want {
				t.Fatalf("%s: expected a redispatch", msg)
			}
		}
	}

	// Empty rev (agent reports nothing applied) → must redispatch.
	hub.reconcileOnConnect(n.ID, "")
	expectDispatch(true, "empty rev")

	// Compute the current rev and feed it back — a synced agent is skipped.
	ruleHops, _ := db.ActiveRuleHopsForPush(hub.DB, n.ID)
	rev := computeRev(buildRules(hub.DB, ruleHops))
	hub.reconcileOnConnect(n.ID, rev)
	expectDispatch(false, "matching rev")

	// A stale rev → redispatch again.
	hub.reconcileOnConnect(n.ID, "deadbeefdeadbeef")
	expectDispatch(true, "stale rev")
}

// Revoking a node grant deletes the user's rules that enter at that node and
// re-dispatches the affected nodes, so forwarding (and billing) actually stops.
func TestRevokeNodeDeletesUserRules(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)

	n, _ := db.CreateNode(d, "granted", "https://p", "s")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, n.ID, 5, 0)
	createOwnedRuleOnNode(t, s, uid, n.ID, cookie)

	if c, _ := db.CountRulesForUser(d, uid); c != 1 {
		t.Fatalf("precondition: want 1 rule, got %d", c)
	}

	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/users/%d/grants/%d", uid, n.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}

	if c, _ := db.CountRulesForUser(d, uid); c != 0 {
		t.Fatalf("rules should be deleted after revoke, got %d", c)
	}
	if _, err := db.GetNodeGrant(d, uid, n.ID); err == nil {
		t.Fatal("grant should be revoked")
	}
}

// createOwnedRuleOnNode creates a rule owned by uid entering at nodeID via the
// user API.
func createOwnedRuleOnNode(t *testing.T, s *Server, uid, nodeID int64, cookie *http.Cookie) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"node_id":%d,"name":"r","proto":"tcp","exit":"9.9.9.9:8443"}`, nodeID))
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule status=%d body=%s", rec.Code, rec.Body.String())
	}
}
