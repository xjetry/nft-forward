package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	Applier    Applier

	// LegacyPaths configures where to look for the three pre-daemon state
	// files (TUI rules.json, agent-state.json, embedded-agent-state.json).
	// Production defaults populated by New; tests inject a temp dir.
	LegacyPaths LegacyMigrationPaths
	Iface       string
	CountersFn  func() ([]nft.Counter, error)
	HTTPListen  string
	TokenPath   string
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
	var httpToken string
	if cfg.TokenPath != "" {
		tok, err := os.ReadFile(cfg.TokenPath)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		httpToken = strings.TrimSpace(string(tok))
		if httpToken == "" {
			return nil, fmt.Errorf("token file is empty")
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
		httpListen:  cfg.HTTPListen,
		httpToken:   httpToken,
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
			if err := SaveState(d.statePath, migrated); err != nil {
				return fmt.Errorf("save migrated state: %w", err)
			}
		}
	}

	owners, err := LoadState(d.statePath)
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
		if err := d.applier.Apply(resolved, d.iface); err != nil {
			return fmt.Errorf("bootstrap apply: %w", err)
		}
	}
	d.mu.Lock()
	d.owners = owners
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

	var httpSrv *http.Server
	if d.httpListen != "" {
		httpSrv = &http.Server{
			Addr:              d.httpListen,
			Handler:           d.httpHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("nft-forward daemon listening on %s (http)", d.httpListen)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("http listener: %v", err)
			}
		}()
	}

	go d.refreshLoop(ctx)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(shutCtx)
		}
		return srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
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
