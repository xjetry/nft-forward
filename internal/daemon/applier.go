package daemon

import (
	"log"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
	"nft-forward/internal/tc"
)

// Applier writes a fully-resolved ruleset into the kernel data plane.
// Production daemons drive nft (DNAT/MASQUERADE), tc (rate limit), and
// firewall-tool compatibility shims. Tests substitute fakes that record
// calls without requiring root.
type Applier interface {
	Apply(rules []nft.Rule, iface string) error
	// Cleanup is called when the daemon shuts down so any owner-tagged
	// rules injected into foreign chains (DOCKER-USER, ufw-user-forward,
	// ...) get removed. Safe to call multiple times.
	Cleanup() error
}

type nftApplier struct {
	shims *shim.Registry
}

func (a nftApplier) Apply(rules []nft.Rule, iface string) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	if a.shims != nil {
		if err := a.shims.SyncAll(rules); err != nil {
			// shim failure is non-fatal: core nft_forward table already
			// applied. Surface as a log line for ops visibility.
			log.Printf("shim sync: %v", err)
		}
	}
	// tc runs after nft so a stale class hierarchy never points at a
	// dest IP nft hasn't published yet. If tc fails the kernel keeps the
	// freshly-applied nft ruleset (traffic still forwards, only shaping
	// is missing); that's preferable to rolling nft back and dropping
	// packets.
	return tc.Apply(rules, iface)
}

func (a nftApplier) Cleanup() error {
	if a.shims == nil {
		return nil
	}
	return a.shims.CleanupAll()
}

// DetectedShims returns the names of shims that detect their target
// chain right now. Used by daemon's startup probe to decide whether
// to warn about FORWARD policy=drop environments.
func (a nftApplier) DetectedShims() []string {
	if a.shims == nil {
		return nil
	}
	return a.shims.DetectedNames()
}

// DefaultApplier returns the production applier wired with the built-in
// shim registry.
func DefaultApplier() Applier {
	return nftApplier{shims: shim.DefaultRegistry()}
}
