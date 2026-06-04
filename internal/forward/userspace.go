package forward

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"

	"nft-forward/internal/nft"
)

// Bound per-port goroutine fan-out so a connection flood cannot exhaust memory.
const maxConnsPerPort = 1024

// listener is one userspace TCP forward: a net.Listener plus the hot-updatable
// dial target and rate limiter shared by all of its connections.
type listener struct {
	port  int
	ln    net.Listener
	tgt   atomic.Pointer[target]
	lim   atomic.Pointer[rate.Limiter]
	bytes atomic.Int64

	sem    chan struct{} // counting semaphore; cap == maxConnsPerPort
	conns  sync.Map     // net.Conn -> struct{}; only so close() can tear them down
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func openListener(r nft.Rule) (*listener, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", r.SrcPort))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &listener{port: r.SrcPort, ln: ln, ctx: ctx, cancel: cancel, sem: make(chan struct{}, maxConnsPerPort)}
	l.tgt.Store(&target{addr: targetAddr(r)})
	l.lim.Store(makeLimiter(r.BandwidthMbps))
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

func (l *listener) acceptLoop() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed (or fatal): stop accepting
		}
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
	upstream, err := net.DialTimeout("tcp4", tgt.addr, dialTimeout)
	if err != nil {
		return
	}
	l.conns.Store(upstream, struct{}{})
	defer func() { l.conns.Delete(upstream); upstream.Close() }()

	done := make(chan struct{}, 2)
	// Inbound (client->upstream): rate-limited + counted, matching nft
	// prerouting counter semantics.
	go func() {
		relayCopy(l.ctx, upstream, client, &l.lim, &l.bytes)
		halfCloseWrite(upstream)
		done <- struct{}{}
	}()
	// Return path (upstream->client): unshaped + uncounted (parity: nft
	// counts only the marked forward direction).
	go func() {
		relayCopy(l.ctx, client, upstream, nil, nil)
		halfCloseWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// close stops accepting, force-closes in-flight connections, and waits for all
// goroutines. No graceful drain — the forwarding layer is intentionally thin.
func (l *listener) close() {
	l.cancel()
	_ = l.ln.Close()
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
}

func newUserspaceBackend() *userspaceBackend {
	return &userspaceBackend{listeners: map[int]*listener{}}
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

	var opened []*listener
	for port, r := range desired {
		if _, ok := b.listeners[port]; ok {
			continue
		}
		l, err := openListener(r)
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
		l.tgt.Store(&target{addr: targetAddr(r)})
		l.lim.Store(makeLimiter(r.BandwidthMbps))
	}

	for port, l := range b.listeners {
		if _, ok := desired[port]; !ok {
			l.close()
			delete(b.listeners, port)
		}
	}
	return nil
}

func (b *userspaceBackend) Counters() []Counter {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Counter, 0, len(b.listeners))
	for _, l := range b.listeners {
		out = append(out, Counter{Proto: "tcp", ListenPort: l.port, Bytes: l.bytes.Load()})
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
}
