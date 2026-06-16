package server

import (
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

// ruleView is the per-rule row the list/detail API renders.
type ruleView struct {
	Rule        *db.Rule
	Path        string
	Entry       string
	EntryNodeID int64
	OwnerName   string
}

func (s *Server) buildRuleView(r *db.Rule) ruleView {
	hops, _ := db.ListRuleHops(s.DB, r.ID)
	names := make([]string, 0, len(hops)+1)
	for _, h := range hops {
		n, err := db.GetNode(s.DB, h.NodeID)
		if err == nil {
			names = append(names, n.Name)
		} else {
			names = append(names, fmt.Sprintf("#%d", h.NodeID))
		}
	}
	names = append(names, net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort)))
	entry := "—"
	var entryNodeID int64
	if len(hops) > 0 && r.EntryListenPort > 0 {
		entryNodeID = hops[0].NodeID
		if n, err := db.GetNode(s.DB, hops[0].NodeID); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(r.EntryListenPort))
		}
	}
	return ruleView{Rule: r, Path: strings.Join(names, " → "), Entry: entry, EntryNodeID: entryNodeID}
}

// ruleListItem is the JSON shape for rule-list endpoints. The embedded *db.Rule
// promotes the rule's own fields (id, node_id, name, proto, ...) to the top
// level so React list rows can read them flat alongside the computed view
// fields. A wrapped {"rule":{...}} shape would leave r.id undefined in the UI.
type ruleListItem struct {
	*db.Rule
	OwnerName   string `json:"owner_name"`
	Path        string `json:"path"`
	Entry       string `json:"entry"`
	EntryNodeID int64  `json:"entry_node_id"`
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
	return ruleListItem{Rule: r, OwnerName: ownerName, Path: v.Path, Entry: v.Entry, EntryNodeID: v.EntryNodeID}
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
