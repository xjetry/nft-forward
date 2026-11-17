package server

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"nft-forward/internal/db"
	"nft-forward/internal/landing"
)

// ruleView is the per-rule row the list/detail API renders.
type ruleView struct {
	Rule        *db.Rule
	Entry       string
	EntryV6     string
	Exit        string
	EntryNodeID int64
	EntryMode   string
	ExitMode    string
	OwnerName   string
}

func (s *Server) buildRuleView(r *db.Rule) ruleView {
	hops, _ := db.ListRuleHops(s.DB, r.ID)
	exit := net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort))
	entry, entryV6 := "—", ""
	var entryNodeID int64
	entryMode, exitMode := "", ""
	if len(hops) > 0 {
		entryMode = hops[0].Mode
		exitMode = hops[len(hops)-1].Mode
	}
	if len(hops) > 0 && r.EntryListenPort > 0 {
		entryNodeID = hops[0].NodeID
		if n, err := db.GetNode(s.DB, hops[0].NodeID); err == nil && n.RelayHost != "" {
			// EntryAddresses returns "" for a family whose relay address the
			// node no longer carries; keep the "—" placeholder instead of
			// rendering an empty host.
			if e, e6 := db.EntryAddresses(r.EntryFamily, n.RelayHost, n.RelayHostV6, r.EntryListenPort); e != "" {
				entry, entryV6 = e, e6
			}
		}
	}
	return ruleView{Rule: r, Entry: entry, EntryV6: entryV6, Exit: exit, EntryNodeID: entryNodeID, EntryMode: entryMode, ExitMode: exitMode}
}

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
	// addresses in buildRuleView.
	EntryV6     string `json:"entry_v6,omitempty"`
	Exit        string `json:"exit"`
	EntryNodeID int64  `json:"entry_node_id"`
	// EntryMode is the first hop's forwarding mode. ExitMode is the last
	// hop's — the exit segment the rule owns — and prefills the edit form's
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

// markSuppressed 给每个规则项填上它此刻不转发的原因；照常转发的规则留空。规则的
// 每个对外面（管理员与用户的列表、详情，/api/v1）装配完都要调它，
// TestEveryRuleSurfaceReportsTheSuppression 逐面对照。
func (s *Server) markSuppressed(items []ruleListItem) {
	reasons, err := db.RuleSuppressions(s.DB)
	if err != nil {
		log.Printf("rules: load suppressions: %v", err)
		return
	}
	for i := range items {
		items[i].Suppressed = reasons[items[i].ID]
	}
}

// chainNode is one physical hop in a rule's flattened chain, resolved to its
// node name and type for display.
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

func (s *Server) buildRuleListItem(r *db.Rule, ownerName string) ruleListItem {
	v := s.buildRuleView(r)
	return ruleListItem{Rule: r, OwnerName: ownerName, Entry: v.Entry, EntryV6: v.EntryV6, Exit: v.Exit, EntryNodeID: v.EntryNodeID, EntryMode: v.EntryMode, ExitMode: v.ExitMode}
}

// fillRuleChains attaches the flattened physical chain to each item, resolving
// every hop's node id to its display name/type via nodesByID. A hop whose node
// can't be resolved is dropped rather than shown as a bare id. nodesByID should
// span all nodes (composite children included), not just the caller's granted
// subset, so a composite's members still resolve.
func (s *Server) fillRuleChains(items []ruleListItem, nodesByID map[int64]*db.Node) {
	if len(items) == 0 {
		return
	}
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	chains, err := db.RuleChainNodeIDs(s.DB, ids)
	if err != nil {
		return
	}
	for i := range items {
		hopIDs := chains[items[i].ID]
		chain := make([]chainNode, 0, len(hopIDs))
		for _, nid := range hopIDs {
			if n := nodesByID[nid]; n != nil {
				chain = append(chain, chainNode{NodeID: nid, Name: n.Name, NodeType: n.NodeType})
			}
		}
		items[i].Chain = chain
	}
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

func validateCIDRList(s string) error {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(part); err != nil {
			return fmt.Errorf("%q: %v", part, err)
		}
	}
	return nil
}

func cidrAllowsAll(list string) bool {
	list = strings.TrimSpace(list)
	if list == "" {
		return true
	}
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

func targetIPInCIDR(ip net.IP, list string) bool {
	if cidrAllowsAll(list) {
		return true
	}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			continue
		}
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
