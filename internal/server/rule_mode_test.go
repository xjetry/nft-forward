package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func singleHopMode(t *testing.T, s *Server, ruleID int64) string {
	t.Helper()
	hops, err := db.ListRuleHops(s.DB, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 {
		t.Fatalf("rule %d hop count = %d, want 1", ruleID, len(hops))
	}
	return hops[0].Mode
}

func adminJSON(t *testing.T, s *Server, admin *http.Cookie, method, url string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d body=%s", method, url, rec.Code, rec.Body.String())
	}
	return rec
}

func TestSingleNodeRuleModeCreateAndEdit(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	n, _ := db.CreateNode(d, "n1", "", "t1")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	rec := adminJSON(t, s, admin, "POST", "/api/rules", map[string]any{
		"node_id": n.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443", "mode": "userspace",
	})
	var resp struct {
		Rule struct {
			ID int64 `json:"id"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ruleID := resp.Rule.ID

	if m := singleHopMode(t, s, ruleID); m != "userspace" {
		t.Fatalf("mode after create = %q, want userspace", m)
	}

	// A header-only edit without mode keeps the current mode.
	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", ruleID), map[string]any{
		"node_id": n.ID, "name": "r1b", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	if m := singleHopMode(t, s, ruleID); m != "userspace" {
		t.Fatalf("mode after modeless edit = %q, want userspace", m)
	}

	// An explicit mode edit switches it.
	adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", ruleID), map[string]any{
		"node_id": n.ID, "name": "r1b", "proto": "tcp", "exit": "9.9.9.9:443", "mode": "kernel",
	})
	if m := singleHopMode(t, s, ruleID); m != "kernel" {
		t.Fatalf("mode after kernel edit = %q, want kernel", m)
	}
}

func TestSingleNodeRuleModeUDPCoercesKernel(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	n, _ := db.CreateNode(d, "n1", "", "t1")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	rec := adminJSON(t, s, admin, "POST", "/api/rules", map[string]any{
		"node_id": n.ID, "name": "r-udp", "proto": "udp", "exit": "9.9.9.9:443", "mode": "userspace",
	})
	var resp struct {
		Rule struct {
			ID int64 `json:"id"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if m := singleHopMode(t, s, resp.Rule.ID); m != "kernel" {
		t.Fatalf("udp rule mode = %q, want kernel (userspace relay is TCP-only)", m)
	}
}
