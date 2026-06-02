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
		d.lastCounters = map[string]int64{}
	}
	seen := make(map[string]bool, len(cur))
	var out []wsproto.CounterSample
	for _, c := range cur {
		key := c.Proto + "/" + strconv.Itoa(c.ListenPort)
		seen[key] = true
		last := d.lastCounters[key]
		delta := c.Bytes - last
		if c.Bytes < last {
			delta = c.Bytes // counter reset
		}
		d.lastCounters[key] = c.Bytes
		if delta > 0 {
			out = append(out, wsproto.CounterSample{ListenPort: c.ListenPort, Proto: c.Proto, BytesDelta: delta})
		}
	}
	for key := range d.lastCounters {
		if !seen[key] {
			delete(d.lastCounters, key) // a removed rule restarts from 0 if re-created
		}
	}
	return out
}
