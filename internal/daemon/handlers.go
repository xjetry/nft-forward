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
	"nft-forward/internal/wsproto"
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

	// panelHook mirrors tuiHook for owner=="panel" writes. Production wires
	// it to dialer.NotifyPanelEdited so a TUI edit to a server-managed
	// forward is reported back to the panel for persistence. Invoked outside
	// d.mu for the same reason as tuiHook.
	panelHook func(rules []nft.Rule)
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

// reconcileOwners is the shared merge→resolve→apply pipeline used by
// every path that writes the owner-segmented ruleset.
//
// The caller must NOT hold d.mu — reconcileOwners acquires it to snapshot
// owners, then releases it before the heavy work (DNS, kernel apply), and
// re-acquires it for the final commit. mutate receives a deep clone of
// d.owners; it may modify the map freely. A nil mutate means re-resolve
// with the current owners unchanged (used by the DNS refresh loop).
//
// On success the returned slice is the freshly-resolved rules that were
// applied, and *committed* reports whether d.owners/d.lastResolved were
// actually updated (false when resolved rules are identical to the
// previous set — the DNS-refresh no-op case).
//
// metaFn, if non-nil, is called under d.mu right before commit to let the
// caller adjust AgentMeta (e.g. record a panel rev or clear MigratedAt).
//
// saveToDisk controls whether SaveState is called as part of the commit.
// The DNS refresh path skips persistence because the resolver output is
// ephemeral (re-derived on restart).
func (d *Daemon) reconcileOwners(
	ctx context.Context,
	mutate func(OwnerRuleset),
	metaFn func(*AgentMeta),
	saveToDisk bool,
) (resolved []nft.Rule, committed bool, err error) {
	d.mu.Lock()
	candidate := cloneOwners(d.owners)
	prev := append([]nft.Rule(nil), d.lastResolved...)
	d.mu.Unlock()

	if mutate != nil {
		mutate(candidate)
	}

	merged, err := MergedRuleset(candidate)
	if err != nil {
		return nil, false, err
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resolved, _, err = d.resolveFn(rctx, merged)
	cancel()
	if err != nil {
		return nil, false, err
	}
	if err := requireResolvedHosts(resolved); err != nil {
		return nil, false, err
	}

	// DNS-refresh callers skip apply+commit when nothing moved.
	if !saveToDisk && !rulesDiffer(prev, resolved) {
		return resolved, false, nil
	}

	if err := d.applySerialized(ctx, resolved); err != nil {
		return nil, false, fmt.Errorf("apply: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	meta := d.meta
	if metaFn != nil {
		metaFn(&meta)
	}
	if saveToDisk {
		if err := SaveState(d.statePath, candidate, meta); err != nil {
			return nil, false, fmt.Errorf("save state: %w", err)
		}
	}
	d.owners = candidate
	d.meta = meta
	d.lastResolved = append([]nft.Rule(nil), resolved...)
	return resolved, true, nil
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
	mux.HandleFunc("/v1/chain/edit", d.handleChainEdit)
	mux.HandleFunc("/v1/chain/delete", d.handleChainDelete)
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
// Replaces the named segment, merges, resolves DNS, applies to the
// kernel, persists, and finally — for owner=="tui" / "panel" — invokes
// the corresponding hook outside the lock so the dialer can push the
// change to the panel. The hook fires after the lock is released so a
// slow callback cannot stall other writes.
//
// When owner=="panel" and rev is non-empty, the panel-segment revision
// identifier is recorded in agent_meta.LastAppliedRev in the same
// SaveState transaction, letting the dialer short-circuit a redundant
// apply_ruleset push on the next reconnect. rev is ignored for other
// owners.
func (d *Daemon) setOwnerRuleset(ctx context.Context, owner string, rules []nft.Rule, rev string) error {
	_, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			if len(rules) == 0 {
				delete(candidate, owner)
			} else {
				candidate[owner] = append([]nft.Rule(nil), rules...)
			}
		},
		func(meta *AgentMeta) {
			if owner == "panel" && rev != "" {
				meta.LastAppliedRev = rev
			}
		},
		true, // persist to disk
	)
	if err != nil {
		return d.classifyWriteError(err)
	}

	// Hooks fire outside d.mu so a slow callback can't stall other writers.
	// Snapshot the hook and segment under the lock, then invoke.
	d.mu.Lock()
	tuiHook := d.tuiHook
	panelHook := d.panelHook
	hookRules := append([]nft.Rule(nil), d.owners[owner]...)
	d.mu.Unlock()

	switch owner {
	case "tui":
		if tuiHook != nil {
			tuiHook(hookRules)
		}
	case "panel":
		if panelHook != nil {
			panelHook(hookRules)
		}
	}
	return nil
}

// classifyWriteError wraps a reconcileOwners error into an ownerWriteError
// with the appropriate HTTP status so the handler can surface a precise
// status code to the client.
func (d *Daemon) classifyWriteError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already claimed"):
		return &ownerWriteError{status: http.StatusConflict, err: err}
	case strings.Contains(msg, "无法解析目标域名"):
		return &ownerWriteError{status: http.StatusBadRequest, err: err}
	default:
		return &ownerWriteError{status: http.StatusInternalServerError, err: err}
	}
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
	_, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
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
		},
		func(meta *AgentMeta) {
			meta.MigratedAt = time.Time{}
			meta.LastAppliedRev = ""
			meta.PanelURL = ""
		},
		true, // persist to disk
	)
	if err != nil {
		return err
	}

	// Notify the dialer (if any is still around between disengagement and
	// process exit) so the panel sees the merged tui snapshot before the
	// session tears down.
	d.mu.Lock()
	hook := d.tuiHook
	hookRules := append([]nft.Rule(nil), d.owners["tui"]...)
	d.mu.Unlock()
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

// handleChainEdit relays a TUI edit of a chain hop to the server through the
// dialer and blocks for the server's verdict. Chain edits are authoritative
// server-side (the relay skeleton spans nodes), so unlike owner-segment
// writes nothing is applied locally; the result returns synchronously so the
// TUI can show success or the server's rejection reason.
func (d *Daemon) handleChainEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ChainID    int64  `json:"chain_id"`
		ListenPort int    `json:"listen_port"`
		Mode       string `json:"mode"`
		Comment    string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dl := d.Dialer()
	if dl == nil {
		http.Error(w, "daemon 未连接面板，无法编辑链路", http.StatusServiceUnavailable)
		return
	}
	ack, err := dl.EditChainHop(r.Context(), wsproto.ChainHopEdit{
		ChainID: req.ChainID, ListenPort: req.ListenPort, Mode: req.Mode, Comment: req.Comment,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"entry": ack.Entry})
}

// handleChainDelete relays a TUI delete of an entire chain to the server
// through the dialer and blocks for the server's verdict. Deleting a chain is
// authoritative server-side (the relay skeleton spans multiple nodes), so
// nothing is torn down locally here; the result returns synchronously so the
// TUI can show success or the server's rejection reason.
func (d *Daemon) handleChainDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ChainID int64 `json:"chain_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dl := d.Dialer()
	if dl == nil {
		http.Error(w, "daemon 未连接面板，无法删除链路", http.StatusServiceUnavailable)
		return
	}
	ack, err := dl.DeleteChain(r.Context(), req.ChainID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
