package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
	"nft-forward/internal/wsproto"
)

// --- JSON helpers ---

func jsonOK(w http.ResponseWriter, data any) {
	if m, ok := data.(map[string]any); ok {
		ensureNonNilSlices(m)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ensureNonNilSlices replaces nil slices with empty slices so JSON
// serialization produces [] instead of null.
func ensureNonNilSlices(m map[string]any) {
	for k, v := range m {
		if v == nil {
			continue
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			m[k] = []any{}
		}
	}
}

// --- API auth middleware ---

func (s *Server) requireAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			jsonErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		u, err := db.GetSessionUser(s.DB, c.Value)
		if err != nil || u == nil {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
			jsonErr(w, http.StatusUnauthorized, "会话已过期")
			return
		}
		if u.Disabled {
			jsonErr(w, http.StatusForbidden, "账号已被禁用")
			return
		}
		ctx := r.Context()
		ctx = withUser(ctx, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- Dispatcher helpers for JSON handlers (no flash cookies) ---

func (s *Server) apiDispatch(nodeID int64) error {
	if s.Dispatcher == nil {
		return nil
	}
	return s.dispatchToNode(nodeID)
}

func (s *Server) apiDispatchFanout(nodeIDs []int64) {
	for _, n := range nodeIDs {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("api dispatch node %d: %v", n, err)
		}
	}
}

// --- Auth endpoints ---

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	u, err := db.GetUserByUsername(s.DB, body.Username)
	if err != nil {
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(body.Password)) != nil {
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "账号已被禁用")
		return
	}
	token, err := db.CreateSession(s.DB, u.ID, sessionTTL)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, MaxAge: int(sessionTTL.Seconds()),
	})
	db.WriteAudit(s.DB, u.ID, "login", "", "")
	jsonOK(w, map[string]any{"user": apiUserView(u)})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = db.DeleteSession(s.DB, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	jsonOK(w, map[string]any{"user": apiUserFullView(u)})
}

func (s *Server) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(body.OldPassword)) != nil {
		jsonErr(w, http.StatusBadRequest, "原密码不正确")
		return
	}
	if len(body.NewPassword) < 6 {
		jsonErr(w, http.StatusBadRequest, "新密码至少 6 位")
		return
	}
	if body.NewPassword == body.OldPassword {
		jsonErr(w, http.StatusBadRequest, "新密码与原密码相同")
		return
	}
	hash, err := HashPassword(body.NewPassword)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "哈希失败")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, u.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur, _ := r.Cookie(sessionCookie)
	if cur != nil {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, u.ID, cur.Value)
	}
	db.WriteAudit(s.DB, u.ID, "user.change_password", "", "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- Dashboard ---

func (s *Server) apiDashboard(w http.ResponseWriter, r *http.Request) {
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, nodes)
	rules, _ := db.ListAllRules(s.DB)
	db.FillRuleTraffic(s.DB, rules)
	users, _ := db.ListUsers(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{"nodes": nodes, "rules": rules, "users": users, "node_by_id": nodeByID})
}

// --- Nodes ---

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.ResolveCompositeOnline(s.DB, nodes)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	jsonOK(w, map[string]any{
		"nodes": nodes, "panel_url": panelURL,
		"server_version": serverVersion(),
	})
}

func (s *Server) apiCreateNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name     string `json:"name"`
		Secret   string `json:"secret"`
		NodeType string `json:"node_type"`
		Hops     []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		jsonErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	nodeType := strings.TrimSpace(body.NodeType)
	if nodeType == "composite" {
		// Create a composite node with hops
		if len(body.Hops) < 2 {
			jsonErr(w, http.StatusBadRequest, "组合节点至少需要 2 个子节点")
			return
		}
		n, err := db.CreateNode(s.DB, body.Name, "", "")
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Set node_type to composite
		if _, err := s.DB.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, n.ID); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		nodeHops := make([]db.NodeHop, len(body.Hops))
		for i, h := range body.Hops {
			mode := h.Mode
			if mode == "" {
				mode = "userspace"
			}
			nodeHops[i] = db.NodeHop{NodeID: n.ID, Position: i, HopNodeID: h.NodeID, Mode: mode}
		}
		if err := db.CreateNodeHops(s.DB, n.ID, nodeHops); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		n, _ = db.GetNode(s.DB, n.ID)
		db.WriteAudit(s.DB, u.ID, "node.create_composite", strconv.FormatInt(n.ID, 10), body.Name)
		jsonOK(w, map[string]any{"node": n})
		return
	}

	// Default: create a remote node
	n, err := db.CreateNode(s.DB, body.Name, "", strings.TrimSpace(body.Secret))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), body.Name)
	_ = s.apiDispatch(n.ID)
	jsonOK(w, map[string]any{"node": n})
}

func (s *Server) apiGetNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	n, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	ruleHops, _ := db.ListRuleHopsByNode(s.DB, n.ID)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")

	resp := map[string]any{
		"node": n, "rule_hops": ruleHops, "panel_url": panelURL,
		"panel_url_configured": panelURL != "",
		"server_version":       serverVersion(),
		"upgrade":              deriveUpgradeStatus(n, time.Now()),
	}

	// Include node_hops if composite, enriched with each child's name.
	if n.NodeType == "composite" {
		nodeHops, _ := db.ListNodeHops(s.DB, n.ID)
		all, _ := db.ListNodes(s.DB)
		byID := buildMap(all, func(nd *db.Node) int64 { return nd.ID })
		views := make([]nodeHopView, len(nodeHops))
		for i, h := range nodeHops {
			nm := ""
			if nd := byID[h.HopNodeID]; nd != nil {
				nm = nd.Name
			}
			views[i] = nodeHopView{NodeHop: h, NodeName: nm}
		}
		resp["node_hops"] = views
		// Online is aggregated from children; reuse the same node list.
		db.ResolveCompositeOnline(s.DB, all)
		for _, c := range all {
			if c.ID == n.ID {
				n.Online = c.Online
				break
			}
		}
	}

	jsonOK(w, resp)
}

func (s *Server) apiListNodeHops(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodeHops, err := db.ListNodeHops(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"node_hops": nodeHops})
}

func (s *Server) apiRenameNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		jsonErr(w, http.StatusBadRequest, "名称不能为空")
		return
	}
	if err := db.RenameNode(s.DB, id, body.Name); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.rename", strconv.FormatInt(id, 10), body.Name)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	var body struct {
		RelayHost string `json:"relay_host"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	host := strings.TrimSpace(body.RelayHost)
	if host != "" && !isValidRelayHost(host) {
		jsonErr(w, http.StatusBadRequest, "中继地址须为 IPv4 或域名")
		return
	}
	if err := db.UpdateNodeRelayHost(s.DB, id, host); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	ruleIDs, _ := db.RulesReferencingNode(s.DB, id)
	if len(ruleIDs) > 0 {
		s.apiRewireRules(ruleIDs)
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpdateNodePortRange(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if node.NodeType == "composite" {
		jsonErr(w, http.StatusBadRequest, "组合节点不支持设置端口范围")
		return
	}
	var body struct {
		PortRange string `json:"port_range"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	spec := strings.TrimSpace(body.PortRange)
	if err := db.ValidatePortRange(spec); err != nil {
		jsonErr(w, http.StatusBadRequest, "端口范围格式错误: "+err.Error())
		return
	}
	if err := db.UpdateNodePortRange(s.DB, id, spec); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_port_range", strconv.FormatInt(id, 10), spec)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResyncNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.dispatchToNode(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpgradeNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	loadSelfBinary()
	if selfBinaryErr != nil {
		jsonErr(w, http.StatusInternalServerError, selfBinaryErr.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	err = s.Hub.SendUpgrade(id, wsproto.Upgrade{
		Version: serverVersion(), SHA256: selfBinarySHA,
		Size: int64(len(selfBinaryBytes)), DownloadAt: panelURL + "/v1/binary",
		Data: selfBinaryBytes,
	})
	// Record the dispatch outcome so the node detail can surface a silent
	// failure later (an acked upgrade whose version never takes).
	status, errText := "acked", ""
	if err != nil {
		status, errText = "error", err.Error()
	}
	db.RecordUpgradeResult(s.DB, id, serverVersion(), status, errText)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade", strconv.FormatInt(id, 10), serverVersion())
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	affectedRules, _ := db.RulesReferencingNode(s.DB, id)
	if err := db.DeleteNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
	s.apiRewireRules(affectedRules)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiToggleNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if err := db.ToggleNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.toggle", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpdateNodeOwner(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	var body struct {
		OwnerID *int64 `json:"owner_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.OwnerID != nil {
		if _, err := db.GetUserByID(s.DB, *body.OwnerID); err != nil {
			jsonErr(w, http.StatusBadRequest, "用户不存在")
			return
		}
	}
	if err := db.UpdateNodeOwner(s.DB, id, body.OwnerID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ownerStr := "nil"
	if body.OwnerID != nil {
		ownerStr = strconv.FormatInt(*body.OwnerID, 10)
	}
	db.WriteAudit(s.DB, u.ID, "node.set_owner", strconv.FormatInt(id, 10), ownerStr)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResyncAllNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if err := s.dispatchToNode(n.ID); err != nil {
			fail++
		} else {
			ok++
		}
	}
	jsonOK(w, map[string]any{"ok": true, "synced": ok, "failed": fail})
}

func (s *Server) apiUpgradeAllNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	loadSelfBinary()
	if selfBinaryErr != nil {
		jsonErr(w, http.StatusInternalServerError, selfBinaryErr.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	upgrade := wsproto.Upgrade{
		Version: serverVersion(), SHA256: selfBinarySHA,
		Size: int64(len(selfBinaryBytes)), DownloadAt: panelURL + "/v1/binary",
		Data: selfBinaryBytes,
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if n.AgentVersion == serverVersion() {
			continue
		}
		err := s.Hub.SendUpgrade(n.ID, upgrade)
		status, errText := "acked", ""
		if err != nil {
			status, errText = "error", err.Error()
			fail++
		} else {
			ok++
		}
		db.RecordUpgradeResult(s.DB, n.ID, serverVersion(), status, errText)
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade_all", "", fmt.Sprintf("ok=%d fail=%d", ok, fail))
	jsonOK(w, map[string]any{"ok": true, "upgraded": ok, "failed": fail})
}

// --- Settings ---

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	jsonOK(w, map[string]any{"panel_url": panelURL})
}

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		PanelURL string `json:"panel_url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := db.SetSetting(s.DB, "panel_url", strings.TrimSpace(body.PanelURL)); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "settings.panel_url", strings.TrimSpace(body.PanelURL), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- Rules ---

func (s *Server) apiListRules(w http.ResponseWriter, r *http.Request) {
	var rules []*db.Rule
	if raw := r.URL.Query().Get("owner_ids"); raw != "" {
		var ids []int64
		for _, part := range strings.Split(raw, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		rules, _ = db.ListRulesByOwnerIDs(s.DB, ids)
	} else {
		rules, _ = db.ListAllRules(s.DB)
	}
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	allUsers, _ := db.ListUsers(s.DB)
	byID := make(map[int64]*db.User, len(allUsers))
	userList := make([]map[string]any, 0, len(allUsers))
	for _, u := range allUsers {
		byID[u.ID] = u
		userList = append(userList, map[string]any{"id": u.ID, "username": u.Username})
	}
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		oname := ""
		if rl.OwnerID.Valid {
			if u := byID[rl.OwnerID.Int64]; u != nil {
				oname = u.Username
			}
		}
		views = append(views, s.buildRuleListItem(rl, oname))
	}
	jsonOK(w, map[string]any{"rules": views, "nodes": nodes, "users": userList})
}

func (s *Server) apiCreateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
		Hops      []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || !validRuleProto(proto) {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp、udp 或 tcp+udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var hops []db.HopInput

	if len(body.Hops) > 0 {
		// Explicit hops provided
		hops = make([]db.HopInput, len(body.Hops))
		for i, h := range body.Hops {
			hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
		}
	} else if body.NodeID > 0 {
		// Check if node is composite => expand node_hops
		node, err := db.GetNode(s.DB, body.NodeID)
		if err != nil {
			jsonErr(w, http.StatusNotFound, "节点不存在")
			return
		}
		if node.NodeType == "composite" {
			nodeHops, _ := db.ListNodeHops(s.DB, body.NodeID)
			if len(nodeHops) == 0 {
				jsonErr(w, http.StatusBadRequest, "组合节点无子节点")
				return
			}
			hops = make([]db.HopInput, len(nodeHops))
			for i, nh := range nodeHops {
				hops[i] = db.HopInput{NodeID: nh.HopNodeID, Mode: nh.Mode}
			}
		} else {
			// Single-node rule: 1 hop
			hops = []db.HopInput{{NodeID: body.NodeID}}
		}
	} else {
		jsonErr(w, http.StatusBadRequest, "需指定 node_id 或 hops")
		return
	}

	if len(hops) == 0 {
		jsonErr(w, http.StatusBadRequest, "至少需要一跳")
		return
	}
	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}

	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()

	// Determine the rule header's node_id: use the entry hop's node when the
	// caller didn't supply an explicit node_id (e.g. when hops are given directly).
	ruleNodeID := body.NodeID
	if ruleNodeID == 0 && len(hops) > 0 {
		ruleNodeID = hops[0].NodeID
	}

	rl := &db.Rule{
		NodeID:   ruleNodeID,
		Name:     name,
		Proto:    proto,
		ExitHost: exitHost,
		ExitPort: exitPort,
		Comment:  strings.TrimSpace(body.Comment),
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rl.ID = id

	entry, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"rule": rl, "entry": entry})
}

func (s *Server) apiGetRule(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	hops, _ := db.ListRuleHops(s.DB, id)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	ownerName := ""
	if rl.OwnerID.Valid {
		if u, e := db.GetUserByID(s.DB, rl.OwnerID.Int64); e == nil {
			ownerName = u.Username
		}
	}
	jsonOK(w, map[string]any{
		"rule": s.buildRuleListItem(rl, ownerName), "hops": hops, "nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiUpdateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	var body struct {
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
		Hops      []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || !validRuleProto(proto) {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp、udp 或 tcp+udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Hops) == 0 {
		jsonErr(w, http.StatusBadRequest, "至少添加一个节点")
		return
	}
	hops := make([]db.HopInput, len(body.Hops))
	for i, h := range body.Hops {
		hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
	}
	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	rl.Name, rl.Proto, rl.ExitHost, rl.ExitPort = name, proto, exitHost, exitPort
	rl.Comment = strings.TrimSpace(body.Comment)
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if err := db.UpdateRuleHeader(tx, rl); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	entry, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.save", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "entry": entry})
}

func (s *Server) apiDeleteRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodes, err := db.DeleteRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.delete", strconv.FormatInt(id, 10), "")
	s.apiDispatchFanout(nodes)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiReallocateRuleHop(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	pos, _ := strconv.Atoi(chi.URLParam(r, "pos"))
	rl, err := db.GetRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	hops, err := db.ListRuleHops(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pos < 0 || pos >= len(hops) {
		jsonErr(w, http.StatusBadRequest, "跳序号非法")
		return
	}
	var body struct {
		Port int `json:"port"`
	}
	_ = decodeJSON(r, &body)

	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
	}
	var avoid map[int64]int
	if body.Port > 0 {
		inputs[pos].DesiredPort = body.Port
	} else {
		avoid = map[int64]int{hops[pos].NodeID: hops[pos].ListenPort}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateRule(tx, rl, inputs, avoid)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.reallocate", strconv.FormatInt(id, 10), strconv.Itoa(pos))
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true})
}

// --- Users ---

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := db.ListUsers(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.FillUserRuleCounts(s.DB, users)
	jsonOK(w, map[string]any{"users": users})
}

func (s *Server) apiGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, id)
	allNodes, _ := db.ListNodes(s.DB)
	rules, _ := db.ListRulesByUser(s.DB, id)
	db.FillRuleTraffic(s.DB, rules)
	jsonOK(w, map[string]any{
		"user": target, "nodes": grantedNodes,
		"grants": grants, "all_nodes": allNodes,
		"rules": rules,
	})
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Username          string `json:"username"`
		Password          string `json:"password"`
		Role              string `json:"role"`
		MaxForwards       int    `json:"max_forwards"`
		TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
		ExpiresAt         string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" || body.Password == "" {
		jsonErr(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}
	role := body.Role
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		jsonErr(w, http.StatusBadRequest, "角色须为 admin 或 user")
		return
	}
	hash, err := HashPassword(body.Password)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := db.CreateUser(s.DB, username, hash, role)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	maxFwd := body.MaxForwards
	if maxFwd <= 0 {
		maxFwd = 100
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
		maxFwd, body.TrafficQuotaBytes, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw := strings.TrimSpace(body.ExpiresAt); raw != "" {
		if et, err := time.Parse("2006-01-02", raw); err == nil {
			s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, et.Unix(), id)
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.create", strconv.FormatInt(id, 10), username)
	created, _ := db.GetUserByID(s.DB, id)
	jsonOK(w, map[string]any{"user": created})
}

func (s *Server) apiGrantNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		NodeID      int64 `json:"node_id"`
		MaxForwards int   `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards <= 0 {
		body.MaxForwards = 10
	}
	if err := db.GrantNode(s.DB, userID, body.NodeID, body.MaxForwards); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.grant_node", strconv.FormatInt(userID, 10), strconv.FormatInt(body.NodeID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiRevokeNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeID")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad node id")
		return
	}
	if err := db.RevokeNode(s.DB, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.revoke_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetUserQuota(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficQuotaBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "字节数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, body.TrafficQuotaBytes, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_quota_bytes", strconv.FormatInt(id, 10), strconv.FormatInt(body.TrafficQuotaBytes, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetUserExpiry(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	var expiresAt int64
	raw := strings.TrimSpace(body.ExpiresAt)
	if raw != "" {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "日期格式无效（需 YYYY-MM-DD）")
			return
		}
		expiresAt = t.Unix()
	}
	if _, err := s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, expiresAt, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_expiry", strconv.FormatInt(id, 10), raw)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetUserTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := db.ResetUserTraffic(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_traffic", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiToggleUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if target.ID == u.ID {
		jsonErr(w, http.StatusBadRequest, "不能禁用自己")
		return
	}
	willDisable := !target.Disabled
	reason := ""
	if willDisable {
		reason = "管理员手动禁用"
	}
	if err := db.SetUserDisabled(s.DB, id, willDisable, reason); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-dispatch either way: disabling drops this user's rules from the
	// affected nodes, enabling restores them.
	if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
		s.apiDispatchFanout(nodes)
	}
	db.WriteAudit(s.DB, u.ID, "user.toggle", strconv.FormatInt(id, 10), fmt.Sprintf("disabled=%v", willDisable))
	jsonOK(w, map[string]any{"ok": true, "disabled": willDisable})
}

func (s *Server) apiResetUserPassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	newPw := db.RandToken(8)
	hash, err := HashPassword(newPw)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	db.WriteAudit(s.DB, u.ID, "user.reset_password", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true, "new_password": newPw})
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if id == u.ID {
		jsonErr(w, http.StatusBadRequest, "不能删除自己")
		return
	}
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	affected, err := db.DeleteRulesForUser(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.DeleteUser(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.apiDispatchFanout(affected)
	db.WriteAudit(s.DB, u.ID, "user.delete", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- My (user self-service) endpoints ---

func (s *Server) apiMyDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"user": apiUserFullView(u), "nodes": grantedNodes, "grants": grants,
		"rules": rules, "all_nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiMyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		views = append(views, s.buildRuleListItem(rl, ""))
	}
	grantedNodes, _, _ := db.ListNodesForUser(s.DB, u.ID)
	jsonOK(w, map[string]any{
		"rules": views, "nodes": grantedNodes,
		"all_nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiMyCreateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		jsonErr(w, http.StatusForbidden, "用户已过期")
		return
	}
	var body struct {
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || !validRuleProto(proto) {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp、udp 或 tcp+udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.NodeID == 0 {
		jsonErr(w, http.StatusBadRequest, "node_id 不能为空")
		return
	}

	// Check node grant
	node, err := db.GetNode(s.DB, body.NodeID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}

	// The grant on the selected node both authorizes the request and carries
	// the per-node forward cap. A composite node is the unit of authorization:
	// granting it authorizes the whole chain, so the check is on the composite
	// itself, not its sub-nodes.
	grant, gerr := db.GetNodeGrant(s.DB, u.ID, body.NodeID)
	if gerr != nil {
		jsonErr(w, http.StatusForbidden, "无权使用该节点")
		return
	}

	var hops []db.HopInput
	if node.NodeType == "composite" {
		nodeHops, _ := db.ListNodeHops(s.DB, body.NodeID)
		if len(nodeHops) == 0 {
			jsonErr(w, http.StatusBadRequest, "组合节点无子节点")
			return
		}
		hops = make([]db.HopInput, len(nodeHops))
		for i, nh := range nodeHops {
			hops[i] = db.HopInput{NodeID: nh.HopNodeID, Mode: nh.Mode}
		}
	} else {
		hops = []db.HopInput{{NodeID: body.NodeID}}
	}

	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}

	// Per-node cap: counted by rule, so a composite rule counts once against
	// the composite node's grant.
	if cnt, _ := db.CountRulesForUserNode(s.DB, u.ID, body.NodeID); cnt+1 > grant.MaxForwards {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("超出该节点的转发上限（%d）", grant.MaxForwards))
		return
	}

	// Global per-user quota.
	if err := s.checkUserRuleQuota(u, len(hops), 0); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()

	rl := &db.Rule{
		NodeID:   body.NodeID,
		OwnerID:  nullInt64(u.ID),
		Name:     name,
		Proto:    proto,
		ExitHost: exitHost,
		ExitPort: exitPort,
		Comment:  strings.TrimSpace(body.Comment),
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rl.ID = id

	entry, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.user_create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "rule_id": id, "entry": entry})
}

func (s *Server) apiMyDeleteRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	if !rl.OwnerID.Valid || rl.OwnerID.Int64 != u.ID {
		jsonErr(w, http.StatusForbidden, "无权操作该规则")
		return
	}
	nodes, err := db.DeleteRule(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.user_delete", strconv.FormatInt(id, 10), "")
	s.apiDispatchFanout(nodes)
	jsonOK(w, map[string]any{"ok": true})
}

// --- Helpers ---

// withUser injects *db.User into ctx (same key as requireAuth).
func withUser(ctx context.Context, u *db.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// apiUserView returns the minimal user representation for login response.
func apiUserView(u *db.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
	}
}

// apiUserFullView includes quota/traffic/expiry fields for /api/me.
func apiUserFullView(u *db.User) map[string]any {
	m := apiUserView(u)
	m["max_forwards"] = u.MaxForwards
	m["traffic_quota_bytes"] = u.TrafficQuotaBytes
	m["traffic_used_bytes"] = u.TrafficUsedBytes
	if u.ExpiresAt.Valid {
		m["expires_at"] = u.ExpiresAt.Int64
	} else {
		m["expires_at"] = nil
	}
	if u.DisableReason.Valid {
		m["disable_reason"] = u.DisableReason.String
	} else {
		m["disable_reason"] = nil
	}
	return m
}

// apiRewireRules regenerates affected rules and dispatches.
func (s *Server) apiRewireRules(ruleIDs []int64) {
	seen := map[int64]bool{}
	var affected []int64
	for _, rid := range ruleIDs {
		aff, err := s.regenerateRuleByID(rid)
		if err != nil {
			log.Printf("api rewire rule %d: %v", rid, err)
			continue
		}
		for _, n := range aff {
			if !seen[n] {
				seen[n] = true
				affected = append(affected, n)
			}
		}
	}
	s.apiDispatchFanout(affected)
}

// toInt64 converts various JSON number representations to int64.
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}

// isValidRelayHost checks that a string is a valid IPv4 or hostname.
func isValidRelayHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	return resolver.IsHostname(host)
}
