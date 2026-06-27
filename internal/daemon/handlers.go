package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
	"nft-forward/internal/portutil"
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
	portRange  string
	dialer     atomic.Pointer[Dialer]

	// reconcileMu serializes the data-plane reconcile/close calls against the
	// DNS refresh and write paths. setOwnerRuleset reconciles while holding
	// d.mu; refreshOnce reconciles without it. Without this lock those paths
	// could mutate the data plane concurrently. Lock order is always
	// d.mu -> reconcileMu (never the reverse), so the two write paths that
	// nest both locks can't deadlock against refreshOnce which takes only
	// reconcileMu.
	reconcileMu sync.Mutex

	mu           sync.Mutex
	owners       OwnerRuleset
	meta         AgentMeta
	lastResolved []nft.Rule
}

// applySerialized runs dp.Reconcile under reconcileMu so concurrent callers
// (the DNS refresh loop and the unix-socket / dialer write paths) never
// mutate the data plane at the same time. Callers may or may not hold d.mu;
// this method never takes d.mu, so the d.mu -> reconcileMu lock order is
// preserved.
func (d *Daemon) applySerialized(ctx context.Context, resolved []nft.Rule) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	return d.dp.Reconcile(ctx, resolved)
}

// reconcileOwners is the shared merge->resolve->apply pipeline used by
// every path that writes the owner-segmented ruleset.
//
// The caller must NOT hold d.mu -- reconcileOwners acquires it to snapshot
// owners, then releases it before the heavy work (DNS, kernel apply), and
// re-acquires it for the final commit. mutate receives a deep clone of
// d.owners; it may modify the map freely. A nil mutate means re-resolve
// with the current owners unchanged (used by the DNS refresh loop).
//
// On success the returned slice is the freshly-resolved rules that were
// applied, and *committed* reports whether d.owners/d.lastResolved were
// actually updated (false when resolved rules are identical to the
// previous set -- the DNS-refresh no-op case).
//
// metaFn, if non-nil, is called under d.mu right before commit to let the
// caller adjust AgentMeta (e.g. record a panel rev).
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

// Handler returns the HTTP mux serving all daemon endpoints.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/counters", d.handleCounters)
	mux.HandleFunc("/v1/status", d.handleStatus)
	mux.HandleFunc("/v1/rules", d.handleRules)
	mux.HandleFunc("/v1/rules/", d.handleRulesWithID)
	mux.HandleFunc("/v1/apply", d.handleApplyRuleset)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleApplyRuleset accepts a full ruleset push from the server's self-node
// dispatch path. This replicates the WS apply_ruleset for the co-located
// daemon: the server replaces the "panel" segment atomically.
func (d *Daemon) handleApplyRuleset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Rules []nft.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.SetPanelRuleset(r.Context(), "", body.Rules); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// statusResp is returned by GET /v1/status.
type statusResp struct {
	Connected bool   `json:"connected"`
	NodeName  string `json:"node_name,omitempty"`
	NodeID    int64  `json:"node_id,omitempty"`
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := statusResp{}
	if dl := d.Dialer(); dl != nil && dl.IsConnected() {
		resp.Connected = true
		resp.NodeName = dl.NodeName()
		resp.NodeID = dl.NodeID()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRules dispatches GET /v1/rules (list) and POST /v1/rules (create).
func (d *Daemon) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleListRules(w, r)
	case http.MethodPost:
		d.handleCreateRule(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRulesWithID dispatches PUT /v1/rules/{id} and DELETE /v1/rules/{id}.
func (d *Daemon) handleRulesWithID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/rules/")
	if id == "" {
		http.Error(w, "rule id required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		d.handleUpdateRule(w, r, id)
	case http.MethodDelete:
		d.handleDeleteRule(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleListRules returns the active rule set: "panel" segment when connected
// to the server, "tui" segment when running standalone.
func (d *Daemon) handleListRules(w http.ResponseWriter, _ *http.Request) {
	segment := "tui"
	if dl := d.Dialer(); dl != nil && dl.IsConnected() {
		segment = "panel"
	}
	d.mu.Lock()
	rules := append([]nft.Rule(nil), d.owners[segment]...)
	d.mu.Unlock()
	if rules == nil {
		rules = []nft.Rule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

type createRuleReq struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
	Name       string `json:"name"`
}

type createRuleResp struct {
	Entry      string `json:"entry"`
	ListenPort int    `json:"listen_port"`
}

// handleCreateRule creates a rule either through the server (when connected)
// or locally in the "tui" segment (when standalone). Local rules get an
// auto-assigned port when ListenPort is 0.
func (d *Daemon) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req createRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Connected: relay to server via WS.
	if dl := d.Dialer(); dl != nil && dl.IsConnected() {
		ack, err := dl.CreateRule(r.Context(), createToWSProto(req))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !ack.OK {
			http.Error(w, ack.Error, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, createRuleResp{Entry: ack.Entry})
		return
	}

	// Standalone: manage locally in "tui" segment.
	listenPort := req.ListenPort
	if listenPort == 0 {
		proto := req.Proto
		if proto == "" {
			proto = "tcp"
		}
		listenPort = d.pickLocalFreePort(proto)
		if listenPort == 0 {
			http.Error(w, "no free port available", http.StatusServiceUnavailable)
			return
		}
	}

	rule := nft.Rule{
		ID:       nft.NewRuleID(),
		Proto:    req.Proto,
		SrcPort:  listenPort,
		DestPort: req.ExitPort,
		Comment:  req.Comment,
		Mode:     req.Mode,
	}
	if ip := net.ParseIP(req.ExitHost); ip != nil {
		rule.DestIP = req.ExitHost
	} else {
		rule.DestHost = req.ExitHost
	}

	_, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			candidate["tui"] = append(candidate["tui"], rule)
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), d.classifyWriteError(err).(*ownerWriteError).status)
		return
	}

	writeJSON(w, http.StatusOK, createRuleResp{ListenPort: listenPort})
}

// handleUpdateRule updates a rule: server-side rules (numeric RuleID) relay
// through WS; local hex-ID rules update in the tui segment directly.
func (d *Daemon) handleUpdateRule(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Proto      string `json:"proto"`
		ExitHost   string `json:"exit_host"`
		ExitPort   int    `json:"exit_port"`
		ListenPort int    `json:"listen_port"`
		Mode       string `json:"mode"`
		Comment    string `json:"comment"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// If the id is a numeric RuleID, route through the dialer to the server.
	if ruleID, ok := parseRuleID(id); ok {
		dl := d.Dialer()
		if dl == nil || !dl.IsConnected() {
			http.Error(w, "daemon not connected to server", http.StatusServiceUnavailable)
			return
		}
		ack, err := dl.UpdateRule(r.Context(), updateToWSProto(ruleID, req))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !ack.OK {
			http.Error(w, ack.Error, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"entry": ack.Entry})
		return
	}

	// Local hex ID: update in "tui" segment.
	found := false
	_, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate["tui"]
			for i := range rules {
				if rules[i].ID == id {
					if req.Proto != "" {
						rules[i].Proto = req.Proto
					}
					if req.ExitHost != "" {
						if ip := net.ParseIP(req.ExitHost); ip != nil {
							rules[i].DestIP = req.ExitHost
							rules[i].DestHost = ""
						} else {
							rules[i].DestHost = req.ExitHost
							rules[i].DestIP = ""
						}
					}
					if req.ExitPort != 0 {
						rules[i].DestPort = req.ExitPort
					}
					if req.ListenPort != 0 {
						rules[i].SrcPort = req.ListenPort
					}
					if req.Mode != "" {
						rules[i].Mode = req.Mode
					}
					rules[i].Comment = req.Comment
					found = true
					break
				}
			}
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), d.classifyWriteError(err).(*ownerWriteError).status)
		return
	}
	if !found {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteRule deletes a rule: numeric RuleID routes through WS to the
// server; hex ID removes from local "tui" segment.
func (d *Daemon) handleDeleteRule(w http.ResponseWriter, r *http.Request, id string) {
	// Numeric RuleID: relay to server.
	if ruleID, ok := parseRuleID(id); ok {
		dl := d.Dialer()
		if dl == nil || !dl.IsConnected() {
			http.Error(w, "daemon not connected to server", http.StatusServiceUnavailable)
			return
		}
		ack, err := dl.DeleteRule(r.Context(), ruleID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !ack.OK {
			http.Error(w, ack.Error, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Local hex ID: remove from "tui" segment.
	found := false
	_, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate["tui"]
			for i := range rules {
				if rules[i].ID == id {
					candidate["tui"] = append(rules[:i], rules[i+1:]...)
					found = true
					break
				}
			}
			if len(candidate["tui"]) == 0 {
				delete(candidate, "tui")
			}
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pickLocalFreePort finds an unoccupied port in the chain-port range
// across all owner segments. A port is considered occupied if any rule
// with a matching or overlapping protocol already uses it.
func (d *Daemon) pickLocalFreePort(proto string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	occupied := make(map[int]bool)
	for _, rules := range d.owners {
		for _, r := range rules {
			if r.Proto == proto || r.Proto == "tcp+udp" || proto == "tcp+udp" {
				occupied[r.SrcPort] = true
			}
		}
	}
	return portutil.PickFreePort(portutil.ChainPortMin, portutil.ChainPortMax, occupied)
}

// ownerWriteError is the typed error returned by reconcileOwners so the
// HTTP handler can map merge conflicts to 409 and unresolved hosts to 400
// without reparsing the error message.
type ownerWriteError struct {
	status int
	err    error
}

func (e *ownerWriteError) Error() string { return e.err.Error() }
func (e *ownerWriteError) Unwrap() error { return e.err }

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

// cloneOwners returns a deep-enough copy that the caller can mutate the
// map (delete/replace keys) without affecting the original. Rule slices
// themselves are shallow-copied -- Rule is a value type so this is safe.
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

// parseRuleID tries to parse a string as a server-side int64 RuleID.
// Returns (id, true) on success, (0, false) on failure.
func parseRuleID(s string) (int64, bool) {
	// Server rule IDs are positive integers; local IDs are hex strings.
	var id int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + int64(c-'0')
	}
	if len(s) == 0 || id <= 0 {
		return 0, false
	}
	return id, true
}

// createToWSProto converts a local create request to the WS protocol struct.
func createToWSProto(req createRuleReq) wsproto.RuleCreate {
	return wsproto.RuleCreate{
		Proto:      req.Proto,
		ExitHost:   req.ExitHost,
		ExitPort:   req.ExitPort,
		ListenPort: req.ListenPort,
		Mode:       req.Mode,
		Comment:    req.Comment,
		Name:       req.Name,
	}
}

// updateToWSProto converts a local update request to the WS protocol struct.
func updateToWSProto(ruleID int64, req struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
	Name       string `json:"name"`
}) wsproto.RuleUpdate {
	return wsproto.RuleUpdate{
		RuleID:     ruleID,
		Proto:      req.Proto,
		ExitHost:   req.ExitHost,
		ExitPort:   req.ExitPort,
		ListenPort: req.ListenPort,
		Mode:       req.Mode,
		Comment:    req.Comment,
		Name:       req.Name,
	}
}
