package daemon

import (
	"nft-forward/internal/nft"
	"nft-forward/internal/tc"
)

// Applier writes a fully-resolved ruleset into the kernel data plane.
// Production daemons drive both nftables (packet forwarding) and tc
// (per-tunnel bandwidth shaping). Tests substitute fakes that record calls
// without requiring root.
type Applier interface {
	Apply(rules []nft.Rule, iface string) error
}

type nftApplier struct{}

func (nftApplier) Apply(rules []nft.Rule, iface string) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	// tc runs after nft so a stale class hierarchy never points at a
	// dest IP nft hasn't published yet. If tc fails the kernel keeps the
	// freshly-applied nft ruleset (traffic still forwards, only shaping
	// is missing); that's preferable to rolling nft back and dropping
	// packets.
	return tc.Apply(rules, iface)
}

// DefaultApplier returns the production applier.
func DefaultApplier() Applier { return nftApplier{} }
