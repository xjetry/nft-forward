package daemon

import "nft-forward/internal/nft"

// Applier hides the concrete nftables call so the daemon can be
// exercised in unit tests without root or a real kernel ruleset.
type Applier interface {
	Apply(rules []nft.Rule) error
}

type nftApplier struct{}

func (nftApplier) Apply(rules []nft.Rule) error { return nft.Apply(rules) }

// DefaultApplier returns the production Applier backed by internal/nft.
func DefaultApplier() Applier { return nftApplier{} }
