package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"nft-forward/internal/forward"
	"nft-forward/internal/wsproto"
)

// handleCounters returns per-rule counters merged across the kernel and
// userspace backends. The poller uses these for tenant traffic accounting;
// exposing them on the daemon (not on every client) keeps the data plane as
// the single source of truth.
func (d *Daemon) handleCounters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counters, err := d.countersFn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if counters == nil {
		counters = []forward.Counter{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"counters": counters})
}

// counterSamples computes per-rule byte deltas since the last call, for the
// dialer to push to the panel. nft re-applies (flush+recreate) the table on
// every reconcile, zeroing kernel counters, so a current value below the last
// observed one is treated as a reset (delta = current).
//
// Limitation: bytes accumulated within a reconcile window that is flushed
// before it is ever polled are lost, so quota accounting is approximate, not
// exact. This is acceptable because reconciles are infrequent relative to the
// poll interval and the panel only needs coarse traffic totals.
func (d *Daemon) counterSamples() []wsproto.CounterSample {
	cur, err := d.dp.Counters()
	if err != nil {
		log.Printf("counters: %v", err)
		return nil
	}
	d.countersMu.Lock()
	defer d.countersMu.Unlock()
	if d.lastCounters == nil {
		d.lastCounters = map[string][2]int64{}
	}
	seen := make(map[string]bool, len(cur))
	var out []wsproto.CounterSample
	for _, c := range cur {
		key := c.Proto + "/" + strconv.Itoa(c.ListenPort)
		seen[key] = true
		last := d.lastCounters[key]
		deltaUp := c.BytesUp - last[0]
		if c.BytesUp < last[0] {
			deltaUp = c.BytesUp
		}
		deltaDown := c.BytesDown - last[1]
		if c.BytesDown < last[1] {
			deltaDown = c.BytesDown
		}
		d.lastCounters[key] = [2]int64{c.BytesUp, c.BytesDown}
		if deltaUp > 0 || deltaDown > 0 {
			out = append(out, wsproto.CounterSample{ListenPort: c.ListenPort, Proto: c.Proto, BytesUp: deltaUp, BytesDown: deltaDown})
		}
	}
	for key := range d.lastCounters {
		if !seen[key] {
			delete(d.lastCounters, key)
		}
	}
	return out
}

// reAddCounters rewinds the sampler cursor by the given deltas after a failed
// send so the next counterSamples() call re-reports them. Without this, a
// dropped counters frame silently discards traffic and undercounts quota. Bytes
// for a key already pruned (its rule was removed) are unrecoverable and dropped.
func (d *Daemon) reAddCounters(samples []wsproto.CounterSample) {
	d.countersMu.Lock()
	defer d.countersMu.Unlock()
	if d.lastCounters == nil {
		return
	}
	for _, s := range samples {
		key := s.Proto + "/" + strconv.Itoa(s.ListenPort)
		if last, ok := d.lastCounters[key]; ok {
			d.lastCounters[key] = [2]int64{last[0] - s.BytesUp, last[1] - s.BytesDown}
		}
	}
}
