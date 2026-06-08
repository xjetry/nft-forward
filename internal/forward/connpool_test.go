package forward

import (
	"io"
	"net"
	"testing"
	"time"
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
