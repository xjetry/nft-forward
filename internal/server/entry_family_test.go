package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	post := func(name, family string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"node_id": n.ID, "name": name, "proto": "tcp", "exit": "9.9.9.9:8443", "entry_family": family,
		})
		req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	t.Run("default v4 unchanged", func(t *testing.T) {
		rec := post("r-v4", "")
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
		rec := post("r-v6", "v6")
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
		rec := post("r-both", "both")
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
		if resp.Rule["entry_v6"] == "" {
			t.Fatal("rule.entry_v6 empty, want a bracketed v6 host:port")
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
		rec := post("r-bad", "v9")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
	})
}
