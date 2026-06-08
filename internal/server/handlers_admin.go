package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

// --- Tunnels ---

func (s *Server) listTunnels(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tunnels, err := db.ListTunnels(s.DB)
	if err != nil {
		log.Printf("list tunnels: %v", err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("list tunnels: list nodes: %v", err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	paged, pager := paginate(tunnels, r)
	s.render(w, "tunnels.html", map[string]any{
		"User":     u,
		"Tunnels":  paged,
		"Nodes":    nodes,
		"NodeByID": nodeByID,
		"Pager":    pager,
		"Flash":    flashFromCookie(w, r),
	})
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodeID, err := parseFormInt64(r, "node_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/tunnels")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.TrimSpace(r.FormValue("proto_mask"))
	portStart, err := parseFormInt(r, "port_start")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/tunnels")
		return
	}
	portEnd, err := parseFormInt(r, "port_end")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/tunnels")
		return
	}
	cidr := strings.TrimSpace(r.FormValue("target_cidr_allow"))
	bw, _ := strconv.Atoi(r.FormValue("bandwidth_mbps"))

	if name == "" || nodeID == 0 || portStart < 1 || portEnd < portStart || portEnd > 65535 {
		s.flashRedirect(w, r, "字段不完整或端口段无效", "/tunnels")
		return
	}
	if proto != "tcp" && proto != "udp" && proto != "tcp+udp" {
		s.flashRedirect(w, r, "协议必须为 tcp、udp 或 tcp+udp", "/tunnels")
		return
	}
	if cidr == "" {
		cidr = "0.0.0.0/0"
	}
	if err := validateCIDRList(cidr); err != nil {
		s.flashRedirect(w, r, "CIDR 无效: "+err.Error(), "/tunnels")
		return
	}
	id, err := db.CreateTunnel(s.DB, &db.Tunnel{
		Name: name, NodeID: nodeID, ProtoMask: proto,
		PortStart: portStart, PortEnd: portEnd,
		TargetCIDRAllow: cidr, BandwidthMbps: bw,
	})
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/tunnels")
		return
	}
	db.WriteAudit(s.DB, u.ID, "tunnel.create", strconv.FormatInt(id, 10), name)
	http.Redirect(w, r, "/tunnels", http.StatusSeeOther)
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	t, err := db.GetTunnel(s.DB, id)
	if err != nil {
		log.Printf("delete tunnel %d: get tunnel: %v", id, err)
	}
	// forwards.tunnel_id is ON DELETE NO ACTION, so a tunnel still backing any
	// forward (a tenant forward or a chain hop) cannot be dropped; reject with a
	// clear message instead of leaking the raw FK error.
	if n, err := db.CountForwardsByTunnel(s.DB, id); err == nil && n > 0 {
		s.flashRedirect(w, r, fmt.Sprintf("通道仍被 %d 条转发占用，请先删除这些转发", n), "/tunnels")
		return
	}
	if err := db.DeleteTunnel(s.DB, id); err != nil {
		s.flashRedirect(w, r, err.Error(), "/tunnels")
		return
	}
	db.WriteAudit(s.DB, u.ID, "tunnel.delete", strconv.FormatInt(id, 10), "")
	if t != nil {
		s.dispatchAfterMutation(w, t.NodeID, "通道删除")
	}
	http.Redirect(w, r, "/tunnels", http.StatusSeeOther)
}

// --- Tenants ---

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenants, err := db.ListTenants(s.DB)
	if err != nil {
		log.Printf("list tenants: %v", err)
	}
	paged, pager := paginate(tenants, r)
	s.render(w, "tenants.html", map[string]any{
		"User":    u,
		"Tenants": paged,
		"Pager":   pager,
		"Flash":   flashFromCookie(w, r),
	})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	maxForwards, _ := strconv.Atoi(r.FormValue("max_forwards"))
	quotaMB, _ := strconv.ParseInt(r.FormValue("traffic_quota_mb"), 10, 64)
	if name == "" {
		s.flashRedirect(w, r, "名称不能为空", "/tenants")
		return
	}
	if maxForwards <= 0 {
		maxForwards = 100
	}
	t := &db.Tenant{
		Name:              name,
		MaxForwards:       maxForwards,
		TrafficQuotaBytes: quotaMB * 1024 * 1024,
	}
	id, err := db.CreateTenant(s.DB, t)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/tenants")
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.create", strconv.FormatInt(id, 10), name)
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", id), http.StatusSeeOther)
}

func (s *Server) showTenant(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	t, err := db.GetTenant(s.DB, id)
	if err != nil {
		http.Error(w, "用户不存在", http.StatusNotFound)
		return
	}
	tunnels, grants, err := db.ListTunnelsForTenant(s.DB, id)
	if err != nil {
		log.Printf("show tenant %d: list tunnels: %v", id, err)
	}
	allTunnels, err := db.ListTunnels(s.DB)
	if err != nil {
		log.Printf("show tenant %d: list all tunnels: %v", id, err)
	}
	allNodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("show tenant %d: list nodes: %v", id, err)
	}
	forwards, err := db.ListForwardsForTenant(s.DB, id)
	if err != nil {
		log.Printf("show tenant %d: list forwards: %v", id, err)
	}
	users, err := db.ListUsers(s.DB)
	if err != nil {
		log.Printf("show tenant %d: list users: %v", id, err)
	}
	var tenantUsers []*db.User
	for _, usr := range users {
		if usr.TenantID.Valid && usr.TenantID.Int64 == id {
			tenantUsers = append(tenantUsers, usr)
		}
	}
	combos, comboGrants, _ := db.ListCombosForTenant(s.DB, id)
	allCombos, _ := db.ListTunnelCombos(s.DB)
	s.render(w, "tenant_detail.html", map[string]any{
		"User":        u,
		"Tenant":      t,
		"Tunnels":     tunnels,
		"Grants":      grants,
		"AllTunnels":  allTunnels,
		"AllNodes":    allNodes,
		"Forwards":    forwards,
		"TenantUsers": tenantUsers,
		"Combos":      combos,
		"ComboGrants": comboGrants,
		"AllCombos":   allCombos,
		"Flash":       flashFromCookie(w, r),
	})
}

func (s *Server) deleteAdminTenant(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	nodes, err := db.DistinctTenantNodes(s.DB, id)
	if err != nil {
		log.Printf("delete tenant %d: list nodes: %v", id, err)
	}
	if err := db.DeleteTenant(s.DB, id); err != nil {
		s.flashRedirect(w, r, err.Error(), "/tenants")
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterFanout(w, nodes, "租户删除")
	http.Redirect(w, r, "/tenants", http.StatusSeeOther)
}

func (s *Server) toggleTenant(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	t, err := db.GetTenant(s.DB, id)
	if err != nil {
		http.Error(w, "用户不存在", http.StatusNotFound)
		return
	}
	target := !t.Disabled
	reason := ""
	if target {
		reason = "管理员手动禁用"
	}
	if err := db.SetTenantDisabled(s.DB, id, target, reason); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", id))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.toggle", strconv.FormatInt(id, 10), fmt.Sprintf("disabled=%v", target))
	nodes, err := db.DistinctTenantNodes(s.DB, id)
	if err != nil {
		log.Printf("toggle tenant %d: list nodes: %v", id, err)
	}
	s.dispatchAfterFanout(w, nodes, "租户启停切换")
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", id), http.StatusSeeOther)
}

func (s *Server) setTenantQuotaBytes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	bytes, err := strconv.ParseInt(r.FormValue("traffic_quota_bytes"), 10, 64)
	if err != nil || bytes < 0 {
		s.flashRedirect(w, r, "字节数无效", fmt.Sprintf("/tenants/%d", id))
		return
	}
	if _, err := s.DB.Exec(`UPDATE tenants SET traffic_quota_bytes=? WHERE id=?`, bytes, id); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", id))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.set_quota_bytes", strconv.FormatInt(id, 10), strconv.FormatInt(bytes, 10))
	s.flashRedirect(w, r, fmt.Sprintf("配额已更新为 %d 字节", bytes), fmt.Sprintf("/tenants/%d", id))
}

func (s *Server) resetTenantTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := db.ResetTenantTraffic(s.DB, id); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", id))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.reset_traffic", strconv.FormatInt(id, 10), "")
	nodes, err := db.DistinctTenantNodes(s.DB, id)
	if err != nil {
		log.Printf("reset tenant traffic %d: list nodes: %v", id, err)
	}
	// Default to the success message; dispatchAfterFanout overwrites the
	// flash cookie when any node fails so the failure note wins.
	setFlash(w, "已重置流量计数并重新启用用户")
	s.dispatchAfterFanout(w, nodes, "流量计数重置")
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", id), http.StatusSeeOther)
}

func (s *Server) grantTenantTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenantID, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tunnelID, err := parseFormInt64(r, "tunnel_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", tenantID))
		return
	}
	maxForwards, _ := strconv.Atoi(r.FormValue("max_forwards"))
	if maxForwards <= 0 {
		maxForwards = 10
	}
	if err := db.GrantTunnel(s.DB, tenantID, tunnelID, maxForwards); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", tenantID))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.grant", strconv.FormatInt(tenantID, 10), strconv.FormatInt(tunnelID, 10))
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", tenantID), http.StatusSeeOther)
}

func (s *Server) revokeTenantTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenantID, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tunnelID, err := urlParamInt64(r, "tunnelID")
	if err != nil {
		http.Error(w, "bad tunnel id", http.StatusBadRequest)
		return
	}
	if err := db.RevokeTunnel(s.DB, tenantID, tunnelID); err != nil {
		setFlash(w, err.Error())
	}
	db.WriteAudit(s.DB, u.ID, "tenant.revoke", strconv.FormatInt(tenantID, 10), strconv.FormatInt(tunnelID, 10))
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", tenantID), http.StatusSeeOther)
}

func (s *Server) createTenantUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenantID, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	redirect := fmt.Sprintf("/tenants/%d", tenantID)
	if username == "" || password == "" {
		s.flashRedirect(w, r, "用户名和密码不能为空", redirect)
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		s.flashRedirect(w, r, "哈希失败: "+err.Error(), redirect)
		return
	}
	id, err := db.CreateTenantUser(s.DB, tenantID, username, hash)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), redirect)
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.create", strconv.FormatInt(id, 10), username)
	s.flashRedirect(w, r, fmt.Sprintf("已创建用户 %s", username), redirect)
}

// --- Users ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	users, err := db.ListUsers(s.DB)
	if err != nil {
		log.Printf("list users: %v", err)
	}
	tenants, err := db.ListTenants(s.DB)
	if err != nil {
		log.Printf("list users: list tenants: %v", err)
	}
	tenantByID := buildMap(tenants, func(t *db.Tenant) int64 { return t.ID })
	s.render(w, "users.html", map[string]any{
		"User":       u,
		"Users":      users,
		"TenantByID": tenantByID,
		"Flash":      flashFromCookie(w, r),
	})
}

func (s *Server) toggleUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		http.Error(w, "用户不存在", http.StatusNotFound)
		return
	}
	if target.ID == u.ID {
		s.flashRedirect(w, r, "不能禁用自己", "/users")
		return
	}
	willDisable := !target.Disabled
	if err := db.SetUserDisabled(s.DB, id, willDisable); err != nil {
		s.flashRedirect(w, r, err.Error(), "/users")
		return
	}
	// Tenant accounts: keep the tenant entity in lockstep so the user's
	// forwards stop matching traffic the moment login is revoked.
	if target.Role == "tenant" && target.TenantID.Valid {
		reason := ""
		if willDisable {
			reason = "登录账号被禁用"
		}
		_ = db.SetTenantDisabled(s.DB, target.TenantID.Int64, willDisable, reason)
		if nodes, err := db.DistinctTenantNodes(s.DB, target.TenantID.Int64); err == nil {
			s.dispatchAfterFanout(w, nodes, "账号启停切换")
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.toggle", strconv.FormatInt(id, 10), fmt.Sprintf("disabled=%v", willDisable))
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		http.Error(w, "用户不存在", http.StatusNotFound)
		return
	}
	newPw := db.RandToken(8) // 16 hex chars
	hash, err := HashPassword(newPw)
	if err != nil {
		s.flashRedirect(w, r, "哈希失败: "+err.Error(), "/users")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, id); err != nil {
		s.flashRedirect(w, r, err.Error(), "/users")
		return
	}
	// Invalidate all live sessions for this user so the old password's
	// existing logins cannot continue.
	_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	db.WriteAudit(s.DB, u.ID, "user.reset_password", strconv.FormatInt(id, 10), "")
	s.flashRedirect(w, r, fmt.Sprintf("已重置 %s 的密码为：%s （请尽快告知用户，本页刷新后不再显示）", target.Username, newPw), "/users")
}

func (s *Server) deleteUserHandler(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if id == u.ID {
		s.flashRedirect(w, r, "不能删除自己", "/users")
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		http.Error(w, "用户不存在", http.StatusNotFound)
		return
	}
	// Tenant accounts: if this was the last login for the tenant, tear the
	// tenant down entirely so its forwards (and the ports they hold) are
	// released. Co-existing tenant accounts keep the tenant intact.
	var affected []int64
	if target.Role == "tenant" && target.TenantID.Valid {
		others, _ := db.CountUsersByTenant(s.DB, target.TenantID.Int64)
		if others <= 1 {
			nodes, err := db.DeleteForwardsForTenant(s.DB, target.TenantID.Int64)
			if err != nil {
				s.flashRedirect(w, r, err.Error(), "/users")
				return
			}
			affected = nodes
			_ = db.DeleteTenant(s.DB, target.TenantID.Int64)
		}
	}
	if err := db.DeleteUser(s.DB, id); err != nil {
		setFlash(w, err.Error())
	}
	s.dispatchAfterFanout(w, affected, "账号删除")
	db.WriteAudit(s.DB, u.ID, "user.delete", strconv.FormatInt(id, 10), "")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// --- helpers ---

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

// cidrAllowsAll reports whether the comma-separated CIDR list is empty or
// contains 0.0.0.0/0, meaning every IPv4 destination is permitted.
func cidrAllowsAll(list string) bool {
	list = strings.TrimSpace(list)
	if list == "" {
		return true
	}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

// targetIPInCIDR reports whether ip falls within any of the CIDR entries in
// the comma-separated list. Empty list ≡ no restriction.
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
