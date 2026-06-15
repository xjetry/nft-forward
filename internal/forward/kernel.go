package forward

import (
	"log"

	"nft-forward/internal/nft"
	"nft-forward/internal/tc"
)

// kernelReconciler is the kernel backend seam; the Dataplane test injects a
// fake (the real one shells nft/tc and needs root).
type kernelReconciler interface {
	Reconcile(rules []nft.Rule) error
	Counters() ([]Counter, error)
}

type kernelBackend struct {
	iface string
}

// Reconcile applies the atomic nftables ruleset, then rebuilds the tc HTB tree.
// nft is atomic (keeps the old table on failure); tc runs after so a stale
// class never points at an unpublished dest IP. A tc failure is logged but
// not propagated — forwarding continues unshaped rather than failing entirely.
func (k kernelBackend) Reconcile(rules []nft.Rule) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	if err := tc.Apply(rules, k.iface); err != nil {
		log.Printf("tc shaping degraded (forwarding unaffected): %v", err)
	}
	return nil
}

func (k kernelBackend) Counters() ([]Counter, error) {
	cs, err := nft.Counters()
	if err != nil {
		return nil, err
	}
	out := make([]Counter, 0, len(cs))
	for _, c := range cs {
		out = append(out, Counter{Proto: c.Proto, ListenPort: c.ListenPort, Bytes: c.Bytes, Packets: c.Packets})
	}
	return out, nil
}
