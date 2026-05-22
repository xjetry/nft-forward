package daemon

import (
	"context"
	"fmt"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// resolveFunc is the apply-time DNS resolver. Production points it at
// nft.ResolveHosts backed by a long-lived resolver.Resolver so positive
// answers are cached; tests inject deterministic fakes.
type resolveFunc func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error)

func defaultResolver(r *resolver.Resolver) resolveFunc {
	return func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
		return nft.ResolveHosts(ctx, rules, r)
	}
}

// requireResolvedHosts returns an error naming the first rule whose DestHost
// did not resolve to an IP. Callers reject the apply rather than silently
// pushing an unreachable rule into nftables.
func requireResolvedHosts(rules []nft.Rule) error {
	for _, r := range rules {
		if r.DestHost != "" && r.DestIP == "" {
			return fmt.Errorf("rule %s/%d: 无法解析目标域名 %s", r.Proto, r.SrcPort, r.DestHost)
		}
	}
	return nil
}
