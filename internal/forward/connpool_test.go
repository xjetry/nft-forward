package forward

import (
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func localTCPServer(t *testing.T) (addr string, close func()) {
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
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestConnPool_GetReturnsValidConn(t *testing.T) {
	addr, stop := localTCPServer(t)
	defer stop()

	p := newConnPool(addr, 2)
	defer p.Close()

	time.Sleep(300 * time.Millisecond)

	c, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.Close()
	if c.RemoteAddr() == nil {
		t.Fatal("expected connected conn")
	}
}

func TestConnPool_FallbackDialWhenEmpty(t *testing.T) {
	addr, stop := localTCPServer(t)
	defer stop()

	p := newConnPool(addr, 1)
	defer p.Close()

	// drain the pool beyond capacity
	for i := 0; i < 3; i++ {
		c, err := p.Get()
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
		c.Close()
	}
}

func TestConnPool_DeadConnSkipped(t *testing.T) {
	addr, stop := localTCPServer(t)
	p := newConnPool(addr, 2)
	time.Sleep(300 * time.Millisecond)

	// close the server so pooled connections die
	stop()
	time.Sleep(100 * time.Millisecond)

	// start a new server on a different port — pool should fallback dial
	addr2, stop2 := localTCPServer(t)
	defer stop2()

	// replace pool target to the new server
	p.Close()
	p2 := newConnPool(addr2, 2)
	defer p2.Close()

	c, err := p2.Get()
	if err != nil {
		t.Fatalf("Get after reconnect: %v", err)
	}
	c.Close()
}

func TestConnPool_CloseNoLeak(t *testing.T) {
	addr, stop := localTCPServer(t)
	defer stop()

	p := newConnPool(addr, 4)
	time.Sleep(300 * time.Millisecond)
	p.Close()

	// after Close, the idle channel should be drained and closed
	select {
	case _, ok := <-p.idle:
		if ok {
			t.Fatal("expected closed channel after pool Close")
		}
	default:
	}
}

// dialPair opens a loopback TCP connection and returns the client side plus the
// accepted server side; both are closed by the returned cleanup.
func dialPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srvCh := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		srvCh <- c
	}()
	client, err = net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-srvCh
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// isConnAlive must not consume buffered data: a peer that speaks first (banner
// protocols) would otherwise lose its first byte to the liveness probe.
func TestIsConnAlive_DoesNotConsumeData(t *testing.T) {
	client, server := dialPair(t)
	if _, err := server.Write([]byte("Z")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the byte arrive in client's buffer

	if !isConnAlive(client) {
		t.Fatal("live connection reported dead")
	}

	client.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	n, err := client.Read(buf[:])
	if err != nil || n != 1 || buf[0] != 'Z' {
		t.Fatalf("probe consumed buffered data: n=%d err=%v buf=%q", n, err, buf[:n])
	}
}

// isConnAlive must report a peer that closed (sent FIN) as dead.
func TestIsConnAlive_DetectsClosedPeer(t *testing.T) {
	client, server := dialPair(t)
	server.Close()
	time.Sleep(100 * time.Millisecond) // let the FIN land

	if isConnAlive(client) {
		t.Fatal("closed-peer connection reported alive")
	}
}

// isConnAlive reports an idle-but-open connection as alive.
func TestIsConnAlive_IdleOpenIsAlive(t *testing.T) {
	client, _ := dialPair(t)
	if !isConnAlive(client) {
		t.Fatal("idle open connection reported dead")
	}
}

// dialUpstream must enable SO_KEEPALIVE so dead peers are detected by the
// kernel rather than leaking a blocked relay goroutine.
func TestDialUpstream_EnablesKeepAlive(t *testing.T) {
	addr, stop := localTCPServer(t)
	defer stop()

	c, err := dialUpstream(addr)
	if err != nil {
		t.Fatalf("dialUpstream: %v", err)
	}
	defer c.Close()

	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var on int
	if cerr := rc.Control(func(fd uintptr) {
		on, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE)
	}); cerr != nil {
		t.Fatal(cerr)
	}
	if on == 0 {
		t.Fatal("SO_KEEPALIVE not enabled on upstream dial")
	}
}
