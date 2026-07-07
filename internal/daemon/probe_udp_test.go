package daemon

import (
	"net"
	"testing"
)

// Echo server: reply must come back with real latency.
func TestDoProbeUDPEcho(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()

	ack := doProbe(pc.LocalAddr().String(), "udp")
	if !ack.OK {
		t.Fatalf("echo probe failed: %+v", ack)
	}
}

// Closed port: the kernel's ICMP port-unreachable surfaces as a hard failure,
// not a silent-OK.
func TestDoProbeUDPRefused(t *testing.T) {
	// Bind then close to get a port that is known-free right now.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()

	ack := doProbe(addr, "udp")
	if ack.OK {
		t.Fatalf("probe of closed port reported OK: %+v", ack)
	}
}

// Silent listener: no reply and no ICMP error is indeterminate and must be
// reported as OK (latency 0), otherwise healthy no-echo services show 不通.
func TestDoProbeUDPSilent(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close() // bound but never replies

	ack := doProbe(pc.LocalAddr().String(), "udp")
	if !ack.OK {
		t.Fatalf("silent listener reported fail: %+v", ack)
	}
	if ack.Latency != 0 {
		t.Fatalf("silent listener should report latency 0, got %d", ack.Latency)
	}
}
