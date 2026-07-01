package server

import (
	"database/sql"
	"fmt"
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
	Exit        string
	EntryNodeID int64
	OwnerName   string
}

func (s *Server) buildRuleView(r *db.Rule) ruleView {
	hops, _ := db.ListRuleHops(s.DB, r.ID)
	exit := net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort))
	entry := "—"
	var entryNodeID int64
	if len(hops) > 0 && r.EntryListenPort > 0 {
		entryNodeID = hops[0].NodeID
		if n, err := db.GetNode(s.DB, hops[0].NodeID); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(r.EntryListenPort))
		}
	}
	return ruleView{Rule: r, Entry: entry, Exit: exit, EntryNodeID: entryNodeID}
}

// ruleListItem is the JSON shape for rule-list endpoints. The embedded *db.Rule
// promotes the rule's own fields (id, node_id, name, proto, ...) to the top
// level so React list rows can read them flat alongside the computed view
// fields. A wrapped {"rule":{...}} shape would leave r.id undefined in the UI.
type ruleListItem struct {
	*db.Rule
	OwnerName   string `json:"owner_name"`
	Entry       string `json:"entry"`
	Exit        string `json:"exit"`
	EntryNodeID int64  `json:"entry_node_id"`
	// ExitKind is "landing" when the exit host:port matches one of the owner's
	// admin-assigned landing nodes, else "custom". LandingURI is the original
	// (direct) proxy URI; RelayURI is that URI with its host:port rewritten to
	// the rule's entry endpoint, so a client dials the relay instead of the
	// landing directly. RelayURI is populated only where the copy action is
	// offered (detail and the user's own list). Matches against the user's own
	// browser-local URIs happen client-side, not here.
	ExitKind       string  `json:"exit_kind"`
	LandingName    string  `json:"landing_name,omitempty"`
	LandingURI     string  `json:"landing_uri,omitempty"`
	RelayURI       string  `json:"relay_uri,omitempty"`
	RateMultiplier float64 `json:"rate_multiplier"`
	BillingRate    float64 `json:"billing_rate"`
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
	return ruleListItem{Rule: r, OwnerName: ownerName, Entry: v.Entry, Exit: v.Exit, EntryNodeID: v.EntryNodeID}
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

// validRuleProto reports whether proto is an accepted forward protocol. tcp+udp
// is accepted: the data plane splits it into a udp kernel DNAT plus a tcp
// userspace relay when the hop runs in userspace mode (see forward.Partition).
func validRuleProto(proto string) bool {
	switch proto {
	case "tcp", "udp", "tcp+udp":
		return true
	default:
		return false
	}
}

func parseExit(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("出口需为 host:port 形式")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("出口端口非法")
	}
	if host == "" {
		return "", 0, fmt.Errorf("出口地址不能为空")
	}
	return host, port, nil
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

// checkUserRuleQuota verifies a user hasn't exceeded their global max_forwards
// limit or per-node grant limits.
func (s *Server) checkUserRuleQuota(u *db.User, hopCount int, existingRuleHops int) error {
	total, _ := db.CountRulesForUser(s.DB, u.ID)
	if (total-existingRuleHops)+hopCount > u.MaxForwards {
		return fmt.Errorf("超出用户最大转发数（%d）", u.MaxForwards)
	}
	return nil
}

// regenerateRuleByID loads a rule and its hops, then calls RegenerateRule
// inside a transaction. Returns the set of affected node IDs.
func (s *Server) regenerateRuleByID(ruleID int64) ([]int64, error) {
	r, err := db.GetRule(s.DB, ruleID)
	if err != nil {
		return nil, err
	}
	hops, err := db.ListRuleHops(s.DB, ruleID)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return nil, nil
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateRule(tx, r, inputs, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}
