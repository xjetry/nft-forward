package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestCreateMyRuleAcceptsTCPUDP(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "edge", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5)

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
	n, _ := db.CreateNode(d, "edge", "https://p", "tok")
	rid, err := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(
		`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,?,?,?,?,?,?)`,
		rid, n.ID, "tcp+udp", 10001, "9.9.9.9", 443, "userspace", ""); err != nil {
		t.Fatal(err)
	}

	occ, err := db.OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !occ[10001] {
		t.Fatalf("tcp query should see tcp+udp port 10001 as occupied, got %v", occ)
	}
	occ, _ = db.OccupiedPortsOnNode(d, n.ID, "udp", 0)
	if !occ[10001] {
		t.Fatalf("udp query should see tcp+udp port 10001 as occupied, got %v", occ)
	}
}

func TestTCPUDPHopCounterFanIn(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "edge", "https://p", "tok")
	rid, _ := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
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
