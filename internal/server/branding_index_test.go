package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// The SPA shell must carry the configured panel name at serve time: the
// browser renders <title> before any JS runs, so a hardcoded title would
// flash the default name on the tab and stay wrong on the login page.
func TestIndexInjectsPanelName(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	if err := db.SetSetting(d, "panel_name", `My "Panel" <X>`); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/index.html", "/login"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s status=%d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<title>My &#34;Panel&#34; &lt;X&gt;</title>") {
			t.Fatalf("%s: title not replaced with escaped panel name:\n%s", path, body)
		}
		if strings.Contains(body, "<title>nft-forward</title>") {
			t.Fatalf("%s: hardcoded default title still present", path)
		}
		if !strings.Contains(body, "window.__BRANDING__") {
			t.Fatalf("%s: branding bootstrap script missing", path)
		}
	}
}

// Without a configured name the shell keeps the built-in default untouched.
func TestIndexDefaultTitleWhenUnset(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>nft-forward</title>") {
		t.Fatal("default title missing when panel_name unset")
	}
}
