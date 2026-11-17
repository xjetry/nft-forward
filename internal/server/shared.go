package server

import (
	"database/sql"
	"net"
	"strconv"

	"nft-forward/internal/db"
	"nft-forward/internal/landing"
)

// ruleListItem is the JSON shape for rule-list endpoints. The embedded *db.Rule
// promotes the rule's own fields (id, node_id, name, proto, ...) to the top
// level so React list rows can read them flat alongside the computed view
// fields. A wrapped {"rule":{...}} shape would leave r.id undefined in the UI.
type ruleListItem struct {
	*db.Rule
	OwnerName string `json:"owner_name"`
	Entry     string `json:"entry"`
	// EntryV6 is the rule's secondary entry address, populated only when
	// entry_family is "both"; computed from the entry node's current relay
	// addresses in ruleItems.
	EntryV6     string `json:"entry_v6,omitempty"`
	Exit        string `json:"exit"`
	EntryNodeID int64  `json:"entry_node_id"`
	// EntryMode is the first hop's forwarding mode. ExitMode is the last
	// hop's — the exit leg, which the rule owns — and prefills the edit form's
	// kernel/userspace picker; on single-node rules the two coincide.
	EntryMode string `json:"entry_mode"`
	ExitMode  string `json:"exit_mode"`
	// ExitKind is "landing" when the exit host:port matches one of the owner's
	// admin-assigned landing nodes, else "custom". LandingURI is the original
	// (direct) proxy URI; RelayURI is that URI with its host:port rewritten to
	// the rule's entry endpoint, so a client dials the relay instead of the
	// landing directly. RelayURI is populated only where the copy action is
	// offered (detail and the user's own list). Matches against the user's own
	// browser-local URIs happen client-side, not here.
	ExitKind        string  `json:"exit_kind"`
	LandingName     string  `json:"landing_name,omitempty"`
	LandingProtocol string  `json:"landing_protocol,omitempty"`
	LandingURI      string  `json:"landing_uri,omitempty"`
	RelayURI        string  `json:"relay_uri,omitempty"`
	RateMultiplier  float64 `json:"rate_multiplier"`
	BillingRate     float64 `json:"billing_rate"`
	// Chain is the flattened physical path (entry → the hop that dials the
	// target, target excluded), with composite segments already expanded into
	// their member nodes. Sourced from rule_hops so it reflects what is actually
	// deployed, not a composite's current definition. Empty until the rule's
	// first regeneration.
	Chain []chainNode `json:"chain,omitempty"`
	// Suppressed 非空表示这条规则此刻不转发，值是原因。由 markSuppressed 填，与决定
	// 下发的是同一个判定（db.RuleSuppressions 与 db.ActiveRuleHopsForPush 共用一份
	// 压制条件）。
	Suppressed db.SuppressReason `json:"suppressed,omitempty"`
}

// ruleItems 装配规则的完整对外形状：管理员与用户的规则列表、规则详情从这里取，
// /api/v1 经 toV1Rule 再收窄。每一项一次给全——规则自身的列（含加权流量，它是规则
// 行上的一列，随行读出）、入口地址与两端模式、所有者名、出口归类、倍率、展平后的
// 物理链、压制原因。
//
// 另有三处响应直接给规则行（db.Rule 自己的列，没有上面这些算出来的字段）：管理员的
// 用户详情、用户仪表盘（/api/my）、规则写接口的回显。
//
// cat 是同一次请求里装配节点视图用的那份目录：入口地址按入口那台机器此刻的中继地址
// 算，链路上每一跳的名字从目录里解析（链路会经过用户没被授权的组合成员，所以必须是
// 全量节点，LoadNodeCatalog 保证）。倍率取规则入口节点（rules.node_id）的
// rate_multiplier，计费读的是同一列（accounting.LoadBook）。relayURI 决定是否算出
// 可复制的代理 URI：只在提供复制操作的面上算（管理员详情、用户的列表与详情），
// 管理员列表只显示出口类型。
//
// 装配要读的任何一张表读不出来就返回错误，不给出缺了那一部分的形状：没有所有者名、
// 没有链路、没有压制原因的规则项，读的人分不清是“这条规则没有”还是“没读到”。
func (s *Server) ruleItems(rules []*db.Rule, cat *db.NodeCatalog, relayURI bool) ([]ruleListItem, error) {
	owners, err := db.UsersByID(s.DB)
	if err != nil {
		return nil, err
	}
	landingByOwner := map[int64]map[string]landing.Node{}
	ids := make([]int64, len(rules))
	items := make([]ruleListItem, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
		it := ruleListItem{Rule: r, BillingRate: 1, RateMultiplier: 1}
		if err := it.fillEndpoints(s.DB, cat); err != nil {
			return nil, err
		}
		var idx map[string]landing.Node
		if r.OwnerID.Valid {
			if u := owners[r.OwnerID.Int64]; u != nil {
				it.OwnerName = u.Username
				if u.BillingRate > 0 {
					it.BillingRate = u.BillingRate
				}
			}
			var ok bool
			if idx, ok = landingByOwner[r.OwnerID.Int64]; !ok {
				if idx, err = s.landingIndexFromDB(r.OwnerID.Int64); err != nil {
					return nil, err
				}
				landingByOwner[r.OwnerID.Int64] = idx
			}
		}
		it.classifyExit(idx, relayURI)
		if n := cat.Node(r.NodeID); n != nil {
			it.RateMultiplier = n.RateMultiplier
		}
		items[i] = it
	}
	chains, err := db.RuleChainNodeIDs(s.DB, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		hopIDs := chains[items[i].ID]
		path := make([]chainNode, 0, len(hopIDs))
		for _, nid := range hopIDs {
			if n := cat.Node(nid); n != nil {
				path = append(path, chainNode{NodeID: nid, Name: n.Name, NodeType: n.NodeType})
			}
		}
		items[i].Chain = path
	}
	if err := s.markSuppressed(items); err != nil {
		return nil, err
	}
	return items, nil
}

// fillEndpoints 填入口地址与两端的转发模式，取自规则落库的物理跳。入口地址按入口
// 那台机器此刻的中继地址拼出；规则还没生成过物理跳、或入口机器已没有对应地址族的
// 中继地址时，入口保持占位符“—”。
func (it *ruleListItem) fillEndpoints(d *sql.DB, cat *db.NodeCatalog) error {
	r := it.Rule
	it.Exit = net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort))
	it.Entry = "—"
	hops, err := db.ListRuleHops(d, r.ID)
	if err != nil {
		return err
	}
	if len(hops) == 0 {
		return nil
	}
	it.EntryMode, it.ExitMode = hops[0].Mode, hops[len(hops)-1].Mode
	if r.EntryListenPort <= 0 {
		return nil
	}
	it.EntryNodeID = hops[0].NodeID
	if n := cat.Node(hops[0].NodeID); n != nil && n.RelayHost != "" {
		if e, e6 := db.EntryAddresses(r.EntryFamily, n.RelayHost, n.RelayHostV6, r.EntryListenPort); e != "" {
			it.Entry, it.EntryV6 = e, e6
		}
	}
	return nil
}

// markSuppressed 给每个规则项填上它此刻不转发的原因；照常转发的规则留空。只由
// ruleItems 调用，所以规则的每个对外面都带着它；TestEveryRuleSurfaceReportsTheSuppression
// 逐面对照。
func (s *Server) markSuppressed(items []ruleListItem) error {
	reasons, err := db.RuleSuppressions(s.DB)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Suppressed = reasons[items[i].ID]
	}
	return nil
}

// chainNode 是规则展平后的物理链上的一跳，带着节点名与类型供展示。/api 的规则
// 形状与 /api/v1 的规则形状（v1Rule.Chain、建规则的 dry-run 预览）用的是同一个类型。
type chainNode struct {
	NodeID   int64  `json:"node_id"`
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
}

// nodeHopView adds the resolved child node name to a composite node's hop so
// the UI shows names instead of bare ids. The embedded *db.NodeHop promotes its
// own fields (node_id, position, hop_node_id, mode) to the top level.
type nodeHopView struct {
	*db.NodeHop
	NodeName string `json:"node_name"`
}

// classifyExit fills the exit-kind / proxy-URI fields. idx maps "host:port" to
// the owner's landing nodes; withURI controls whether the copyable relay URI is
// computed (skipped for the admin list, which only shows the kind badge).
func (it *ruleListItem) classifyExit(idx map[string]landing.Node, withURI bool) {
	it.ExitKind = "custom"
	relayHost, relayPort, entryOK := splitEntry(it.Entry)
	if node, ok := idx[it.Exit]; ok {
		it.ExitKind = "landing"
		it.LandingName = node.Name
		it.LandingProtocol = node.Protocol
		it.LandingURI = node.URI
		if withURI && entryOK {
			if u, err := landing.RewriteEndpoint(node.URI, relayHost, relayPort); err == nil {
				it.RelayURI = u
			}
		}
	}
}

// splitEntry parses a "host:port" entry string; entry is "—" before the rule's
// first regeneration, which fails the split and reports ok=false.
func splitEntry(entry string) (host string, port int, ok bool) {
	h, p, err := net.SplitHostPort(entry)
	if err != nil {
		return "", 0, false
	}
	pp, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, false
	}
	return h, pp, true
}
