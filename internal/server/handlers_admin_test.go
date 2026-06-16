package server

import (
	"database/sql"
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
