package server

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

func (s *Server) tenantDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	tunnels, grants, _ := db.ListTunnelsForTenant(s.DB, t.ID)
	forwards, _ := db.ListForwardsForTenant(s.DB, t.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	tunnelByID := map[int64]*db.Tunnel{}
	for _, tn := range tunnels {
		tunnelByID[tn.ID] = tn
	}
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
	tunnels, grants, _ := db.ListTunnelsForTenant(s.DB, t.ID)
	forwards, _ := db.ListForwardsForTenant(s.DB, t.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	tunnelByID := map[int64]*db.Tunnel{}
	for _, tn := range tunnels {
		tunnelByID[tn.ID] = tn
	}
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
		setFlash(w, "用户已被禁用："+nullStr(t.DisableReason.String))
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	if t.ExpiresAt.Valid && t.ExpiresAt.Int64 > 0 && t.ExpiresAt.Int64 < time.Now().Unix() {
		setFlash(w, "用户已过期")
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}

	tunnelID, _ := strconv.ParseInt(r.FormValue("tunnel_id"), 10, 64)
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	listenPortStr := strings.TrimSpace(r.FormValue("listen_port"))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	targetPort, _ := strconv.Atoi(r.FormValue("target_port"))
	comment := strings.TrimSpace(r.FormValue("comment"))

	grant, err := db.GetGrant(s.DB, t.ID, tunnelID)
	if err != nil {
		setFlash(w, "无权使用该通道")
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	tunnel, err := db.GetTunnel(s.DB, tunnelID)
	if err != nil {
		setFlash(w, "通道不存在")
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	var listenPort int
	if listenPortStr == "" {
		// Auto-allocate within the tunnel range, skipping anything already
		// installed on the same (node, proto).
		used, err := db.UsedPortsOnNode(s.DB, tunnel.NodeID, proto, tunnel.PortStart, tunnel.PortEnd)
		if err != nil {
			setFlash(w, "端口分配失败: "+err.Error())
			http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
			return
		}
		listenPort = pickFreePort(tunnel.PortStart, tunnel.PortEnd, used)
		if listenPort == 0 {
			setFlash(w, fmt.Sprintf("通道 %d-%d 内已无可用端口", tunnel.PortStart, tunnel.PortEnd))
			http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
			return
		}
	} else {
		listenPort, _ = strconv.Atoi(listenPortStr)
	}
	if err := validateAgainstTunnel(tunnel, proto, listenPort, targetIP, targetPort); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	totalCount, _ := db.CountForwardsForTenant(s.DB, t.ID)
	if totalCount >= t.MaxForwards {
		setFlash(w, fmt.Sprintf("已达用户最大转发数（%d）", t.MaxForwards))
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	tunCount, _ := db.CountForwardsForTenantTunnel(s.DB, t.ID, tunnelID)
	if tunCount >= grant.MaxForwards {
		setFlash(w, fmt.Sprintf("已达该通道最大转发数（%d）", grant.MaxForwards))
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}

	f := &db.Forward{
		NodeID:     tunnel.NodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
	}
	f.TenantID.Int64 = t.ID
	f.TenantID.Valid = true
	f.TunnelID.Int64 = tunnel.ID
	f.TunnelID.Valid = true
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.tenant_create", strconv.FormatInt(id, 10),
		fmt.Sprintf("tenant=%d tunnel=%d %s/%d→%s:%d", t.ID, tunnel.ID, proto, listenPort, targetIP, targetPort))
	s.Pusher.Schedule(tunnel.NodeID)
	http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
}

func (s *Server) tenantDeleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
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
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.tenant_delete", strconv.FormatInt(id, 10), "")
	s.Pusher.Schedule(nodeID)
	http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
}

func (s *Server) tenantContext(u *db.User) (*db.Tenant, error) {
	if !u.TenantID.Valid {
		return nil, errors.New("当前账号未绑定用户")
	}
	return db.GetTenant(s.DB, u.TenantID.Int64)
}

func validateAgainstTunnel(t *db.Tunnel, proto string, listenPort int, targetIP string, targetPort int) error {
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
	ip := net.ParseIP(targetIP)
	if ip == nil || ip.To4() == nil {
		return errors.New("目标地址必须为有效 IPv4")
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

// pickFreePort returns 0 when the range is exhausted. Random offset keeps
// the assignment unpredictable so two near-simultaneous tenants don't keep
// colliding on the same port.
func pickFreePort(start, end int, used map[int]bool) int {
	span := end - start + 1
	if span <= 0 {
		return 0
	}
	offset := rand.Intn(span)
	for i := 0; i < span; i++ {
		p := start + ((offset + i) % span)
		if !used[p] {
			return p
		}
	}
	return 0
}
