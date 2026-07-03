package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// The entry family is derived, not chosen: a rule exposes an entry endpoint for
// every IP family its entry node can serve. relay_host (v4) is mandatory, so a
// rule is "both" when the node also has a v6 relay and the protocol is TCP
// (v6 ingress rides the TCP-only userspace relay), else "v4".
func TestEntryFamilyDerived(t *testing.T) {
	d := openDB(t)
	dual, _ := db.CreateNode(d, "dual", "", "")
	_ = db.UpdateNodeRelayHost(d, dual.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, dual.ID, "2001:db8::1")
	v4only, _ := db.CreateNode(d, "v4only", "", "")
	_ = db.UpdateNodeRelayHost(d, v4only.ID, "3.3.3.3")

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)

	create := func(name string, nodeID int64, proto string, extra map[string]any) *httptest.ResponseRecorder {
		payload := map[string]any{"node_id": nodeID, "name": name, "proto": proto, "exit": "9.9.9.9:8443"}
		for k, v := range extra {
			payload[k] = v
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	// A v4-only node yields a v4 entry and no v6 endpoint, whatever the caller
	// asks for (entry_family is ignored — it's derived).
	t.Run("v4-only node derives v4", func(t *testing.T) {
		rec := create("r-v4only", v4only.ID, "tcp", map[string]any{"entry_family": "v6"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Rule    map[string]any `json:"rule"`
			Entry   string         `json:"entry"`
			EntryV6 string         `json:"entry_v6"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Rule["entry_family"] != "v4" {
			t.Errorf("entry_family = %v, want v4", resp.Rule["entry_family"])
		}
		if !strings.HasPrefix(resp.Entry, "3.3.3.3:") {
			t.Errorf("entry = %q, want 3.3.3.3:", resp.Entry)
		}
		if resp.EntryV6 != "" {
			t.Errorf("entry_v6 = %q, want empty", resp.EntryV6)
		}
	})

	// A dual-stack TCP rule exposes both entries and silently runs its entry hop
	// in userspace even when kernel is requested (only userspace accepts v6).
	t.Run("dual-stack tcp derives both and forces userspace entry", func(t *testing.T) {
		rec := create("r-dual", dual.ID, "tcp", map[string]any{"mode": "kernel"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Rule    map[string]any `json:"rule"`
			Entry   string         `json:"entry"`
			EntryV6 string         `json:"entry_v6"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Rule["entry_family"] != "both" {
			t.Errorf("entry_family = %v, want both", resp.Rule["entry_family"])
		}
		if !strings.HasPrefix(resp.Entry, "1.1.1.1:") {
			t.Errorf("entry = %q, want 1.1.1.1:", resp.Entry)
		}
		if !strings.HasPrefix(resp.EntryV6, "[2001:db8::1]:") {
			t.Errorf("entry_v6 = %q, want [2001:db8::1]:", resp.EntryV6)
		}
		id := int64(resp.Rule["id"].(float64))
		hops, _ := db.ListRuleHops(d, id)
		if hops[0].Mode != "userspace" {
			t.Errorf("entry hop mode = %q, want userspace (silent downgrade for v6 ingress)", hops[0].Mode)
		}

		// The list read path recomputes the same pair from the persisted family.
		getReq := httptest.NewRequest("GET", "/api/rules", nil)
		getReq.AddCookie(cookie)
		getRec := httptest.NewRecorder()
		s.Router().ServeHTTP(getRec, getReq)
		var listResp struct {
			Rules []map[string]any `json:"rules"`
		}
		json.Unmarshal(getRec.Body.Bytes(), &listResp)
		var found map[string]any
		for _, r := range listResp.Rules {
			if int64(r["id"].(float64)) == id {
				found = r
			}
		}
		if found == nil || found["entry_v6"] == "" || found["entry_v6"] == nil {
			t.Errorf("list entry_v6 = %v, want non-empty", found["entry_v6"])
		}
	})

	// A dual-stack node stays v4-only for UDP: the userspace relay v6 ingress
	// needs is TCP-only, so no v6 entry is offered.
	t.Run("dual-stack udp stays v4", func(t *testing.T) {
		rec := create("r-dual-udp", dual.ID, "udp", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Rule    map[string]any `json:"rule"`
			EntryV6 string         `json:"entry_v6"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Rule["entry_family"] != "v4" {
			t.Errorf("entry_family = %v, want v4 for udp", resp.Rule["entry_family"])
		}
		if resp.EntryV6 != "" {
			t.Errorf("entry_v6 = %q, want empty for udp", resp.EntryV6)
		}
	})

	// Clearing the v6 relay is refused while a rule serving a v6 entry exists,
	// so the derived both-family rules don't get bricked.
	t.Run("clearing v6 relay blocked while both rules exist", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"relay_host_v6": ""})
		req := httptest.NewRequest("POST", "/api/nodes/"+strconv.FormatInt(dual.ID, 10)+"/relay-host-v6", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s, want 409 while v6-entry rules exist", rec.Code, rec.Body.String())
		}
	})
}
