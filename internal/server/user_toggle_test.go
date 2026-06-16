package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func postAdmin(t *testing.T, s *Server, admin *http.Cookie, path string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
}

// Resetting traffic and enabling a user are independent actions: reset must
// zero usage without re-enabling, and toggle is what flips disabled state.
func TestResetTrafficKeepsDisabledThenToggleEnables(t *testing.T) {
	d := openDB(t)
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, "quota-user", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an over-quota auto-disable.
	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes=500, disabled=1, disable_reason='流量超额' WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	postAdmin(t, s, admin, fmt.Sprintf("/api/users/%d/reset-traffic", uid))
	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("reset should zero traffic, got %d", u.TrafficUsedBytes)
	}
	if !u.Disabled {
		t.Fatalf("reset traffic must NOT re-enable the user")
	}

	postAdmin(t, s, admin, fmt.Sprintf("/api/users/%d/toggle", uid))
	u, _ = db.GetUserByID(d, uid)
	if u.Disabled {
		t.Fatalf("toggle should enable the disabled user")
	}
}
