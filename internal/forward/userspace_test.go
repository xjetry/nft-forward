package forward

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

// freePort grabs an ephemeral port number, then releases it so the relay can
// bind it. Small TOCTOU window, acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// echoServer accepts connections and echoes everything back.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestUserspace_LoopbackEchoAndCounter(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	listen := freePort(t)
	b := newUserspaceBackend()
	defer b.Close()

	rule := nft.Rule{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace}
	if err := b.Reconcile([]nft.Rule{rule}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello-relay")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: got %q", buf)
	}

	deadline := time.Now().Add(time.Second)
	for {
		cs := b.Counters()
		if len(cs) == 1 && cs[0].ListenPort == listen && cs[0].Bytes >= int64(len(msg)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("counter not updated: %+v", cs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserspace_ReconcileAddRemove(t *testing.T) {
	b := newUserspaceBackend()
	defer b.Close()

	p1, p2 := freePort(t), freePort(t)
	r1 := nft.Rule{ID: "1", Proto: "tcp", SrcPort: p1, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}
	r2 := nft.Rule{ID: "2", Proto: "tcp", SrcPort: p2, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}

	if err := b.Reconcile([]nft.Rule{r1, r2}); err != nil {
		t.Fatal(err)
	}
	if len(b.listeners) != 2 {
		t.Fatalf("want 2 listeners, got %d", len(b.listeners))
	}
	if err := b.Reconcile([]nft.Rule{r2}); err != nil {
		t.Fatal(err)
	}
	if len(b.listeners) != 1 {
		t.Fatalf("want 1 listener after removal, got %d", len(b.listeners))
	}
	if _, ok := b.listeners[p2]; !ok {
		t.Fatalf("listener %d should remain", p2)
	}
	probe, err := net.Listen("tcp4", fmt.Sprintf(":%d", p1))
	if err != nil {
		t.Fatalf("port %d not released: %v", p1, err)
	}
	probe.Close()
}

func TestUserspace_TargetHotUpdate(t *testing.T) {
	a, stopA := echoServer(t)
	defer stopA()
	b2, stopB := echoServer(t)
	defer stopB()

	listen := freePort(t)
	be := newUserspaceBackend()
	defer be.Close()

	ahost, aps, _ := net.SplitHostPort(a)
	ap, _ := strconv.Atoi(aps)
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: ahost, DestPort: ap, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatal(err)
	}
	bhost, bps, _ := net.SplitHostPort(b2)
	bp, _ := strconv.Atoi(bps)
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: bhost, DestPort: bp, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo after retarget: %v", err)
	}
}

// A bandwidth change must hot-update the existing listener's limiter in place,
// never tear down and re-bind the socket (which would drop live connections).
func TestUserspace_BandwidthHotUpdateNoRestart(t *testing.T) {
	be := newUserspaceBackend()
	defer be.Close()

	listen := freePort(t)
	base := nft.Rule{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}

	unlimited := base
	unlimited.BandwidthMbps = 0
	if err := be.Reconcile([]nft.Rule{unlimited}); err != nil {
		t.Fatalf("reconcile unlimited: %v", err)
	}
	before := be.listeners[listen]
	if before == nil {
		t.Fatalf("listener %d not created", listen)
	}
	if before.lim.Load() != nil {
		t.Fatalf("unlimited rule should have nil limiter")
	}

	limited := base
	limited.BandwidthMbps = 8
	if err := be.Reconcile([]nft.Rule{limited}); err != nil {
		t.Fatalf("reconcile limited: %v", err)
	}
	after := be.listeners[listen]
	if after != before {
		t.Fatalf("listener was restarted on bandwidth change (before=%p after=%p)", before, after)
	}
	if after.lim.Load() == nil {
		t.Fatalf("limiter not installed after bandwidth update")
	}
}

// Close must force-close in-flight connections (so the peer observes EOF) and
// wait for every relay goroutine to exit, leaving no listeners behind.
func TestUserspace_CloseTearsDownInflight(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	listen := freePort(t)
	be := newUserspaceBackend()
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()
	// Round-trip a byte so the relay has an established upstream connection in
	// flight before we tear it down.
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	ack := make([]byte, 1)
	if _, err := io.ReadFull(conn, ack); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	done := make(chan struct{})
	go func() { be.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return promptly (goroutines not torn down)")
	}

	if len(be.listeners) != 0 {
		t.Fatalf("listeners not cleared after Close: %d", len(be.listeners))
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(ack); err == nil {
		t.Fatal("expected client connection to be closed after Close")
	}
}

// A bind conflict in the middle of a Reconcile must roll back every listener
// opened in that call (make-before-break), leaving the prior set — here empty —
// intact, and freeing the port it managed to open.
func TestUserspace_BindConflictRollback(t *testing.T) {
	be := newUserspaceBackend()
	defer be.Close()

	taken := freePort(t)
	blocker, err := net.Listen("tcp4", fmt.Sprintf(":%d", taken))
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer blocker.Close()

	free := freePort(t)
	rules := []nft.Rule{
		{ID: "ok", Proto: "tcp", SrcPort: free, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace},
		{ID: "bad", Proto: "tcp", SrcPort: taken, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace},
	}
	if err := be.Reconcile(rules); err == nil {
		t.Fatal("expected Reconcile to fail on bind conflict")
	}
	if len(be.listeners) != 0 {
		t.Fatalf("rollback incomplete, listeners=%d", len(be.listeners))
	}
	// The port the failed Reconcile opened for the good rule must be released.
	probe, err := net.Listen("tcp4", fmt.Sprintf(":%d", free))
	if err != nil {
		t.Fatalf("port %d not rolled back: %v", free, err)
	}
	probe.Close()
}

// A bandwidth cap must actually pace the forwarded stream. We send enough data
// that the time spent waiting on the limiter dominates the initial burst, then
// assert the elapsed time with wide margins to stay reliable on slow CI.
func TestUserspace_RateLimitPaces(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	listen := freePort(t)
	be := newUserspaceBackend()
	defer be.Close()

	// 8 Mbps == 1 MB/s, with a 1 MB token-bucket burst.
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: host, DestPort: upPort, BandwidthMbps: 8, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	const total = 3 << 20 // 3 MB: ~2s of pacing once the 1 MB burst is spent.
	payload := make([]byte, total)

	start := time.Now()
	go func() {
		if _, werr := conn.Write(payload); werr != nil {
			t.Logf("write ended: %v", werr)
		}
	}()
	if _, err := io.ReadFull(conn, make([]byte, total)); err != nil {
		t.Fatalf("read back: %v", err)
	}
	elapsed := time.Since(start)

	// Expected ~2s (burst 1 MB then 2 MB at 1 MB/s); assert a loose lower
	// bound well below that to absorb burst + socket buffering, and a generous
	// upper bound so a wedge still fails.
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("transfer too fast for the cap: %v (rate limiter not pacing)", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("transfer absurdly slow: %v", elapsed)
	}
}
