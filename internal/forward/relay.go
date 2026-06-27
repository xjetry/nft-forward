package forward

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"nft-forward/internal/nft"
)

// relayBufSize is the copy buffer per direction. Wrapping the source reader
// (below) makes io.CopyBuffer take the generic buffered path instead of
// splice — required so byte accounting updates continuously over the life of
// a connection, not only at close. This mirrors realm's behavior when its
// traffic counter is enabled; we always count (for quota), so we always copy.
const relayBufSize = 64 * 1024

const dialTimeout = 10 * time.Second

// keepAlivePeriod paces TCP keepalive probes on both the client and upstream
// legs so a peer that dies without sending FIN/RST is detected by the kernel
// and the relay goroutine unblocks instead of leaking.
const keepAlivePeriod = 30 * time.Second

// relayLinger bounds how long the surviving direction may run after the other
// direction has half-closed. A peer stuck in FIN_WAIT2 is not probed by
// keepalive (the kernel only keepalives ESTABLISHED sockets), so without this
// the goroutine — and its per-port semaphore slot — would pin forever. It is a
// var, not a const, so tests can shrink it. The window is generous because a
// half-close legitimately precedes a long tail of response data.
var relayLinger = 60 * time.Second

type target struct{ addr string }

func targetAddr(r nft.Rule) string {
	return net.JoinHostPort(r.DestIP, strconv.Itoa(r.DestPort))
}

// setKeepAlive enables TCP keepalive on c when it is a TCP connection; it is a
// no-op for any other conn type (e.g. test pipes).
func setKeepAlive(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(keepAlivePeriod)
	}
}

// dialUpstream is the single entry point for opening an upstream leg, so every
// pooled or on-demand connection gets the same dial timeout and keepalive.
func dialUpstream(addr string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	setKeepAlive(c)
	return c, nil
}

// makeLimiter converts a Mbps cap into a byte/sec token-bucket limiter, or nil
// when unlimited. Burst must be >= the largest single WaitN call (one buffer).
func makeLimiter(mbps int) *rate.Limiter {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := float64(mbps) * 1e6 / 8.0
	burst := int(bytesPerSec)
	if burst < relayBufSize {
		burst = relayBufSize
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// meteredReader rate-limits and/or counts each Read. limPtr may hold nil
// (unlimited); counter may be nil (don't count this direction).
type meteredReader struct {
	src     io.Reader
	limPtr  *atomic.Pointer[rate.Limiter]
	counter *atomic.Int64
	ctx     context.Context
}

func (r *meteredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		if r.limPtr != nil {
			if lim := r.limPtr.Load(); lim != nil {
				if werr := lim.WaitN(r.ctx, n); werr != nil {
					return n, werr
				}
			}
		}
		if r.counter != nil {
			r.counter.Add(int64(n))
		}
	}
	return n, err
}

// relayCopy copies src->dst. When limPtr or counter is non-nil it wraps src so
// each chunk is paced/counted; otherwise it is a plain buffered copy.
func relayCopy(ctx context.Context, dst io.Writer, src io.Reader, limPtr *atomic.Pointer[rate.Limiter], counter *atomic.Int64) {
	var r io.Reader = src
	if limPtr != nil || counter != nil {
		r = &meteredReader{src: src, limPtr: limPtr, counter: counter, ctx: ctx}
	}
	buf := make([]byte, relayBufSize)
	_, _ = io.CopyBuffer(dst, r, buf)
}

// halfCloseWrite propagates a one-directional EOF so protocols that signal end
// of stream by closing one half keep working.
func halfCloseWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
