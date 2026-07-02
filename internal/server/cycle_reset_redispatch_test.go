package server

import (
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// A per-grant quota overrun never disables the user, so the old reset path
// (which only re-dispatched disabled users) left suppressed rules dead until
// an unrelated push. The cycle rollover itself must trigger the re-push.
func TestCycleResetRedispatchesWithoutDisable(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "cr1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 1000)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)

	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1.ID)
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=?, last_traffic_reset_at=0 WHERE id=?`,
		time.Now().Unix()-31*86400, uid)

	hub := NewHub(d)
	got := make(chan []int64, 1)
	hub.Redispatch = func(nodes []int64) { got <- nodes }

	port := getHopPort(t, d, ruleID, n1.ID)
	hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesUp: 1}})

	select {
	case nodes := <-got:
		found := false
		for _, n := range nodes {
			if n == n1.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("redispatch nodes %v missing %d", nodes, n1.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cycle reset must redispatch even when the user is not disabled")
	}
	g, _ := db.GetNodeGrant(d, uid, n1.ID)
	if g.TrafficUsedBytes != 1 {
		t.Fatalf("grant counter should be reset then accumulate the sample, got %d", g.TrafficUsedBytes)
	}
}
