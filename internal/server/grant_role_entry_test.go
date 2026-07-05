package server

import (
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// A via-only node whose grant overrides to entry becomes a usable rule start
// for that user: a single-hop rule (no via) dials straight to the exit.
func TestGrantEntryOverrideAllowsViaNodeAsEntry(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeRoles(d, m.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}

	uid, cookie := loginAsUser(t, d, 20)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	if err := db.SetGrantRoles(d, uid, m.ID, db.NodeRoleEntry); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, m.ID, nil, "start-at-middle")
	if rec.Code != http.StatusOK {
		t.Fatalf("create with entry override: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 || rules[0].NodeID != m.ID {
		t.Fatalf("want one rule entering at middle node, got %+v", rules)
	}
}

// Without the override the same via-only node is rejected as an entry — the
// grant alone doesn't confer entry usability.
func TestViaNodeWithoutOverrideRejectedAsEntry(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeRoles(d, m.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}

	uid, cookie := loginAsUser(t, d, 21)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, m.ID, nil, "should-fail")
	if rec.Code == http.StatusOK {
		t.Fatalf("via-only node without override must not be a valid entry; got 200")
	}
}
