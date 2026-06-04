package server

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
)

func (s *Server) tenantDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	tunnels, grants, err := db.ListTunnelsForTenant(s.DB, t.ID)
	if err != nil {
		log.Printf("tenant dashboard %d: list tunnels: %v", t.ID, err)
	}
	forwards, err := db.ListForwardsForTenant(s.DB, t.ID)
	if err != nil {
		log.Printf("tenant dashboard %d: list forwards: %v", t.ID, err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("tenant dashboard %d: list nodes: %v", t.ID, err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	tunnelByID := buildMap(tunnels, func(tn *db.Tunnel) int64 { return tn.ID })
	s.render(w, "my_dashboard.html", map[string]any{
		"User":       u,
		"Tenant":     t,
		"Tunnels":    tunnels,
		"Grants":     grants,
		"Forwards":   forwards,
		"NodeByID":   nodeByID,
		"TunnelByID": tunnelByID,
		"Flash":      flashFromCookie(w, r),
	})
}

func (s *Server) tenantListForwards(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	tunnels, grants, err := db.ListTunnelsForTenant(s.DB, t.ID)
	if err != nil {
		log.Printf("tenant list forwards %d: list tunnels: %v", t.ID, err)
	}
	forwards, err := db.ListForwardsForTenant(s.DB, t.ID)
	if err != nil {
		log.Printf("tenant list forwards %d: list forwards: %v", t.ID, err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("tenant list forwards %d: list nodes: %v", t.ID, err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	tunnelByID := buildMap(tunnels, func(tn *db.Tunnel) int64 { return tn.ID })
	s.render(w, "my_forwards.html", map[string]any{
		"User":       u,
		"Tenant":     t,
		"Tunnels":    tunnels,
		"Grants":     grants,
		"Forwards":   forwards,
		"NodeByID":   nodeByID,
		"TunnelByID": tunnelByID,
		"Flash":      flashFromCookie(w, r),
	})
}

func (s *Server) tenantCreateForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if t.Disabled {
		s.flashRedirect(w, r, "用户已被禁用："+nullStr(t.DisableReason.String), "/my/forwards")
		return
	}
	if t.ExpiresAt.Valid && t.ExpiresAt.Int64 > 0 && t.ExpiresAt.Int64 < time.Now().Unix() {
		s.flashRedirect(w, r, "用户已过期", "/my/forwards")
		return
	}

	tunnelID, err := parseFormInt64(r, "tunnel_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/my/forwards")
		return
	}
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	listenPortStr := strings.TrimSpace(r.FormValue("listen_port"))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	targetPort, _ := strconv.Atoi(r.FormValue("target_port"))
	comment := strings.TrimSpace(r.FormValue("comment"))
	mode := strings.TrimSpace(r.FormValue("mode"))

	if !validMode(mode) {
		s.flashRedirect(w, r, "无效的转发模式", "/my/forwards")
		return
	}
	if mode == "userspace" && proto == "udp" {
		s.flashRedirect(w, r, "UDP 不支持用户态转发", "/my/forwards")
		return
	}

	grant, err := db.GetGrant(s.DB, t.ID, tunnelID)
	if err != nil {
		s.flashRedirect(w, r, "无权使用该通道", "/my/forwards")
		return
	}
	tunnel, err := db.GetTunnel(s.DB, tunnelID)
	if err != nil {
		s.flashRedirect(w, r, "通道不存在", "/my/forwards")
		return
	}
	occupied, err := db.OccupiedPortsOnNode(s.DB, tunnel.NodeID, proto, 0)
	if err != nil {
		s.flashRedirect(w, r, "端口检查失败: "+err.Error(), "/my/forwards")
		return
	}
	var listenPort int
	if listenPortStr == "" {
		listenPort = db.PickFreePort(tunnel.PortStart, tunnel.PortEnd, occupied)
		if listenPort == 0 {
			s.flashRedirect(w, r, fmt.Sprintf("通道 %d-%d 内已无可用端口", tunnel.PortStart, tunnel.PortEnd), "/my/forwards")
			return
		}
	} else {
		listenPort, _ = strconv.Atoi(listenPortStr)
		if occupied[listenPort] {
			s.flashRedirect(w, r, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort), "/my/forwards")
			return
		}
	}
	if err := validateAgainstTunnel(tunnel, proto, listenPort, targetIP, targetPort); err != nil {
		s.flashRedirect(w, r, err.Error(), "/my/forwards")
		return
	}
	totalCount, _ := db.CountForwardsForTenant(s.DB, t.ID)
	if totalCount >= t.MaxForwards {
		s.flashRedirect(w, r, fmt.Sprintf("已达用户最大转发数（%d）", t.MaxForwards), "/my/forwards")
		return
	}
	tunCount, _ := db.CountForwardsForTenantTunnel(s.DB, t.ID, tunnelID)
	if tunCount >= grant.MaxForwards {
		s.flashRedirect(w, r, fmt.Sprintf("已达该通道最大转发数（%d）", grant.MaxForwards), "/my/forwards")
		return
	}

	f := &db.Forward{
		NodeID:     tunnel.NodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
		Mode:       mode,
	}
	f.TenantID.Int64 = t.ID
	f.TenantID.Valid = true
	f.TunnelID.Int64 = tunnel.ID
	f.TunnelID.Valid = true
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/my/forwards")
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.tenant_create", strconv.FormatInt(id, 10),
		fmt.Sprintf("tenant=%d tunnel=%d %s/%d→%s:%d", t.ID, tunnel.ID, proto, listenPort, targetIP, targetPort))
	s.dispatchAfterMutation(w, tunnel.NodeID, "转发新增")
	http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
}

func (s *Server) tenantDeleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	f, err := db.GetForward(s.DB, id)
	if err != nil {
		http.Error(w, "转发不存在", http.StatusNotFound)
		return
	}
	if !f.TenantID.Valid || f.TenantID.Int64 != t.ID {
		http.Error(w, "无权操作该转发", http.StatusForbidden)
		return
	}
	nodeID, err := db.DeleteForward(s.DB, id)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/my/forwards")
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.tenant_delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterMutation(w, nodeID, "转发删除")
	http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
}

func (s *Server) tenantContext(u *db.User) (*db.Tenant, error) {
	if !u.TenantID.Valid {
		return nil, errors.New("当前账号未绑定用户")
	}
	return db.GetTenant(s.DB, u.TenantID.Int64)
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
		// hostname path — only allowed when the tunnel imposes no CIDR
		// restriction (we can't statically prove a hostname lands inside a CIDR).
		if !resolver.IsHostname(target) {
			return errors.New("目标地址格式非法")
		}
		if strings.TrimSpace(t.TargetCIDRAllow) != "" {
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

func nullStr(s string) string {
	if s == "" {
		return "(无说明)"
	}
	return s
}
