package server

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

func (s *Server) tenantListChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	chains, _ := db.ListChainsByTenant(s.DB, t.ID)
	views := make([]chainView, 0, len(chains))
	for _, c := range chains {
		views = append(views, s.buildChainView(c))
	}
	s.render(w, "my_chains.html", map[string]any{"User": u, "Chains": views, "Flash": flashFromCookie(w, r)})
}

func (s *Server) tenantNewChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	tunnels, _, _ := db.ListTunnelsForTenant(s.DB, t.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "my_chain_form.html", map[string]any{
		"User": u, "Tunnels": tunnels, "NodeByID": nodeByID, "Flash": flashFromCookie(w, r),
	})
}

// tenantHopInputs reads hop_tunnel[] + hop_mode[], verifies each tunnel is
// granted to the tenant, and derives the node from the tunnel. It also returns
// the last hop's tunnel so the caller can enforce the exit CIDR.
func (s *Server) tenantHopInputs(r *http.Request, tenantID int64) ([]db.HopInput, *db.Tunnel, error) {
	tunnelIDs := r.Form["hop_tunnel"]
	modes := r.Form["hop_mode"]
	if len(tunnelIDs) == 0 {
		return nil, nil, fmt.Errorf("至少添加一个通道")
	}
	hops := make([]db.HopInput, 0, len(tunnelIDs))
	var last *db.Tunnel
	for i, idStr := range tunnelIDs {
		tid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || tid == 0 {
			return nil, nil, fmt.Errorf("第 %d 跳通道非法", i+1)
		}
		if _, err := db.GetGrant(s.DB, tenantID, tid); err != nil {
			return nil, nil, fmt.Errorf("无权使用通道 %d", tid)
		}
		tun, err := db.GetTunnel(s.DB, tid)
		if err != nil {
			return nil, nil, fmt.Errorf("通道 %d 不存在", tid)
		}
		mode := "kernel"
		if i < len(modes) {
			mode = modes[i]
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, TunnelID: nullInt64(tid), Mode: mode})
		last = tun
	}
	return hops, last, nil
}

// nullInt64 wraps a valid int64 for the TenantID/TunnelID sql.NullInt64 fields.
func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func (s *Server) tenantCreateChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if t.Disabled {
		setFlash(w, "用户已被禁用")
		http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
		return
	}
	if t.ExpiresAt.Valid && t.ExpiresAt.Int64 > 0 && t.ExpiresAt.Int64 < time.Now().Unix() {
		setFlash(w, "用户已过期")
		http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	hops, lastTunnel, err := s.tenantHopInputs(r, t.ID)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	// 出口必须落在末跳通道的 CIDR 白名单内（中间跳目标是受信中继地址，免检）。
	if err := exitAllowedByTunnel(lastTunnel, exitHost); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	// 配额：新链路新增 len(hops) 条 forward（每个通道 1 条）。
	if err := s.checkTenantChainQuota(t, hops, 0); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	c := &db.Chain{TenantID: nullInt64(t.ID), Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		tx.Rollback()
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.tenant_create", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
}

func (s *Server) tenantDeleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil || !c.TenantID.Valid || c.TenantID.Int64 != t.ID {
		http.Error(w, "无权操作该链路", http.StatusForbidden)
		return
	}
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.tenant_delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterFanout(w, nodes, "链路删除")
	http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
}

// exitAllowedByTunnel enforces the tenant security model on the user-chosen exit
// (the only arbitrary destination in a tenant chain): IPv4 must fall in the
// tunnel CIDR allowlist; a hostname exit is rejected when any CIDR is set
// (can't statically prove containment) — mirrors validateAgainstTunnel.
func exitAllowedByTunnel(t *db.Tunnel, exitHost string) error {
	if t == nil {
		return fmt.Errorf("末跳通道缺失")
	}
	ip := net.ParseIP(exitHost)
	if ip == nil {
		if strings.TrimSpace(t.TargetCIDRAllow) != "" {
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

// checkTenantChainQuota verifies the tenant's total + per-tunnel max_forwards can
// absorb this chain's hops. existingChainForwards is the count this chain already
// holds (0 for a new chain; >0 on edit) so editing in place isn't double-counted.
func (s *Server) checkTenantChainQuota(t *db.Tenant, hops []db.HopInput, existingChainForwards int) error {
	total, _ := db.CountForwardsForTenant(s.DB, t.ID)
	if (total-existingChainForwards)+len(hops) > t.MaxForwards {
		return fmt.Errorf("超出用户最大转发数（%d）", t.MaxForwards)
	}
	for _, h := range hops {
		if !h.TunnelID.Valid {
			continue
		}
		grant, err := db.GetGrant(s.DB, t.ID, h.TunnelID.Int64)
		if err != nil {
			return fmt.Errorf("无权使用通道 %d", h.TunnelID.Int64)
		}
		cnt, _ := db.CountForwardsForTenantTunnel(s.DB, t.ID, h.TunnelID.Int64)
		// 同节点禁重复 => 每通道至多 1 跳，故 +1。
		if cnt+1 > grant.MaxForwards {
			return fmt.Errorf("通道 %d 已达最大转发数（%d）", h.TunnelID.Int64, grant.MaxForwards)
		}
	}
	return nil
}
