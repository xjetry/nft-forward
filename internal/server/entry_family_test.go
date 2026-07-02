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

func TestCreateRuleEntryFamily(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "dual", "", "")
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, n.ID, "2001:db8::1")

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)

	// v6/both entries require the userspace relay (kernel DNAT can't cross
	// address families), so non-v4 cases send mode userspace.
	post := func(name, family string, extra map[string]any) *httptest.ResponseRecorder {
		payload := map[string]any{
			"node_id": n.ID, "name": name, "proto": "tcp", "exit": "9.9.9.9:8443", "entry_family": family,
		}
		if family == "v6" || family == "both" {
			payload["mode"] = "userspace"
		}
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

	t.Run("default v4 unchanged", func(t *testing.T) {
		rec := post("r-v4", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Rule  map[string]any `json:"rule"`
			Entry string         `json:"entry"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Rule["entry_family"] != "v4" {
			t.Errorf("entry_family = %v, want v4", resp.Rule["entry_family"])
		}
		if !strings.HasPrefix(resp.Entry, "1.1.1.1:") {
			t.Errorf("entry = %q, want to start with 1.1.1.1:", resp.Entry)
		}
		if v6, ok := resp.Rule["entry_v6"]; ok && v6 != "" {
			t.Errorf("entry_v6 = %v, want empty for v4-only rule", v6)
		}
	})

	t.Run("v6 selects the v6 relay address", func(t *testing.T) {
		rec := post("r-v6", "v6", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct{ Entry string }
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resp.Entry, "[2001:db8::1]:") {
			t.Fatalf("entry = %q, expected a bracketed v6 host:port", resp.Entry)
		}
	})

	t.Run("both returns primary v4 plus secondary v6", func(t *testing.T) {
		rec := post("r-both", "both", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Rule    map[string]any `json:"rule"`
			Entry   string         `json:"entry"`
			EntryV6 string         `json:"entry_v6"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resp.EntryV6, "[2001:db8::1]:") {
			t.Fatalf("entry_v6 = %q, want a bracketed v6 host:port", resp.EntryV6)
		}

		// The list/detail read path (buildRuleListItem) must recompute the same
		// pair from the persisted entry_family, not just echo the create response.
		id := int64(resp.Rule["id"].(float64))
		getReq := httptest.NewRequest("GET", "/api/rules", nil)
		getReq.AddCookie(cookie)
		getRec := httptest.NewRecorder()
		s.Router().ServeHTTP(getRec, getReq)
		var listResp struct {
			Rules []map[string]any `json:"rules"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &listResp); err != nil {
			t.Fatal(err)
		}
		var found map[string]any
		for _, r := range listResp.Rules {
			if int64(r["id"].(float64)) == id {
				found = r
			}
		}
		if found == nil {
			t.Fatal("created rule not found in /api/rules list")
		}
		if found["entry_v6"] == "" || found["entry_v6"] == nil {
			t.Errorf("list entry_v6 = %v, want non-empty", found["entry_v6"])
		}
	})

	t.Run("v6 rejected when node has no v6 relay", func(t *testing.T) {
		v4Only, _ := db.CreateNode(d, "v4only", "", "")
		_ = db.UpdateNodeRelayHost(d, v4Only.ID, "3.3.3.3")
		body, _ := json.Marshal(map[string]any{
			"node_id": v4Only.ID, "name": "r-v4only-v6", "proto": "tcp", "exit": "9.9.9.9:8443", "entry_family": "v6",
		})
		req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("garbage entry_family rejected", func(t *testing.T) {
		rec := post("r-bad", "v9", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("v6 with kernel entry segment rejected", func(t *testing.T) {
		rec := post("r-v6-kernel", "v6", map[string]any{"mode": "kernel"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400 (kernel DNAT can't serve v6 ingress)", rec.Code, rec.Body.String())
		}
	})

	t.Run("v6 with udp proto rejected", func(t *testing.T) {
		rec := post("r-v6-udp", "v6", map[string]any{"proto": "udp"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400 (userspace relay is TCP-only)", rec.Code, rec.Body.String())
		}
	})

	t.Run("edit without entry_family keeps stored family", func(t *testing.T) {
		rec := post("r-keep", "v6", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
		}
		var created struct {
			Rule map[string]any `json:"rule"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		id := int64(created.Rule["id"].(float64))

		// A client predating entry_family sends everything except it.
		body, _ := json.Marshal(map[string]any{
			"name": "r-keep-renamed", "proto": "tcp", "exit": "9.9.9.9:8443",
		})
		req := httptest.NewRequest("PUT", "/api/rules/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		editRec := httptest.NewRecorder()
		s.Router().ServeHTTP(editRec, req)
		if editRec.Code != http.StatusOK {
			t.Fatalf("edit status=%d body=%s", editRec.Code, editRec.Body.String())
		}
		rl, err := db.GetRule(d, id)
		if err != nil {
			t.Fatal(err)
		}
		if rl.EntryFamily != "v6" {
			t.Errorf("entry_family after family-less edit = %q, want v6", rl.EntryFamily)
		}
	})

	t.Run("clearing relay_host_v6 blocked while v6 rules exist", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"relay_host_v6": ""})
		req := httptest.NewRequest("POST", "/api/nodes/"+strconv.FormatInt(n.ID, 10)+"/relay-host-v6", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s, want 409 while v6-entry rules exist", rec.Code, rec.Body.String())
		}
	})
}
