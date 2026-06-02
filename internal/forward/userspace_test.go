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
