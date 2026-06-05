package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
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
	dialerCountersInterval = 30 * time.Second
	dialerReadTimeout      = 30 * time.Second
	dialerWriteTimeout     = 10 * time.Second
	dialerBackoffInitial   = 1 * time.Second
	dialerBackoffMax       = 60 * time.Second
)

// DialerConfig wires the dialer to its host daemon without import cycles.
// GetState/OnRegister/OnApply give the dialer read-and-write access to the
// owner-segmented state through plain function values so the test in
// dialer_test.go can substitute fakes without spinning up a daemon.
type DialerConfig struct {
	URL          string
	Token        string
	AgentVersion string

	GetState      func() (OwnerRuleset, AgentMeta)
	OnRegister    func(forwards []wsproto.Forward) // called when register_local_ack arrives
	OnApply       func(ctx context.Context, rev string, rules []nft.Rule) error
	OnTuiNotice   func(forwards []wsproto.Forward) // optional; nil = skip notice
	OnPanelNotice func(forwards []wsproto.Forward) // optional; nil = skip notice

	// CountersFn returns deltas since the last call. nil = skip counters.
	CountersFn func() []wsproto.CounterSample
}

type Dialer struct {
	cfg DialerConfig

	tuiCh   chan []nft.Rule
	panelCh chan []nft.Rule

	cmdCh     chan wsproto.Envelope
	pendMu    sync.Mutex
	pending   map[string]chan wsproto.ChainCmdAck
	idSeq     atomic.Uint64
	connected atomic.Bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{} // closed when Run() returns
}

func NewDialer(cfg DialerConfig) *Dialer {
	return &Dialer{
		cfg:     cfg,
		tuiCh:   make(chan []nft.Rule, 1),
		panelCh: make(chan []nft.Rule, 1),
		cmdCh:   make(chan wsproto.Envelope),
		pending: make(map[string]chan wsproto.ChainCmdAck),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
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

// NotifyTuiChanged accepts a new tui-segment snapshot from the
// unix-socket handler. Last-write-wins: if a previous snapshot is still
// queued, drain it and push the newer one so only the latest state
// reaches the panel.
func (d *Dialer) NotifyTuiChanged(rules []nft.Rule) {
	cp := append([]nft.Rule(nil), rules...)
	select {
	case d.tuiCh <- cp:
	default:
		// Channel full — drain the stale snapshot and replace it.
		select {
		case <-d.tuiCh:
		default:
		}
		select {
		case d.tuiCh <- cp:
		default:
		}
	}
}

// NotifyPanelEdited accepts a new panel-segment snapshot from the
// unix-socket handler after a TUI edit to a server-managed forward.
// Last-write-wins, mirroring NotifyTuiChanged: drain the stale snapshot
// and push the newer one so only the latest state reaches the panel.
func (d *Dialer) NotifyPanelEdited(rules []nft.Rule) {
	cp := append([]nft.Rule(nil), rules...)
	select {
	case d.panelCh <- cp:
	default:
		select {
		case <-d.panelCh:
		default:
		}
		select {
		case d.panelCh <- cp:
		default:
		}
	}
}

// EditChainHop sends a chain_hop_edit to the server and blocks for the ack.
// The chain hop's port/mode/comment edit is authoritative server-side; this
// returns the server's verdict (ack.OK / ack.Error) so the TUI can show a
// precise success or failure (e.g. "端口被占用") instead of failing silently.
func (d *Dialer) EditChainHop(ctx context.Context, e wsproto.ChainHopEdit) (wsproto.ChainCmdAck, error) {
	p, err := json.Marshal(e)
	if err != nil {
		return wsproto.ChainCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeChainHopEdit, p)
}

// DeleteChain sends a chain_delete to the server and blocks for the ack.
func (d *Dialer) DeleteChain(ctx context.Context, chainID int64) (wsproto.ChainCmdAck, error) {
	p, err := json.Marshal(wsproto.ChainDelete{ChainID: chainID})
	if err != nil {
		return wsproto.ChainCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeChainDelete, p)
}

// sendCommand writes a command frame tagged with a fresh request ID and waits
// for the matching ChainCmdAck (correlated by Envelope.ID) or ctx expiry. It
// fails fast when no session is up: with no serve loop draining cmdCh the send
// would otherwise block until the caller's timeout. This fast-fail also rejects
// commands during a reconnect-backoff gap (connected=false between sessions);
// the command is not queued for the next session, so retries belong to the
// caller (in this project the TUI user re-presses to retry). A disconnect
// mid-wait is surfaced by runOnce, which drains pending with a connection-lost
// ack.
func (d *Dialer) sendCommand(ctx context.Context, frameType string, payload json.RawMessage) (wsproto.ChainCmdAck, error) {
	if !d.connected.Load() {
		return wsproto.ChainCmdAck{}, errors.New("daemon 未连接面板")
	}
	id := "cmd-" + strconv.FormatUint(d.idSeq.Add(1), 36)
	resCh := make(chan wsproto.ChainCmdAck, 1)
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
		return wsproto.ChainCmdAck{}, ctx.Err()
	case <-d.stop:
		return wsproto.ChainCmdAck{}, errors.New("daemon 停止中")
	}
	select {
	case ack := <-resCh:
		return ack, nil
	case <-ctx.Done():
		return wsproto.ChainCmdAck{}, ctx.Err()
	case <-d.stop:
		return wsproto.ChainCmdAck{}, errors.New("daemon 停止中")
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
// successfully completed hello_ack — the caller uses this to reset the
// reconnect backoff so long-lived sessions don't pay a minute-long penalty
// after a single panel hiccup.
func (d *Dialer) runOnce(ctx context.Context) (helloAcked bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dctx, d.cfg.URL, nil)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", d.cfg.URL, err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	_, currentMeta := d.cfg.GetState()
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:      d.cfg.Token,
		AgentVersion:   d.cfg.AgentVersion,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		LastAppliedRev: currentMeta.LastAppliedRev,
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

	// Trigger register_local if needed.
	owners, meta := d.cfg.GetState()
	if meta.MigratedAt.IsZero() && len(owners["tui"]) > 0 {
		fwds := rulesToForwards(owners["tui"])
		rlp, err := json.Marshal(wsproto.RegisterLocal{Forwards: fwds})
		if err != nil {
			return helloAcked, fmt.Errorf("marshal register_local: %w", err)
		}
		if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "reg-1", Payload: rlp}); err != nil {
			return helloAcked, fmt.Errorf("write register_local: %w", err)
		}
		rlAck, err := readOne(ctx, ws, dialerReadTimeout)
		if err != nil {
			return helloAcked, fmt.Errorf("read register_local_ack: %w", err)
		}
		if rlAck.Type == wsproto.TypeRegisterLocalAck {
			var ack wsproto.RegisterLocalAck
			if err := json.Unmarshal(rlAck.Payload, &ack); err != nil {
				log.Printf("dialer: unmarshal %s: %v", rlAck.Type, err)
			}
			if ack.Error == "" && d.cfg.OnRegister != nil {
				d.cfg.OnRegister(fwds)
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
			case ch <- wsproto.ChainCmdAck{Error: "与面板的连接已断开"}:
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
				if d.cfg.OnApply != nil {
					if err := d.cfg.OnApply(ctx, ar.Rev, ar.Rules); err != nil {
						ok = false
						errMsg = err.Error()
					}
				}
				ap, err := json.Marshal(wsproto.ApplyAck{Rev: ar.Rev, OK: ok, Error: errMsg})
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
				ack := d.handleUpgrade(ctx, u)
				ap, _ := json.Marshal(ack)
				if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeUpgradeAck, ID: env.ID, Payload: ap}); err != nil {
					return helloAcked, err
				}
			case wsproto.TypePong:
				// reset is implicit; readOne uses fresh deadline each call
			case wsproto.TypeError:
				log.Printf("dialer: server error frame: %s", string(env.Payload))
			case wsproto.TypeChainCmdAck:
				var ack wsproto.ChainCmdAck
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
			}
		case rules := <-d.tuiCh:
			if d.cfg.OnTuiNotice == nil {
				continue
			}
			fwds := rulesToForwards(rules)
			tp, err := json.Marshal(wsproto.TuiSegmentChanged{Forwards: fwds})
			if err != nil {
				log.Printf("dialer: marshal %s: %v", wsproto.TypeTuiSegmentChanged, err)
				continue
			}
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeTuiSegmentChanged, Payload: tp}); err != nil {
				log.Printf("dialer: write %s: %v", wsproto.TypeTuiSegmentChanged, err)
			}
		case rules := <-d.panelCh:
			if d.cfg.OnPanelNotice == nil {
				continue
			}
			fwds := rulesToForwards(rules)
			pp, err := json.Marshal(wsproto.PanelSegmentEdit{Forwards: fwds})
			if err != nil {
				log.Printf("dialer: marshal %s: %v", wsproto.TypePanelSegmentEdit, err)
				continue
			}
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypePanelSegmentEdit, Payload: pp}); err != nil {
				log.Printf("dialer: write %s: %v", wsproto.TypePanelSegmentEdit, err)
			}
		case env := <-d.cmdCh:
			if err := writeOne(ctx, ws, env); err != nil {
				return helloAcked, err
			}
		}
	}
}

func rulesToForwards(rs []nft.Rule) []wsproto.Forward {
	out := make([]wsproto.Forward, 0, len(rs))
	for _, r := range rs {
		f := wsproto.Forward{
			Proto:         r.Proto,
			ListenPort:    r.SrcPort,
			TargetPort:    r.DestPort,
			Comment:       r.Comment,
			BandwidthMbps: r.BandwidthMbps,
			Mode:          r.Mode,
		}
		if r.DestIP != "" {
			f.TargetIP = r.DestIP
		} else {
			f.TargetIP = r.DestHost
		}
		out = append(out, f)
	}
	return out
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
