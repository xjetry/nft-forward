package forward

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"nft-forward/internal/nft"
)

type fakeKernel struct {
	err    error
	rules  []nft.Rule
	counts []Counter
}

func (k *fakeKernel) Reconcile(rules []nft.Rule) error {
	if k.err != nil {
		return k.err
	}
	k.rules = append([]nft.Rule(nil), rules...)
	return nil
}
func (k *fakeKernel) Counters() ([]Counter, error) { return k.counts, nil }

// newTestDataplane builds a Dataplane with an injectable kernel, a real
// userspace backend (loopback-safe), and a no-op firewall.
func newTestDataplane(k kernelReconciler) *Dataplane {
	return &Dataplane{kernel: k, userspace: newUserspaceBackend(), fw: firewall{shims: nil}}
}

func TestDataplane_KernelFailureRollsBackUserspace(t *testing.T) {
	dp := newTestDataplane(&fakeKernel{err: errors.New("nft boom")})
	defer dp.Close(context.Background())

	l, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	rules := []nft.Rule{{ID: "x", Proto: "tcp", SrcPort: port, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}}
	if err := dp.Reconcile(context.Background(), rules); err == nil {
		t.Fatal("expected kernel error to surface")
	}
	if n := len(dp.userspace.listeners); n != 0 {
		t.Fatalf("userspace not rolled back: %d listeners remain", n)
	}
	probe, err := net.Listen("tcp4", ":"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port not released after rollback: %v", err)
	}
	probe.Close()
}

func TestDataplane_CountersMerged(t *testing.T) {
	dp := newTestDataplane(&fakeKernel{counts: []Counter{{Proto: "udp", ListenPort: 53, Bytes: 100}}})
	defer dp.Close(context.Background())
	cs, err := dp.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Proto != "udp" {
		t.Fatalf("want merged kernel counter, got %+v", cs)
	}
}
