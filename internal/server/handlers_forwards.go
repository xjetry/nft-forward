package server

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

func (s *Server) listForwards(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	allForwards, err := db.ListForwards(s.DB)
	if err != nil {
		log.Printf("list forwards: %v", err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("list forwards: list nodes: %v", err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	hopInfo, err := db.ChainHopInfoMap(s.DB)
	if err != nil {
		log.Printf("list forwards: chain hop info: %v", err)
	}
	tenantByID, _ := db.TenantsByID(s.DB)
	combos, _ := db.ListTunnelCombos(s.DB)

	tab := r.URL.Query().Get("tab")
	if tab != "chain" {
		tab = "normal"
	}
	owner := r.URL.Query().Get("owner")
	if owner != "all" {
		owner = "admin"
	}
	var filtered []*db.Forward
	for _, f := range allForwards {
		if tab == "chain" && !f.ChainID.Valid {
			continue
		}
		if tab == "normal" && f.ChainID.Valid {
			continue
		}
		if owner == "admin" && f.TenantID.Valid {
			continue
		}
		filtered = append(filtered, f)
	}

	forwards, pager := paginate(filtered, r)
	pager.Extra = "tab=" + tab + "&owner=" + owner + "&"

	s.render(w, "forwards.html", map[string]any{
		"User":       u,
		"Forwards":   forwards,
		"Nodes":      nodes,
		"NodeByID":   nodeByID,
		"HopInfo":    hopInfo,
		"TenantByID": tenantByID,
		"Combos":     combos,
		"Pager":      pager,
		"Tab":        tab,
		"Owner":      owner,
		"Flash":      flashFromCookie(w, r),
	})
}

func (s *Server) createForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodeVal := strings.TrimSpace(r.FormValue("node_id"))
	if strings.HasPrefix(nodeVal, "combo:") {
		s.createForwardFromCombo(w, r, u, nodeVal)
		return
	}
	nodeID, err := parseFormInt64(r, "node_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	targetIP, targetPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	comment := strings.TrimSpace(r.FormValue("comment"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !validMode(mode) {
		s.flashRedirect(w, r, "无效的转发模式", "/forwards")
		return
	}

	occupied, err := db.OccupiedPortsOnNode(s.DB, nodeID, proto, 0)
	if err != nil {
		s.flashRedirect(w, r, "端口检查失败: "+err.Error(), "/forwards")
		return
	}
	listenPortStr := strings.TrimSpace(r.FormValue("listen_port"))
	var listenPort int
	if listenPortStr == "" {
		listenPort = db.PickFreePort(db.ChainPortMin, db.ChainPortMax, occupied)
		if listenPort == 0 {
			s.flashRedirect(w, r, fmt.Sprintf("端口段 %d-%d 内已无可用端口", db.ChainPortMin, db.ChainPortMax), "/forwards")
			return
		}
	} else {
		listenPort, err = strconv.Atoi(listenPortStr)
		if err != nil || listenPort < 1 || listenPort > 65535 {
			s.flashRedirect(w, r, "监听端口非法", "/forwards")
			return
		}
		if occupied[listenPort] {
			s.flashRedirect(w, r, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort), "/forwards")
			return
		}
	}

	f := &db.Forward{
		NodeID:     nodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
		Mode:       mode,
	}
	testRule := nft.Rule{
		Proto:    proto,
		SrcPort:  listenPort,
		DestPort: targetPort,
		Mode:     mode,
	}
	if resolver.IsHostname(targetIP) {
		testRule.DestHost = targetIP
	} else {
		testRule.DestIP = targetIP
	}
	if err := nft.Validate(testRule); err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/forwards")
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.create", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", nodeID, proto, listenPort, targetIP, targetPort))
	s.dispatchAfterMutation(w, nodeID, "转发新增")
	http.Redirect(w, r, "/forwards", http.StatusSeeOther)
}

func (s *Server) editForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
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
	if f.ChainID.Valid {
		s.flashRedirect(w, r, "链路跳的转发请在链路详情页编辑", "/forwards")
		return
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("edit forward %d: list nodes: %v", id, err)
	}
	s.render(w, "forward_edit.html", map[string]any{
		"User":    u,
		"Forward": f,
		"Nodes":   nodes,
		"Flash":   flashFromCookie(w, r),
	})
}

func (s *Server) saveForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	redirect := fmt.Sprintf("/forwards/%d/edit", id)

	nodeID, err := parseFormInt64(r, "node_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	listenPort, err := parseFormInt(r, "listen_port")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	targetPort, err := parseFormInt(r, "target_port")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	comment := strings.TrimSpace(r.FormValue("comment"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !validMode(mode) {
		s.flashRedirect(w, r, "无效的转发模式", redirect)
		return
	}

	testRule := nft.Rule{Proto: proto, SrcPort: listenPort, DestPort: targetPort, Mode: mode}
	if resolver.IsHostname(targetIP) {
		testRule.DestHost = targetIP
	} else {
		testRule.DestIP = targetIP
	}
	if err := nft.Validate(testRule); err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}

	oldNodeID, err := db.UpdateForwardByID(s.DB, id, nodeID, proto, listenPort, targetIP, targetPort, comment, mode)
	if err != nil {
		s.flashRedirect(w, r, "保存失败: "+err.Error(), redirect)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.edit", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", nodeID, proto, listenPort, targetIP, targetPort))

	s.dispatchAfterMutation(w, nodeID, "转发编辑")
	if oldNodeID != nodeID {
		s.dispatchAfterMutation(w, oldNodeID, "转发迁出")
	}
	s.flashRedirect(w, r, "转发已保存", "/forwards")
}

func (s *Server) createForwardFromCombo(w http.ResponseWriter, r *http.Request, u *db.User, comboVal string) {
	comboID, err := strconv.ParseInt(comboVal[6:], 10, 64)
	if err != nil || comboID == 0 {
		s.flashRedirect(w, r, "组合通道 ID 无效", "/forwards")
		return
	}
	combo, err := db.GetTunnelCombo(s.DB, comboID)
	if err != nil {
		s.flashRedirect(w, r, "组合通道不存在", "/forwards")
		return
	}
	comboHops, err := db.ListComboHops(s.DB, comboID)
	if err != nil || len(comboHops) == 0 {
		s.flashRedirect(w, r, "组合通道为空", "/forwards")
		return
	}

	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if proto != "tcp" && proto != "udp" {
		s.flashRedirect(w, r, "协议须为 tcp 或 udp", "/forwards")
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}

	chainName := strings.TrimSpace(r.FormValue("chain_name"))
	if chainName == "" {
		chainName = combo.Name + "-" + exitHost
	}

	hops := make([]db.HopInput, 0, len(comboHops))
	for _, ch := range comboHops {
		tun, err := db.GetTunnel(s.DB, ch.TunnelID)
		if err != nil {
			s.flashRedirect(w, r, fmt.Sprintf("组合通道内的通道 %d 不存在", ch.TunnelID), "/forwards")
			return
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, Mode: ch.Mode})
	}
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	defer tx.Rollback()
	c := &db.Chain{Name: chainName, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		s.flashRedirect(w, r, "创建链路失败: "+err.Error(), "/forwards")
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	if err := tx.Commit(); err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create_from_combo", strconv.FormatInt(id, 10), chainName)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, "/forwards", http.StatusSeeOther)
}

func (s *Server) deleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	nodeID, err := db.DeleteForward(s.DB, id)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterMutation(w, nodeID, "转发删除")
	http.Redirect(w, r, "/forwards", http.StatusSeeOther)
}
