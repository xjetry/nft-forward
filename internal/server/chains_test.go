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

func TestCreateChainWiresForwardsAndShowsEntry(t *testing.T) {
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
	req := httptest.NewRequest("POST", "/api/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, chains[0].ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 hop forwards, got %d", len(fws))
	}
	c, _ := db.GetChain(d, chains[0].ID)
	if !c.EntryNodeID.Valid || c.EntryListenPort == 0 {
		t.Fatalf("entry not recorded: %+v", c)
	}
}

// apiPostChain drives the create-chain JSON API and fails the test unless
// it returns 200, returning the single admin chain that was persisted.
func apiPostChain(t *testing.T, s *Server, d *sql.DB, admin *http.Cookie, name string, hopNodes []int64) *db.Chain {
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
	req := httptest.NewRequest("POST", "/api/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create chain status = %d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListAdminChains(d)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain after create, got %d", len(chains))
	}
	return chains[0]
}

func TestSaveChainReorderKeepsForwards(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	c := apiPostChain(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	body, _ := json.Marshal(map[string]any{
		"name":  "vless",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"node_id": h.ID, "mode": "kernel"},
			{"node_id": g.ID, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/chains/%d", c.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d body=%s", rec.Code, rec.Body.String())
	}

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain after save, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, c.ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 hop forwards after reorder, got %d", len(fws))
	}
}

func TestReallocateHopChangesPort(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	c := apiPostChain(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	hops, _ := db.ListChainHops(d, c.ID)
	pos0Node := hops[0].NodeID
	before, _ := db.ListForwardsByChain(d, c.ID)
	portByNode := map[int64]int{}
	for _, f := range before {
		portByNode[f.NodeID] = f.ListenPort
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/chains/%d/hops/0/reallocate", c.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reallocate status = %d body=%s", rec.Code, rec.Body.String())
	}

	after, _ := db.ListForwardsByChain(d, c.ID)
	newPort := map[int64]int{}
	for _, f := range after {
		newPort[f.NodeID] = f.ListenPort
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
	// Invalid value is rejected (API returns 400).
	apiSetRelayHost("not a host!!", false)
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "5.6.7.8" {
		t.Fatalf("invalid relay_host should be rejected; got %q", got.RelayHost)
	}
	// Empty clears it.
	apiSetRelayHost("", true)
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "" {
		t.Fatalf("empty relay_host should clear; got %q", got.RelayHost)
	}
}

func TestCreateChainRejectsNodeWithoutRelayHost(t *testing.T) {
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
	req := httptest.NewRequest("POST", "/api/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 0 {
		t.Fatalf("chain must not persist when a hop node lacks relay_host; got %d", len(chains))
	}
}

// forwardOnNode returns the chain's generated forward that lives on nodeID.
func forwardOnNode(t *testing.T, d *sql.DB, chainID, nodeID int64) *db.Forward {
	t.Helper()
	fws, _ := db.ListForwardsByChain(d, chainID)
	for _, f := range fws {
		if f.NodeID == nodeID {
			return f
		}
	}
	t.Fatalf("no chain %d forward found on node %d", chainID, nodeID)
	return nil
}

// apiNodeAction sends a JSON API request for a node-scoped action.
// For DELETE, path should be the node endpoint and method "DELETE".
// For POST with body (e.g. relay-host), pass the JSON body.
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

	c := apiPostChain(t, s, d, admin, "vless", []int64{a.ID, b.ID})

	if got := forwardOnNode(t, d, c.ID, a.ID).TargetIP; got != "2.2.2.2" {
		t.Fatalf("upstream hop target_ip = %q, want 2.2.2.2", got)
	}

	body, _ := json.Marshal(map[string]any{"relay_host": "8.8.8.8"})
	rec := apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/relay-host", b.ID), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay-host status = %d body=%s", rec.Code, rec.Body.String())
	}

	if got := forwardOnNode(t, d, c.ID, a.ID).TargetIP; got != "8.8.8.8" {
		t.Fatalf("upstream hop target_ip after relay-host change = %q, want 8.8.8.8", got)
	}
}

func TestDeleteMidNodeRewiresChain(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "node-a", "https://p", "t1")
	b, _ := db.CreateNode(d, "node-b", "https://p", "t2")
	cNode, _ := db.CreateNode(d, "node-c", "https://p", "t3")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, cNode.ID, "3.3.3.3")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	chain := apiPostChain(t, s, d, admin, "vless", []int64{a.ID, b.ID, cNode.ID})

	if got := forwardOnNode(t, d, chain.ID, a.ID).TargetIP; got != "2.2.2.2" {
		t.Fatalf("pre-delete upstream hop target_ip = %q, want 2.2.2.2", got)
	}

	rec := apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d", b.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete node status = %d body=%s", rec.Code, rec.Body.String())
	}

	hops, _ := db.ListChainHops(d, chain.ID)
	if len(hops) != 2 {
		t.Fatalf("chain should have 2 hops after deleting mid node, got %d", len(hops))
	}
	if got := forwardOnNode(t, d, chain.ID, a.ID).TargetIP; got != "3.3.3.3" {
		t.Fatalf("upstream hop must re-wire to surviving next hop 3.3.3.3, got %q", got)
	}
}

func TestDeleteNodeTearsDownTenantChainOnCIDRViolation(t *testing.T) {
	d := openDB(t)
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 100})
	n1, _ := db.CreateNode(d, "node-1", "https://p", "t1")
	n2, _ := db.CreateNode(d, "node-2", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: n1.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "10.0.0.0/8"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: n2.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, tid, tunA, 5)
	_ = db.GrantTunnel(d, tid, tunB, 5)

	// Seed a tenant chain whose exit (9.9.9.9) is allowed only because the last
	// hop is tunnel B (0.0.0.0/0); tunnel A alone (10.0.0.0/8) would forbid it.
	cid, _ := db.CreateChain(d, &db.Chain{TenantID: sql.NullInt64{Int64: tid, Valid: true}, Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := db.GetChain(d, cid)
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RegenerateChain(tx, c, []db.HopInput{
		{NodeID: n1.ID, TunnelID: sql.NullInt64{Int64: tunA, Valid: true}, Mode: "kernel"},
		{NodeID: n2.ID, TunnelID: sql.NullInt64{Int64: tunB, Valid: true}, Mode: "kernel"},
	}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if fws, _ := db.ListForwardsByChain(d, cid); len(fws) != 2 {
		t.Fatalf("want 2 seeded chain forwards, got %d", len(fws))
	}

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	// Deleting n2 shrinks the chain to [n1]; the new last tunnel A
	// (10.0.0.0/8) does NOT contain exit 9.9.9.9, so the chain must be torn down.
	rec := apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d", n2.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete node status = %d body=%s", rec.Code, rec.Body.String())
	}

	if chains, _ := db.ListChainsByTenant(d, tid); len(chains) != 0 {
		t.Fatalf("tenant chain must be removed when the promoted last hop forbids the exit, got %d", len(chains))
	}
	if fws, _ := db.ListForwardsByChain(d, cid); len(fws) != 0 {
		t.Fatalf("chain forwards must be gone after teardown, got %d", len(fws))
	}
}
