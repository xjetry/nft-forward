package daemon

import (
	"fmt"
	"sort"

	"nft-forward/internal/nft"
)

// MergedRuleset flattens all owner segments into a single ruleset suitable
// for nft.Apply. Owners are sorted by name so the output is deterministic
// across runs — nft.Apply diffs against the kernel and a flapping order
// would cause unnecessary replace cycles.
//
// A (proto, src_port) collision either within one owner or across owners
// is rejected with an error that names the port and the colliding owners,
// so the rejected client can surface a clear message to the user.
func MergedRuleset(owners OwnerRuleset) ([]nft.Rule, error) {
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	sort.Strings(names)

	type holder struct {
		owner string
		rule  nft.Rule
	}
	// (proto, src_port) → first owner that took it.
	seen := make(map[string]holder)
	merged := make([]nft.Rule, 0)

	for _, name := range names {
		for _, r := range owners[name] {
			key := fmt.Sprintf("%s/%d", r.Proto, r.SrcPort)
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf(
					"port %s already claimed by owner %q (rule %q); rejecting owner %q (rule %q)",
					key, prev.owner, prev.rule.ID, name, r.ID,
				)
			}
			seen[key] = holder{owner: name, rule: r}
			merged = append(merged, r)
		}
	}
	return merged, nil
}
