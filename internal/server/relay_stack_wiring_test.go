package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestApiListNodesIncludesCompositeRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, n := range resp.Nodes {
		if int64(n["id"].(float64)) == comp.ID {
			found = n
		}
	}
	if found == nil {
		t.Fatal("composite node not found in /api/nodes response")
	}
	if found["entry_relay_host"] != "1.1.1.1" {
		t.Errorf("entry_relay_host = %v, want 1.1.1.1", found["entry_relay_host"])
	}
	if found["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", found["exit_relay_host_v6"])
	}
}

func TestApiGetNodeCompositeIncludesRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHostV6(d, a.ID, "2001:db8::1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/nodes/%d", comp.ID), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Node map[string]any `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Node["entry_relay_host_v6"] != "2001:db8::1" {
		t.Errorf("entry_relay_host_v6 = %v, want 2001:db8::1", resp.Node["entry_relay_host_v6"])
	}
	if resp.Node["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", resp.Node["exit_relay_host_v6"])
	}
}

func TestApiMyListRulesNodesIncludeCompositeRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my/rules", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, n := range resp.Nodes {
		if int64(n["id"].(float64)) == comp.ID {
			found = n
		}
	}
	if found == nil {
		t.Fatal("composite node not found in /api/my/rules nodes")
	}
	if found["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", found["exit_relay_host_v6"])
	}
}

func TestApiMyGetRuleNodesIncludeCompositeRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	// RegenerateRule requires a v4 relay_host on every hop regardless of v6,
	// so the tail hop needs both to let the rule actually get created.
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)

	s, _ := New(d)
	createRec := createMyRule(t, s, cookie, comp.ID, "vless")
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var createResp struct {
		RuleID int64 `json:"rule_id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/my/rules/%d", createResp.RuleID), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, n := range resp.Nodes {
		if int64(n["id"].(float64)) == comp.ID {
			found = n
		}
	}
	if found == nil {
		t.Fatal("composite node not found in /api/my/rules/{id} nodes")
	}
	if found["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", found["exit_relay_host_v6"])
	}
}
