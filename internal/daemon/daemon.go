package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/tc"
)

const (
	DefaultSocketPath = "/var/run/nft-forward.sock"
	DefaultStatePath  = "/var/lib/nft-forward/state.json"
	DefaultGroupName  = "nft-forward"
)

// Config wires the daemon's external dependencies. Fields not set are
// filled from the Default* constants by New so a zero-value Config
// "just works" in production.
type Config struct {
	SocketPath string
	StatePath  string
	GroupName  string

	// Dataplane is the kernel+userspace forwarding backend. Production
	// defaults to forward.New (wired with Iface); tests inject a fake.
	Dataplane Dataplane

	// LegacyPaths configures where to look for the three pre-daemon state
	// files (TUI rules.json, agent-state.json, embedded-agent-state.json).
	// Production defaults populated by New; tests inject a temp dir.
	LegacyPaths LegacyMigrationPaths
	Iface       string

	// ConnectURL, when non-empty, makes the daemon dial out to a panel
	// WebSocket endpoint (e.g. wss://panel/v1/agents). When empty the
	// daemon stays in tui/server-local mode and only serves its unix
	// socket. ConnectToken is the bearer credential sent in the hello
	// frame; required when ConnectURL is set.
	ConnectURL   string
	ConnectToken string
	PortRange    string

	// DeclaredRelayHost/DeclaredRelayHostV6, when set, are sent with every
	// hello as the authoritative data-plane address for this node — see
	// cmd/nft-agent's --relay-host/--relay-host-v6 flags.
	DeclaredRelayHost   string
	DeclaredRelayHostV6 string
}

// New constructs a Daemon ready to Bootstrap and Run. Dataplane defaults to
// the production forward backend wired with the resolved iface.
func New(cfg Config) (*Daemon, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	if cfg.StatePath == "" {
		cfg.StatePath = DefaultStatePath
	}
	if cfg.GroupName == "" {
		cfg.GroupName = DefaultGroupName
	}
	if cfg.LegacyPaths == (LegacyMigrationPaths{}) {
		cfg.LegacyPaths = DefaultLegacyPaths()
	}
	iface := cfg.Iface
	if iface == "" {
		iface = tc.DefaultIface()
	}
	if iface != "" {
		if _, err := net.InterfaceByName(iface); err != nil {
			log.Printf("WARN: tc interface %q not found — bandwidth shaping disabled", iface)
			iface = ""
		}
	} else {
		log.Printf("WARN: no default-route interface detected — bandwidth shaping disabled (use --iface to set manually)")
	}
	if cfg.Dataplane == nil {
		cfg.Dataplane = forward.New(forward.Config{Iface: iface})
	}
	return &Daemon{
		socketPath:          cfg.SocketPath,
		statePath:           cfg.StatePath,
		groupName:           cfg.GroupName,
		dp:                  cfg.Dataplane,
		legacyPaths:         cfg.LegacyPaths,
		countersFn:          cfg.Dataplane.Counters,
		resolveFn:           defaultResolver(resolver.New()),
		connectURL:          cfg.ConnectURL,
		connectTok:          cfg.ConnectToken,
		portRange:           cfg.PortRange,
		declaredRelayHost:   cfg.DeclaredRelayHost,
		declaredRelayHostV6: cfg.DeclaredRelayHostV6,
	}, nil
}

// downgradePanelRule turns a panel-pushed rule into a standalone tui rule:
// fresh local id, panel metadata cleared. Group shaping is dropped with it —
// the limit is priced by a panel-side grant that no longer governs this
// daemon — while the self-contained legacy per-rule cap is kept.
func downgradePanelRule(r nft.Rule) nft.Rule {
	r.ID = nft.NewRuleID()
	r.RuleID = 0
	r.RuleName = ""
	r.OwnerName = ""
	r.HopCount = 0
	r.ShapeGroup = 0
	r.RateMBytes = 0
	return r
}

// Bootstrap loads persisted state and re-applies it so the kernel ruleset
// reflects the last known good configuration immediately on daemon startup.
// Must be called before Run.
func (d *Daemon) Bootstrap() error {
	// If daemon's own state.json does not exist, this is potentially a
	// first-boot upgrade from the pre-daemon binaries — try importing
	// their legacy state files. We only attempt migration on first boot
	// so a later legacy file showing up after the daemon has been running
	// (e.g. a stale leftover) does not silently overwrite live state.
	if _, err := os.Stat(d.statePath); os.IsNotExist(err) {
		migrated, mErr := migrateLegacyState(d.legacyPaths)
		if mErr != nil {
			return fmt.Errorf("migrate legacy state: %w", mErr)
		}
		if len(migrated) > 0 {
			if err := SaveState(d.statePath, migrated, AgentMeta{}); err != nil {
				return fmt.Errorf("save migrated state: %w", err)
			}
		}
	}

	owners, meta, err := LoadState(d.statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Downgrade: when the daemon starts without a --connect config but has
	// rules in the "panel" segment (left from a previous connected session),
	// copy them to "tui" with server metadata cleared, then drop "panel".
	// This keeps the rules functional in standalone mode.
	if d.connectURL == "" && len(owners["panel"]) > 0 {
		downgraded := make([]nft.Rule, len(owners["panel"]))
		for i, r := range owners["panel"] {
			downgraded[i] = downgradePanelRule(r)
		}
		owners["tui"] = append(owners["tui"], downgraded...)
		delete(owners, "panel")
		meta.LastAppliedRev = ""
		if err := SaveState(d.statePath, owners, meta); err != nil {
			return fmt.Errorf("save downgraded state: %w", err)
		}
	}

	// Backfill: assign local IDs to any tui rules that lack one (left by
	// an older downgrade that did not generate IDs).
	if rules := owners["tui"]; len(rules) > 0 {
		patched := false
		for i := range rules {
			if rules[i].ID == "" {
				rules[i].ID = nft.NewRuleID()
				patched = true
			}
		}
		if patched {
			_ = SaveState(d.statePath, owners, meta)
		}
	}

	merged, err := MergedRuleset(owners)
	if err != nil {
		return fmt.Errorf("persisted state has conflict: %w", err)
	}
	var resolved []nft.Rule
	if len(merged) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		resolved, _, err = d.resolveFn(ctx, merged)
		if err != nil {
			log.Printf("bootstrap: partial DNS failure (will retry in refresh loop): %v", err)
		}
		// Apply only rules that resolved successfully; unresolved hosts
		// will be picked up by the periodic refresh loop.
		applyable, _ := partitionResolved(resolved)
		resolved = applyable
		if len(resolved) > 0 {
			if err := d.applySerialized(ctx, resolved); err != nil {
				return fmt.Errorf("bootstrap apply: %w", err)
			}
		}
	}
	d.mu.Lock()
	d.owners = owners
	d.meta = meta
	if len(resolved) > 0 {
		d.lastResolved = append([]nft.Rule(nil), resolved...)
	}
	d.mu.Unlock()
	return nil
}

// Run is the main lifecycle: bootstrap -> listen -> serve -> block until ctx is
// cancelled. The socket file is removed on exit so subsequent runs do not
// hit a stale file. Returns nil on clean shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Bootstrap(); err != nil {
		return err
	}
	l, err := ListenSocket(d.socketPath, d.groupName)
	if err != nil {
		return err
	}
	defer os.Remove(d.socketPath)
	srv := &http.Server{Handler: d.Handler()}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	go d.probeFirewallEnvironment()
	go d.refreshLoop(ctx)

	if d.connectURL != "" {
		dl := NewDialer(DialerConfig{
			URL:                 d.connectURL,
			Token:               d.connectTok,
			AgentVersion:        agentVersion(),
			AgentSHA:            agentSHA(),
			PortRange:           d.portRange,
			DeclaredRelayHost:   d.declaredRelayHost,
			DeclaredRelayHostV6: d.declaredRelayHostV6,
			GetState:            d.SnapshotForDialer,
			OnApply:             d.SetPanelRuleset,
			OnMigrated:          d.clearTuiSegment,
			CountersFn:          d.counterSamples,
			CountersReadd:       d.reAddCounters,
			OnConfigUpdate: func(poolSize int) {
				if dp, ok := d.dp.(*forward.Dataplane); ok {
					dp.SetPoolSize(poolSize)
				}
			},
		})
		d.dialer.Store(dl)
		go dl.Run(ctx)
	}

	var shutdownErr error
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Wait for the dialer goroutine to finish so any in-flight
		// SetPanelRuleset (which writes nft rules + shim INSERTs via
		// dp.Reconcile) completes before we proceed to dp.Close (shim
		// DELETEs, relay teardown). Without this, Close could race the
		// INSERT and leave the shim chain half-cleaned. Bounded by the
		// same 5s budget that srv.Shutdown gets — they run sequentially.
		if dl := d.dialer.Load(); dl != nil {
			dl.Stop()
			select {
			case <-dl.Done():
			case <-shutCtx.Done():
				log.Printf("daemon: dialer shutdown timeout — proceeding with data-plane close")
			}
		}
		shutdownErr = srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			shutdownErr = nil
		} else {
			shutdownErr = err
		}
	}
	if closeErr := d.closeSerialized(context.Background()); closeErr != nil {
		log.Printf("data-plane close: %v", closeErr)
	}
	return shutdownErr
}

// detectForwardDropNoShim returns true when the iptables FORWARD chain
// has a drop default policy AND no known shim was detected. Pure
// function so tests can drive it with fixture input.
func detectForwardDropNoShim(iptablesForwardListOutput string, detectedShims []string) bool {
	if iptablesForwardListOutput == "" {
		return false
	}
	if !strings.Contains(iptablesForwardListOutput, "policy DROP") {
		return false
	}
	return len(detectedShims) == 0
}

func (d *Daemon) probeFirewallEnvironment() {
	out, err := exec.Command("iptables", "-nL", "FORWARD").CombinedOutput()
	if err != nil {
		return // probe failed; silently skip — no signal to surface
	}
	var detected []string
	if d.dp != nil {
		if probed, ok := d.dp.(interface{ DetectedShims() []string }); ok {
			detected = probed.DetectedShims()
		}
	}
	if detectForwardDropNoShim(string(out), detected) {
		log.Printf("WARN: FORWARD chain has drop policy but no known firewall shim detected; " +
			"forwarded traffic may be blocked. supported shims: docker-user, ufw.")
	}
}

// refreshLoop periodically re-resolves and re-applies the ruleset on a
// configurable interval. It exits when ctx is Done.
func (d *Daemon) refreshLoop(ctx context.Context) {
	interval := dnsInterval()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.refreshOnce(ctx); err != nil {
				log.Printf("dns refresh: %v", err)
			}
		}
	}
}

// RunWithSignals is the production entry point: SIGINT / SIGTERM trigger a
// graceful shutdown via Run's context cancellation.
func (d *Daemon) RunWithSignals() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("nft-forward daemon: listening on %s", d.socketPath)
	return d.Run(ctx)
}

// SetPanelRuleset is invoked by the dialer when an apply_ruleset frame
// arrives. Thin wrapper over the unified write path so every panel-
// segment mutation funnels through reconcileOwners; rev is recorded in
// agent_meta.LastAppliedRev as part of the same SaveState transaction
// so a reconnect won't replay the same payload.
func (d *Daemon) SetPanelRuleset(ctx context.Context, rev string, rules []nft.Rule) (string, error) {
	_, unresolved, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			if len(rules) == 0 {
				delete(candidate, "panel")
			} else {
				candidate["panel"] = append([]nft.Rule(nil), rules...)
			}
		},
		func(meta *AgentMeta) {
			if rev != "" {
				meta.LastAppliedRev = rev
			}
		},
		true,
	)
	if err != nil {
		return "", d.classifyWriteError(err)
	}
	return summarizeUnresolved(unresolved), nil
}

// summarizeUnresolved renders a short, human-readable note naming the rules
// whose target could not be resolved, for display as a node-level warning.
func summarizeUnresolved(rules []nft.Rule) string {
	if len(rules) == 0 {
		return ""
	}
	const maxList = 5
	parts := make([]string, 0, len(rules))
	for i, r := range rules {
		if i == maxList {
			break
		}
		parts = append(parts, fmt.Sprintf("端口 %d → %s", r.SrcPort, r.DestHost))
	}
	if len(rules) > maxList {
		parts = append(parts, fmt.Sprintf("等共 %d 条", len(rules)))
	}
	return fmt.Sprintf("%d 条规则的目标无法解析：%s", len(rules), strings.Join(parts, "，"))
}

// clearTuiSegment removes the "tui" segment after a successful migration
// to the server. Called from the dialer's OnMigrated callback.
func (d *Daemon) clearTuiSegment() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			delete(candidate, "tui")
		}, nil, true)
	if err != nil {
		log.Printf("daemon: clear tui segment after migration: %v", err)
	}
}

// Dialer returns the currently-active Dialer, or nil when --connect
// was not set. Safe for concurrent read from goroutines outside Run.
func (d *Daemon) Dialer() *Dialer {
	return d.dialer.Load()
}

// SnapshotForDialer returns defensive copies of owners and meta so the
// dialer can read state without holding d.mu past the call boundary.
func (d *Daemon) SnapshotForDialer() (OwnerRuleset, AgentMeta) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := OwnerRuleset{}
	for k, v := range d.owners {
		cp[k] = append([]nft.Rule(nil), v...)
	}
	return cp, d.meta
}
