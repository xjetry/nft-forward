package server

import (
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
