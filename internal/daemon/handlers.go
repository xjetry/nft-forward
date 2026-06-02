package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
)

// Daemon holds the in-memory owner-segmented ruleset and data-plane wiring
// shared by the HTTP handlers and the lifecycle code.
// Fields are unexported; production callers go through New().
type Daemon struct {
	socketPath  string
	statePath   string
	groupName   string
	dp          Dataplane
	legacyPaths LegacyMigrationPaths
	countersFn  func() ([]forward.Counter, error)
	resolveFn   resolveFunc

	// countersMu guards lastCounters, the per-rule byte total observed on the
	// previous counterSamples call. The sampler computes deltas against it.
	countersMu   sync.Mutex
	lastCounters map[string]int64

	// connectURL/connectTok configure the outbound WebSocket dialer to
	// the panel. Empty connectURL = tui/server-local mode (no dialer).
	// dialer is atomic so unix-socket handlers running on their own
	// goroutines can safely read it (e.g. to push tui_segment_changed)
	// without coordinating with Run's lifecycle code.
	connectURL string
	connectTok string
	dialer     atomic.Pointer[Dialer]

	// reconcileMu serializes the data-plane reconcile/close calls against the
	// DNS refresh and write paths. setOwnerRuleset and demoteToTui reconcile
	// while holding d.mu; refreshOnce reconciles without it. Without this lock
	// those paths could mutate the data plane concurrently. Lock order is
	// always d.mu → reconcileMu (never the reverse), so the two write paths
	// that nest both locks can't deadlock against refreshOnce which takes only
	// reconcileMu.
	reconcileMu sync.Mutex

	mu           sync.Mutex
	owners       OwnerRuleset
	meta         AgentMeta
	lastResolved []nft.Rule

	// tuiHook, if non-nil, is invoked after a successful write to
	// owners["tui"] via setOwnerRuleset. Production wires this to
	// dialer.NotifyTuiChanged so the panel sees local TUI edits. Tests
	// substitute a fake. Invoked outside d.mu so the callback (which may
	// block on a channel send) cannot stall other writers.
	tuiHook func(rules []nft.Rule)
}

// applySerialized runs dp.Reconcile under reconcileMu so concurrent callers
// (the DNS refresh loop and the unix-socket / dialer write paths) never
// mutate the data plane at the same time. Callers may or may not hold d.mu;
// this method never takes d.mu, so the d.mu → reconcileMu lock order is
// preserved.
func (d *Daemon) applySerialized(ctx context.Context, resolved []nft.Rule) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	return d.dp.Reconcile(ctx, resolved)
}

// closeSerialized runs dp.Close under the same reconcileMu as applySerialized
// so a shutdown-time close (firewall-shim DELETEs, relay teardown) can't
// overlap an in-flight refresh-loop reconcile (shim INSERTs) and leave the
// shim chain half-cleaned. The refresh loop exits on ctx cancel, but a tick
// already inside applySerialized when the signal lands would otherwise race
// here.
func (d *Daemon) closeSerialized(ctx context.Context) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	return d.dp.Close(ctx)
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
	mux.HandleFunc("/v1/admin/demote-to-tui", d.handleDemoteToTui)
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

	if err := d.setOwnerRuleset(r.Context(), owner, p.Rules, ""); err != nil {
		status := http.StatusInternalServerError
		var oe *ownerWriteError
		if errors.As(err, &oe) {
			status = oe.status
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": len(p.Rules)})
}

// ownerWriteError is the typed error returned by setOwnerRuleset so the
// HTTP handler can map merge conflicts to 409 and unresolved hosts to 400
// without reparsing the error message.
type ownerWriteError struct {
	status int
	err    error
}

func (e *ownerWriteError) Error() string { return e.err.Error() }
func (e *ownerWriteError) Unwrap() error { return e.err }

// setOwnerRuleset is the unified write path for owner-segmented rules.
// Snapshots d.owners, replaces the named segment, merges, resolves DNS,
// applies to the kernel, persists, and finally — for owner=="tui" only —
// invokes tuiHook outside the lock so the dialer can push the change to
// the panel. The mutate / apply / save sequence runs under d.mu to keep
// concurrent writers serialized; the hook fires after the lock is
// released so a slow callback cannot stall other writes.
//
// When owner=="panel" and rev is non-empty, the panel-segment revision
// identifier is recorded in agent_meta.LastAppliedRev under the same
// d.mu so a single SaveState persists both the new ruleset and the
// rev together, letting the dialer short-circuit a redundant
// apply_ruleset push on the next reconnect. rev is ignored for other
// owners.
func (d *Daemon) setOwnerRuleset(ctx context.Context, owner string, rules []nft.Rule, rev string) error {
	d.mu.Lock()
	candidate := cloneOwners(d.owners)
	if len(rules) == 0 {
		delete(candidate, owner)
	} else {
		candidate[owner] = append([]nft.Rule(nil), rules...)
	}

	merged, err := MergedRuleset(candidate)
	if err != nil {
		d.mu.Unlock()
		return &ownerWriteError{status: http.StatusConflict, err: err}
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resolved, _, err := d.resolveFn(rctx, merged)
	cancel()
	if err != nil {
		d.mu.Unlock()
		return &ownerWriteError{status: http.StatusInternalServerError, err: err}
	}
	if err := requireResolvedHosts(resolved); err != nil {
		d.mu.Unlock()
		return &ownerWriteError{status: http.StatusBadRequest, err: err}
	}
	if err := d.applySerialized(ctx, resolved); err != nil {
		d.mu.Unlock()
		return &ownerWriteError{status: http.StatusInternalServerError, err: fmt.Errorf("apply: %w", err)}
	}
	meta := d.meta
	if owner == "panel" && rev != "" {
		meta.LastAppliedRev = rev
	}
	if err := SaveState(d.statePath, candidate, meta); err != nil {
		// Kernel ruleset is already updated by the Apply above; the disk
		// state lags behind. A daemon restart would reload the old state
		// and Apply that, rolling the kernel back. We accept this rare
		// window because SaveState failure is extremely unlikely outside
		// of a disk full / read-only fs situation, and reporting 500 lets
		// the client retry or escalate.
		d.mu.Unlock()
		return &ownerWriteError{status: http.StatusInternalServerError, err: fmt.Errorf("save state: %w", err)}
	}
	d.owners = candidate
	d.meta = meta
	d.lastResolved = append([]nft.Rule(nil), resolved...)
	hook := d.tuiHook
	hookRules := append([]nft.Rule(nil), candidate[owner]...)
	d.mu.Unlock()

	if owner == "tui" && hook != nil {
		hook(hookRules)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// demoteToTui folds the panel segment into the tui segment with panel
// rules winning any (proto, src_port) collision, then clears panel
// ownership and the panel-specific meta fields. Used when an agent
// node leaves panel management (install.sh uninstall agent without
// --purge) so panel-pushed forwards survive as locally-managed rules
// instead of disappearing the moment the dialer goes away.
func (d *Daemon) demoteToTui(ctx context.Context) error {
	d.mu.Lock()
	candidate := cloneOwners(d.owners)
	tui := append([]nft.Rule(nil), candidate["tui"]...)
	panel := candidate["panel"]

	type key struct {
		Proto string
		Port  int
	}
	idx := make(map[key]int, len(tui))
	for i, r := range tui {
		idx[key{r.Proto, r.SrcPort}] = i
	}
	for _, p := range panel {
		k := key{p.Proto, p.SrcPort}
		if i, ok := idx[k]; ok {
			tui[i] = p
		} else {
			tui = append(tui, p)
			idx[k] = len(tui) - 1
		}
	}
	if len(tui) == 0 {
		delete(candidate, "tui")
	} else {
		candidate["tui"] = tui
	}
	delete(candidate, "panel")

	newMeta := d.meta
	newMeta.MigratedAt = time.Time{}
	newMeta.LastAppliedRev = ""
	newMeta.PanelURL = ""

	merged, err := MergedRuleset(candidate)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resolved, _, err := d.resolveFn(rctx, merged)
	cancel()
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if err := requireResolvedHosts(resolved); err != nil {
		d.mu.Unlock()
		return err
	}
	if err := d.applySerialized(ctx, resolved); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("apply: %w", err)
	}
	if err := SaveState(d.statePath, candidate, newMeta); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("save state: %w", err)
	}
	d.owners = candidate
	d.meta = newMeta
	d.lastResolved = append([]nft.Rule(nil), resolved...)
	hook := d.tuiHook
	hookRules := append([]nft.Rule(nil), candidate["tui"]...)
	d.mu.Unlock()

	// Notify the dialer (if any is still around between disengagement and
	// process exit) so the panel sees the merged tui snapshot before the
	// session tears down.
	if hook != nil {
		hook(hookRules)
	}
	return nil
}

// handleDemoteToTui serves POST /v1/admin/demote-to-tui. No request body
// is read; the merge logic is fully driven by current daemon state.
func (d *Daemon) handleDemoteToTui(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.demoteToTui(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
