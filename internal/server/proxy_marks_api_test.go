package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"nft-forward/internal/db"
)

func postProxyMarks(t *testing.T, s *Server, admin *http.Cookie, payload any) (int, map[string]int) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/api/node-roles", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var out struct {
		Roles map[string]int `json:"roles"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Roles
}

func storedProxyMarks(t *testing.T, s *Server) map[string]int {
	t.Helper()
	roles, err := s.loadProxyMarks()
	if err != nil {
		t.Fatal(err)
	}
	return roles
}

type markChange struct {
	Keys []string `json:"keys"`
	Mark int      `json:"role"`
	On   bool     `json:"on"`
}

func TestAPISetProxyMarksAppliesBitChanges(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	steps := []struct {
		change markChange
		want   map[string]int
	}{
		{markChange{[]string{"vless:a:443", "trojan:b:8443"}, markLanding, true}, map[string]int{"vless:a:443": 1, "trojan:b:8443": 1}},
		{markChange{[]string{"vless:a:443"}, markDirect, true}, map[string]int{"vless:a:443": 3, "trojan:b:8443": 1}},
		{markChange{[]string{"vless:a:443"}, markLanding, false}, map[string]int{"vless:a:443": 2, "trojan:b:8443": 1}},
		// Clearing a key's last bit removes the key.
		{markChange{[]string{"trojan:b:8443"}, markLanding, false}, map[string]int{"vless:a:443": 2}},
		// Clearing a bit that is not set changes nothing.
		{markChange{[]string{"vless:a:443", "ss:c:1"}, markLanding, false}, map[string]int{"vless:a:443": 2}},
	}
	for i, step := range steps {
		code, got := postProxyMarks(t, s, admin, step.change)
		if code != http.StatusOK {
			t.Fatalf("step %d: want 200, got %d", i, code)
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("step %d: response table = %v, want %v", i, got, step.want)
		}
		if stored := storedProxyMarks(t, s); !reflect.DeepEqual(stored, step.want) {
			t.Fatalf("step %d: stored table = %v, want %v", i, stored, step.want)
		}
	}
}

// A write names only the keys it changes, so it cannot carry a stale copy of
// the rest of the table: marks made by someone else in the meantime survive.
func TestAPISetProxyMarksKeepsKeysItDoesNotName(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	// Includes a value in the string form used before roles became a bitmask.
	if err := db.SetSetting(d, proxyMarksSetting, `{"vless:other:443":2,"ss:old:1":"landing"}`); err != nil {
		t.Fatal(err)
	}

	code, got := postProxyMarks(t, s, admin, markChange{[]string{"trojan:mine:443"}, markLanding, true})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	want := map[string]int{"vless:other:443": 2, "ss:old:1": 1, "trojan:mine:443": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("table = %v, want %v", got, want)
	}
}

func TestAPISetProxyMarksRejectsMalformedChanges(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	if err := db.SetSetting(d, proxyMarksSetting, `{"vless:keep:443":1}`); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{
		"no keys":            markChange{nil, markLanding, true},
		"no role":            markChange{[]string{"vless:a:443"}, 0, true},
		"unknown role bit":   markChange{[]string{"vless:a:443"}, 4, true},
		"whole-table format": map[string]any{"roles": map[string]int{"vless:a:443": 1}},
	}
	for name, payload := range bad {
		code, _ := postProxyMarks(t, s, admin, payload)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", name, code)
		}
	}
	if stored := storedProxyMarks(t, s); !reflect.DeepEqual(stored, map[string]int{"vless:keep:443": 1}) {
		t.Fatalf("a rejected change must leave the table as it was, got %v", stored)
	}
}

func TestAPISetProxyMarksConcurrentChangesAllLand(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	const writers = 40
	var wg sync.WaitGroup
	codes := make([]int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = postProxyMarks(t, s, admin, markChange{[]string{fmt.Sprintf("vless:h%d:443", i)}, markLanding, true})
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("writer %d: want 200, got %d", i, code)
		}
	}
	if stored := storedProxyMarks(t, s); len(stored) != writers {
		t.Fatalf("want all %d concurrent marks stored, got %d", writers, len(stored))
	}
}

func TestAPIGetProxyMarksFailsOnUnreadableTable(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	if err := db.SetSetting(d, proxyMarksSetting, `{not json`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/node-roles", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unreadable table must not be served as an empty one: want 500, got %d %s", rec.Code, rec.Body.String())
	}
	// A write on top of it must fail too, instead of replacing the row.
	if code, _ := postProxyMarks(t, s, admin, markChange{[]string{"vless:a:443"}, markLanding, true}); code != http.StatusInternalServerError {
		t.Fatalf("want 500 for a write over an unreadable table, got %d", code)
	}
	if raw, _ := db.GetSetting(d, proxyMarksSetting); raw != `{not json` {
		t.Fatalf("the unreadable row must be left as it is, got %q", raw)
	}
}
