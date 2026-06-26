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

// Resetting traffic for a quota-disabled user must zero usage AND lift the
// ban atomically so the user's rules are re-pushed to the kernel. An
// admin-disabled user (reason != "流量超额") is unaffected by a traffic reset.
func TestResetTrafficReEnablesQuotaDisabledUser(t *testing.T) {
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
	// Quota-disabled ban must be lifted when traffic is reset, so rules
	// flow back to the kernel immediately after the reset.
	if u.Disabled {
		t.Fatalf("reset traffic must re-enable a quota-disabled user")
	}
}

// A user disabled by an admin (not by quota) must remain disabled after a
// traffic reset — manual disables are not affected by traffic accounting.
func TestResetTrafficKeepsAdminDisabled(t *testing.T) {
	d := openDB(t)
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, "admin-disabled-user", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes=500, disabled=1, disable_reason='管理员手动禁用' WHERE id=?`, uid); err != nil {
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
		t.Fatalf("admin-disabled user must remain disabled after traffic reset")
	}
}
