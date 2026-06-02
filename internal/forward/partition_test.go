package forward

import (
	"testing"

	"nft-forward/internal/nft"
)

func TestPartition_KernelPassthrough(t *testing.T) {
	in := []nft.Rule{
		{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
		{ID: "b", Proto: "udp", SrcPort: 53, DestIP: "10.0.0.2", DestPort: 53, Mode: nft.ModeKernel},
	}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 || len(u) != 0 {
		t.Fatalf("want 2 kernel / 0 userspace, got %d/%d", len(k), len(u))
	}
}

func TestPartition_UserspaceTCP(t *testing.T) {
	in := []nft.Rule{{ID: "a", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 0 || len(u) != 1 || u[0].Proto != "tcp" {
		t.Fatalf("want 0 kernel / 1 tcp userspace, got %d/%d", len(k), len(u))
	}
}

func TestPartition_TCPUDPUserspaceSplits(t *testing.T) {
	in := []nft.Rule{{ID: "a", Proto: "tcp+udp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 1 || k[0].Proto != "udp" || k[0].EffectiveMode() != nft.ModeKernel {
		t.Fatalf("want udp kernel half, got %+v", k)
	}
	if len(u) != 1 || u[0].Proto != "tcp" || u[0].EffectiveMode() != nft.ModeUserspace {
		t.Fatalf("want tcp userspace half, got %+v", u)
	}
}

func TestPartition_OverlapRejected(t *testing.T) {
	// tcp+udp kernel on 8443 occupies tcp/8443 AND udp/8443; a tcp userspace
	// rule on 8443 then collides on tcp/8443.
	in := []nft.Rule{
		{ID: "a", Proto: "tcp+udp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443},
		{ID: "b", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.2", DestPort: 443, Mode: nft.ModeUserspace},
	}
	if _, _, err := Partition(in); err == nil {
		t.Fatal("expected overlap error, got nil")
	}
}
