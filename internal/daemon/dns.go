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

// carryResolvedTargets 返回 rules 的一份拷贝，其中还没有地址的域名规则填上 live 里
// 同一域名已解析出的地址；live 里没有该域名时保持为空。
//
// live 是上一次真正下到数据面的规则集（d.lastResolved，由 reconcileOwners 在提交时
// 写入），其中域名规则的 DestIP 就是该域名此刻生效的地址。按域名而不是按规则匹配：
// 地址是域名的属性，规则的目标一旦改成别的域名，就不会取到旧域名的地址。
//
// 返回拷贝而不就地修改 rules：传进来的规则取自内存里的规则集（d.owners），它保存的是用户或面板
// 给出的规则原样（域名规则不带地址），落盘到 state.json 的也是它；解析出的地址只
// 进 d.lastResolved 与数据面。
func carryResolvedTargets(rules, live []nft.Rule) []nft.Rule {
	known := make(map[string]string, len(live))
	for _, r := range live {
		if r.DestHost != "" && r.DestIP != "" {
			known[r.DestHost] = r.DestIP
		}
	}
	out := make([]nft.Rule, len(rules))
	copy(out, rules)
	for i := range out {
		if out[i].DestHost != "" && out[i].DestIP == "" {
			out[i].DestIP = known[out[i].DestHost]
		}
	}
	return out
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
