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
	"strconv"
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

	Iface string

	// Mode 是这个 daemon 的运行形态：单机（零值）、由远端面板托管、面板机本机。
	// 见 RunMode。
	Mode      RunMode
	PortRange string

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
		countersFn:          cfg.Dataplane.Counters,
		resolveFn:           defaultResolver(resolver.New()),
		mode:                cfg.Mode,
		portRange:           cfg.PortRange,
		declaredRelayHost:   cfg.DeclaredRelayHost,
		declaredRelayHostV6: cfg.DeclaredRelayHostV6,
	}, nil
}

// downgradePanelRule 把一条面板下发的规则变成本机自有的本地规则：去掉面板给的全部
// 字段（哪些算面板给的由 nft.Rule.WithoutPanelBinding 定义），再给它一个新的本地 id
// ——本地规则靠 ID 寻址，面板下发的规则没有。
func downgradePanelRule(r nft.Rule) nft.Rule {
	r = r.WithoutPanelBinding()
	r.ID = nft.NewRuleID()
	return r
}

// Bootstrap loads persisted state and re-applies it so the kernel ruleset
// reflects the last known good configuration immediately on daemon startup.
// Must be called before Run.
func (d *Daemon) Bootstrap() error {
	owners, meta, err := LoadState(d.statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// 降级：单机 daemon（不归任何面板管）启动时若 "panel" 名下还有规则——上一次
	// 归面板管时留下的——就把它们去掉面板侧的元数据搬到 "tui" 名下，再删掉 "panel"
	// 这个键，这样规则在单机模式下照常转发、可在 TUI 里管理。
	//
	// 归面板管的 daemon 不做这件事：面板在它连上之后会重新下发 "panel" 名下的规则，降级出
	// 来的 "tui" 副本会和重新下发的规则抢同一个端口。
	if !d.mode.Managed() && len(owners[ownerPanel]) > 0 {
		downgraded := make([]nft.Rule, len(owners[ownerPanel]))
		for i, r := range owners[ownerPanel] {
			downgraded[i] = downgradePanelRule(r)
		}
		owners[ownerLocal] = append(owners[ownerLocal], downgraded...)
		delete(owners, ownerPanel)
		meta.LastAppliedRev = ""
		if err := SaveState(d.statePath, owners, meta); err != nil {
			return fmt.Errorf("save downgraded state: %w", err)
		}
	}

	// Backfill: assign local IDs to any tui rules that lack one (left by
	// an older downgrade that did not generate IDs).
	if rules := owners[ownerLocal]; len(rules) > 0 {
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

	if d.mode.Managed() {
		// 两种托管形态共用同一个拨号器，只在“往哪拨”上不同，由 d.mode 给出。拨号器
		// 只在这里创建，所以“有拨号器”与“托管形态”是同一件事。
		dl := NewDialer(DialerConfig{
			Panel:               d.mode,
			AgentVersion:        agentVersion(),
			AgentSHA:            agentSHA(),
			PortRange:           d.portRange,
			DeclaredRelayHost:   d.declaredRelayHost,
			DeclaredRelayHostV6: d.declaredRelayHostV6,
			GetState:            d.SnapshotForDialer,
			OnApply:             d.SetPanelRuleset,
			OnMigrated:          d.clearLocalRules,
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

// SetPanelRuleset 把 "panel" 名下的规则整体替换为 rules，走统一的 reconcileOwners 写路径。
// daemon 对外服务期间 "panel" 名下只在这里被写（另一处写入是 Bootstrap 的降级，在
// 启动阶段、开始服务之前）。调用方有两个：拨号器收到 apply_ruleset 帧时（rev 取
// 帧里的值），以及本机清空面板名下的规则时（rev 为空）。
//
// rev 与规则在同一次 SaveState 里落盘，所以 agent_meta.LastAppliedRev 始终是“当前
// 面板名下的规则对应面板的哪个版本”：握手时 daemon 把它报给面板，面板算出的版本与它相同
// 就不再重发。空 rev 表示当前内容不对应面板的任何版本——本机清空之后必须如此，
// 否则面板下次连上时会以为规则仍在而跳过下发。
//
// 同一次写入里，本地名下与新下发的面板规则占同一位置的规则让位（见
// dropLocalRulesClaimedByPanel）。
func (d *Daemon) SetPanelRuleset(ctx context.Context, rev string, rules []nft.Rule) (string, error) {
	var superseded []nft.Rule
	_, unresolved, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			if len(rules) == 0 {
				delete(candidate, ownerPanel)
			} else {
				candidate[ownerPanel] = append([]nft.Rule(nil), rules...)
			}
			superseded = dropLocalRulesClaimedByPanel(candidate)
		},
		func(meta *AgentMeta) {
			meta.LastAppliedRev = rev
		},
		true,
	)
	if err != nil {
		return "", d.classifyWriteError(err)
	}
	for _, r := range superseded {
		log.Printf("daemon: 本地规则 %s（%s）让位给面板下发的同端口规则", r.ID, portClaim(r))
	}
	return joinWarnings(summarizeUnresolved(unresolved), summarizeUnshaped(rules)), nil
}

// joinWarnings 把几条节点告警连成一条，空的不算。
func joinWarnings(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "；")
}

// summarizeUnshaped 点名面板规则里要求了分组限速、内核数据面却限不了速的那些
// （nft.KernelCannotShape）。它们照常转发、不限速；不报出来，面板上看到的就是一份
// 设了限速、节点也应用成功的授权。
func summarizeUnshaped(rules []nft.Rule) string {
	var ports []string
	for _, r := range rules {
		if nft.KernelCannotShape(r) {
			ports = append(ports, strconv.Itoa(r.SrcPort))
		}
	}
	if len(ports) == 0 {
		return ""
	}
	const maxList = 5
	listed := ports
	if len(listed) > maxList {
		listed = append(append([]string{}, ports[:maxList]...), fmt.Sprintf("等共 %d 条", len(ports)))
	}
	return fmt.Sprintf("%d 条规则的限速分组 id 超过 65535，内核模式下无法限速，这些规则当前不限速：端口 %s",
		len(ports), strings.Join(listed, "，"))
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

// clearLocalRules 在面板应答接管成功后清掉本地名下的全部规则，由拨号器的 OnMigrated 回调
// 调用。
//
// 清的是全部，而交给面板的是拨号器在此之前取的一份快照（Dialer.adoptLocalRules）。
// 两者之间本地名下不会多出快照里没有的规则：拨号器只在托管形态下存在，而托管形态
// 下本机控制口不建本地规则（handleCreateRule 一律交给面板），其余写本地名下的路径
// 只改或只删已有的规则。所以全部清掉不会带走一条面板没接管的规则。这个窗口里对
// 本地规则的修改或删除则不受保护：面板接管的是快照里的版本。
func (d *Daemon) clearLocalRules() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			delete(candidate, ownerLocal)
		}, nil, true)
	if err != nil {
		log.Printf("daemon: clear local rules after migration: %v", err)
	}
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
