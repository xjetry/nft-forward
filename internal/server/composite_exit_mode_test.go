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

// makeCompositeHopMode is makeComposite with an explicit per-hop config mode,
// so tests can tell "mode came from node config" apart from "mode came from
// the rule request".
func makeCompositeHopMode(t *testing.T, d *sql.DB, name, hopMode string, childIDs ...int64) *db.Node {
	t.Helper()
	c, err := db.CreateNode(d, name, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	hops := make([]db.NodeHop, len(childIDs))
	for i, cid := range childIDs {
		hops[i] = db.NodeHop{NodeID: c.ID, Position: i, HopNodeID: cid, Mode: hopMode}
	}
	if err := db.CreateNodeHops(d, c.ID, hops); err != nil {
		t.Fatal(err)
	}
	n, _ := db.GetNode(d, c.ID)
	return n
}

// threeNodeChain creates three relay-ready nodes plus a composite over them
// with every config hop mode set to hopMode.
func threeNodeChain(t *testing.T, d *sql.DB, hopMode string) *db.Node {
	t.Helper()
	a, _ := db.CreateNode(d, "hop-a", "", "")
	b, _ := db.CreateNode(d, "hop-b", "", "")
	c, _ := db.CreateNode(d, "hop-c", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, c.ID, "3.3.3.3")
	return makeCompositeHopMode(t, d, "chain", hopMode, a.ID, b.ID, c.ID)
}

func chainModes(t *testing.T, s *Server, ruleID int64) []string {
	t.Helper()
	hops, err := db.ListRuleHops(s.DB, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	modes := make([]string, len(hops))
	for i, h := range hops {
		modes[i] = h.Mode
	}
	return modes
}

func wantModes(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hop modes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hop modes = %v, want %v", got, want)
		}
	}
}

func userJSON(t *testing.T, s *Server, cookie *http.Cookie, method, url string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d body=%s", method, url, rec.Code, rec.Body.String())
	}
	return rec
}

func adminCreateRuleID(t *testing.T, s *Server, admin *http.Cookie, body map[string]any) int64 {
	t.Helper()
	rec := adminJSON(t, s, admin, "POST", "/api/rules", body)
	var resp struct {
		Rule struct {
			ID int64 `json:"id"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Rule.ID
}

// The exit segment (last hop -> exit target) belongs to the rule: its mode
// comes from the request's exit_mode, while inter-node hops keep the
// composite config's modes.
func TestCompositeRuleCreateUsesRequestModeForExitHop(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	comp := threeNodeChain(t, d, "kernel")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": comp.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443", "exit_mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "kernel", "kernel", "userspace")
}

// Composite chains only honor exit_mode: legacy clients predating that field
// always sent mode (prefilled from the first hop) even though the server
// ignored it for composites, so honoring it now would let a stale web bundle
// silently rewrite the exit segment on every edit.
func TestCompositeRuleLegacyModeFieldIgnored(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	comp := threeNodeChain(t, d, "userspace")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": comp.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443", "mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "userspace", "userspace", "kernel")

	// A legacy edit carrying only mode keeps the exit hop untouched.
	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", id), map[string]any{
		"node_id": comp.ID, "name": "r1b", "proto": "tcp", "exit": "9.9.9.9:443", "mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "userspace", "userspace", "kernel")
}

// A composite rule created without a mode gets the kernel default on its exit
// hop — the same default a single-node rule gets — not the config's tail mode.
func TestCompositeRuleCreateDefaultsExitHopKernel(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	comp := threeNodeChain(t, d, "userspace")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": comp.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	wantModes(t, chainModes(t, s, id), "userspace", "userspace", "kernel")
}

// A header-only edit without mode must not silently reset the exit hop back to
// the config/default mode; an explicit mode switches it.
func TestCompositeRuleEditExitHopMode(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	comp := threeNodeChain(t, d, "kernel")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": comp.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443", "exit_mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "kernel", "kernel", "userspace")

	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", id), map[string]any{
		"node_id": comp.ID, "name": "r1b", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	wantModes(t, chainModes(t, s, id), "kernel", "kernel", "userspace")

	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", id), map[string]any{
		"node_id": comp.ID, "name": "r1b", "proto": "tcp", "exit": "9.9.9.9:443", "exit_mode": "kernel",
	})
	wantModes(t, chainModes(t, s, id), "kernel", "kernel", "kernel")
}

// The my-side create/edit endpoints carry the same exit-segment semantics as
// the admin ones.
func TestMyCompositeRuleExitHopMode(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	comp := threeNodeChain(t, d, "kernel")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)

	rec := userJSON(t, s, cookie, "POST", "/api/my/rules", map[string]any{
		"node_id": comp.ID, "name": "vless", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "userspace",
	})
	var resp struct {
		RuleID int64 `json:"rule_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	wantModes(t, chainModes(t, s, resp.RuleID), "kernel", "kernel", "userspace")

	userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", resp.RuleID), map[string]any{
		"name": "vless2", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	wantModes(t, chainModes(t, s, resp.RuleID), "kernel", "kernel", "userspace")

	// A stale bundle's edit (legacy mode only) must not touch the exit hop.
	userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", resp.RuleID), map[string]any{
		"name": "vless2", "proto": "tcp", "exit": "9.9.9.9:8443", "mode": "kernel",
	})
	wantModes(t, chainModes(t, s, resp.RuleID), "kernel", "kernel", "userspace")

	userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", resp.RuleID), map[string]any{
		"name": "vless2", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "kernel",
	})
	wantModes(t, chainModes(t, s, resp.RuleID), "kernel", "kernel", "kernel")
}

// Rule list items expose exit_mode (the last hop's mode) so the edit form can
// prefill the exit-segment picker; entry_mode alone reflects the first hop.
func TestRuleListItemExposesExitMode(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	comp := threeNodeChain(t, d, "kernel")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)

	userJSON(t, s, cookie, "POST", "/api/my/rules", map[string]any{
		"node_id": comp.ID, "name": "vless", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "userspace",
	})

	lreq := httptest.NewRequest("GET", "/api/my/rules", nil)
	lreq.AddCookie(cookie)
	lrec := httptest.NewRecorder()
	s.Router().ServeHTTP(lrec, lreq)
	if lrec.Code != http.StatusOK {
		t.Fatalf("list status=%d", lrec.Code)
	}
	var resp struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(resp.Rules))
	}
	if m := resp.Rules[0]["exit_mode"]; m != "userspace" {
		t.Fatalf("exit_mode = %v, want userspace", m)
	}
	if m := resp.Rules[0]["entry_mode"]; m != "kernel" {
		t.Fatalf("entry_mode = %v, want kernel (first hop from config)", m)
	}
}

// On single-node rules exit_mode and the legacy mode name the same (only)
// hop; exit_mode wins when both are present so old and new clients coexist.
func TestSingleNodeRuleExitModeField(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	n, _ := db.CreateNode(d, "n1", "", "t1")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": n.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443",
		"mode": "kernel", "exit_mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "userspace")
}

// Pins the existing invariant: udp cannot ride the TCP-only userspace relay,
// so every hop of a udp chain is coerced to kernel no matter what the request
// or the composite config asks for.
func TestCompositeRuleUDPCoercesAllHopsKernel(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	comp := threeNodeChain(t, d, "userspace")

	id := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": comp.ID, "name": "r-udp", "proto": "udp", "exit": "9.9.9.9:443", "mode": "userspace",
	})
	wantModes(t, chainModes(t, s, id), "kernel", "kernel", "kernel")
}
