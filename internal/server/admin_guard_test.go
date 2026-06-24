package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// Admin accounts can't be reset or deleted through the user-management API,
// regardless of any frontend gating.
func TestAdminUserCannotBeResetOrDeleted(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	hash, _ := HashPassword("pw")
	otherAdmin, err := db.CreateUser(d, "admin-2", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := db.CreateUser(d, "user-1", hash, "user")
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(admin)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := do("POST", fmt.Sprintf("/api/users/%d/reset-password", otherAdmin)); code != http.StatusForbidden {
		t.Errorf("reset-password admin: status = %d, want 403", code)
	}
	if code := do("DELETE", fmt.Sprintf("/api/users/%d", otherAdmin)); code != http.StatusForbidden {
		t.Errorf("delete admin: status = %d, want 403", code)
	}
	// A regular user is still resettable/deletable.
	if code := do("POST", fmt.Sprintf("/api/users/%d/reset-password", regular)); code != http.StatusOK {
		t.Errorf("reset-password regular: status = %d, want 200", code)
	}
	if code := do("DELETE", fmt.Sprintf("/api/users/%d", regular)); code != http.StatusOK {
		t.Errorf("delete regular: status = %d, want 200", code)
	}
}
