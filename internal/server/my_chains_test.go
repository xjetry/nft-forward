package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// loginAsTenant creates a tenant + bound user + session, returns the cookie.
func loginAsTenant(t *testing.T, d *sql.DB, tenantID int64) *http.Cookie {
	t.Helper()
	hash, _ := HashPassword("pw")
	uid, err := db.CreateTenantUser(d, tenantID, fmt.Sprintf("tenant-%d", tenantID), hash)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func TestTenantCreateChainAcrossGrantedTunnels(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: h.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, tid, tunA, 5)
	_ = db.GrantTunnel(d, tid, tunB, 5)

	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "vless")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(tunA), fmt.Sprint(tunB)}
	form["hop_mode"] = []string{"userspace", "userspace"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 1 {
		t.Fatalf("want 1 tenant chain, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, chains[0].ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 forwards, got %d", len(fws))
	}
	for _, f := range fws {
		if !f.TenantID.Valid || f.TenantID.Int64 != tid {
			t.Fatalf("tenant chain forward must carry tenant_id")
		}
		if !f.TunnelID.Valid {
			t.Fatalf("tenant chain forward must carry tunnel_id")
		}
		// 端口落在对应通道段内
		if f.NodeID == g.ID && (f.ListenPort < 30000 || f.ListenPort > 30100) {
			t.Fatalf("hop on gomami port %d out of tunnel range", f.ListenPort)
		}
	}
}

func TestTenantCreateChainRejectsUngrantedTunnel(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	other, _ := db.CreateTunnel(d, &db.Tunnel{Name: "x", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	// 不 grant
	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(other)}
	form["hop_mode"] = []string{"kernel"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 0 {
		t.Fatalf("ungranted tunnel must be rejected; got %d chains", len(chains))
	}
}

func TestTenantCreateChainRejectsExitOutsideCIDR(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	tun, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "10.0.0.0/8"})
	_ = db.GrantTunnel(d, tid, tun, 5)

	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443") // 不在 10.0.0.0/8 内
	form["hop_tunnel"] = []string{fmt.Sprint(tun)}
	form["hop_mode"] = []string{"kernel"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 0 {
		t.Fatalf("exit outside tunnel CIDR must be rejected; got %d chains", len(chains))
	}
}

func TestTenantCreateChainRejectsOverQuota(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 1})
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: h.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, tid, tunA, 5)
	_ = db.GrantTunnel(d, tid, tunB, 5)

	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(tunA), fmt.Sprint(tunB)} // 2 跳 > MaxForwards 1
	form["hop_mode"] = []string{"kernel", "kernel"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 0 {
		t.Fatalf("chain exceeding max_forwards must be rejected; got %d chains", len(chains))
	}
}

func TestTenantDeleteChainBlocksCrossTenant(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	tidA, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	tidB, _ := db.CreateTenant(d, &db.Tenant{Name: "beta", MaxForwards: 10})
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, tidA, tunA, 5)

	s, _ := New(d)
	cookieA := loginAsTenant(t, d, tidA)
	cookieB := loginAsTenant(t, d, tidB)

	// Tenant A creates a chain via the HTTP path.
	form := url.Values{}
	form.Set("name", "a-chain")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(tunA)}
	form["hop_mode"] = []string{"kernel"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookieA)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tenant A create status=%d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListChainsByTenant(d, tidA)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain for tenant A, got %d", len(chains))
	}
	chainID := chains[0].ID

	// Tenant B tries to delete tenant A's chain.
	delReq := httptest.NewRequest("POST", fmt.Sprintf("/my/chains/%d/delete", chainID), nil)
	delReq.AddCookie(cookieB)
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant delete must be 403, got %d", delRec.Code)
	}
	if _, err := db.GetChain(d, chainID); err != nil {
		t.Fatalf("tenant A's chain must survive cross-tenant delete attempt: %v", err)
	}
}
