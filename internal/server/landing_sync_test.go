package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestResolveLandingExitsManualOnly(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	nodes, ok := s.resolveLandingExits(u, false)
	if !ok || len(nodes) != 1 || nodes[0].Host != "1.2.3.4" {
		t.Fatalf("manual-only resolution must succeed, ok=%v nodes=%+v", ok, nodes)
	}
}

// The SSRF guard refuses non-public targets, so a loopback subscription URL is
// a deterministic fetch failure — exactly the case that must not flip the
// materialized set.
func TestResolveLandingExitsSubFailure(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "http://127.0.0.1:1/sub", "vless://u@1.2.3.4:443#HK")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	if _, ok := s.resolveLandingExits(u, true); ok {
		t.Fatal("subscription fetch failure must report ok=false")
	}
}

func TestSyncLandingExitsMaterializes(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK\nvless://u@1.2.3.4:443#DUP")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	nodes, ok := s.resolveLandingExits(u, false)
	if !ok {
		t.Fatal("resolve")
	}
	s.syncLandingExits(u, nodes)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 || exits[0].Name != "HK" {
		t.Fatalf("dedup keeps the first node per host:port, got %+v", exits)
	}
}
