package daemon

import (
	"encoding/json"
	"net/http"
	"sync"

	"nft-forward/internal/nft"
)

// Daemon holds the in-memory ruleset and applier wiring shared by both
// the HTTP handlers and the lifecycle code (next file).
// Fields are unexported; production callers go through New() — handlers
// tests construct Daemon directly because they only need the handler surface.
type Daemon struct {
	socketPath string
	statePath  string
	groupName  string
	applier    Applier

	mu    sync.Mutex
	rules []nft.Rule
}

type rulesetPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// Handler returns the HTTP mux serving all daemon endpoints. The lifecycle
// code mounts it on whichever transport (unix socket today, optionally TCP
// in a later phase) is configured.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/ruleset", d.handleRuleset)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (d *Daemon) handleRuleset(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleGetRuleset(w, r)
	case http.MethodPost:
		d.handlePostRuleset(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Daemon) handleGetRuleset(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	out := append([]nft.Rule{}, d.rules...)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, rulesetPayload{Rules: out})
}

func (d *Daemon) handlePostRuleset(w http.ResponseWriter, r *http.Request) {
	var p rulesetPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Apply first; a kernel rejection must not poison the on-disk state,
	// otherwise a daemon restart would re-apply a known-bad ruleset forever.
	if err := d.applier.Apply(p.Rules); err != nil {
		http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := SaveState(d.statePath, p.Rules); err != nil {
		// Kernel ruleset is already updated by the Apply above; the disk
		// state lags behind. A daemon restart would reload the old state
		// and Apply that, rolling the kernel back. We accept this rare
		// window because SaveState failure is extremely unlikely outside
		// of a disk full / read-only fs situation, and reporting 500 lets
		// the client retry or escalate.
		http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.rules = p.Rules
	writeJSON(w, http.StatusOK, map[string]int{"count": len(p.Rules)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
