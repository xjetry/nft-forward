package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// loginAsUser creates a user with role "user" and the given max_forwards quota,
// returns the user ID and a session cookie.
func loginAsUser(t *testing.T, d *sql.DB, maxForwards int) (int64, *http.Cookie) {
	t.Helper()
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, fmt.Sprintf("user-%d", time.Now().UnixNano()), hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET max_forwards=? WHERE id=?`, maxForwards, uid); err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return uid, &http.Cookie{Name: sessionCookie, Value: tok}
}

func TestUserCreateChainAcrossGrantedTunnels(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	uid, cookie := loginAsUser(t, d, 10)
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: h.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, uid, tunA, 5)
	_ = db.GrantTunnel(d, uid, tunB, 5)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"name":  "vless",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"tunnel_id": tunA, "mode": "userspace"},
			{"tunnel_id": tunB, "mode": "userspace"},
		},
	})
	req := httptest.NewRequest("POST", "/api/my/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListChainsByUser(d, uid)
	if len(chains) != 1 {
		t.Fatalf("want 1 user chain, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, chains[0].ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 forwards, got %d", len(fws))
	}
	for _, f := range fws {
		if !f.OwnerID.Valid || f.OwnerID.Int64 != uid {
			t.Fatalf("user chain forward must carry owner_id")
		}
		if !f.TunnelID.Valid {
			t.Fatalf("user chain forward must carry tunnel_id")
		}
		if f.NodeID == g.ID && (f.ListenPort < 30000 || f.ListenPort > 30100) {
			t.Fatalf("hop on gomami port %d out of tunnel range", f.ListenPort)
		}
	}
}

func TestUserCreateChainRejectsUngrantedTunnel(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	other, _ := db.CreateTunnel(d, &db.Tunnel{Name: "x", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	// not granted
	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"name":  "x",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"tunnel_id": other, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/my/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByUser(d, uid)
	if len(chains) != 0 {
		t.Fatalf("ungranted tunnel must be rejected; got %d chains", len(chains))
	}
}

func TestUserCreateChainRejectsExitOutsideCIDR(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	tun, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "10.0.0.0/8"})
	_ = db.GrantTunnel(d, uid, tun, 5)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"name":  "x",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"tunnel_id": tun, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/my/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByUser(d, uid)
	if len(chains) != 0 {
		t.Fatalf("exit outside tunnel CIDR must be rejected; got %d chains", len(chains))
	}
}

func TestUserCreateChainRejectsOverQuota(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	uid, cookie := loginAsUser(t, d, 1)
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: h.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, uid, tunA, 5)
	_ = db.GrantTunnel(d, uid, tunB, 5)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"name":  "x",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"tunnel_id": tunA, "mode": "kernel"},
			{"tunnel_id": tunB, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/my/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByUser(d, uid)
	if len(chains) != 0 {
		t.Fatalf("chain exceeding max_forwards must be rejected; got %d chains", len(chains))
	}
}

func TestUserDeleteChainBlocksCrossUser(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uidA, cookieA := loginAsUser(t, d, 10)
	_, cookieB := loginAsUser(t, d, 10)
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, uidA, tunA, 5)

	s, _ := New(d)

	// User A creates a chain via the JSON API.
	body, _ := json.Marshal(map[string]any{
		"name":  "a-chain",
		"proto": "tcp",
		"exit":  "9.9.9.9:8443",
		"hops": []map[string]any{
			{"tunnel_id": tunA, "mode": "kernel"},
		},
	})
	req := httptest.NewRequest("POST", "/api/my/chains", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("user A create status=%d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListChainsByUser(d, uidA)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain for user A, got %d", len(chains))
	}
	chainID := chains[0].ID

	// User B tries to delete user A's chain.
	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/api/my/chains/%d", chainID), nil)
	delReq.AddCookie(cookieB)
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusForbidden {
		t.Fatalf("cross-user delete must be 403, got %d", delRec.Code)
	}
	if _, err := db.GetChain(d, chainID); err != nil {
		t.Fatalf("user A's chain must survive cross-user delete attempt: %v", err)
	}
}
