package server

import (
	"sort"
	"sync"
	"time"
)

// SpeedEntry holds the instantaneous throughput for a single node,
// computed from the most recent counter batch the agent reported.
type SpeedEntry struct {
	NodeID int64 `json:"node_id"`
	Up     int64 `json:"up"`
	Down   int64 `json:"down"`
	TS     int64 `json:"ts"`
}

// speedCache aggregates per-node throughput derived from agent counter
// batches. Entries older than 30 seconds are excluded from snapshots so
// disconnected nodes fade out automatically.
type speedCache struct {
	mu sync.RWMutex
	// per node: aggregated speed
	nodes map[int64]*nodeSpeedState
}

type nodeSpeedState struct {
	lastSeen time.Time
	hops     map[string]*hopState
}

type hopState struct {
	lastUp   int64
	lastDown int64
	lastTime time.Time
	upBps    float64
	downBps  float64
	// ownerID attributes this hop's throughput to the user who owns its rule,
	// so a per-user snapshot can show a user only their own share of the node.
	// 0 means no owner (an admin-created rule with no owner).
	ownerID int64
}

func newSpeedCache() *speedCache {
	return &speedCache{nodes: map[int64]*nodeSpeedState{}}
}

type counterDelta struct {
	proto         string
	listenPortStr string
	bytesUp       int64
	bytesDown     int64
	ownerID       int64
}

// update folds a counter batch into the cache. The bytes/sec rate is
// derived from the elapsed time since the previous batch for this hop;
// the first batch is skipped (no rate without a prior reference point).
func (sc *speedCache) update(nodeID int64, samples []counterDelta) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	ns, ok := sc.nodes[nodeID]
	if !ok {
		ns = &nodeSpeedState{hops: map[string]*hopState{}}
		sc.nodes[nodeID] = ns
	}
	now := time.Now()
	ns.lastSeen = now
	for _, s := range samples {
		key := s.proto + "/" + s.listenPortStr
		hs, ok := ns.hops[key]
		if !ok {
			hs = &hopState{lastTime: now}
			ns.hops[key] = hs
		}
		// A listen port can be reassigned to a different user's rule between
		// batches, so the owner is refreshed every sample rather than only on
		// creation.
		hs.ownerID = s.ownerID
		elapsed := now.Sub(hs.lastTime).Seconds()
		if elapsed > 0.5 {
			hs.upBps = float64(s.bytesUp) / elapsed
			hs.downBps = float64(s.bytesDown) / elapsed
		}
		hs.lastTime = now
	}
}

// snapshot returns the node-total throughput for every node updated within the
// last 30 s (all owners summed), sorted by node ID for deterministic output.
// This is the admin/aggregate view.
func (sc *speedCache) snapshot() []SpeedEntry {
	return sc.snapshotFiltered(func(*hopState) bool { return true })
}

// snapshotForUser returns per-node throughput counting only hops owned by the
// given user, so a user sees their own share of each node rather than its
// total. A node where the user has no active hop is omitted entirely (no zero
// row), matching how the dashboard treats a missing entry as "idle".
func (sc *speedCache) snapshotForUser(userID int64) []SpeedEntry {
	return sc.snapshotFiltered(func(hs *hopState) bool { return hs.ownerID == userID })
}

// snapshotFiltered aggregates each node's hops that pass keep into one entry,
// dropping nodes not seen in the last 30 s and nodes with no matching hop.
func (sc *speedCache) snapshotFiltered(keep func(*hopState) bool) []SpeedEntry {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	cutoff := time.Now().Add(-30 * time.Second)
	out := make([]SpeedEntry, 0, len(sc.nodes))
	for nid, ns := range sc.nodes {
		if ns.lastSeen.Before(cutoff) {
			continue
		}
		var totalUp, totalDown float64
		matched := false
		for _, hs := range ns.hops {
			if !keep(hs) {
				continue
			}
			matched = true
			totalUp += hs.upBps
			totalDown += hs.downBps
		}
		if !matched {
			continue
		}
		out = append(out, SpeedEntry{
			NodeID: nid,
			Up:     int64(totalUp),
			Down:   int64(totalDown),
			TS:     ns.lastSeen.UnixMilli(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
