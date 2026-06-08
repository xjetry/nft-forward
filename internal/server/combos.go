package server

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

func (s *Server) listCombos(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	combos, err := db.ListTunnelCombos(s.DB)
	if err != nil {
		log.Printf("list combos: %v", err)
	}
	tunnels, err := db.ListTunnels(s.DB)
	if err != nil {
		log.Printf("list combos: list tunnels: %v", err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("list combos: list nodes: %v", err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	tunnelByID := buildMap(tunnels, func(t *db.Tunnel) int64 { return t.ID })

	type comboView struct {
		Combo *db.TunnelCombo
		Hops  []*db.TunnelComboHop
		Path  string
	}
	views := make([]comboView, 0, len(combos))
	for _, c := range combos {
		hops, _ := db.ListComboHops(s.DB, c.ID)
		names := make([]string, 0, len(hops))
		for _, h := range hops {
			t := tunnelByID[h.TunnelID]
			if t == nil {
				names = append(names, fmt.Sprintf("#%d", h.TunnelID))
				continue
			}
			n := nodeByID[t.NodeID]
			if n != nil {
				names = append(names, n.Name)
			} else {
				names = append(names, t.Name)
			}
		}
		views = append(views, comboView{Combo: c, Hops: hops, Path: strings.Join(names, " → ")})
	}
	s.render(w, "combos.html", map[string]any{
		"User":    u,
		"Combos":  views,
		"Tunnels": tunnels,
		"NodeByID": nodeByID,
		"Flash":   flashFromCookie(w, r),
	})
}

func (s *Server) createCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if err := r.ParseForm(); err != nil {
		s.flashRedirect(w, r, "表单解析失败", "/combos")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.flashRedirect(w, r, "名称必填", "/combos")
		return
	}
	tunnelIDs := r.Form["hop_tunnel"]
	modes := r.Form["hop_mode"]
	if len(tunnelIDs) < 2 {
		s.flashRedirect(w, r, "组合通道至少需要 2 个通道", "/combos")
		return
	}
	hops := make([]db.TunnelComboHop, 0, len(tunnelIDs))
	for i, idStr := range tunnelIDs {
		tid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || tid == 0 {
			s.flashRedirect(w, r, fmt.Sprintf("第 %d 个通道 ID 无效", i+1), "/combos")
			return
		}
		mode := "userspace"
		if i < len(modes) && modes[i] != "" {
			mode = modes[i]
		}
		hops = append(hops, db.TunnelComboHop{TunnelID: tid, Mode: mode})
	}
	id, err := db.CreateTunnelCombo(s.DB, name, hops)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/combos")
		return
	}
	db.WriteAudit(s.DB, u.ID, "combo.create", strconv.FormatInt(id, 10), name)
	setFlash(w, "组合通道已创建")
	http.Redirect(w, r, "/combos", http.StatusSeeOther)
}

func (s *Server) deleteCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteTunnelCombo(s.DB, id); err != nil {
		s.flashRedirect(w, r, err.Error(), "/combos")
		return
	}
	db.WriteAudit(s.DB, u.ID, "combo.delete", strconv.FormatInt(id, 10), "")
	http.Redirect(w, r, "/combos", http.StatusSeeOther)
}

func (s *Server) grantTenantCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenantID, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	comboID, err := parseFormInt64(r, "combo_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", tenantID))
		return
	}
	maxForwards, _ := strconv.Atoi(r.FormValue("combo_max_forwards"))
	if maxForwards <= 0 {
		maxForwards = 10
	}
	if err := db.GrantCombo(s.DB, tenantID, comboID, maxForwards); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", tenantID))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.grant_combo", strconv.FormatInt(tenantID, 10), strconv.FormatInt(comboID, 10))
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", tenantID), http.StatusSeeOther)
}

func (s *Server) revokeTenantCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tenantID, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	comboID, err := urlParamInt64(r, "comboID")
	if err != nil {
		http.Error(w, "bad combo id", http.StatusBadRequest)
		return
	}
	if err := db.RevokeCombo(s.DB, tenantID, comboID); err != nil {
		s.flashRedirect(w, r, err.Error(), fmt.Sprintf("/tenants/%d", tenantID))
		return
	}
	db.WriteAudit(s.DB, u.ID, "tenant.revoke_combo", strconv.FormatInt(tenantID, 10), strconv.FormatInt(comboID, 10))
	http.Redirect(w, r, fmt.Sprintf("/tenants/%d", tenantID), http.StatusSeeOther)
}
