package server

import (
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// The per-node grant limit (user_nodes.max_forwards) must cap how many rules a
// user can create on that node, independently of the generous global limit.
func TestUserCreateRuleRespectsPerNodeLimit(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "node", "", "")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 100) // generous global max_forwards
	_ = db.GrantNode(d, uid, g.ID, 1, 0)     // per-node cap = 1

	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, g.ID, "r1"); rec.Code != http.StatusOK {
		t.Fatalf("first rule should succeed: %d %s", rec.Code, rec.Body.String())
	}
	rec := createMyRule(t, s, cookie, g.ID, "r2")
	if rec.Code == http.StatusOK {
		t.Fatalf("second rule on the node must be rejected by the per-node limit")
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("per-node cap should allow only 1 rule, got %d", len(rules))
	}
}
