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

	const perPage = 10
	total := len(allForwards)
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > total {
		end = total
	}
	forwards := allForwards[start:end]

	s.render(w, "forwards.html", map[string]any{
		"User":       u,
		"Forwards":   forwards,
		"Nodes":      nodes,
		"NodeByID":   nodeByID,
		"HopInfo":    hopInfo,
		"Total":      total,
		"Page":       page,
		"TotalPages": totalPages,
		"Flash":      flashFromCookie(w, r),
	})
}

func (s *Server) createForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodeID, err := parseFormInt64(r, "node_id")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	listenPort, err := parseFormInt(r, "listen_port")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	targetPort, err := parseFormInt(r, "target_port")
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/forwards")
		return
	}
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	comment := strings.TrimSpace(r.FormValue("comment"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !validMode(mode) {
		s.flashRedirect(w, r, "无效的转发模式", "/forwards")
		return
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
	occupied, err := db.OccupiedPortsOnNode(s.DB, nodeID, proto, 0)
	if err != nil {
		s.flashRedirect(w, r, "端口检查失败: "+err.Error(), "/forwards")
		return
	}
	if occupied[listenPort] {
		s.flashRedirect(w, r, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort), "/forwards")
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
