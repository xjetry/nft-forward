package server

import (
	"encoding/json"
	"net/http/httptest"
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

func TestLandingSyncPassBackfillsManualUsers(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)

	s.landingSyncPass(true)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 {
		t.Fatalf("startup pass must backfill manual-URI users, got %d rows", len(exits))
	}

	// the periodic pass skips manual-only users (their set changes on save only)
	d.Exec(`DELETE FROM user_landing_exits`)
	s.landingSyncPass(false)
	if exits, _ := db.ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatal("periodic pass must skip manual-only users")
	}
}

func TestLandingSyncPassKeepsSetOnSubFailure(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)
	s.landingSyncPass(true)

	// switching to a failing subscription must not flip the existing rows
	db.SetUserLandingSource(d, uid, "http://127.0.0.1:1/sub", "vless://u@1.2.3.4:443#HK")
	s.landingSyncPass(true)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 || !exits[0].Present {
		t.Fatalf("failed resolution must leave the set untouched, got %+v", exits)
	}
}

func TestMyLandingNodesCarriesLedger(t *testing.T) {
	d := openDB(t)
	uid, cookie := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)

	req := httptest.NewRequest("GET", "/api/my/landing-nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []struct {
			Host       string `json:"host"`
			QuotaBytes int64  `json:"quota_bytes"`
			UsedBytes  int64  `json:"used_bytes"`
			Exceeded   bool   `json:"exceeded"`
		} `json:"nodes"`
		Stale bool `json:"stale"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Nodes) != 1 || resp.Stale {
		t.Fatalf("resp = %+v", resp)
	}

	// exhausted ledger surfaces as exceeded
	d.Exec(`UPDATE user_landing_exits SET quota_bytes=100, used_bytes=100 WHERE user_id=?`, uid)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Nodes[0].Exceeded || resp.Nodes[0].UsedBytes != 100 {
		t.Fatalf("ledger not joined: %+v", resp.Nodes[0])
	}
}

func TestMyLandingNodesStaleFallback(t *testing.T) {
	d := openDB(t)
	uid, cookie := loginAsUser(t, d, 10)
	// materialize while healthy, then break the subscription
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless", URI: "vless://u@1.2.3.4:443#HK"}}, "", "")
	d.Exec(`UPDATE users SET landing_sub_url='http://127.0.0.1:1/sub' WHERE id=?`, uid)
	s, _ := New(d)

	req := httptest.NewRequest("GET", "/api/my/landing-nodes?refresh=1", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var resp struct {
		Nodes []struct {
			Host string `json:"host"`
			URI  string `json:"uri"`
		} `json:"nodes"`
		Stale bool `json:"stale"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Stale || len(resp.Nodes) != 1 || resp.Nodes[0].Host != "1.2.3.4" {
		t.Fatalf("stale fallback should serve the snapshot, got %+v", resp)
	}
}
