package daemon

import (
	"fmt"
	"sort"

	"nft-forward/internal/nft"
)

// 归属段的名字，即 OwnerRuleset 的键。一条规则从哪来由它所在的段决定，不由规则
// 自己的字段决定：
//
//   - segLocal：本地规则，由本机控制口建、改、删，以 nft.Rule.ID 标识；
//   - segPanel：面板下发的规则，只由 SetPanelRuleset 整段替换，以 nft.Rule.RuleID
//     （面板数据库里的规则 id）标识。
//
// 键的字面值就是 state.json 里 owners 对象的键，不能改：已部署主机上的 state.json
// 用的是这两个键；并且 install.sh 的 update 在健康检查不过时会换回旧版二进制
// （rollback_update），旧版要能读新版已经写下的 state.json。本地段叫 "tui" 是沿用
// 下来的名字——最初只有 TUI 写这一段。
const (
	segLocal = "tui"
	segPanel = "panel"
)

// portClaim 是一条规则在数据面上占的位置：(协议, 监听端口)。MergedRuleset 用它判定
// 两条规则是否相撞，dropLocalRulesClaimedByPanel 用它判定本地规则是否该让位——两处
// 必须是同一个键，让位的恰好就是否则会被判相撞的那些。
func portClaim(r nft.Rule) string {
	return fmt.Sprintf("%s/%d", r.Proto, r.SrcPort)
}

// dropLocalRulesClaimedByPanel 把 "tui" 段里与 "panel" 段占同一位置的规则去掉，返回
// 被去掉的那些。
//
// 归面板管的 daemon 上，一个位置归面板还是归本地以面板的下发为准。本地规则被面板
// 接管（Dialer.adoptLocalRules）就是靠它落地的：面板为本地规则建出的面板规则随下发
// 到达时，本地那一份在同一次写入里换成面板这一份，数据面上这个位置始终恰有一条
// 规则。面板对接管的应答（→ clearTuiSegment）与这次下发在线路上谁先到没有保证，
// 有了让位，两种顺序收敛到同一个结果；没有它，下发先到时 MergedRuleset 会因两段抢
// 同一位置拒绝整次下发，随后应答又把本地段清掉，规则就从数据面上消失了。
func dropLocalRulesClaimedByPanel(owners OwnerRuleset) []nft.Rule {
	if len(owners[segLocal]) == 0 || len(owners[segPanel]) == 0 {
		return nil
	}
	claimed := make(map[string]bool, len(owners[segPanel]))
	for _, r := range owners[segPanel] {
		claimed[portClaim(r)] = true
	}
	var kept, dropped []nft.Rule
	for _, r := range owners[segLocal] {
		if claimed[portClaim(r)] {
			dropped = append(dropped, r)
		} else {
			kept = append(kept, r)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	if len(kept) == 0 {
		delete(owners, segLocal)
	} else {
		owners[segLocal] = kept
	}
	return dropped
}

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
			key := portClaim(r)
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
