package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
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

func TestImportTuiSnapshotInsertsForwardsAndDispatches(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal([]wsproto.Forward{
		{Proto: "tcp", ListenPort: 8443, TargetIP: "10.0.0.1", TargetPort: 8443},
		{Proto: "udp", ListenPort: 53, TargetIP: "10.0.0.2", TargetPort: 53},
	})
	if err := db.UpsertTuiSnapshot(d, n.ID, string(snapshot)); err != nil {
		t.Fatal(err)
	}

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/nodes/%d/import-tui", n.ID), nil)
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	fws, _ := db.ListForwardsByNode(d, n.ID)
	if len(fws) != 2 {
		t.Fatalf("expected 2 imported forwards, got %d", len(fws))
	}
}

func TestImportTuiSnapshotEmptyRedirectsWithoutInsert(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-2", "https://p", "tok2")
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/nodes/%d/import-tui", n.ID), nil)
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d body=%s", rec.Code, rec.Body.String())
	}
	fws, _ := db.ListForwardsByNode(d, n.ID)
	if len(fws) != 0 {
		t.Fatalf("expected 0 forwards (no snapshot), got %d", len(fws))
	}
}
