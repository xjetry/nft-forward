package forward

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
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
			return net.DialTimeout("tcp4", p.addr, dialTimeout)
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

		c, err := net.DialTimeout("tcp4", p.addr, dialTimeout)
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

// isConnAlive performs a zero-byte read with an immediate deadline to detect
// connections that were closed by the peer while sitting in the pool.
func isConnAlive(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now())
	var buf [1]byte
	_, err := c.Read(buf[:])
	_ = c.SetReadDeadline(time.Time{})
	if err == nil {
		return true
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}
