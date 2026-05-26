package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/tc"
	"nft-forward/internal/wsproto"
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
	Applier    Applier

	// LegacyPaths configures where to look for the three pre-daemon state
	// files (TUI rules.json, agent-state.json, embedded-agent-state.json).
	// Production defaults populated by New; tests inject a temp dir.
	LegacyPaths LegacyMigrationPaths
	Iface       string
	CountersFn  func() ([]nft.Counter, error)

	// ConnectURL, when non-empty, makes the daemon dial out to a panel
	// WebSocket endpoint (e.g. wss://panel/v1/agents). When empty the
	// daemon stays in tui/server-local mode and only serves its unix
	// socket. ConnectToken is the bearer credential sent in the hello
	// frame; required when ConnectURL is set.
	ConnectURL   string
	ConnectToken string
}

// New constructs a Daemon ready to Bootstrap and Run. Applier defaults to
// the production nft-backed implementation.
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
	if cfg.Applier == nil {
		cfg.Applier = DefaultApplier()
	}
	if cfg.LegacyPaths == (LegacyMigrationPaths{}) {
		cfg.LegacyPaths = DefaultLegacyPaths()
	}
	if cfg.CountersFn == nil {
		cfg.CountersFn = defaultCounters
	}
	iface := cfg.Iface
	if iface == "" {
		iface = tc.DefaultIface()
		if iface == "" {
			iface = "eth0"
		}
	}
	return &Daemon{
		socketPath:  cfg.SocketPath,
		statePath:   cfg.StatePath,
		groupName:   cfg.GroupName,
		applier:     cfg.Applier,
		legacyPaths: cfg.LegacyPaths,
		iface:       iface,
		countersFn:  cfg.CountersFn,
		resolveFn:   defaultResolver(resolver.New()),
		connectURL:  cfg.ConnectURL,
		connectTok:  cfg.ConnectToken,
	}, nil
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
			return fmt.Errorf("bootstrap resolve: %w", err)
		}
		if err := requireResolvedHosts(resolved); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if err := d.applySerialized(resolved); err != nil {
			return fmt.Errorf("bootstrap apply: %w", err)
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

// Run is the main lifecycle: bootstrap → listen → serve → block until ctx is
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
			URL:          d.connectURL,
			Token:        d.connectTok,
			AgentVersion: agentVersion(),
			GetState:     d.SnapshotForDialer,
			OnRegister:   func(_ []wsproto.Forward) { _ = d.OnLocalMigrated() },
			OnApply:      d.SetPanelRuleset,
			// Non-nil marker so the dialer emits tui_segment_changed
			// frames; payloads are constructed inside the dialer from
			// its tuiCh, so this callback itself is a no-op.
			OnTuiNotice: func(_ []wsproto.Forward) {},
		})
		d.dialer.Store(dl)
		// Forward local tui-segment writes into the dialer so the panel
		// learns about edits performed via the unix socket without
		// having to poll. Read dialer through the atomic accessor so
		// the closure stays correct even if Run is restructured later
		// (defensive — Run only ever stores once today).
		d.mu.Lock()
		d.tuiHook = func(rules []nft.Rule) {
			if dl := d.Dialer(); dl != nil {
				dl.NotifyTuiChanged(rules)
			}
		}
		d.mu.Unlock()
		go dl.Run(ctx)
	}

	var shutdownErr error
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Wait for the dialer goroutine to finish so any in-flight
		// SetPanelRuleset (which writes nft rules + shim INSERTs via
		// applier.Apply) completes before we proceed to applier.Cleanup
		// (shim DELETEs). Without this, Cleanup could race the INSERT
		// and leave the shim chain half-cleaned. Bounded by the same
		// 5s budget that srv.Shutdown gets — they run sequentially.
		if dl := d.dialer.Load(); dl != nil {
			dl.Stop()
			select {
			case <-dl.Done():
			case <-shutCtx.Done():
				log.Printf("daemon: dialer shutdown timeout — proceeding with applier cleanup")
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
	if cleanupErr := d.cleanupSerialized(); cleanupErr != nil {
		log.Printf("applier cleanup: %v", cleanupErr)
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
	if d.applier != nil {
		if probed, ok := d.applier.(interface{ DetectedShims() []string }); ok {
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

// OnLocalMigrated is invoked by the dialer after the panel ACKs a
// register_local. Clears the tui segment, stamps meta.MigratedAt the
// first time, and persists. Honors the "ACK before clear" invariant:
// once MigratedAt is non-zero we never re-emit register_local for the
// same agent lifetime, so a subsequent invocation only ever reconciles
// the cleared segment back to disk.
func (d *Daemon) OnLocalMigrated() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.owners, "tui")
	if d.meta.MigratedAt.IsZero() {
		d.meta.MigratedAt = time.Now().UTC()
	}
	return SaveState(d.statePath, d.owners, d.meta)
}

// SetPanelRuleset is invoked by the dialer when an apply_ruleset frame
// arrives. Thin wrapper over the unified write path so every panel-
// segment mutation funnels through setOwnerRuleset; rev is recorded in
// agent_meta.LastAppliedRev as part of the same SaveState transaction
// so a reconnect won't replay the same payload.
func (d *Daemon) SetPanelRuleset(ctx context.Context, rev string, rules []nft.Rule) error {
	return d.setOwnerRuleset(ctx, "panel", rules, rev)
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

// agentVersion is a coarse identifier surfaced in the hello frame for
// ops visibility. Falls back to "dev" when build info is unavailable
// (e.g. `go run` or a binary stripped of module data).
func agentVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
