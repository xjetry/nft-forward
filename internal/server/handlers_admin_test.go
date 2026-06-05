package server

import (
	"database/sql"
	"fmt"
	"net/http"
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

func TestDeleteTunnelInUseRejected(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	tunID, err := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: n.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID:     n.ID,
		TunnelID:   sql.NullInt64{Int64: tunID, Valid: true},
		Proto:      "tcp",
		ListenPort: 30000,
		TargetIP:   "10.0.0.1",
		TargetPort: 443,
	}); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	postNode(t, s, admin, fmt.Sprintf("/tunnels/%d/delete", tunID), nil)

	if _, err := db.GetTunnel(d, tunID); err != nil {
		t.Fatalf("tunnel backing a forward must not be deleted, but GetTunnel errored: %v", err)
	}
}

func TestDeleteEmptyTunnelSucceeds(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	tunID, err := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: n.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	if err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	postNode(t, s, admin, fmt.Sprintf("/tunnels/%d/delete", tunID), nil)

	if _, err := db.GetTunnel(d, tunID); err == nil {
		t.Fatalf("tunnel with no forwards must be deleted, but GetTunnel still succeeds")
	}
}
