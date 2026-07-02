package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// mustNode creates a relay-capable node in a fresh test DB, failing the test on
// error so callers don't have to repeat the check.
func mustNode(t *testing.T, d *sql.DB, name string) *db.Node {
	t.Helper()
	n, err := db.CreateNode(d, name, "https://p", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateMyRuleAcceptsTCPUDP(t *testing.T) {
	d := openDB(t)
	g := mustNode(t, d, "edge")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5, 0)

	s, _ := New(d)
	for _, tc := range []struct {
		proto string
		want  int
	}{
		{"tcp+udp", http.StatusOK},
		{"udp", http.StatusOK},
		{"sctp", http.StatusBadRequest},
	} {
		body, _ := json.Marshal(map[string]any{
			"node_id": g.ID, "name": "r-" + tc.proto, "proto": tc.proto, "exit": "9.9.9.9:8443",
		})
		req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("proto %s: status=%d want=%d body=%s", tc.proto, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestOccupiedPortsCrossProto(t *testing.T) {
	d := openDB(t)
	n := mustNode(t, d, "edge")
	rid, err := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(
		`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,?,?,?,?,?,?)`,
		rid, n.ID, "tcp+udp", 10001, "9.9.9.9", 443, "userspace", ""); err != nil {
		t.Fatal(err)
	}

	for _, proto := range []string{"tcp", "udp"} {
		occ, err := db.OccupiedPortsOnNode(d, n.ID, proto, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !occ[10001] {
			t.Fatalf("%s query should see tcp+udp port 10001 as occupied, got %v", proto, occ)
		}
	}
}

func TestTCPUDPHopCounterFanIn(t *testing.T) {
	d := openDB(t)
	n := mustNode(t, d, "edge")
	rid, err := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(
		`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,?,?,?,?,?,?)`,
		rid, n.ID, "tcp+udp", 10001, "9.9.9.9", 443, "userspace", ""); err != nil {
		t.Fatal(err)
	}

	m, err := db.RuleHopMapByNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tcp/10001", "udp/10001", "tcp+udp/10001"} {
		if m[key] == nil {
			t.Fatalf("key %s should map to the tcp+udp hop, got nil", key)
		}
	}
	if m["tcp/10001"] != m["udp/10001"] {
		t.Fatalf("tcp and udp keys must point to the same hop row")
	}
}

// A tcp chain must not be able to claim a port a tcp+udp chain already holds on
// the same node: tcp overlaps the tcp+udp claim, and letting it through would
// produce a (proto, port) overlap the daemon's forward.Partition rejects,
// failing the whole ruleset apply.
func TestRegenerateRuleCrossProtoPortConflict(t *testing.T) {
	d := openDB(t)
	n := mustNode(t, d, "edge")

	regenerate := func(name, proto string) error {
		r := &db.Rule{NodeID: n.ID, Name: name, Proto: proto, ExitHost: "9.9.9.9", ExitPort: 443}
		id, err := db.CreateRule(d, r)
		if err != nil {
			return err
		}
		r.ID = id
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, _, _, err := db.RegenerateRule(tx, r, []db.HopInput{{NodeID: n.ID, DesiredPort: 10001}}, nil); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	if err := regenerate("chain-tcpudp", "tcp+udp"); err != nil {
		t.Fatalf("first tcp+udp chain should claim port 10001: %v", err)
	}
	if err := regenerate("chain-tcp", "tcp"); err == nil {
		t.Fatal("tcp chain claiming a tcp+udp-held port should fail with a conflict, got nil")
	}
}
