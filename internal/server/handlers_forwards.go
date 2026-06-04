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
	forwards, err := db.ListForwards(s.DB)
	if err != nil {
		log.Printf("list forwards: %v", err)
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("list forwards: list nodes: %v", err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	s.render(w, "forwards.html", map[string]any{
		"User":     u,
		"Forwards": forwards,
		"Nodes":    nodes,
		"NodeByID": nodeByID,
		"Flash":    flashFromCookie(w, r),
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
