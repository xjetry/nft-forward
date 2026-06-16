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

func TestCreateRuleWiresHopsAndShowsEntry(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"name":  "vless",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"node_id": g.ID, "mode": "userspace"},
			{"node_id": h.ID, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	rules, _ := db.ListAllRules(d)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 2 {
		t.Fatalf("want 2 rule hops, got %d", len(hops))
	}
	r := rules[0]
	if r.EntryListenPort == 0 {
		t.Fatalf("entry not recorded: %+v", r)
	}
}

func apiPostRule(t *testing.T, s *Server, d *sql.DB, admin *http.Cookie, name string, hopNodes []int64) *db.Rule {
	t.Helper()
	hops := make([]map[string]any, len(hopNodes))
	for i, n := range hopNodes {
		hops[i] = map[string]any{"node_id": n, "mode": "kernel"}
	}
	body, _ := json.Marshal(map[string]any{
		"name":  name,
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops":  hops,
	})
	req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule status = %d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListAllRules(d)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule after create, got %d", len(rules))
	}
	return rules[0]
}

func TestSaveRuleReorderKeepsHops(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	rl := apiPostRule(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	body, _ := json.Marshal(map[string]any{
		"name":  "vless",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"node_id": h.ID, "mode": "kernel"},
			{"node_id": g.ID, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/rules/%d", rl.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d body=%s", rec.Code, rec.Body.String())
	}

	hops, _ := db.ListRuleHops(d, rl.ID)
	if len(hops) != 2 {
		t.Fatalf("want 2 rule hops after reorder, got %d", len(hops))
	}
}

func TestReallocateRuleHopChangesPort(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	rl := apiPostRule(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	hops, _ := db.ListRuleHops(d, rl.ID)
	pos0Node := hops[0].NodeID
	portByNode := map[int64]int{}
	for _, hop := range hops {
		portByNode[hop.NodeID] = hop.ListenPort
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/rules/%d/hops/0/reallocate", rl.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reallocate status = %d body=%s", rec.Code, rec.Body.String())
	}

	newHops, _ := db.ListRuleHops(d, rl.ID)
	newPort := map[int64]int{}
	for _, hop := range newHops {
		newPort[hop.NodeID] = hop.ListenPort
	}
	if newPort[pos0Node] == portByNode[pos0Node] {
		t.Fatalf("reallocated hop port unchanged: %d", newPort[pos0Node])
	}
}

func TestSetNodeRelayHost(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	apiSetRelayHost := func(val string, wantOK bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"relay_host": val})
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/nodes/%d/relay-host", n.ID), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(admin)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if wantOK && rec.Code != http.StatusOK {
			t.Fatalf("relay-host status = %d body=%s", rec.Code, rec.Body.String())
		}
		if !wantOK && rec.Code == http.StatusOK {
			t.Fatalf("relay-host should have been rejected, got 200")
		}
	}

	apiSetRelayHost("5.6.7.8", true)
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "5.6.7.8" {
		t.Fatalf("relay_host = %q, want 5.6.7.8", got.RelayHost)
	}
	apiSetRelayHost("not a host!!", false)
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "5.6.7.8" {
		t.Fatalf("invalid relay_host should be rejected; got %q", got.RelayHost)
	}
	apiSetRelayHost("", true)
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "" {
		t.Fatalf("empty relay_host should clear; got %q", got.RelayHost)
	}
}

func TestCreateRuleRejectsNodeWithoutRelayHost(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	bare, _ := db.CreateNode(d, "bare", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"name":  "x",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"node_id": g.ID, "mode": "kernel"},
			{"node_id": bare.ID, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	rules, _ := db.ListAllRules(d)
	if len(rules) != 0 {
		t.Fatalf("rule must not persist when a hop node lacks relay_host; got %d", len(rules))
	}
}

func hopOnNode(t *testing.T, d *sql.DB, ruleID, nodeID int64) *db.RuleHop {
	t.Helper()
	hops, _ := db.ListRuleHops(d, ruleID)
	for _, h := range hops {
		if h.NodeID == nodeID {
			return h
		}
	}
	t.Fatalf("no rule %d hop found on node %d", ruleID, nodeID)
	return nil
}

func apiNodeAction(t *testing.T, s *Server, admin *http.Cookie, method, path string, jsonBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if jsonBody != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestSetNodeRelayHostRewiresUpstream(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "node-a", "https://p", "t1")
	b, _ := db.CreateNode(d, "node-b", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	rl := apiPostRule(t, s, d, admin, "vless", []int64{a.ID, b.ID})

	if got := hopOnNode(t, d, rl.ID, a.ID).TargetHost; got != "2.2.2.2" {
		t.Fatalf("upstream hop target_host = %q, want 2.2.2.2", got)
	}

	body, _ := json.Marshal(map[string]any{"relay_host": "8.8.8.8"})
	rec := apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/relay-host", b.ID), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay-host status = %d body=%s", rec.Code, rec.Body.String())
	}

	if got := hopOnNode(t, d, rl.ID, a.ID).TargetHost; got != "8.8.8.8" {
		t.Fatalf("upstream hop target_host after relay-host change = %q, want 8.8.8.8", got)
	}
}

func TestDeleteMidNodeRewiresRule(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "node-a", "https://p", "t1")
	b, _ := db.CreateNode(d, "node-b", "https://p", "t2")
	cNode, _ := db.CreateNode(d, "node-c", "https://p", "t3")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, cNode.ID, "3.3.3.3")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	rl := apiPostRule(t, s, d, admin, "vless", []int64{a.ID, b.ID, cNode.ID})

	if got := hopOnNode(t, d, rl.ID, a.ID).TargetHost; got != "2.2.2.2" {
		t.Fatalf("pre-delete upstream hop target_host = %q, want 2.2.2.2", got)
	}

	rec := apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d", b.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete node status = %d body=%s", rec.Code, rec.Body.String())
	}

	hops, _ := db.ListRuleHops(d, rl.ID)
	if len(hops) != 2 {
		t.Fatalf("rule should have 2 hops after deleting mid node, got %d", len(hops))
	}
	if got := hopOnNode(t, d, rl.ID, a.ID).TargetHost; got != "3.3.3.3" {
		t.Fatalf("upstream hop must re-wire to surviving next hop 3.3.3.3, got %q", got)
	}
}
