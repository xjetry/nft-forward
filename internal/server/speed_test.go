package server

import (
	"testing"
	"time"
)

// A node carries hops from several users. The node-total snapshot sums every
// hop, while a per-user snapshot keeps only the requesting user's hops so a
// user sees their own throughput on the node, not everyone's.
func TestSpeedCacheSnapshotPerUser(t *testing.T) {
	sc := newSpeedCache()
	sc.nodes[1] = &nodeSpeedState{
		lastSeen: time.Now(),
		hops: map[string]*hopState{
			"tcp/1000": {upBps: 100, downBps: 200, ownerID: 100},
			"tcp/1001": {upBps: 30, downBps: 40, ownerID: 200},
			"tcp/1002": {upBps: 5, downBps: 6, ownerID: 100},
		},
	}

	total := entryByNode(sc.snapshot())
	if got := total[1]; got.Up != 135 || got.Down != 246 {
		t.Fatalf("node total: got up=%d down=%d, want up=135 down=246", got.Up, got.Down)
	}

	u100 := entryByNode(sc.snapshotForUser(100))
	if got := u100[1]; got.Up != 105 || got.Down != 206 {
		t.Fatalf("user 100 on node: got up=%d down=%d, want up=105 down=206", got.Up, got.Down)
	}

	u200 := entryByNode(sc.snapshotForUser(200))
	if got := u200[1]; got.Up != 30 || got.Down != 40 {
		t.Fatalf("user 200 on node: got up=%d down=%d, want up=30 down=40", got.Up, got.Down)
	}

	// A user with no hop on the node gets no entry rather than a zero row.
	if _, ok := entryByNode(sc.snapshotForUser(999))[1]; ok {
		t.Fatalf("user 999 should have no entry on node 1")
	}
}

func entryByNode(entries []SpeedEntry) map[int64]SpeedEntry {
	m := map[int64]SpeedEntry{}
	for _, e := range entries {
		m[e.NodeID] = e
	}
	return m
}
