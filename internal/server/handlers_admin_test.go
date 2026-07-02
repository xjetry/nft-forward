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

// loginAsAdmin creates an admin user + active session and returns the
// session cookie an admin-only request must carry.
func loginAsAdmin(t *testing.T, d *sql.DB) *http.Cookie {
	t.Helper()
	hash, err := HashPassword("testpass")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := db.CreateUser(d, "admin-test", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: token}
}

func TestDeleteNodeSucceeds(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/nodes/%d", n.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/nodes/%d status = %d body=%s", n.ID, rec.Code, rec.Body.String())
	}

	if _, err := db.GetNode(d, n.ID); err == nil {
		t.Fatalf("node should be deleted, but GetNode still succeeds")
	}
}

// setNodeRelayHost posts a relay_host value to the manual admin endpoint
// and returns the HTTP status code, so callers can assert accept/reject.
func setNodeRelayHost(t *testing.T, s *Server, admin *http.Cookie, nodeID int64, host string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"relay_host": host})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/nodes/%d/relay-host", nodeID), bytes.NewReader(body))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code
}

func TestSetNodeRelayHostRejectsIPv6Literal(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	if code := setNodeRelayHost(t, s, admin, n.ID, "2001:db8::1"); code != http.StatusBadRequest {
		t.Fatalf("relay-host with IPv6 literal: status = %d, want 400", code)
	}

	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "" {
		t.Errorf("RelayHost = %q, want unchanged empty string after rejected update", got.RelayHost)
	}
}

func TestSetNodeRelayHostAcceptsIPv4AndHostname(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	if code := setNodeRelayHost(t, s, admin, n.ID, "203.0.113.9"); code != http.StatusOK {
		t.Fatalf("relay-host with IPv4 literal: status = %d, want 200", code)
	}
	gotV4, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotV4.RelayHost != "203.0.113.9" {
		t.Errorf("RelayHost = %q, want 203.0.113.9", gotV4.RelayHost)
	}

	if code := setNodeRelayHost(t, s, admin, n.ID, "relay.example.com"); code != http.StatusOK {
		t.Fatalf("relay-host with hostname: status = %d, want 200", code)
	}

	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "relay.example.com" {
		t.Errorf("RelayHost = %q, want relay.example.com", got.RelayHost)
	}
}

func TestSetNodeRelayHostRejectsWhenDeclared(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	if err := db.UpdateNodeRelayHost(d, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNodeRelayHostDeclared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	if code := setNodeRelayHost(t, s, admin, n.ID, "198.51.100.1"); code != http.StatusConflict {
		t.Fatalf("relay-host on a declared field: status = %d, want 409", code)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.9" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.9 (declared field must reject manual edits)", got.RelayHost)
	}
}

func TestSetNodeRelayHostV6RejectsWhenDeclared(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	if err := db.UpdateNodeRelayHostV6(d, n.ID, "2001:db8::9"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNodeRelayHostV6Declared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]string{"relay_host_v6": "2001:db8::99"})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/nodes/%d/relay-host-v6", n.ID), bytes.NewReader(body))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("relay-host-v6 on a declared field: status = %d, want 409", rec.Code)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::9" {
		t.Errorf("RelayHostV6 = %q, want unchanged 2001:db8::9 (declared field must reject manual edits)", got.RelayHostV6)
	}
}

func TestNodeRolesAndBindingsEndpoints(t *testing.T) {
	d := openDB(t)
	up, _ := db.CreateNode(d, "entry-hk", "", "")
	mid, _ := db.CreateNode(d, "akari", "", "")
	cookie := loginAsAdmin(t, d)
	s, _ := New(d)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		return w
	}

	if w := do("POST", fmt.Sprintf("/api/nodes/%d/roles", mid.ID), `{"roles":3}`); w.Code != 200 {
		t.Fatalf("set roles: %d %s", w.Code, w.Body.String())
	}
	n, _ := db.GetNode(d, mid.ID)
	if n.Roles != 3 {
		t.Fatalf("roles want 3, got %d", n.Roles)
	}
	// downstream must have via role to be bound
	body := fmt.Sprintf(`{"bindings":[{"upstream_node_id":%d,"mode":"kernel"}]}`, up.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), body); w.Code != 200 {
		t.Fatalf("set bindings: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), ""); w.Code != 200 ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"mode":"kernel"`)) {
		t.Fatalf("list bindings: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/api/node-bindings", ""); w.Code != 200 ||
		!bytes.Contains(w.Body.Bytes(), []byte(fmt.Sprintf(`"upstream_node_id":%d`, up.ID))) {
		t.Fatalf("list all: %d %s", w.Code, w.Body.String())
	}
	// remove via role then try to bind -> 400
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("bind non-via: want 400, got %d", w.Code)
	}
}
