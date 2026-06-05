package server

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		log.Printf("list nodes: get panel_url: %v", err)
	}
	paged, pager := paginate(nodes, r)
	s.render(w, "nodes.html", map[string]any{
		"User":          u,
		"Nodes":         paged,
		"AllNodes":      nodes,
		"PanelURL":      panelURL,
		"ServerVersion": serverVersion(),
		"Pager":         pager,
		"Flash":         flashFromCookie(w, r),
	})
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	if name == "" {
		s.flashRedirect(w, r, "name 不能为空", "/nodes")
		return
	}
	// Remote nodes dial the panel in reverse (WebSocket, matched by token), so
	// the panel never stores a control-plane address for them.
	n, err := db.CreateNode(s.DB, name, "", secret)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/nodes")
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), name)
	s.dispatchAfterMutation(w, n.ID, "节点创建")
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", n.ID), http.StatusSeeOther)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	panelURL := strings.TrimSpace(r.FormValue("panel_url"))
	if err := db.SetSetting(s.DB, "panel_url", panelURL); err != nil {
		s.flashRedirect(w, r, "保存失败: "+err.Error(), "/nodes")
		return
	}
	db.WriteAudit(s.DB, u.ID, "settings.panel_url", panelURL, "")
	s.flashRedirect(w, r, "面板地址已保存", "/nodes")
}

func (s *Server) showNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	n, err := db.GetNode(s.DB, id)
	if err != nil {
		http.Error(w, "节点不存在", http.StatusNotFound)
		return
	}
	forwards, err := db.ListForwardsByNode(s.DB, n.ID)
	if err != nil {
		log.Printf("show node %d: list forwards: %v", n.ID, err)
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		log.Printf("show node %d: get panel_url: %v", n.ID, err)
	}
	panelConfigured := panelURL != ""
	if !panelConfigured {
		// Fall back to the host the admin is browsing on, so the install command
		// is copy-pasteable even before panel_url is set.
		panelURL = "https://" + r.Host
	}
	s.render(w, "node_detail.html", map[string]any{
		"User":               u,
		"Node":               n,
		"Forwards":           forwards,
		"PanelURL":           panelURL,
		"PanelURLConfigured": panelConfigured,
		"ServerVersion":      serverVersion(),
		"Flash":              flashFromCookie(w, r),
	})
}

func (s *Server) renameNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.flashRedirect(w, r, "名称不能为空", fmt.Sprintf("/nodes/%d", id))
		return
	}
	if err := db.RenameNode(s.DB, id, name); err != nil {
		s.flashRedirect(w, r, "重命名失败: "+err.Error(), fmt.Sprintf("/nodes/%d", id))
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.rename", strconv.FormatInt(id, 10), name)
	s.flashRedirect(w, r, "节点已重命名", fmt.Sprintf("/nodes/%d", id))
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Capture chains routing through this node before the delete cascades their
	// hop rows away; their upstream hops materialize this node's relay_host and
	// must be re-wired (around the gap) afterward.
	affectedChains, err := db.ChainsReferencingNode(s.DB, id)
	if err != nil {
		log.Printf("delete node %d: list affected chains: %v", id, err)
	}
	if err := db.DeleteNode(s.DB, id); err != nil {
		s.flashRedirect(w, r, err.Error(), "/nodes")
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
	s.rewireChainsAfterNodeChange(w, affectedChains, "节点删除，链路重连")
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) resyncNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.dispatchToNode(id); err != nil {
		s.flashRedirect(w, r, "重新同步失败: "+err.Error(), fmt.Sprintf("/nodes/%d", id))
		return
	}
	s.flashRedirect(w, r, "已触发重新同步", fmt.Sprintf("/nodes/%d", id))
}
