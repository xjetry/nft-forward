package server

import (
	"fmt"
	"log"
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
	Chain       *db.Chain
	Path        string // "gomami → nnc-hk → seednet:8443"
	Entry       string // "1.1.1.1:20000" or "—"
	EntryNodeID int64  // entry node for remote probe
	TenantName  string // owning tenant display name, empty for admin chains
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
	names = append(names, net.JoinHostPort(c.ExitHost, strconv.Itoa(c.ExitPort)))
	entry := "—"
	if c.EntryNodeID.Valid && c.EntryListenPort > 0 {
		if n, err := db.GetNode(s.DB, c.EntryNodeID.Int64); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(c.EntryListenPort))
		}
	}
	var entryNodeID int64
	if c.EntryNodeID.Valid {
		entryNodeID = c.EntryNodeID.Int64
	}
	return chainView{Chain: c, Path: strings.Join(names, " → "), Entry: entry, EntryNodeID: entryNodeID}
}

func (s *Server) listChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	chains, err := db.ListAllChains(s.DB)
	if err != nil {
		log.Printf("list chains: %v", err)
	}
	tenants, _ := db.TenantsByID(s.DB)
	views := make([]chainView, 0, len(chains))
	for _, c := range chains {
		v := s.buildChainView(c)
		if c.TenantID.Valid {
			if t := tenants[c.TenantID.Int64]; t != nil {
				v.TenantName = t.Name
			}
		}
		views = append(views, v)
	}
	nodes, _ := db.ListNodes(s.DB)
	combos, _ := db.ListTunnelCombos(s.DB)
	s.render(w, "chains.html", map[string]any{"User": u, "Chains": views, "Nodes": nodes, "Combos": combos, "Flash": flashFromCookie(w, r)})
}

func (s *Server) newChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("new chain form: list nodes: %v", err)
	}
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

// adminHopInputs reads the ordered hop_node[] + hop_mode[] arrays.
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
		s.flashRedirect(w, r, "表单解析失败", "/chains/new")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		s.flashRedirect(w, r, "名称必填，协议须为 tcp 或 udp", "/chains/new")
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains/new")
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains/new")
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains/new")
		return
	}
	defer tx.Rollback()
	c := &db.Chain{Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		s.flashRedirect(w, r, "创建失败: "+err.Error(), "/chains/new")
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains/new")
		return
	}
	if err := tx.Commit(); err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains/new")
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) showChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	hops, err := db.ListChainHops(s.DB, id)
	if err != nil {
		log.Printf("show chain %d: list hops: %v", id, err)
	}
	forwards, err := db.ListForwardsByChain(s.DB, id)
	if err != nil {
		log.Printf("show chain %d: list forwards: %v", id, err)
	}
	fwByNode := buildMap(forwards, func(f *db.Forward) int64 { return f.NodeID })
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("show chain %d: list nodes: %v", id, err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	s.render(w, "chain_detail.html", map[string]any{
		"User": u, "View": s.buildChainView(c), "Chain": c,
		"Hops": hops, "FwByNode": fwByNode, "NodeByID": nodeByID,
		"Nodes": nodes, "Flash": flashFromCookie(w, r),
	})
}

func (s *Server) saveChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	redirect := fmt.Sprintf("/chains/%d", id)
	if err := r.ParseForm(); err != nil {
		s.flashRedirect(w, r, "表单解析失败", redirect)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		s.flashRedirect(w, r, "名称必填，协议须为 tcp 或 udp", redirect)
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	c.Name, c.Proto, c.ExitHost, c.ExitPort = name, proto, exitHost, exitPort

	tx, err := s.DB.Begin()
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	defer tx.Rollback()
	if err := db.UpdateChainHeader(tx, c); err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	if err := tx.Commit(); err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.save", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已保存，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路保存")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) deleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/chains")
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
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	pos, _ := strconv.Atoi(chi.URLParam(r, "pos"))
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	redirect := fmt.Sprintf("/chains/%d", id)
	hops, err := db.ListChainHops(s.DB, id)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	if pos < 0 || pos >= len(hops) {
		s.flashRedirect(w, r, "跳序号非法", redirect)
		return
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	avoid := map[int64]int{hops[pos].NodeID: hops[pos].ListenPort}

	tx, err := s.DB.Begin()
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateChain(tx, c, inputs, avoid)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	if err := tx.Commit(); err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.reallocate", strconv.FormatInt(id, 10), strconv.Itoa(pos))
	setFlash(w, "已为该跳重新分配端口")
	s.dispatchAfterFanout(w, affected, "链路端口重分配")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

// regenerateChainByID re-materializes one chain from its CURRENT (surviving)
// hops and returns the nodes whose kernel state must be re-dispatched. A chain
// with no hops left (its only node was just deleted) is a no-op.
func (s *Server) regenerateChainByID(chainID int64) ([]int64, error) {
	c, err := db.GetChain(s.DB, chainID)
	if err != nil {
		return nil, err
	}
	hops, err := db.ListChainHops(s.DB, chainID)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return nil, nil
	}
	// Tenant chains: a shrunk hop set (a relay node was removed) can promote a
	// different tunnel to the last hop, whose CIDR allowlist may not permit the
	// tenant's chosen exit. Re-validate and tear the chain down rather than route
	// to a now-disallowed destination. (relay_host edits don't change the hop set,
	// and tunnel CIDRs are immutable, so this is a no-op outside the shrink case.)
	if c.TenantID.Valid {
		if lastTun, terr := db.GetTunnel(s.DB, hops[len(hops)-1].TunnelID.Int64); terr == nil {
			if cerr := exitAllowedByTunnel(lastTun, c.ExitHost); cerr != nil {
				nodes, derr := db.DeleteChain(s.DB, chainID)
				if derr != nil {
					return nil, derr
				}
				log.Printf("chain %d (tenant %d) removed after node change: exit %s no longer allowed by last-hop tunnel %d: %v",
					chainID, c.TenantID.Int64, c.ExitHost, lastTun.ID, cerr)
				return nodes, nil
			}
		}
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateChain(tx, c, inputs, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}

// rewireChainsAfterNodeChange regenerates the given chains (their upstream hops
// materialize a peer's relay_host, so a node address change or removal must
// re-wire them) and re-dispatches the touched nodes. Best-effort per chain.
func (s *Server) rewireChainsAfterNodeChange(w http.ResponseWriter, chainIDs []int64, action string) {
	if len(chainIDs) == 0 {
		return
	}
	seen := map[int64]bool{}
	var affected []int64
	for _, cid := range chainIDs {
		aff, err := s.regenerateChainByID(cid)
		if err != nil {
			log.Printf("rewire chain %d after %s: %v", cid, action, err)
			continue
		}
		for _, n := range aff {
			if !seen[n] {
				seen[n] = true
				affected = append(affected, n)
			}
		}
	}
	if len(affected) > 0 {
		s.dispatchAfterFanout(w, affected, action)
	}
}

// setNodeRelayHost saves a node's data-plane address from the node detail page.
func (s *Server) setNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		http.Error(w, "节点不存在", http.StatusNotFound)
		return
	}
	redirect := fmt.Sprintf("/nodes/%d", id)
	host := strings.TrimSpace(r.FormValue("relay_host"))
	if host != "" && net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		s.flashRedirect(w, r, "中继地址须为 IPv4 或域名", redirect)
		return
	}
	if err := db.UpdateNodeRelayHost(s.DB, id, host); err != nil {
		s.flashRedirect(w, r, err.Error(), redirect)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	chains, err := db.ChainsReferencingNode(s.DB, id)
	if err != nil {
		log.Printf("set relay host %d: list affected chains: %v", id, err)
	}
	setFlash(w, "中继地址已更新")
	s.rewireChainsAfterNodeChange(w, chains, "中继地址变更，链路重连")
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
}
