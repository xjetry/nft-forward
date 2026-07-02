package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"nft-forward/internal/nft"
)

// Bound per-port goroutine fan-out so a connection flood cannot exhaust memory.
const maxConnsPerPort = 1024

// listener is one userspace TCP forward: a net.Listener plus the hot-updatable
// dial target and rate limiter shared by all of its connections.
type listener struct {
	port int
	ln   net.Listener
	tgt  atomic.Pointer[target]
	lim  atomic.Pointer[rate.Limiter]
	// limDown mirrors lim for the upstream→client direction. Group-shaped
	// rules point both at the same shared limiter (the cap is the combined
	// two-way total); legacy per-rule caps leave it nil (download unshaped).
	limDown   atomic.Pointer[rate.Limiter]
	bytesUp   atomic.Int64
	bytesDown atomic.Int64
	pool      atomic.Pointer[connPool]
	poolSize  int

	sem    chan struct{} // counting semaphore; cap == maxConnsPerPort
	conns  sync.Map      // net.Conn -> struct{}; only so close() can tear them down
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func openListener(r nft.Rule, poolSize int) (*listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.SrcPort))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := targetAddr(r)
	l := &listener{port: r.SrcPort, ln: ln, ctx: ctx, cancel: cancel, sem: make(chan struct{}, maxConnsPerPort), poolSize: poolSize}
	l.tgt.Store(&target{addr: addr})
	if poolSize > 0 {
		l.pool.Store(newConnPool(addr, poolSize))
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

func (l *listener) acceptLoop() {
	defer l.wg.Done()
	var backoff time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed: normal shutdown
			}
			// A transient accept error (fd exhaustion, ECONNABORTED) must not
			// kill the port permanently. Back off (capped) and retry so the
			// listener recovers once the condition clears, instead of silently
			// going dead until the next Reconcile.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else {
				backoff *= 2
			}
			if backoff > time.Second {
				backoff = time.Second
			}
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		setKeepAlive(conn)
		// Back-pressure: wait for a free slot so we never exceed
		// maxConnsPerPort concurrent handler goroutines.
		select {
		case l.sem <- struct{}{}:
		case <-l.ctx.Done():
			conn.Close()
			return
		}
		l.wg.Add(1)
		go func() {
			defer func() { <-l.sem }()
			defer l.wg.Done()
			l.handle(conn)
		}()
	}
}

func (l *listener) handle(client net.Conn) {
	l.conns.Store(client, struct{}{})
	defer func() { l.conns.Delete(client); client.Close() }()

	tgt := l.tgt.Load()
	if tgt == nil {
		return
	}
	var upstream net.Conn
	var err error
	if p := l.pool.Load(); p != nil {
		upstream, err = p.Get()
	} else {
		upstream, err = dialUpstream(tgt.addr)
	}
	if err != nil {
		return
	}
	l.conns.Store(upstream, struct{}{})
	defer func() { l.conns.Delete(upstream); upstream.Close() }()

	done := make(chan struct{}, 2)
	// Inbound (client→upstream): rate-limited + counted.
	go func() {
		relayCopy(l.ctx, upstream, client, &l.lim, &l.bytesUp)
		halfCloseWrite(upstream)
		done <- struct{}{}
	}()
	// Return path (upstream→client): counted; shaped only under a group bucket.
	go func() {
		relayCopy(l.ctx, client, upstream, &l.limDown, &l.bytesDown)
		halfCloseWrite(client)
		done <- struct{}{}
	}()
	<-done
	// One direction has finished (the connection is now half-closed). Bound how
	// long the other direction may linger: a peer wedged in FIN_WAIT2 is not
	// probed by keepalive, so without this the surviving goroutine and its
	// semaphore slot would be pinned forever. Force-closing both ends unblocks
	// the stuck copy — the same race-free teardown close() relies on.
	timer := time.AfterFunc(relayLinger, func() {
		client.Close()
		upstream.Close()
	})
	<-done
	timer.Stop()
}

// close stops accepting, force-closes in-flight connections, and waits for all
// goroutines. No graceful drain — the forwarding layer is intentionally thin.
func (l *listener) close() {
	l.cancel()
	_ = l.ln.Close()
	if p := l.pool.Load(); p != nil {
		p.Close()
	}
	l.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
	l.wg.Wait()
}

// userspaceBackend keeps one listener per userspace TCP rule, keyed by port.
type userspaceBackend struct {
	mu        sync.Mutex
	listeners map[int]*listener
	// groups holds one shared limiter per shape group, stable across
	// Reconcile calls: rate changes SetLimit the existing limiter instead of
	// replacing it, so bucket state (accumulated debt) survives a re-apply.
	groups   map[int64]*rate.Limiter
	poolSize int
}

func newUserspaceBackend() *userspaceBackend {
	return &userspaceBackend{listeners: map[int]*listener{}, groups: map[int64]*rate.Limiter{}, poolSize: envPoolSize()}
}

// Reconcile makes the running listener set match rules. New listeners open
// first (make-before-break: a bind failure rolls back the just-opened ones and
// leaves the previous set intact); targets/limits hot-update without restart;
// removed listeners are closed.
func (b *userspaceBackend) Reconcile(rules []nft.Rule) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	desired := make(map[int]nft.Rule, len(rules))
	for _, r := range rules {
		desired[r.SrcPort] = r
	}

	desiredGroups := map[int64]int{}
	for _, r := range rules {
		if r.ShapeGroup > 0 && r.RateMBytes > 0 {
			desiredGroups[r.ShapeGroup] = r.RateMBytes
		}
	}
	for sg, mb := range desiredGroups {
		bytesPerSec := float64(mb) * 1048576
		if lim, ok := b.groups[sg]; ok {
			lim.SetLimit(rate.Limit(bytesPerSec))
			lim.SetBurst(groupBurst(bytesPerSec))
		} else {
			b.groups[sg] = rate.NewLimiter(rate.Limit(bytesPerSec), groupBurst(bytesPerSec))
		}
	}
	for sg := range b.groups {
		if _, ok := desiredGroups[sg]; !ok {
			delete(b.groups, sg)
		}
	}

	var opened []*listener
	for port, r := range desired {
		if _, ok := b.listeners[port]; ok {
			continue
		}
		l, err := openListener(r, b.poolSize)
		if err != nil {
			for _, ol := range opened {
				ol.close()
				delete(b.listeners, ol.port)
			}
			return fmt.Errorf("listen tcp/%d: %w", port, err)
		}
		b.listeners[port] = l
		opened = append(opened, l)
	}

	for port, r := range desired {
		l := b.listeners[port]
		newAddr := targetAddr(r)
		oldTgt := l.tgt.Load()
		l.tgt.Store(&target{addr: newAddr})
		b.setLimits(l, r)
		if oldTgt != nil && oldTgt.addr != newAddr && b.poolSize > 0 {
			if p := l.pool.Load(); p != nil {
				p.Close()
			}
			l.pool.Store(newConnPool(newAddr, b.poolSize))
		}
	}

	for port, l := range b.listeners {
		if _, ok := desired[port]; !ok {
			l.close()
			delete(b.listeners, port)
		}
	}
	return nil
}

// setLimits points a listener's limiters at the right bucket: group-shaped
// rules share the group's bidirectional limiter; legacy per-rule caps (from
// pre-group panels) keep their historical semantics — a private bucket, upload
// only.
func (b *userspaceBackend) setLimits(l *listener, r nft.Rule) {
	if r.ShapeGroup > 0 && r.RateMBytes > 0 {
		g := b.groups[r.ShapeGroup]
		l.lim.Store(g)
		l.limDown.Store(g)
		return
	}
	l.lim.Store(makeLimiter(r.BandwidthMbps))
	l.limDown.Store(nil)
}

func (b *userspaceBackend) Counters() []Counter {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Counter, 0, len(b.listeners))
	for _, l := range b.listeners {
		out = append(out, Counter{Proto: "tcp", ListenPort: l.port, BytesUp: l.bytesUp.Load(), BytesDown: l.bytesDown.Load()})
	}
	return out
}

func (b *userspaceBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for port, l := range b.listeners {
		l.close()
		delete(b.listeners, port)
	}
	b.groups = map[int64]*rate.Limiter{}
}

func (b *userspaceBackend) SetPoolSize(n int) {
	b.mu.Lock()
	b.poolSize = n
	b.mu.Unlock()
}
