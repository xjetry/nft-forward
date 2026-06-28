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
	type key struct {
		proto string
		port  int
	}
	m := map[key]*Counter{}
	for _, c := range cs {
		k := key{c.Proto, c.ListenPort}
		fc, ok := m[k]
		if !ok {
			fc = &Counter{Proto: c.Proto, ListenPort: c.ListenPort}
			m[k] = fc
		}
		switch c.Direction {
		case "original":
			fc.BytesUp = c.Bytes
		case "reply":
			fc.BytesDown = c.Bytes
		default:
			fc.BytesUp += c.Bytes
		}
		fc.Packets += c.Packets
	}
	out := make([]Counter, 0, len(m))
	for _, fc := range m {
		out = append(out, *fc)
	}
	return out, nil
}
