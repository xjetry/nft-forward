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
}

func newSpeedCache() *speedCache {
	return &speedCache{nodes: map[int64]*nodeSpeedState{}}
}

type counterDelta struct {
	proto         string
	listenPortStr string
	bytesUp       int64
	bytesDown     int64
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
		elapsed := now.Sub(hs.lastTime).Seconds()
		if elapsed > 0.5 {
			hs.upBps = float64(s.bytesUp) / elapsed
			hs.downBps = float64(s.bytesDown) / elapsed
		}
		hs.lastTime = now
	}
}

// snapshot returns a copy of all entries updated within the last 30 s,
// sorted by node ID for deterministic output.
func (sc *speedCache) snapshot() []SpeedEntry {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	cutoff := time.Now().Add(-30 * time.Second)
	out := make([]SpeedEntry, 0, len(sc.nodes))
	for nid, ns := range sc.nodes {
		if ns.lastSeen.Before(cutoff) {
			continue
		}
		var totalUp, totalDown float64
		for _, hs := range ns.hops {
			totalUp += hs.upBps
			totalDown += hs.downBps
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
