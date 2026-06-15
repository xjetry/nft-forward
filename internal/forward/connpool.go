package forward

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultPoolSize  = 4
	poolRetryBackoff = 2 * time.Second
	poolConnMaxAge   = 90 * time.Second
)

type timedConn struct {
	net.Conn
	created time.Time
}

func envPoolSize() int {
	s := os.Getenv("NFT_FORWARD_POOL_SIZE")
	if s == "" {
		return defaultPoolSize
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return defaultPoolSize
	}
	return n
}

// connPool maintains a fixed-size pool of pre-established TCP connections to a
// single target address. Get returns a ready connection from the pool when
// available, falling back to a synchronous dial when the pool is empty.
type connPool struct {
	addr string
	size int
	idle chan timedConn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newConnPool(addr string, size int) *connPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &connPool{
		addr:   addr,
		size:   size,
		idle:   make(chan timedConn, size),
		ctx:    ctx,
		cancel: cancel,
	}
	p.wg.Add(1)
	go p.replenish()
	return p
}

// Get returns a pre-connected TCP connection when one is available, otherwise
// falls back to a synchronous dial so latency is never worse than without a
// pool.
func (p *connPool) Get() (net.Conn, error) {
	for {
		select {
		case tc := <-p.idle:
			if time.Since(tc.created) > poolConnMaxAge {
				tc.Close()
				continue
			}
			if isConnAlive(tc.Conn) {
				return tc.Conn, nil
			}
			tc.Close()
			continue
		default:
			return dialUpstream(p.addr)
		}
	}
}

// replenish continuously fills the idle channel up to capacity.
func (p *connPool) replenish() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		if len(p.idle) >= p.size {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		c, err := dialUpstream(p.addr)
		if err != nil {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(poolRetryBackoff):
			}
			continue
		}

		select {
		case p.idle <- timedConn{Conn: c, created: time.Now()}:
		case <-p.ctx.Done():
			c.Close()
			return
		}
	}
}

func (p *connPool) Close() {
	p.cancel()
	p.wg.Wait()
	close(p.idle)
	for tc := range p.idle {
		tc.Close()
	}
}

// isConnAlive reports whether a pooled connection is still usable. It peeks at
// the socket (MSG_PEEK) so any data the peer already sent stays in the kernel
// buffer for the real relay to read — a plain Read would consume and discard
// the first byte, corrupting protocols where the upstream speaks first (SSH,
// SMTP, FTP banners). MSG_DONTWAIT keeps the probe non-blocking.
func isConnAlive(c net.Conn) bool {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return true // not a TCP conn (test pipe); can't probe, assume usable
	}
	rc, err := tcp.SyscallConn()
	if err != nil {
		return false
	}
	alive := false
	cerr := rc.Read(func(fd uintptr) bool {
		var buf [1]byte
		n, _, rerr := unix.Recvfrom(int(fd), buf[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case rerr == unix.EAGAIN || rerr == unix.EWOULDBLOCK:
			alive = true // socket open, just no data buffered
		case rerr != nil:
			alive = false // RST or other hard error
		case n == 0:
			alive = false // peer sent FIN: EOF
		default:
			alive = true // data waiting; peeked, not consumed
		}
		return true // always done — never wait for readiness
	})
	if cerr != nil {
		return false
	}
	return alive
}
