package server

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

func exitUsed(t *testing.T, d *sql.DB, uid int64) int64 {
	t.Helper()
	var used int64
	if err := d.QueryRow(`SELECT used_bytes FROM user_landing_exits WHERE user_id=?`, uid).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

// Chain rule n1→n2, exit 8.8.8.8:443. Only the final hop's raw bytes reach the
// exit ledger. The global user quota is billed once, at the entry hop — the
// same 1000 bytes flow through both hops, so entry-only billing counts them a
// single time regardless of chain length.
func TestExitLedgerCountsFinalHopOnly(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "m2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	p1 := getHopPort(t, d, ruleID, n1.ID)
	p2 := getHopPort(t, d, ruleID, n2.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p1, BytesUp: 300, BytesDown: 700}})
	s.Hub.applyCounters(n2.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p2, BytesUp: 400, BytesDown: 600}})

	if used := exitUsed(t, d, uid); used != 1000 {
		t.Fatalf("exit ledger wants the final hop's 1000 raw bytes, got %d", used)
	}
	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 1000 {
		t.Fatalf("user ledger bills the entry hop once (1000), got %d", u.TrafficUsedBytes)
	}
}

// A middle hop whose target coincides with the exit host:port must not meter:
// final-hop detection keys on position, not on target matching.
func TestExitLedgerIgnoresRelayCollision(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m3", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "m4", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)
	// forge the middle hop's target to collide with the exit
	d.Exec(`UPDATE rule_hops SET target_host='8.8.8.8', target_port=443 WHERE rule_id=? AND position=0`, ruleID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	p1 := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p1, BytesUp: 500, BytesDown: 500}})

	if used := exitUsed(t, d, uid); used != 0 {
		t.Fatalf("middle hop must not meter into the exit ledger, got %d", used)
	}
}

// Unidirectional nodes bill uplink only, but the exit ledger records real
// traffic to the destination — and its growth alone must still trigger the
// quota callback (weighted is 0 for a downlink-only batch).
func TestExitLedgerUnidirectionalAndTouch(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m5", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	d.Exec(`UPDATE nodes SET unidirectional=1 WHERE id=?`, n1.ID)
	db.GrantNode(d, uid, n1.ID, 10, 0)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	touched := make(chan struct{}, 1)
	s.Hub.OnTrafficUpdate = func(userID, nodeID int64) {
		select {
		case touched <- struct{}{}:
		default:
		}
	}
	port := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesDown: 800}})

	if used := exitUsed(t, d, uid); used != 800 {
		t.Fatalf("exit ledger ignores unidirectional billing, want 800 got %d", used)
	}
	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("unidirectional downlink must not bill the user, got %d", u.TrafficUsedBytes)
	}
	select {
	case <-touched:
	case <-time.After(2 * time.Second):
		t.Fatal("exit ledger growth must trigger OnTrafficUpdate")
	}
}

func TestExitLedgerSkipsAbsentAndForeign(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m6", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)
	// present=0: rule reverts to ordinary accounting only
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)
	d.Exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=?`, uid)

	s, _ := New(d)
	port := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesUp: 100, BytesDown: 100}})

	if used := exitUsed(t, d, uid); used != 0 {
		t.Fatalf("absent exit must not meter, got %d", used)
	}
}
