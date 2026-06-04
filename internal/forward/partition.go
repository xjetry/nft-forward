package forward

import (
	"fmt"

	"nft-forward/internal/nft"
)

// Partition splits resolved rules into the kernel and userspace rule sets.
// A tcp+udp userspace rule is split into a udp kernel rule and a tcp userspace
// rule (same target/bandwidth). It returns an error when two rules' effective
// (proto, port) tuples overlap — treating tcp+udp as occupying both tcp/port
// and udp/port — which also catches the latent tcp+udp-vs-tcp ambiguity that
// the owner-level merge (keyed by the literal proto string) cannot see.
//
// Callers pass already-resolved, already-Validated rules; a stray
// udp+userspace rule (which Validate rejects) is handled defensively.
func Partition(rules []nft.Rule) (kernel, userspace []nft.Rule, err error) {
	claimed := map[string]string{} // "tcp/8443" -> who claimed it

	claim := func(proto string, port int, who string) error {
		protos := []string{proto}
		if proto == "tcp+udp" {
			protos = []string{"tcp", "udp"}
		}
		for _, p := range protos {
			key := fmt.Sprintf("%s/%d", p, port)
			if prev, dup := claimed[key]; dup {
				return fmt.Errorf("port %s claimed by both %s and %s", key, prev, who)
			}
			claimed[key] = who
		}
		return nil
	}

	for _, r := range rules {
		who := fmt.Sprintf("rule %s (%s/%d, %s)", r.ID, r.Proto, r.SrcPort, r.EffectiveMode())
		if r.EffectiveMode() == nft.ModeKernel {
			if cerr := claim(r.Proto, r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			kernel = append(kernel, r)
			continue
		}
		switch r.Proto {
		case "tcp":
			if cerr := claim("tcp", r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			userspace = append(userspace, r)
		case "tcp+udp":
			if cerr := claim("tcp+udp", r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			udp := r
			udp.Proto = "udp"
			udp.Mode = nft.ModeKernel
			kernel = append(kernel, udp)
			tcp := r
			tcp.Proto = "tcp"
			tcp.Mode = nft.ModeUserspace
			userspace = append(userspace, tcp)
		default:
			return nil, nil, fmt.Errorf("rule %s: proto %s is not supported in userspace mode", r.ID, r.Proto)
		}
	}
	return kernel, userspace, nil
}
