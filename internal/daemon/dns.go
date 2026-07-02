package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// resolveFunc is the apply-time DNS resolver. Production points it at
// nft.ResolveHosts backed by a long-lived resolver.Resolver so positive
// answers are cached; tests inject deterministic fakes.
// The bool return signals whether any DestIP in the returned slice differs from
// the input — meaningful only when err == nil. The apply path discards it;
// the refresh loop uses it to skip a no-op Apply.
type resolveFunc func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error)

func defaultResolver(r *resolver.Resolver) resolveFunc {
	return func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
		return nft.ResolveHosts(ctx, rules, r)
	}
}

// partitionResolved splits rules into those safe to apply now (a literal IP,
// or a hostname that already resolved to an IP) and those still unresolved (a
// hostname with no IP yet). Unresolved rules are held back so one bad target
// can never block the rest; the refresh loop retries them, and callers that
// care surface them (e.g. as a node-level warning).
func partitionResolved(rules []nft.Rule) (applyable, unresolved []nft.Rule) {
	for _, r := range rules {
		if r.DestHost == "" || r.DestIP != "" {
			applyable = append(applyable, r)
		} else {
			unresolved = append(unresolved, r)
		}
	}
	return
}

// refreshOnce performs a single DNS refresh pass: re-resolve and re-apply
// only when at least one IP changed. The last-applied set is held in
// d.lastResolved so subsequent passes can detect "nothing moved" without
// an extra system call.
func (d *Daemon) refreshOnce(ctx context.Context) error {
	_, _, _, err := d.reconcileOwners(ctx, nil, nil, false)
	if err != nil {
		return fmt.Errorf("dns refresh: %w", err)
	}
	return nil
}

func rulesDiffer(a, b []nft.Rule) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// dnsInterval honours NFT_FORWARD_DNS_INTERVAL for parity with the previous
// agent loop. When the env var is set to a zero or invalid duration the loop
// is disabled; when unset, the default is 60 s.
func dnsInterval() time.Duration {
	if s := os.Getenv("NFT_FORWARD_DNS_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}
