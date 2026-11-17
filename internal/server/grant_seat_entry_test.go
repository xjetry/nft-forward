package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"nft-forward/internal/chain"
	"nft-forward/internal/db"
)

// A via-only node whose grant overrides to entry becomes a usable rule start
// for that user: a single-hop rule (no via) dials straight to the exit.
func TestGrantEntryOverrideAllowsViaNodeAsEntry(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeSeats(d, m.ID, chain.SeatVia); err != nil {
		t.Fatal(err)
	}

	uid, cookie := loginAsUser(t, d, 20)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	if err := db.SetGrantSeats(d, uid, m.ID, chain.SeatEntry); err != nil {
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
	if err := db.UpdateNodeSeats(d, m.ID, chain.SeatVia); err != nil {
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

// The my rule-form node list reports the grantee's effective role: a via-only
// node with an entry override surfaces as entry-capable (bit 0 set).
func TestMyNodesReportEffectiveSeats(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeSeats(d, m.ID, chain.SeatVia); err != nil {
		t.Fatal(err)
	}
	uid, cookie := loginAsUser(t, d, 22)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	if err := db.SetGrantSeats(d, uid, m.ID, chain.SeatEntry); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my/rules", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my rules: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []struct {
			ID    int64      `json:"id"`
			Seats chain.Seat `json:"roles"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range resp.Nodes {
		if n.ID == m.ID {
			found = true
			if n.Seats&chain.SeatEntry == 0 {
				t.Fatalf("middle node effective roles = %d, want entry bit set", n.Seats)
			}
		}
	}
	if !found {
		t.Fatalf("granted node %d missing from my nodes", m.ID)
	}
}

// The per-node roles endpoint writes the override; an illegal bit is rejected.
func TestSetGrantSeatsEndpoint(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	uid, _ := loginAsUser(t, d, 23)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	adminCookie := loginAsAdmin(t, d)

	s, _ := New(d)

	setRoles := func(val chain.Seat) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"roles": val})
		req := httptest.NewRequest("POST",
			"/api/users/"+strconv.FormatInt(uid, 10)+"/nodes/"+strconv.FormatInt(m.ID, 10)+"/roles",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	if rec := setRoles(chain.SeatEntry); rec.Code != http.StatusOK {
		t.Fatalf("set entry override: %d %s", rec.Code, rec.Body.String())
	}
	g, _ := db.GetNodeGrant(d, uid, m.ID)
	if g.Seats != chain.SeatEntry {
		t.Fatalf("stored roles = %d, want %d", g.Seats, chain.SeatEntry)
	}

	if rec := setRoles(4); rec.Code == http.StatusOK {
		t.Fatalf("illegal role bit must be rejected; got 200")
	}
}

// The my overview reports the same effective roles as the rule form: a grant
// override replaces the node's own mask in both directions, and a grant
// without one inherits it.
func TestMyDashboardReportsEffectiveSeats(t *testing.T) {
	d := openDB(t)
	overridden, _ := db.CreateNode(d, "via-node-entry-grant", "", "")
	inherited, _ := db.CreateNode(d, "via-node-plain-grant", "", "")
	for _, n := range []*db.Node{overridden, inherited} {
		if err := db.UpdateNodeSeats(d, n.ID, chain.SeatVia); err != nil {
			t.Fatal(err)
		}
	}
	uid, cookie := loginAsUser(t, d, 22)
	_ = db.GrantNode(d, uid, overridden.ID, 5, 0)
	_ = db.GrantNode(d, uid, inherited.ID, 5, 0)
	if err := db.SetGrantSeats(d, uid, overridden.ID, chain.SeatEntry); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my dashboard: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []struct {
			ID    int64      `json:"id"`
			Seats chain.Seat `json:"roles"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[int64]chain.Seat{}
	for _, n := range resp.Nodes {
		got[n.ID] = n.Seats
	}
	if got[overridden.ID] != chain.SeatEntry {
		t.Fatalf("node with an entry override: roles = %d, want %d (the override, not the node's via mask)", got[overridden.ID], chain.SeatEntry)
	}
	if got[inherited.ID] != chain.SeatVia {
		t.Fatalf("node without an override: roles = %d, want the node's own %d", got[inherited.ID], chain.SeatVia)
	}
}
