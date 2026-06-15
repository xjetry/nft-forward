package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
)

// chainView is the per-chain row the list/detail API renders.
type chainView struct {
	Chain       *db.Chain
	Path        string
	Entry       string
	EntryNodeID int64
	OwnerName   string
}

func (s *Server) buildChainView(c *db.Chain) chainView {
	hops, _ := db.ListChainHops(s.DB, c.ID)
	names := make([]string, 0, len(hops)+1)
	for _, h := range hops {
		n, err := db.GetNode(s.DB, h.NodeID)
		if err == nil {
			names = append(names, n.Name)
		} else {
			names = append(names, fmt.Sprintf("#%d", h.NodeID))
		}
	}
	names = append(names, net.JoinHostPort(c.ExitHost, strconv.Itoa(c.ExitPort)))
	entry := "—"
	if c.EntryNodeID.Valid && c.EntryListenPort > 0 {
		if n, err := db.GetNode(s.DB, c.EntryNodeID.Int64); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(c.EntryListenPort))
		}
	}
	var entryNodeID int64
	if c.EntryNodeID.Valid {
		entryNodeID = c.EntryNodeID.Int64
	}
	return chainView{Chain: c, Path: strings.Join(names, " → "), Entry: entry, EntryNodeID: entryNodeID}
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
	if net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		return "", 0, fmt.Errorf("出口地址格式非法")
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

func validateAgainstTunnel(t *db.Tunnel, proto string, listenPort int, target string, targetPort int) error {
	switch proto {
	case "tcp", "udp":
	default:
		return errors.New("协议必须为 tcp 或 udp")
	}
	if t.ProtoMask != "tcp+udp" && t.ProtoMask != proto {
		return fmt.Errorf("该通道仅允许 %s", t.ProtoMask)
	}
	if listenPort < t.PortStart || listenPort > t.PortEnd {
		return fmt.Errorf("监听端口必须落在 %d-%d", t.PortStart, t.PortEnd)
	}
	if targetPort < 1 || targetPort > 65535 {
		return errors.New("目标端口超出范围")
	}
	if target == "" {
		return errors.New("目标地址不能为空")
	}
	ip := net.ParseIP(target)
	if ip == nil {
		if !resolver.IsHostname(target) {
			return errors.New("目标地址格式非法")
		}
		if !cidrAllowsAll(t.TargetCIDRAllow) {
			return errors.New("该通道限制了目标 CIDR，仅允许 IPv4 目标")
		}
		return nil
	}
	if ip.To4() == nil {
		return errors.New("目标地址必须为 IPv4")
	}
	if !targetIPInCIDR(ip, t.TargetCIDRAllow) {
		return fmt.Errorf("目标地址不在允许的 CIDR 内（%s）", t.TargetCIDRAllow)
	}
	return nil
}

func exitAllowedByTunnel(t *db.Tunnel, exitHost string) error {
	if t == nil {
		return fmt.Errorf("末跳通道缺失")
	}
	ip := net.ParseIP(exitHost)
	if ip == nil {
		if !cidrAllowsAll(t.TargetCIDRAllow) {
			return fmt.Errorf("末跳通道限制了目标 CIDR，出口仅允许 IPv4")
		}
		return nil
	}
	if ip.To4() == nil {
		return fmt.Errorf("出口必须为 IPv4")
	}
	if !targetIPInCIDR(ip, t.TargetCIDRAllow) {
		return fmt.Errorf("出口地址不在末跳通道允许的 CIDR 内（%s）", t.TargetCIDRAllow)
	}
	return nil
}

func (s *Server) checkUserChainQuota(u *db.User, hops []db.HopInput, existingChainForwards int) error {
	total, _ := db.CountForwardsForUser(s.DB, u.ID)
	if (total-existingChainForwards)+len(hops) > u.MaxForwards {
		return fmt.Errorf("超出用户最大转发数（%d）", u.MaxForwards)
	}
	for _, h := range hops {
		if !h.TunnelID.Valid {
			continue
		}
		grant, err := db.GetGrant(s.DB, u.ID, h.TunnelID.Int64)
		if err != nil {
			return fmt.Errorf("无权使用通道 %d", h.TunnelID.Int64)
		}
		cnt, _ := db.CountForwardsForUserTunnel(s.DB, u.ID, h.TunnelID.Int64)
		if cnt+1 > grant.MaxForwards {
			return fmt.Errorf("通道 %d 已达最大转发数（%d）", h.TunnelID.Int64, grant.MaxForwards)
		}
	}
	return nil
}

func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func (s *Server) regenerateChainByID(chainID int64) ([]int64, error) {
	c, err := db.GetChain(s.DB, chainID)
	if err != nil {
		return nil, err
	}
	hops, err := db.ListChainHops(s.DB, chainID)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return nil, nil
	}
	if c.OwnerID.Valid {
		if lastTun, terr := db.GetTunnel(s.DB, hops[len(hops)-1].TunnelID.Int64); terr == nil {
			if cerr := exitAllowedByTunnel(lastTun, c.ExitHost); cerr != nil {
				nodes, derr := db.DeleteChain(s.DB, chainID)
				if derr != nil {
					return nil, derr
				}
				log.Printf("chain %d (owner %d) removed after node change: exit %s no longer allowed by last-hop tunnel %d: %v",
					chainID, c.OwnerID.Int64, c.ExitHost, lastTun.ID, cerr)
				return nodes, nil
			}
		}
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateChain(tx, c, inputs, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}
