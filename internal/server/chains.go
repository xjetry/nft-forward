package server

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
)

// chainView is the per-chain row the list/detail templates render.
type chainView struct {
	Chain *db.Chain
	Path  string // "gomami → nnc-hk → seednet:8443"
	Entry string // "1.1.1.1:20000" or "—"
}

// buildChainView assembles the display path + entry endpoint for a chain.
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
	names = append(names, fmt.Sprintf("%s:%d", c.ExitHost, c.ExitPort))
	entry := "—"
	if c.EntryNodeID.Valid && c.EntryListenPort > 0 {
		if n, err := db.GetNode(s.DB, c.EntryNodeID.Int64); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(c.EntryListenPort))
		}
	}
	return chainView{Chain: c, Path: strings.Join(names, " → "), Entry: entry}
}

func (s *Server) listChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	chains, _ := db.ListAdminChains(s.DB)
	views := make([]chainView, 0, len(chains))
	for _, c := range chains {
		views = append(views, s.buildChainView(c))
	}
	s.render(w, "chains.html", map[string]any{"User": u, "Chains": views, "Flash": flashFromCookie(w, r)})
}

func (s *Server) newChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, _ := db.ListNodes(s.DB)
	s.render(w, "chain_form.html", map[string]any{
		"User": u, "Nodes": nodes, "Chain": nil, "Hops": nil, "Flash": flashFromCookie(w, r),
	})
}

// parseExit splits an "host:port" exit string. host may be IPv4 or hostname.
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

// adminHopInputs reads the ordered hop_node[] + hop_mode[] arrays the builder
// posts into structural HopInputs (no tunnel for admin chains).
func adminHopInputs(r *http.Request) ([]db.HopInput, error) {
	nodeIDs := r.Form["hop_node"]
	modes := r.Form["hop_mode"]
	if len(nodeIDs) == 0 {
		return nil, fmt.Errorf("至少添加一个节点")
	}
	hops := make([]db.HopInput, 0, len(nodeIDs))
	for i, idStr := range nodeIDs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("第 %d 跳节点非法", i+1)
		}
		mode := "kernel"
		if i < len(modes) {
			mode = modes[i]
		}
		hops = append(hops, db.HopInput{NodeID: id, Mode: mode})
	}
	return hops, nil
}

func (s *Server) createChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	c := &db.Chain{Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		tx.Rollback()
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) showChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	hops, _ := db.ListChainHops(s.DB, id)
	forwards, _ := db.ListForwardsByChain(s.DB, id)
	fwByNode := map[int64]*db.Forward{}
	for _, f := range forwards {
		fwByNode[f.NodeID] = f
	}
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "chain_detail.html", map[string]any{
		"User": u, "View": s.buildChainView(c), "Chain": c,
		"Hops": hops, "FwByNode": fwByNode, "NodeByID": nodeByID,
		"Nodes": nodes, "Flash": flashFromCookie(w, r),
	})
}

func (s *Server) saveChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	c.Name, c.Proto, c.ExitHost, c.ExitPort = name, proto, exitHost, exitPort

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err := db.UpdateChainHeader(tx, c); err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.save", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已保存，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路保存")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) deleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterFanout(w, nodes, "链路删除")
	http.Redirect(w, r, "/chains", http.StatusSeeOther)
}

// reallocateHop forces one hop off its current port (used when the daemon
// reports a cross-segment 409 or a userspace bind failure on that node) and
// re-dispatches.
func (s *Server) reallocateHop(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	pos, _ := strconv.Atoi(chi.URLParam(r, "pos"))
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	hops, _ := db.ListChainHops(s.DB, id)
	if pos < 0 || pos >= len(hops) {
		setFlash(w, "跳序号非法")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	avoid := map[int64]int{hops[pos].NodeID: hops[pos].ListenPort}

	tx, _ := s.DB.Begin()
	_, affected, err := db.RegenerateChain(tx, c, inputs, avoid)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	tx.Commit()
	db.WriteAudit(s.DB, u.ID, "chain.reallocate", strconv.FormatInt(id, 10), strconv.Itoa(pos))
	setFlash(w, "已为该跳重新分配端口")
	s.dispatchAfterFanout(w, affected, "链路端口重分配")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

// setNodeRelayHost saves a node's data-plane address from the node detail page.
func (s *Server) setNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	host := strings.TrimSpace(r.FormValue("relay_host"))
	if host != "" && net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		setFlash(w, "中继地址须为 IPv4 或域名")
		http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
		return
	}
	if err := db.UpdateNodeRelayHost(s.DB, id, host); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	setFlash(w, "中继地址已更新")
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
}
