package daemon

import (
	"encoding/json"
	"net/http"

	"nft-forward/internal/nft"
)

// handleCounters returns per-rule counters scraped from nftables. The poller
// uses these for tenant traffic accounting; exposing them on the daemon (not
// on every client) keeps the kernel as the single source of truth.
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
		counters = []nft.Counter{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"counters": counters})
}

// defaultCounters delegates to the live nftables source. We expose this as a
// field so unit tests can swap in a fake without requiring root.
func defaultCounters() ([]nft.Counter, error) {
	return nft.Counters()
}
