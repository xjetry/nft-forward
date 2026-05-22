package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"nft-forward/internal/nft"
)

// Daemon holds the in-memory owner-segmented ruleset and applier wiring
// shared by the HTTP handlers and the lifecycle code.
// Fields are unexported; production callers go through New().
type Daemon struct {
	socketPath  string
	statePath   string
	groupName   string
	applier     Applier
	legacyPaths LegacyMigrationPaths
	countersFn  func() ([]nft.Counter, error)

	mu     sync.Mutex
	owners OwnerRuleset
}

// segmentPayload is the body of POST /v1/ruleset/{owner} — replaces the
// entire ruleset segment owned by {owner}.
type segmentPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// fullPayload is the body of GET /v1/ruleset — every owner segment in
// one response so the caller can inspect the full daemon state.
type fullPayload struct {
	Owners OwnerRuleset `json:"owners"`
}

// Handler returns the HTTP mux serving all daemon endpoints.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/counters", d.handleCounters)
	mux.HandleFunc("/v1/ruleset", d.handleRulesetRoot)
	mux.HandleFunc("/v1/ruleset/", d.handleRulesetOwner)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRulesetRoot serves GET /v1/ruleset (segmented payload) and rejects
// POST/PUT/etc explicitly: the flat POST that previously existed is gone,
// callers MUST use /v1/ruleset/{owner} now.
func (d *Daemon) handleRulesetRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.mu.Lock()
		out := cloneOwners(d.owners)
		d.mu.Unlock()
		writeJSON(w, http.StatusOK, fullPayload{Owners: out})
	case http.MethodPost:
		// The flat endpoint is intentionally removed — return 410 with a
		// directive so existing clients (manual smoke tests, scripts) get
		// a clear pointer to the new shape rather than a generic 404.
		http.Error(w, "use POST /v1/ruleset/{owner} to write owner-scoped ruleset", http.StatusGone)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRulesetOwner serves POST /v1/ruleset/{owner}. Empty owner segment
// is allowed in body (clears the segment). Path may not end with a slash.
func (d *Daemon) handleRulesetOwner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	owner := strings.TrimPrefix(r.URL.Path, "/v1/ruleset/")
	if owner == "" || strings.ContainsAny(owner, "/") {
		http.Error(w, "owner segment required: POST /v1/ruleset/{owner}", http.StatusBadRequest)
		return
	}

	var p segmentPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Build the would-be new owner map, then merge to detect conflicts
	// before touching kernel state.
	candidate := cloneOwners(d.owners)
	if len(p.Rules) == 0 {
		delete(candidate, owner)
	} else {
		candidate[owner] = append([]nft.Rule(nil), p.Rules...)
	}
	merged, err := MergedRuleset(candidate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := d.applier.Apply(merged); err != nil {
		http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := SaveState(d.statePath, candidate); err != nil {
		// Kernel ruleset is already updated by the Apply above; the disk
		// state lags behind. A daemon restart would reload the old state
		// and Apply that, rolling the kernel back. We accept this rare
		// window because SaveState failure is extremely unlikely outside
		// of a disk full / read-only fs situation, and reporting 500 lets
		// the client retry or escalate.
		http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.owners = candidate
	writeJSON(w, http.StatusOK, map[string]int{"count": len(p.Rules)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cloneOwners returns a deep-enough copy that the caller can mutate the
// map (delete/replace keys) without affecting the original. Rule slices
// themselves are shallow-copied — Rule is a value type so this is safe.
func cloneOwners(src OwnerRuleset) OwnerRuleset {
	if src == nil {
		return OwnerRuleset{}
	}
	out := make(OwnerRuleset, len(src))
	for k, v := range src {
		out[k] = append([]nft.Rule(nil), v...)
	}
	return out
}
