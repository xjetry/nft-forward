package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

const (
	dialerPingInterval     = 10 * time.Second
	dialerCountersInterval = 5 * time.Second
	dialerReadTimeout      = 30 * time.Second
	dialerWriteTimeout     = 10 * time.Second
	dialerBackoffInitial   = 1 * time.Second
	dialerBackoffMax       = 60 * time.Second
)

const (
	probeV4Target = "8.8.8.8:80"
	probeV6Target = "[2001:4860:4860::8888]:80"
)

// DialerConfig wires the dialer to its host daemon without import cycles.
// GetState/OnApply give the dialer read-and-write access to the
// owner-segmented state through plain function values so the test in
// dialer_test.go can substitute fakes without spinning up a daemon.
type DialerConfig struct {
	URL          string
	Token        string
	AgentVersion string
	AgentSHA     string
	PortRange    string

	// DeclaredRelayHost/DeclaredRelayHostV6 come from the daemon's
	// --relay-host/--relay-host-v6 flags. Non-empty values are sent with
	// every hello so the panel treats them as authoritative — see
	// hub.go's applyDeclaredRelayHosts.
	DeclaredRelayHost   string
	DeclaredRelayHostV6 string

	GetState func() (OwnerRuleset, AgentMeta)
	OnApply  func(ctx context.Context, rev string, rules []nft.Rule) (warning string, err error)

	// OnMigrated is called after a successful migrate_rules handshake.
	// The daemon clears the tui segment so rules now live server-side only.
	OnMigrated func()

	// CountersFn returns deltas since the last call. nil = skip counters.
	CountersFn func() []wsproto.CounterSample

	// CountersReadd rolls a batch of deltas back into the sampler's cursor after
	// a failed send, so the bytes are re-reported next tick instead of being
	// silently lost (which would systematically undercount quota/billing).
	CountersReadd func([]wsproto.CounterSample)

	// OnConfigUpdate is called when the panel pushes a pool_size change,
	// either via HelloAck on connect or via a config_update frame at runtime.
	OnConfigUpdate func(poolSize int)
}

type upgradeResult struct {
	id  string
	ack wsproto.UpgradeAck
}

type Dialer struct {
	cfg DialerConfig

	upgradeCh chan upgradeResult

	cmdCh     chan wsproto.Envelope
	pendMu    sync.Mutex
	pending   map[string]chan wsproto.RuleCmdAck
	idSeq     atomic.Uint64
	connected atomic.Bool

	// nodeMu guards nodeID/nodeName set on successful hello_ack.
	nodeMu   sync.Mutex
	nodeID   int64
	nodeName string

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{} // closed when Run() returns
}

func NewDialer(cfg DialerConfig) *Dialer {
	return &Dialer{
		cfg:       cfg,
		upgradeCh: make(chan upgradeResult, 1),
		cmdCh:     make(chan wsproto.Envelope),
		pending:   make(map[string]chan wsproto.RuleCmdAck),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (d *Dialer) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

// Done returns a channel that is closed when Run has fully returned.
// Callers use it to wait for goroutine teardown before tearing down
// shared state (e.g. the data plane) that the dialer's OnApply callback
// may still be writing to.
func (d *Dialer) Done() <-chan struct{} {
	return d.done
}

// IsConnected reports whether the dialer has an active session.
func (d *Dialer) IsConnected() bool {
	return d.connected.Load()
}

// NodeName returns the node name received from the server's hello_ack.
func (d *Dialer) NodeName() string {
	d.nodeMu.Lock()
	defer d.nodeMu.Unlock()
	return d.nodeName
}

// NodeID returns the node ID received from the server's hello_ack.
func (d *Dialer) NodeID() int64 {
	d.nodeMu.Lock()
	defer d.nodeMu.Unlock()
	return d.nodeID
}

// CreateRule sends a rule_create command to the server and blocks for the ack.
func (d *Dialer) CreateRule(ctx context.Context, req wsproto.RuleCreate) (wsproto.RuleCmdAck, error) {
	p, err := json.Marshal(req)
	if err != nil {
		return wsproto.RuleCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeRuleCreate, p)
}

// UpdateRule sends a rule_update command to the server and blocks for the ack.
func (d *Dialer) UpdateRule(ctx context.Context, req wsproto.RuleUpdate) (wsproto.RuleCmdAck, error) {
	p, err := json.Marshal(req)
	if err != nil {
		return wsproto.RuleCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeRuleUpdate, p)
}

// MigrateRules sends a migrate_rules command to the server and blocks for ack.
func (d *Dialer) MigrateRules(ctx context.Context, rules []nft.Rule) (wsproto.RuleCmdAck, error) {
	p, err := json.Marshal(wsproto.MigrateRules{Rules: rules})
	if err != nil {
		return wsproto.RuleCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeMigrateRules, p)
}

// EditRuleHop sends a rule_hop_edit to the server and blocks for the ack.
// The rule hop's port/mode/comment edit is authoritative server-side; this
// returns the server's verdict (ack.OK / ack.Error) so the TUI can show a
// precise success or failure (e.g. port conflict) instead of failing silently.
func (d *Dialer) EditRuleHop(ctx context.Context, e wsproto.RuleHopEdit) (wsproto.RuleCmdAck, error) {
	p, err := json.Marshal(e)
	if err != nil {
		return wsproto.RuleCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeRuleHopEdit, p)
}

// DeleteRule sends a rule_delete to the server and blocks for the ack.
func (d *Dialer) DeleteRule(ctx context.Context, ruleID int64) (wsproto.RuleCmdAck, error) {
	p, err := json.Marshal(wsproto.RuleDelete{RuleID: ruleID})
	if err != nil {
		return wsproto.RuleCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeRuleDelete, p)
}

// sendCommand writes a command frame tagged with a fresh request ID and waits
// for the matching RuleCmdAck (correlated by Envelope.ID) or ctx expiry. It
// fails fast when no session is up: with no serve loop draining cmdCh the send
// would otherwise block until the caller's timeout. This fast-fail also rejects
// commands during a reconnect-backoff gap (connected=false between sessions);
// the command is not queued for the next session, so retries belong to the
// caller (in this project the TUI user re-presses to retry). A disconnect
// mid-wait is surfaced by runOnce, which drains pending with a connection-lost
// ack.
func (d *Dialer) sendCommand(ctx context.Context, frameType string, payload json.RawMessage) (wsproto.RuleCmdAck, error) {
	if !d.connected.Load() {
		return wsproto.RuleCmdAck{}, errors.New("daemon 未连接面板")
	}
	id := "cmd-" + strconv.FormatUint(d.idSeq.Add(1), 36)
	resCh := make(chan wsproto.RuleCmdAck, 1)
	d.pendMu.Lock()
	d.pending[id] = resCh
	d.pendMu.Unlock()
	defer func() {
		d.pendMu.Lock()
		delete(d.pending, id)
		d.pendMu.Unlock()
	}()

	select {
	case d.cmdCh <- wsproto.Envelope{Type: frameType, ID: id, Payload: payload}:
	case <-ctx.Done():
		return wsproto.RuleCmdAck{}, ctx.Err()
	case <-d.stop:
		return wsproto.RuleCmdAck{}, errors.New("daemon 停止中")
	}
	select {
	case ack := <-resCh:
		return ack, nil
	case <-ctx.Done():
		return wsproto.RuleCmdAck{}, ctx.Err()
	case <-d.stop:
		return wsproto.RuleCmdAck{}, errors.New("daemon 停止中")
	}
}

// Run loops forever, dialing + serving + reconnecting with backoff.
// Returns when ctx is canceled or Stop() is called. Closes d.done on
// exit so external shutdown coordinators can wait for any in-flight
// OnApply (which writes nft rules) to finish before tearing down the
// data plane.
func (d *Dialer) Run(ctx context.Context) {
	defer close(d.done)
	backoff := dialerBackoffInitial
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		default:
		}
		helloAcked, err := d.runOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dialer: connection ended: %v", err)
		}
		// Reset backoff when a session got past hello_ack: a node that's
		// been authenticated and serving for a while shouldn't pay a
		// minute-long reconnect penalty for one panel hiccup. Quick-fail
		// sessions (token bad, dial refused, hello timeout) keep growing
		// the backoff so we don't hammer a broken panel.
		if helloAcked {
			backoff = dialerBackoffInitial
		}
		sleep := jitter(backoff)
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > dialerBackoffMax {
			backoff = dialerBackoffMax
		}
	}
}

// runOnce dials, performs hello + optional register, then enters the
// read/write loop until disconnection. helloAcked is true when the session
// successfully completed hello_ack -- the caller uses this to reset the
// reconnect backoff so long-lived sessions don't pay a minute-long penalty
// after a single panel hiccup.
func (d *Dialer) runOnce(ctx context.Context) (helloAcked bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dctx, d.cfg.URL, nil)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", d.cfg.URL, err)
	}
	// The panel may push the upgrade binary inline over WS (~13MB); the default
	// read limit (32KB) would reject that frame and break upgrades.
	ws.SetReadLimit(64 << 20)
	defer ws.Close(websocket.StatusNormalClosure, "")

	_, currentMeta := d.cfg.GetState()
	probedV4, probedV6 := probeOutboundIPs()
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:           d.cfg.Token,
		AgentVersion:        d.cfg.AgentVersion,
		AgentSHA:            d.cfg.AgentSHA,
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		LastAppliedRev:      currentMeta.LastAppliedRev,
		PortRange:           d.cfg.PortRange,
		ProbedV4:            probedV4,
		ProbedV6:            probedV6,
		DeclaredRelayHost:   d.cfg.DeclaredRelayHost,
		DeclaredRelayHostV6: d.cfg.DeclaredRelayHostV6,
	})
	if err != nil {
		return false, fmt.Errorf("marshal hello: %w", err)
	}
	if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHello, ID: "hello-1", Payload: helloPayload}); err != nil {
		return false, fmt.Errorf("write hello: %w", err)
	}

	helloAck, err := readOne(ctx, ws, dialerReadTimeout)
	if err != nil {
		return false, fmt.Errorf("read hello_ack: %w", err)
	}
	if helloAck.Type != wsproto.TypeHelloAck {
		return false, fmt.Errorf("unexpected first reply %q", helloAck.Type)
	}
	var ha wsproto.HelloAck
	if err := json.Unmarshal(helloAck.Payload, &ha); err != nil {
		log.Printf("dialer: unmarshal %s: %v", helloAck.Type, err)
	}
	if ha.Error != "" {
		return false, fmt.Errorf("hello rejected: %s", ha.Error)
	}
	helloAcked = true

	// Store node identity from the server for status queries.
	d.nodeMu.Lock()
	d.nodeID = ha.NodeID
	d.nodeName = ha.Name
	d.nodeMu.Unlock()

	if ha.PoolSize > 0 && d.cfg.OnConfigUpdate != nil {
		d.cfg.OnConfigUpdate(ha.PoolSize)
	}

	// Migrate local tui rules to the server on first connect so they become
	// server-managed. On success, the callback clears the tui segment locally.
	owners, _ := d.cfg.GetState()
	if tuiRules := owners["tui"]; len(tuiRules) > 0 {
		migPayload, _ := json.Marshal(wsproto.MigrateRules{Rules: tuiRules})
		if err := writeOne(ctx, ws, wsproto.Envelope{
			Type:    wsproto.TypeMigrateRules,
			ID:      "migrate-1",
			Payload: migPayload,
		}); err != nil {
			log.Printf("dialer: write migrate_rules: %v", err)
		} else {
			migAck, err := readOne(ctx, ws, dialerReadTimeout)
			if err != nil {
				log.Printf("dialer: read migrate_rules ack: %v", err)
			} else if migAck.Type == wsproto.TypeRuleCmdAck {
				var ack wsproto.RuleCmdAck
				if err := json.Unmarshal(migAck.Payload, &ack); err == nil && ack.OK {
					if d.cfg.OnMigrated != nil {
						d.cfg.OnMigrated()
					}
				} else if err == nil && !ack.OK {
					log.Printf("dialer: migrate_rules rejected: %s", ack.Error)
				}
			}
		}
	}

	d.connected.Store(true)
	defer func() {
		d.connected.Store(false)
		// Wake any in-flight command waiters so they don't hang until their
		// own ctx times out; a fresher session can't deliver an ack tagged
		// with an ID minted on this dead connection.
		d.pendMu.Lock()
		for id, ch := range d.pending {
			select {
			case ch <- wsproto.RuleCmdAck{Error: "与面板的连接已断开"}:
			default:
			}
			delete(d.pending, id)
		}
		d.pendMu.Unlock()
	}()

	// Reader runs in its own goroutine because ws.Read blocks; the serve
	// loop pulls frames via readCh + errors via errCh. errCh is buffered
	// (1) so the reader can always push its terminal error and exit even
	// after the serve loop has already returned and stopped draining.
	readCh := make(chan wsproto.Envelope, 4)
	errCh := make(chan error, 1)
	go func() {
		for {
			env, err := readOne(ctx, ws, dialerReadTimeout)
			if err != nil {
				errCh <- err
				return
			}
			readCh <- env
		}
	}()
	pingT := time.NewTicker(dialerPingInterval)
	defer pingT.Stop()
	countersT := time.NewTicker(dialerCountersInterval)
	defer countersT.Stop()

	for {
		select {
		case <-ctx.Done():
			return helloAcked, ctx.Err()
		case <-d.stop:
			return helloAcked, nil
		case err := <-errCh:
			return helloAcked, err
		case env := <-readCh:
			switch env.Type {
			case wsproto.TypeApplyRuleset:
				var ar wsproto.ApplyRuleset
				if err := json.Unmarshal(env.Payload, &ar); err != nil {
					log.Printf("dialer: unmarshal %s: %v", env.Type, err)
					continue
				}
				ok := true
				errMsg := ""
				warning := ""
				if d.cfg.OnApply != nil {
					w, err := d.cfg.OnApply(ctx, ar.Rev, ar.Rules)
					warning = w
					if err != nil {
						ok = false
						errMsg = err.Error()
					}
				}
				ap, err := json.Marshal(wsproto.ApplyAck{Rev: ar.Rev, OK: ok, Error: errMsg, Warning: warning})
				if err != nil {
					log.Printf("dialer: marshal %s: %v", wsproto.TypeApplyAck, err)
					continue
				}
				if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: ap}); err != nil {
					return helloAcked, err
				}
			case wsproto.TypeUpgrade:
				var u wsproto.Upgrade
				if err := json.Unmarshal(env.Payload, &u); err != nil {
					log.Printf("dialer: unmarshal %s: %v", env.Type, err)
					continue
				}
				go func(id string) {
					ack := d.handleUpgrade(ctx, u)
					select {
					case d.upgradeCh <- upgradeResult{id: id, ack: ack}:
					default:
					}
				}(env.ID)
			case wsproto.TypeProbe:
				var p wsproto.Probe
				if err := json.Unmarshal(env.Payload, &p); err != nil {
					log.Printf("dialer: unmarshal %s: %v", env.Type, err)
					continue
				}
				go func(id string) {
					ack := doProbe(p.Target)
					raw, _ := json.Marshal(ack)
					select {
					case d.cmdCh <- wsproto.Envelope{Type: wsproto.TypeProbeAck, ID: id, Payload: raw}:
					default:
					}
				}(env.ID)
			case wsproto.TypePong:
				// reset is implicit; readOne uses fresh deadline each call
			case wsproto.TypeError:
				log.Printf("dialer: server error frame: %s", string(env.Payload))
			case wsproto.TypeConfigUpdate:
				var cu wsproto.ConfigUpdate
				if err := json.Unmarshal(env.Payload, &cu); err != nil {
					log.Printf("dialer: unmarshal %s: %v", env.Type, err)
					continue
				}
				if d.cfg.OnConfigUpdate != nil {
					d.cfg.OnConfigUpdate(cu.PoolSize)
				}
			case wsproto.TypeRuleCmdAck:
				var ack wsproto.RuleCmdAck
				if err := json.Unmarshal(env.Payload, &ack); err != nil {
					log.Printf("dialer: unmarshal %s: %v", env.Type, err)
					continue
				}
				d.pendMu.Lock()
				if ch, ok := d.pending[env.ID]; ok {
					select {
					case ch <- ack:
					default:
					}
				}
				d.pendMu.Unlock()
			}
		case <-pingT.C:
			pp, err := json.Marshal(wsproto.Ping{TS: time.Now().UnixMilli()})
			if err != nil {
				log.Printf("dialer: marshal %s: %v", wsproto.TypePing, err)
				continue
			}
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypePing, ID: "ping-" + strconv.FormatInt(time.Now().UnixMilli(), 36), Payload: pp}); err != nil {
				return helloAcked, err
			}
		case <-countersT.C:
			if d.cfg.CountersFn == nil {
				continue
			}
			samples := d.cfg.CountersFn()
			if len(samples) == 0 {
				continue
			}
			cp, err := json.Marshal(wsproto.Counters{Samples: samples})
			if err != nil {
				log.Printf("dialer: marshal %s: %v", wsproto.TypeCounters, err)
				continue
			}
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cp}); err != nil {
				log.Printf("dialer: write %s: %v", wsproto.TypeCounters, err)
				if d.cfg.CountersReadd != nil {
					d.cfg.CountersReadd(samples)
				}
			}
		case res := <-d.upgradeCh:
			ap, _ := json.Marshal(res.ack)
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeUpgradeAck, ID: res.id, Payload: ap}); err != nil {
				return helloAcked, err
			}
		case env := <-d.cmdCh:
			if err := writeOne(ctx, ws, env); err != nil {
				return helloAcked, err
			}
		}
	}
}

func readOne(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (wsproto.Envelope, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, b, err := ws.Read(rctx)
	if err != nil {
		return wsproto.Envelope{}, err
	}
	var env wsproto.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return wsproto.Envelope{}, err
	}
	return env, nil
}

func writeOne(ctx context.Context, ws *websocket.Conn, env wsproto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, dialerWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

func doProbe(target string) wsproto.ProbeAck {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		return wsproto.ProbeAck{Error: err.Error()}
	}
	conn.Close()
	return wsproto.ProbeAck{OK: true, Latency: int(elapsed.Milliseconds())}
}

// probeOutboundIP dials target over the given UDP network ("udp4" or "udp6")
// and returns the local address the OS routing table picked for it. A UDP
// dial never sends a packet — this is a pure local route lookup, so it works
// even when target itself is unreachable or firewalled. Returns "" if the
// family has no usable route (e.g. no IPv6 connectivity at all).
func probeOutboundIP(network, target string) string {
	conn, err := net.DialTimeout(network, target, 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// probeOutboundIPs returns this host's best-guess v4/v6 outbound addresses.
// Re-probed fresh on every call (cheap — no packets sent) so a network change
// between reconnects is picked up without an agent restart.
func probeOutboundIPs() (v4, v6 string) {
	return probeOutboundIP("udp4", probeV4Target), probeOutboundIP("udp6", probeV6Target)
}
