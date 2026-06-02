package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	form := url.Values{}
	form.Set("name", "vless")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_node"] = []string{fmt.Sprint(g.ID), fmt.Sprint(h.ID)}
	form["hop_mode"] = []string{"userspace", "kernel"}

	req := httptest.NewRequest("POST", "/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
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
	// 入口端口落库
	c, _ := db.GetChain(d, chains[0].ID)
	if !c.EntryNodeID.Valid || c.EntryListenPort == 0 {
		t.Fatalf("entry not recorded: %+v", c)
	}
}

// postChain drives the create-chain handler and fails the test unless it
// redirects (success), returning the single admin chain that was persisted.
// admin is reused across requests because loginAsAdmin inserts a fixed-username
// user that can only exist once per DB.
func postChain(t *testing.T, s *Server, d *sql.DB, admin *http.Cookie, name string, hopNodes []int64) *db.Chain {
	t.Helper()
	form := url.Values{}
	form.Set("name", name)
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	for _, n := range hopNodes {
		form.Add("hop_node", fmt.Sprint(n))
		form.Add("hop_mode", "kernel")
	}
	req := httptest.NewRequest("POST", "/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
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

	c := postChain(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	// Reorder the two hops and POST the edit; regeneration must succeed.
	form := url.Values{}
	form.Set("name", "vless")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_node"] = []string{fmt.Sprint(h.ID), fmt.Sprint(g.ID)}
	form["hop_mode"] = []string{"kernel", "kernel"}
	req := httptest.NewRequest("POST", fmt.Sprintf("/chains/%d", c.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
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

	c := postChain(t, s, d, admin, "vless", []int64{g.ID, h.ID})

	// Capture position-0 hop's forward port before reallocation.
	hops, _ := db.ListChainHops(d, c.ID)
	pos0Node := hops[0].NodeID
	before, _ := db.ListForwardsByChain(d, c.ID)
	portByNode := map[int64]int{}
	for _, f := range before {
		portByNode[f.NodeID] = f.ListenPort
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/chains/%d/hops/0/reallocate", c.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
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

	post := func(val string) {
		t.Helper()
		form := url.Values{}
		form.Set("relay_host", val)
		req := httptest.NewRequest("POST", fmt.Sprintf("/nodes/%d/relay-host", n.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(admin)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("relay-host status = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	// Valid IPv4 updates.
	post("5.6.7.8")
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "5.6.7.8" {
		t.Fatalf("relay_host = %q, want 5.6.7.8", got.RelayHost)
	}
	// Invalid value is rejected, leaving the prior value intact.
	post("not a host!!")
	if got, _ := db.GetNode(d, n.ID); got.RelayHost != "5.6.7.8" {
		t.Fatalf("invalid relay_host should be rejected; got %q", got.RelayHost)
	}
	// Empty clears it.
	post("")
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
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_node"] = []string{fmt.Sprint(g.ID), fmt.Sprint(bare.ID)}
	form["hop_mode"] = []string{"kernel", "kernel"}
	req := httptest.NewRequest("POST", "/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 0 {
		t.Fatalf("chain must not persist when a hop node lacks relay_host; got %d", len(chains))
	}
}
