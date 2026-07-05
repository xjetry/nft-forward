package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"nft-forward/internal/landing"
	"nft-forward/internal/resolver"
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

// jsonRegenerateErr reports a RegenerateRule error over HTTP. A duplicate
// physical node in the resolved chain is a conflict between the entry/via
// selections the caller picked, not a malformed request, so it maps to 409;
// every other RegenerateRule failure (bad relay host, exhausted ports, ...)
// stays 400.
func jsonRegenerateErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, db.ErrDuplicateChainNode) {
		code = http.StatusConflict
	}
	jsonErr(w, code, err.Error())
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

func (s *Server) requireTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			jsonErr(w, http.StatusUnauthorized, "缺少 API Token")
			return
		}
		u, t, err := db.GetUserByAPIToken(s.DB, token)
		if err != nil {
			jsonErr(w, http.StatusUnauthorized, "无效的 API Token")
			return
		}
		if t.Disabled {
			jsonErr(w, http.StatusForbidden, "Token 已停用")
			return
		}
		if u.Disabled {
			jsonErr(w, http.StatusForbidden, "账号已被禁用")
			return
		}
		db.TouchAPITokenUsage(s.DB, t.ID)
		ctx := withUser(r.Context(), u)
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
	limitKey := clientIP(r) + "\x00" + body.Username
	if s.loginLimiter != nil && !s.loginLimiter.allowed(limitKey) {
		jsonErr(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试")
		return
	}
	u, err := db.GetUserByUsername(s.DB, body.Username)
	if err != nil {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(limitKey)
		}
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(body.Password)) != nil {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(limitKey)
		}
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "账号已被禁用")
		return
	}
	if s.loginLimiter != nil {
		s.loginLimiter.recordSuccess(limitKey)
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

func (s *Server) apiBranding(w http.ResponseWriter, r *http.Request) {
	panelName, _ := db.GetSetting(s.DB, "panel_name")
	jsonOK(w, map[string]any{"panel_name": panelName})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	panelName, _ := db.GetSetting(s.DB, "panel_name")
	jsonOK(w, map[string]any{"user": apiUserFullView(u), "panel_name": panelName, "version": serverVersion()})
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

func (s *Server) apiChangeUsername(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Username string `json:"username"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		jsonErr(w, http.StatusBadRequest, "用户名不能为空")
		return
	}
	if username == u.Username {
		jsonOK(w, map[string]any{"ok": true})
		return
	}
	existing, _ := db.GetUserByUsername(s.DB, username)
	if existing != nil {
		jsonErr(w, http.StatusConflict, "用户名已存在")
		return
	}
	if err := db.RenameUser(s.DB, u.ID, username); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.rename", strconv.FormatInt(u.ID, 10), username)
	jsonOK(w, map[string]any{"ok": true, "username": username})
}

// --- Dashboard ---

func (s *Server) apiDashboard(w http.ResponseWriter, r *http.Request) {
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, nodes)
	nodeTraffic, _ := db.NodeTrafficSums(s.DB)
	// The dashboard only shows aggregates over rules/users, so compute them
	// server-side instead of shipping the full rules and users arrays (plus a
	// node_by_id map the UI never read) on every load.
	ruleCount, _ := db.CountAllRules(s.DB)
	ruleCountByNode, _ := db.RuleCountByNode(s.DB)
	totalBytes, _ := db.TotalRuleTrafficBytes(s.DB)
	userCount, _ := db.CountUsers(s.DB)
	jsonOK(w, map[string]any{
		"nodes":              nodes,
		"node_traffic":       nodeTraffic,
		"rule_count":         ruleCount,
		"rule_count_by_node": ruleCountByNode,
		"total_bytes":        totalBytes,
		"user_count":         userCount,
	})
}

// --- Nodes ---

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.ResolveCompositeOnline(s.DB, nodes)
	db.ResolveCompositeRelayStack(s.DB, nodes)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	panelName, _ := db.GetSetting(s.DB, "panel_name")
	showRate, _ := db.GetSetting(s.DB, "show_rate_to_user")
	nodeTraffic, _ := db.NodeTrafficSums(s.DB)
	nodeRawTraffic, _ := db.NodeRawTraffic(s.DB)
	// A load error leaves these maps nil, which JSON-encodes as null and would
	// bypass the frontend's destructuring defaults (they only cover undefined);
	// an empty map degrades the column to zeros instead of crashing the page.
	if nodeTraffic == nil {
		nodeTraffic = map[int64]int64{}
	}
	if nodeRawTraffic == nil {
		nodeRawTraffic = map[int64]int64{}
	}
	jsonOK(w, map[string]any{
		"nodes": nodes, "panel_url": panelURL, "panel_name": panelName,
		"node_traffic":         nodeTraffic,
		"node_raw_traffic":     nodeRawTraffic,
		"latest_agent_version": serverVersion(),
		"show_rate_to_user":    showRate == "1",
	})
}

// defaultGrantMaxForwards is the per-node forward cap applied when a grant is
// created without an explicit value.
const defaultGrantMaxForwards = 10

// grantInitialUsers grants the given users access to a freshly created node
// with the same defaults the per-user grant endpoint applies (max_forwards
// fallback, quota inherited from the global user quota). Grant failures do
// not fail node creation; the grants can be re-applied from the user pages.
func (s *Server) grantInitialUsers(actorID, nodeID int64, userIDs []int64) {
	for _, uid := range userIDs {
		if err := db.GrantNode(s.DB, uid, nodeID, defaultGrantMaxForwards, 0); err != nil {
			log.Printf("grant user %d on new node %d: %v", uid, nodeID, err)
			continue
		}
		db.WriteAudit(s.DB, actorID, "user.grant_node", strconv.FormatInt(uid, 10), strconv.FormatInt(nodeID, 10))
	}
}

func (s *Server) apiCreateNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name           string  `json:"name"`
		Secret         string  `json:"secret"`
		NodeType       string  `json:"node_type"`
		PortRange      string  `json:"port_range"`
		RateMultiplier float64 `json:"rate_multiplier"`
		Unidirectional bool    `json:"unidirectional"`
		UserIDs        []int64 `json:"user_ids"`
		Hops           []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
			// TrafficMultiplier is deprecated and ignored: a composite's
			// billing multiplier is its own rate_multiplier column now
			// (set via the top-level RateMultiplier field), not a per-hop
			// sum. The field stays so older clients still get a valid
			// decode.
			TrafficMultiplier *float64 `json:"traffic_multiplier"`
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
		childIDs := make([]int64, len(body.Hops))
		for i, h := range body.Hops {
			childIDs[i] = h.NodeID
		}
		if err := s.validateCompositeChildren(childIDs); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
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
			nodeHops[i] = db.NodeHop{NodeID: n.ID, Position: i, HopNodeID: h.NodeID, Mode: mode, TrafficMultiplier: 0}
		}
		if err := db.CreateNodeHops(s.DB, n.ID, nodeHops); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Create can't distinguish an unset multiplier from an explicit 0: an
		// absent JSON field decodes to 0 too. So 0 keeps the default 1.0 here;
		// configuring a node as free (0) is done afterward via the dedicated
		// rate-multiplier endpoint, which alone treats 0 as the free marker.
		if body.RateMultiplier > 0 && body.RateMultiplier != 1.0 {
			_ = db.UpdateNodeRateMultiplier(s.DB, n.ID, body.RateMultiplier)
		}
		n, _ = db.GetNode(s.DB, n.ID)
		db.WriteAudit(s.DB, u.ID, "node.create_composite", strconv.FormatInt(n.ID, 10), body.Name)
		s.grantInitialUsers(u.ID, n.ID, body.UserIDs)
		jsonOK(w, map[string]any{"node": n})
		return
	}

	// Default: create a remote node
	n, err := db.CreateNode(s.DB, body.Name, "", strings.TrimSpace(body.Secret))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Absent field and explicit 0 are indistinguishable on create, so 0 keeps
	// the default 1.0; free (0) is set later via the dedicated endpoint.
	if body.RateMultiplier > 0 && body.RateMultiplier != 1.0 {
		_ = db.UpdateNodeRateMultiplier(s.DB, n.ID, body.RateMultiplier)
	}
	if body.Unidirectional {
		_ = db.UpdateNodeUnidirectional(s.DB, n.ID, true)
	}
	if body.PortRange != "" {
		if err := db.ValidatePortRange(body.PortRange); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := db.UpdateNodePortRange(s.DB, n.ID, body.PortRange); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.RateMultiplier > 0 || body.PortRange != "" || body.Unidirectional {
		n, _ = db.GetNode(s.DB, n.ID)
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), body.Name)
	s.grantInitialUsers(u.ID, n.ID, body.UserIDs)
	_ = s.apiDispatch(n.ID)
	jsonOK(w, map[string]any{"node": n})
}

// nodeWithSecret re-exposes a node's secret on the admin node-detail response
// only. db.Node.Secret is json:"-" so it never leaks elsewhere; the embedded
// pointer promotes every other node field, and the explicit Secret field (at
// depth 0) shadows the hidden embedded one, serializing back as "secret" for
// the install-command view.
type nodeWithSecret struct {
	*db.Node
	Secret string `json:"secret"`
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
	if n.NodeType == "composite" {
		compositeHops, _ := db.ListRuleHopsByCompositeNode(s.DB, n.ID)
		ruleHops = append(ruleHops, compositeHops...)
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")

	type ruleHopView struct {
		*db.RuleHop
		TotalHops  int    `json:"total_hops"`
		NodeType   string `json:"node_type"`
		RuleNodeID int64  `json:"rule_node_id"`
		OwnerID    int64  `json:"owner_id,omitempty"`
		OwnerName  string `json:"owner_name,omitempty"`
	}
	views := make([]ruleHopView, len(ruleHops))
	type ruleMeta struct {
		hopCount  int
		nodeType  string
		nodeID    int64
		ownerID   int64
		ownerName string
	}
	metaCache := make(map[int64]*ruleMeta)
	for _, rh := range ruleHops {
		if _, ok := metaCache[rh.RuleID]; !ok {
			m := &ruleMeta{}
			s.DB.QueryRow(`SELECT COUNT(*) FROM rule_hops WHERE rule_id=?`, rh.RuleID).Scan(&m.hopCount)
			s.DB.QueryRow(`SELECT COALESCE(n.node_type,'single'), r.node_id FROM rules r JOIN nodes n ON n.id=r.node_id WHERE r.id=?`, rh.RuleID).Scan(&m.nodeType, &m.nodeID)
			var ownerID sql.NullInt64
			s.DB.QueryRow(`SELECT owner_id FROM rules WHERE id=?`, rh.RuleID).Scan(&ownerID)
			if ownerID.Valid {
				m.ownerID = ownerID.Int64
				if u, e := db.GetUserByID(s.DB, ownerID.Int64); e == nil {
					m.ownerName = u.Username
				}
			}
			metaCache[rh.RuleID] = m
		}
	}
	for i, rh := range ruleHops {
		m := metaCache[rh.RuleID]
		views[i] = ruleHopView{RuleHop: rh, TotalHops: m.hopCount, NodeType: m.nodeType, RuleNodeID: m.nodeID, OwnerID: m.ownerID, OwnerName: m.ownerName}
	}

	grantedUsers, _ := db.ListUsersForNode(s.DB, n.ID)
	resp := map[string]any{
		"node": nodeWithSecret{Node: n, Secret: n.Secret}, "rule_hops": views, "panel_url": panelURL,
		"panel_url_configured": panelURL != "",
		"latest_agent_version": serverVersion(),
		"upgrade":              deriveUpgradeStatus(n, serverVersion(), time.Now()),
		"granted_users":        grantedUsers,
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
		singleNodes := make([]*db.Node, 0)
		for _, nd := range all {
			if nd.NodeType != "composite" {
				singleNodes = append(singleNodes, nd)
			}
		}
		resp["single_nodes"] = singleNodes
		// Online is aggregated from children; reuse the same node list.
		db.ResolveCompositeOnline(s.DB, all)
		db.ResolveCompositeRelayStack(s.DB, all)
		for _, c := range all {
			if c.ID == n.ID {
				n.Online = c.Online
				n.EntryRelayHost = c.EntryRelayHost
				n.EntryRelayHostV6 = c.EntryRelayHostV6
				n.ExitRelayHostV6 = c.ExitRelayHostV6
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

// apiUpdateNodeHops replaces a composite node's hop chain (membership, order
// and per-hop mode). The config modes shape rules subsequently created on the
// composite, and only for the inter-node segments: the exit segment's mode
// belongs to the rule (see hopsForChain), so the stored last-hop mode stays
// dormant until a reorder turns that hop into an inter-node one. Existing
// rules keep the modes captured when they were expanded. The multiplier is no
// longer a per-hop attribute — a composite's billing multiplier is its own
// rate_multiplier column, so hop rows carry a dormant 0 here.
func (s *Server) apiUpdateNodeHops(w http.ResponseWriter, r *http.Request) {
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
	if node.NodeType != "composite" {
		jsonErr(w, http.StatusBadRequest, "非组合节点")
		return
	}
	type hopUpdate struct {
		NodeID int64  `json:"node_id"`
		Mode   string `json:"mode"`
		// TrafficMultiplier is deprecated and ignored: a composite's billing
		// multiplier is its own rate_multiplier column now, not a per-hop
		// sum. The field stays so older clients still get a valid decode.
		TrafficMultiplier float64 `json:"traffic_multiplier"`
	}
	var body struct {
		Hops []hopUpdate `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Hops) < 2 {
		jsonErr(w, http.StatusBadRequest, "组合节点至少需要 2 个子节点")
		return
	}
	childIDs := make([]int64, len(body.Hops))
	for i, hu := range body.Hops {
		if hu.NodeID == 0 {
			jsonErr(w, http.StatusBadRequest, "子节点 ID 不能为空")
			return
		}
		childIDs[i] = hu.NodeID
	}
	if err := s.validateCompositeChildren(childIDs); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hops := make([]db.NodeHop, len(body.Hops))
	for i, hu := range body.Hops {
		mode := strings.ToLower(strings.TrimSpace(hu.Mode))
		if mode == "" {
			mode = "userspace"
		}
		if mode != "kernel" && mode != "userspace" {
			jsonErr(w, http.StatusBadRequest, "转发模式必须为 kernel 或 userspace")
			return
		}
		hops[i] = db.NodeHop{NodeID: id, Position: i, HopNodeID: hu.NodeID, Mode: mode, TrafficMultiplier: 0}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if err := db.DeleteNodeHops(tx, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.CreateNodeHops(tx, id, hops); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.update_hops", strconv.FormatInt(id, 10), node.Name)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpdateNodeRolesMask(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Roles int64 `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Roles&^(db.NodeRoleEntry|db.NodeRoleVia) != 0 || body.Roles == 0 {
		jsonErr(w, http.StatusBadRequest, "roles invalid")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "node not found")
		return
	}
	if err := db.UpdateNodeRoles(s.DB, id, body.Roles); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.roles", strconv.FormatInt(id, 10), strconv.FormatInt(body.Roles, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiListNodeBindings(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	bs, err := db.ListBindingsForDownstream(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

func (s *Server) apiListAllNodeBindings(w http.ResponseWriter, r *http.Request) {
	bs, err := db.ListAllNodeBindings(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

func (s *Server) apiUpdateNodeBindings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "node not found")
		return
	}
	if node.Roles&db.NodeRoleVia == 0 {
		jsonErr(w, http.StatusBadRequest, "node is not a middle layer; enable the middle layer role first")
		return
	}
	var body struct {
		Bindings []struct {
			UpstreamNodeID int64  `json:"upstream_node_id"`
			Mode           string `json:"mode"`
		} `json:"bindings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request format")
		return
	}
	bs := make([]db.NodeBinding, len(body.Bindings))
	seenUpstream := make(map[int64]bool, len(body.Bindings))
	for i, b := range body.Bindings {
		if b.UpstreamNodeID == id {
			jsonErr(w, http.StatusBadRequest, "cannot bind to self")
			return
		}
		if seenUpstream[b.UpstreamNodeID] {
			jsonErr(w, http.StatusBadRequest, "上游节点重复")
			return
		}
		seenUpstream[b.UpstreamNodeID] = true
		if _, err := db.GetNode(s.DB, b.UpstreamNodeID); err != nil {
			jsonErr(w, http.StatusBadRequest, "upstream node not found")
			return
		}
		// The schema and the design for junction segments both default a
		// binding edge to userspace; only NormalizeForwardMode's caller-wide
		// fallback is kernel, which would silently override that default for
		// a row that omitted mode.
		mode := strings.ToLower(strings.TrimSpace(b.Mode))
		if mode == "" {
			mode = "userspace"
		}
		if mode != "kernel" && mode != "userspace" {
			jsonErr(w, http.StatusBadRequest, "转发模式必须为 kernel 或 userspace")
			return
		}
		bs[i] = db.NodeBinding{UpstreamNodeID: b.UpstreamNodeID, DownstreamNodeID: id, Mode: mode}
	}
	if err := db.ReplaceBindingsForDownstream(s.DB, id, bs); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.bindings", strconv.FormatInt(id, 10), fmt.Sprintf("%d edges", len(bs)))
	jsonOK(w, map[string]any{"ok": true})
}

// apiListNodeDownstreamBindings lists the edges where this node is the upstream
// (the nodes cascading in behind it). Admin only.
func (s *Server) apiListNodeDownstreamBindings(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	bs, err := db.ListBindingsForUpstream(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

// apiUpdateNodeDownstreamBindings replaces the whole set of edges where this
// node is the upstream. The node must be able to act as an upstream (entry or
// via), and every named downstream must carry the via role so it can sit behind
// this one as a middle layer. Admin only.
func (s *Server) apiUpdateNodeDownstreamBindings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "node not found")
		return
	}
	if node.Roles&(db.NodeRoleEntry|db.NodeRoleVia) == 0 {
		jsonErr(w, http.StatusBadRequest, "node cannot host downstreams; enable the entry or middle layer role first")
		return
	}
	var body struct {
		Bindings []struct {
			DownstreamNodeID int64  `json:"downstream_node_id"`
			Mode             string `json:"mode"`
		} `json:"bindings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request format")
		return
	}
	bs := make([]db.NodeBinding, len(body.Bindings))
	seen := make(map[int64]bool, len(body.Bindings))
	for i, b := range body.Bindings {
		if b.DownstreamNodeID == id {
			jsonErr(w, http.StatusBadRequest, "cannot bind to self")
			return
		}
		if seen[b.DownstreamNodeID] {
			jsonErr(w, http.StatusBadRequest, "下游节点重复")
			return
		}
		seen[b.DownstreamNodeID] = true
		down, err := db.GetNode(s.DB, b.DownstreamNodeID)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "downstream node not found")
			return
		}
		if down.Roles&db.NodeRoleVia == 0 {
			jsonErr(w, http.StatusBadRequest, "下游节点需先开启中间层角色")
			return
		}
		// Match the downstream-side default: an omitted mode is userspace, not
		// NormalizeForwardMode's kernel fallback.
		mode := strings.ToLower(strings.TrimSpace(b.Mode))
		if mode == "" {
			mode = "userspace"
		}
		if mode != "kernel" && mode != "userspace" {
			jsonErr(w, http.StatusBadRequest, "转发模式必须为 kernel 或 userspace")
			return
		}
		bs[i] = db.NodeBinding{UpstreamNodeID: id, DownstreamNodeID: b.DownstreamNodeID, Mode: mode}
	}
	if err := db.ReplaceBindingsForUpstream(s.DB, id, bs); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.downstream_bindings", strconv.FormatInt(id, 10), fmt.Sprintf("%d edges", len(bs)))
	jsonOK(w, map[string]any{"ok": true})
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
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if node.RelayHostDeclared {
		jsonErr(w, http.StatusConflict, "该字段由节点 daemon 的 --relay-host 参数管理，如需修改请更新节点配置后重启 daemon")
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
		jsonErr(w, http.StatusBadRequest, "中继地址须为 IPv4 或域名，IPv6 请使用 IPv6 中继地址字段")
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

func (s *Server) apiSetNodeRelayHostV6(w http.ResponseWriter, r *http.Request) {
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
	if node.RelayHostV6Declared {
		jsonErr(w, http.StatusConflict, "该字段由节点 daemon 的 --relay-host-v6 参数管理，如需修改请更新节点配置后重启 daemon")
		return
	}
	var body struct {
		RelayHostV6 string `json:"relay_host_v6"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	host := strings.TrimSpace(body.RelayHostV6)
	if host != "" && !isValidRelayHostV6(host) {
		jsonErr(w, http.StatusBadRequest, "IPv6 中继地址须为有效的 IPv6 地址")
		return
	}
	// Clearing the v6 relay would brick rules whose entry family needs it:
	// RegenerateRule hard-fails them on every subsequent edit/rewire, so the
	// owner couldn't even save an unrelated rename until flipping the entry
	// type. Refuse the clear while such rules exist.
	if host == "" {
		if cnt, err := db.CountV6EntryRulesOnNode(s.DB, id); err == nil && cnt > 0 {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("该节点上有 %d 条使用 v6 入口的规则，需先将它们的入口类型改为 v4 才能清除 IPv6 中继地址", cnt))
			return
		}
	}
	if err := db.UpdateNodeRelayHostV6(s.DB, id, host); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host_v6", strconv.FormatInt(id, 10), host)
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

func (s *Server) apiSetNodeRateMultiplier(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	// Composite nodes are edited like any other: composite pricing lives on
	// the node's own rate_multiplier column, not on its hop rows.
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	var body struct {
		RateMultiplier float64 `json:"rate_multiplier"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// 0 is a deliberate free marker (billing accrues no global usage for the
	// node). A negative value is invalid input, not free, so it falls back to the
	// neutral 1.0 rather than persisting as a free 0.
	if body.RateMultiplier < 0 {
		body.RateMultiplier = 1.0
	}
	if err := db.UpdateNodeRateMultiplier(s.DB, id, body.RateMultiplier); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_rate_multiplier", strconv.FormatInt(id, 10), fmt.Sprintf("%.2f", body.RateMultiplier))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetNodeUnidirectional(w http.ResponseWriter, r *http.Request) {
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
		Unidirectional bool `json:"unidirectional"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := db.UpdateNodeUnidirectional(s.DB, id, body.Unidirectional); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := "双向"
	if body.Unidirectional {
		label = "单向"
	}
	db.WriteAudit(s.DB, u.ID, "node.set_unidirectional", strconv.FormatInt(id, 10), label)
	jsonOK(w, map[string]any{"ok": true})
}

// Existing rules are left untouched on purpose: the flag gates chain
// derivation (create/edit), not already-expanded hops, mirroring how binding
// mode edits only affect later (re)expansions.
func (s *Server) apiSetNodeNoDirectExit(w http.ResponseWriter, r *http.Request) {
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
		NoDirectExit bool `json:"no_direct_exit"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := db.UpdateNodeNoDirectExit(s.DB, id, body.NoDirectExit); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := "允许直接转发"
	if body.NoDirectExit {
		label = "禁止直接转发"
	}
	db.WriteAudit(s.DB, u.ID, "node.set_no_direct_exit", strconv.FormatInt(id, 10), label)
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
	art, err := s.loadAgentArtifact()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	err = s.Hub.SendUpgrade(id, upgradeFor(node, art, panelURL))
	// Record the dispatch outcome so the node detail can surface a silent
	// failure later (an acked upgrade whose version never takes).
	status, errText := "acked", ""
	if err != nil {
		status, errText = "error", err.Error()
	}
	db.RecordUpgradeResult(s.DB, id, art.Version, status, errText)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade", strconv.FormatInt(id, 10), art.Version)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
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
		// A composite is logical: it never appears as a physical hop, so it can't
		// be spliced out of a chain the way a physical mid-hop can. A rule running
		// through a deleted composite (as entry or middle layer) therefore can't
		// survive — delete those rules and re-push their sibling physical nodes so
		// the composite's children don't keep stale kernel state (the FK cascade
		// alone would drop the rules but never touch the children).
		rerun, err := db.DeleteRulesUsingNode(s.DB, id)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := db.DeleteNode(s.DB, id); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
		s.apiDispatchFanout(rerun)
		jsonOK(w, map[string]any{"ok": true})
		return
	}
	// A physical node: re-wire the rules it participated in, splicing it out of
	// each chain (its hop rows FK-cascade away on delete, and rewire
	// re-materializes the surviving hops onto their new neighbors).
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

// apiReorderNodes persists the manual node display order from a drag-and-drop
// reorder in the admin node list.
func (s *Server) apiReorderNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := db.ReorderNodes(s.DB, body.IDs); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.reorder", "", "")
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
		// Composite nodes have no agent of their own to dispatch to; skipping
		// them mirrors apiUpgradeAllNodes below and avoids stamping a spurious
		// dispatch-failure into their last_error.
		if n.NodeType == "composite" {
			continue
		}
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
	art, err := s.loadAgentArtifact()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if n.NodeType == "composite" || n.Disabled {
			continue
		}
		err := s.Hub.SendUpgrade(n.ID, upgradeFor(n, art, panelURL))
		status, errText := "acked", ""
		if err != nil {
			status, errText = "error", err.Error()
			fail++
		} else {
			ok++
		}
		db.RecordUpgradeResult(s.DB, n.ID, art.Version, status, errText)
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade_all", "", fmt.Sprintf("ok=%d fail=%d", ok, fail))
	jsonOK(w, map[string]any{"ok": true, "upgraded": ok, "failed": fail})
}

// --- Settings ---

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	panelName, _ := db.GetSetting(s.DB, "panel_name")
	showRate, _ := db.GetSetting(s.DB, "show_rate_to_user")
	poolSizeStr, _ := db.GetSetting(s.DB, "pool_size")
	poolSize := 4
	if n, err := strconv.Atoi(poolSizeStr); err == nil && n >= 0 {
		poolSize = n
	}
	jsonOK(w, map[string]any{"panel_url": panelURL, "panel_name": panelName, "show_rate_to_user": showRate == "1", "pool_size": poolSize})
}

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		PanelURL       string  `json:"panel_url"`
		PanelName      *string `json:"panel_name"`
		ShowRateToUser *bool   `json:"show_rate_to_user"`
		PoolSize       *int    `json:"pool_size"`
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
	if body.PanelName != nil {
		name := strings.TrimSpace(*body.PanelName)
		if err := db.SetSetting(s.DB, "panel_name", name); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		db.WriteAudit(s.DB, u.ID, "settings.panel_name", name, "")
	}
	if body.ShowRateToUser != nil {
		val := ""
		if *body.ShowRateToUser {
			val = "1"
		}
		if err := db.SetSetting(s.DB, "show_rate_to_user", val); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.PoolSize != nil {
		ps := *body.PoolSize
		if ps < 0 || ps > 64 {
			jsonErr(w, http.StatusBadRequest, "pool_size 必须在 0-64 之间")
			return
		}
		if err := db.SetSetting(s.DB, "pool_size", strconv.Itoa(ps)); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Hub.BroadcastConfigUpdate(ps)
	}
	jsonOK(w, map[string]any{"ok": true})
}

// Node role is a bitmask so a node can be both a rule exit ("落地") and appear
// in the user's own proxy list ("直连") at the same time.
const (
	roleLanding = 1
	roleDirect  = 2
	roleMask    = roleLanding | roleDirect
)

func (s *Server) apiGetNodeRoles(w http.ResponseWriter, r *http.Request) {
	val, _ := db.GetSetting(s.DB, "node_roles")
	roles := map[string]int{}
	if val != "" {
		var raw map[string]json.RawMessage
		json.Unmarshal([]byte(val), &raw)
		for k, v := range raw {
			var n int
			if err := json.Unmarshal(v, &n); err == nil {
				if n &= roleMask; n != 0 {
					roles[k] = n
				}
				continue
			}
			// Pre-bitmask data stored the role as the string 'landing'/'direct'.
			var str string
			if err := json.Unmarshal(v, &str); err == nil {
				switch str {
				case "landing":
					roles[k] = roleLanding
				case "direct":
					roles[k] = roleDirect
				}
			}
		}
	}
	jsonOK(w, map[string]any{"roles": roles})
}

func (s *Server) apiSetNodeRoles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Roles map[string]int `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	clean := make(map[string]int)
	for k, v := range body.Roles {
		if v &= roleMask; v != 0 {
			clean[k] = v
		}
	}
	b, _ := json.Marshal(clean)
	if err := db.SetSetting(s.DB, "node_roles", string(b)); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	db.ResolveCompositeRelayStack(s.DB, nodes)
	db.ResolveCompositeHops(s.DB, nodes)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	allUsers, _ := db.ListUsers(s.DB)
	byID := make(map[int64]*db.User, len(allUsers))
	userList := make([]map[string]any, 0, len(allUsers))
	for _, u := range allUsers {
		byID[u.ID] = u
		userList = append(userList, map[string]any{"id": u.ID, "username": u.Username})
	}
	// Per-owner landing index, built once per owner from the materialized landing
	// set — the same table that drives metering, so the badge matches billing.
	// The admin list only needs the kind badge, so withURI=false — no relay URI
	// is computed here.
	idxByOwner := map[int64]map[string]landing.Node{}
	ownerIndex := func(ownerID int64) map[string]landing.Node {
		if idx, ok := idxByOwner[ownerID]; ok {
			return idx
		}
		idx := s.landingIndexFromDB(ownerID)
		idxByOwner[ownerID] = idx
		return idx
	}
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		oname := ""
		var idx map[string]landing.Node
		if rl.OwnerID.Valid {
			if u := byID[rl.OwnerID.Int64]; u != nil {
				oname = u.Username
			}
			idx = ownerIndex(rl.OwnerID.Int64)
		}
		item := s.buildRuleListItem(rl, oname)
		item.classifyExit(idx, false)
		if n := nodeByID[rl.NodeID]; n != nil {
			item.RateMultiplier = n.RateMultiplier
		} else {
			item.RateMultiplier = 1
		}
		item.BillingRate = 1
		if rl.OwnerID.Valid {
			if u := byID[rl.OwnerID.Int64]; u != nil && u.BillingRate > 0 {
				item.BillingRate = u.BillingRate
			}
		}
		views = append(views, item)
	}
	s.fillRuleChains(views, nodeByID)
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
		// ExitMode is the exit-segment forwarding mode: the only hop of a
		// single-node rule, or the last hop of a composite chain (whose
		// inter-node hops take their modes from the node config). Mode is its
		// legacy alias honored for single-node rules only — see hopsForChain.
		Mode     string `json:"mode"`
		ExitMode string `json:"exit_mode"`
		Hops     []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
		// ViaNodeIDs is the ordered middle-layer path. A pointer tells "not
		// sent" (edits keep the stored path — old clients must not silently
		// strip layers) apart from an explicit empty list (clear the layers).
		ViaNodeIDs *[]int64 `json:"via_node_ids"`
		// EntryFamily selects the entry endpoint's IP family: "v4" (default),
		// "v6", or "both". Empty defaults to "v4".
		EntryFamily string `json:"entry_family"`
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
	entryFamily, err := normalizeEntryFamily(body.EntryFamily)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var hops []db.HopInput
	// vias is the persisted middle-layer path; only the entry-node branch
	// derives a chain from it, so an explicit-hops request keeps it empty.
	var vias []int64

	if len(body.Hops) > 0 {
		// Explicit hops: an admin-only escape hatch for arbitrary chains, so role
		// and binding-edge fitness are intentionally not enforced here. The
		// no_direct_exit invariant is a hard safety rule and still applies — a
		// node that forbids launching the exit must not be seated at the exit
		// even on this path.
		hops = make([]db.HopInput, len(body.Hops))
		for i, h := range body.Hops {
			hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.NodeID}
		}
		if name, bad := s.exitHopForbidsDirect(hops); bad {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("节点 %s 禁止直接转发，必须在其后选择线路层", name))
			return
		}
	} else if body.NodeID > 0 {
		vias = viasOf(body.ViaNodeIDs)
		derived, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode, nil)
		if derr != nil {
			jsonErr(w, http.StatusBadRequest, derr.Error())
			return
		}
		hops = derived
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

	// Rules created through the admin API are admin-managed: bind them to the
	// creating admin so ownership listings and traffic accounting have a
	// subject (this route group is admin-only). The agent/WS path instead
	// inherits the node owner.
	rl := &db.Rule{
		NodeID:      ruleNodeID,
		OwnerID:     sql.NullInt64{Int64: u.ID, Valid: true},
		Name:        name,
		Proto:       proto,
		ExitHost:    exitHost,
		ExitPort:    exitPort,
		Comment:     strings.TrimSpace(body.Comment),
		EntryFamily: entryFamily,
		ViaNodeIDs:  vias,
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rl.ID = id

	entry, entryV6, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonRegenerateErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"rule": rl, "entry": entry, "entry_v6": entryV6})
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
	db.FillRuleTraffic(s.DB, []*db.Rule{rl})
	hops, _ := db.ListRuleHops(s.DB, id)
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeRelayStack(s.DB, nodes)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	ownerName := ""
	var idx map[string]landing.Node
	billingRate := 1.0
	if rl.OwnerID.Valid {
		if u, e := db.GetUserByID(s.DB, rl.OwnerID.Int64); e == nil {
			ownerName = u.Username
			idx = s.landingIndexFromDB(rl.OwnerID.Int64)
			if u.BillingRate > 0 {
				billingRate = u.BillingRate
			}
		}
	}
	item := s.buildRuleListItem(rl, ownerName)
	item.classifyExit(idx, true)
	if n := nodeByID[rl.NodeID]; n != nil {
		item.RateMultiplier = n.RateMultiplier
	} else {
		item.RateMultiplier = 1
	}
	item.BillingRate = billingRate
	jsonOK(w, map[string]any{
		"rule": item, "hops": hops, "nodes": nodes, "node_by_id": nodeByID,
	})
}

// expandSegment expands one logical node into its physical hops: a single node
// is itself; a composite is its ordered children with the config's inter-node
// modes. Every hop carries the segment's logical node id for provenance. Mode
// of the segment's tail hop is left as stored (dormant for composites) — the
// chain assembler overwrites it with the junction edge mode or the rule's
// exit mode.
func (s *Server) expandSegment(nodeID int64) ([]db.HopInput, bool, error) {
	node, err := db.GetNode(s.DB, nodeID)
	if err != nil {
		return nil, false, fmt.Errorf("节点不存在")
	}
	if node.NodeType == "composite" {
		nh, _ := db.ListNodeHops(s.DB, nodeID)
		if len(nh) == 0 {
			return nil, true, fmt.Errorf("组合节点无子节点")
		}
		hops := make([]db.HopInput, len(nh))
		for i, h := range nh {
			hops[i] = db.HopInput{NodeID: h.HopNodeID, Mode: h.Mode, ViaNodeID: nodeID}
		}
		return hops, true, nil
	}
	return []db.HopInput{{NodeID: nodeID, Mode: "", ViaNodeID: nodeID}}, false, nil
}

// hopsForChain assembles a rule's physical chain: the entry segment followed
// by each middle-layer (via) segment. At a non-final junction a single-node
// segment's tail takes the binding edge's mode, while a composite segment
// mid-chain keeps its own configured last-hop mode (the composite owns how its
// exit leg forwards; the edge is only checked for reachability). The final hop
// takes the rule's exitMode (empty falls back to singleMode for a bare
// single-node chain — the legacy alias — else the kernel default). The entry node must
// carry the entry role, and every via must carry the via role and be
// reachable from its predecessor through a binding edge — this is the
// authoritative check, since a picker filtering the UI's node list is only
// a convenience and grants alone don't imply role fitness. Grants remain the
// caller's policy. The composite flag tells edit handlers which fields count
// as an explicit exit-mode request: any chain beyond a bare single node never
// honors the legacy mode alias.
// validateCompositeChildren rejects a composite whose member is missing or is
// itself a composite. Composites are flat combos of single nodes; a nested
// composite would put a composite id into rule_hops.node_id at expansion time,
// where no agent exists to serve it. The composite editors already offer only
// single nodes — this is the authoritative server-side guard for direct API use.
func (s *Server) validateCompositeChildren(childIDs []int64) error {
	for _, cid := range childIDs {
		n, err := db.GetNode(s.DB, cid)
		if err != nil {
			return fmt.Errorf("子节点 %d 不存在", cid)
		}
		if n.NodeType == "composite" {
			return fmt.Errorf("组合节点不能嵌套组合节点（%s）", n.Name)
		}
	}
	return nil
}

// exitHopForbidsDirect reports whether the chain's final physical hop is a
// no_direct_exit node (which would launch the exit segment it is forbidden to
// launch), returning that node's name. Shared by the derived and explicit-hops
// rule-building paths so neither can seat a no-direct-exit node at the exit.
func (s *Server) exitHopForbidsDirect(hops []db.HopInput) (string, bool) {
	if len(hops) == 0 {
		return "", false
	}
	n, err := db.GetNode(s.DB, hops[len(hops)-1].NodeID)
	if err != nil {
		return "", false // a missing node is caught downstream by RegenerateRule
	}
	return n.Name, n.NoDirectExit
}

// grantRoleOverrides returns a user's per-grant role overrides keyed by node
// id, including only grants that actually override (roles != 0). hopsForChain
// consults it so a node's usability as entry/via is judged by the user's
// effective role, not the node's global mask. nil/absent entries fall back to
// the node mask via db.EffectiveNodeRoles.
func (s *Server) grantRoleOverrides(userID int64) map[int64]int64 {
	_, grants, err := db.ListNodesForUser(s.DB, userID)
	if err != nil {
		return nil
	}
	out := map[int64]int64{}
	for _, g := range grants {
		if g.Roles != 0 {
			out[g.NodeID] = g.Roles
		}
	}
	return out
}

func (s *Server) hopsForChain(entryID int64, vias []int64, singleMode, exitMode string, effRoles map[int64]int64) ([]db.HopInput, bool, error) {
	entryNode, err := db.GetNode(s.DB, entryID)
	if err != nil {
		return nil, false, fmt.Errorf("节点不存在")
	}
	if db.EffectiveNodeRoles(entryNode.Roles, effRoles[entryID])&db.NodeRoleEntry == 0 {
		return nil, false, fmt.Errorf("节点 %s 不是入口", entryNode.Name)
	}
	hops, entryComposite, err := s.expandSegment(entryID)
	if err != nil {
		return nil, false, err
	}
	prev := entryID
	prevComposite := entryComposite
	for _, viaID := range vias {
		viaNode, err := db.GetNode(s.DB, viaID)
		if err != nil {
			return nil, true, fmt.Errorf("中间层节点不存在")
		}
		if db.EffectiveNodeRoles(viaNode.Roles, effRoles[viaID])&db.NodeRoleVia == 0 {
			return nil, true, fmt.Errorf("节点 %s 不是中间层", viaNode.Name)
		}
		edge, err := db.GetNodeBinding(s.DB, prev, viaID)
		if err != nil {
			return nil, true, fmt.Errorf("中间层 %s 未绑定到所选上游", viaNode.Name)
		}
		seg, viaComposite, err := s.expandSegment(viaID)
		if err != nil {
			return nil, true, err
		}
		// The junction is the leg from the previous segment's tail into this
		// segment's head. A single-node previous segment takes the binding edge's
		// mode for that leg. A composite previous segment sitting mid-chain owns
		// how its own exit leg forwards — its last hop keeps the mode configured
		// in the composite, so we don't overwrite it with the edge. The binding
		// edge is still required for reachability; only its mode defers here.
		if !prevComposite {
			hops[len(hops)-1].Mode = edge.Mode
		}
		hops = append(hops, seg...)
		prev = viaID
		prevComposite = viaComposite
	}
	// The physical node that actually launches the exit segment is the last hop,
	// which for a composite tail is its last child — not the logical tail node.
	// A no_direct_exit node may never launch the exit, so the chain must cascade
	// further. Checked here — not in the picker — so a hand-crafted request can't
	// bypass it.
	if name, bad := s.exitHopForbidsDirect(hops); bad {
		return nil, len(vias) > 0, fmt.Errorf("节点 %s 禁止直接转发，必须在其后选择线路层", name)
	}
	composite := entryComposite || len(vias) > 0
	mode := exitMode
	if mode == "" && !composite {
		mode = singleMode
	}
	hops[len(hops)-1].Mode = mode
	return hops, composite, nil
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
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
		// ExitMode is the exit-segment forwarding mode (single hop or a
		// chain's last hop); empty keeps the current mode so clients that
		// don't send it can't silently reset a userspace exit back to kernel.
		// Mode is its legacy alias honored for single-node rules only — see
		// hopsForChain.
		Mode     string `json:"mode"`
		ExitMode string `json:"exit_mode"`
		Hops     []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
		// ViaNodeIDs is the ordered middle-layer path. A pointer tells "not
		// sent" (keep the stored path — old clients must not silently strip
		// layers) apart from an explicit empty list (clear the layers).
		ViaNodeIDs *[]int64 `json:"via_node_ids"`
		// EntryFamily selects the entry endpoint's IP family: "v4" (default),
		// "v6", or "both". Empty defaults to "v4".
		EntryFamily string `json:"entry_family"`
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
	entryFamily, err := normalizeEntryFamily(body.EntryFamily)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Three ways to resolve the chain, in priority order:
	//   node_id / via_node_ids -> re-derive from the selected entry node and
	//               middle-layer path (single/composite); this is how the entry
	//               node or the via path gets switched. A sent via_node_ids
	//               field alone (even with node_id absent) re-derives so an
	//               explicit empty list can clear the layers.
	//   hops     -> explicit chain (reorder/mode edits).
	//   neither  -> keep the existing chain; RegenerateRule reuses each node's
	//               current listen port so a header-only edit doesn't churn the
	//               entry endpoint or installed ports.
	var hops []db.HopInput
	switch {
	case body.NodeID > 0 || body.ViaNodeIDs != nil:
		entryID := body.NodeID
		if entryID == 0 {
			entryID = rl.NodeID
		}
		vias := rl.ViaNodeIDs
		if body.ViaNodeIDs != nil {
			vias = *body.ViaNodeIDs
		}
		var ownerOverrides map[int64]int64
		if rl.OwnerID.Valid {
			ownerOverrides = s.grantRoleOverrides(rl.OwnerID.Int64)
		}
		derived, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode, ownerOverrides)
		if derr != nil {
			jsonErr(w, http.StatusBadRequest, derr.Error())
			return
		}
		hops = derived
		// A same-entry edit without an explicit exit-segment mode keeps the
		// exit hop's current mode, so clients that don't send one can't
		// silently reset a userspace exit back to kernel. Any chain beyond a
		// bare single node treats only exit_mode as explicit — the legacy mode
		// field never was an exit-segment request for them.
		explicit := body.ExitMode != "" || (!composite && body.Mode != "")
		if !explicit && entryID == rl.NodeID {
			if existing, _ := db.ListRuleHops(s.DB, id); len(existing) > 0 {
				hops[len(hops)-1].Mode = existing[len(existing)-1].Mode
			}
		}
		rl.NodeID = entryID
		rl.ViaNodeIDs = vias
	case len(body.Hops) == 0:
		existing, err := db.ListRuleHops(s.DB, id)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(existing) == 0 {
			jsonErr(w, http.StatusBadRequest, "至少添加一个节点")
			return
		}
		hops = make([]db.HopInput, len(existing))
		for i, h := range existing {
			hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID}
		}
		// A header-only edit still owns the exit segment: an explicit
		// exit_mode (or the single-node legacy mode) applies to the last hop
		// instead of being silently dropped with a 200.
		if body.ExitMode != "" {
			hops[len(hops)-1].Mode = body.ExitMode
		} else if body.Mode != "" && len(hops) == 1 {
			hops[0].Mode = body.Mode
		}
	default:
		hops = make([]db.HopInput, len(body.Hops))
		for i, h := range body.Hops {
			hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.NodeID}
		}
	}
	if body.EntryPort > 0 && len(hops) > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	rl.Name, rl.Proto, rl.ExitHost, rl.ExitPort = name, proto, exitHost, exitPort
	rl.Comment = strings.TrimSpace(body.Comment)
	// Absent entry_family keeps the stored family — only an explicit value
	// changes it, mirroring how an empty mode keeps the exit segment.
	if entryFamily != "" {
		rl.EntryFamily = entryFamily
	}
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
	entry, entryV6, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonRegenerateErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.save", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "entry": entry, "entry_v6": entryV6})
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
		inputs[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID}
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
	_, _, affected, err := db.RegenerateRule(tx, rl, inputs, avoid)
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
	// The landing_nodes preview doubles as a sync point: any successful
	// resolution keeps the materialized set fresh without waiting for the
	// background pass.
	landingPreview, lok := s.resolveLandingExits(target, false)
	if lok {
		s.syncLandingExits(target, landingPreview)
	} else {
		landingPreview = s.landingNodesFor(target, false)
	}
	jsonOK(w, map[string]any{
		"user": apiUserFullView(target), "nodes": grantedNodes,
		"grants": grants, "all_nodes": allNodes,
		"rules":         rules,
		"landing_nodes": landingPreview,
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
		LandingSubURL     string `json:"landing_sub_url"`
		AdminNote         string `json:"admin_note"`
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
	if body.LandingSubURL != "" || body.AdminNote != "" {
		s.DB.Exec(`UPDATE users SET landing_sub_url=?, admin_note=? WHERE id=?`, strings.TrimSpace(body.LandingSubURL), strings.TrimSpace(body.AdminNote), id)
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
		NodeID            int64   `json:"node_id"`
		NodeIDs           []int64 `json:"node_ids"`
		MaxForwards       int     `json:"max_forwards"`
		TrafficQuotaBytes int64   `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards <= 0 {
		body.MaxForwards = defaultGrantMaxForwards
	}
	ids := body.NodeIDs
	if len(ids) == 0 && body.NodeID != 0 {
		ids = []int64{body.NodeID}
	}
	if len(ids) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择节点")
		return
	}
	for _, nid := range ids {
		if err := db.GrantNode(s.DB, userID, nid, body.MaxForwards, body.TrafficQuotaBytes); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		db.WriteAudit(s.DB, u.ID, "user.grant_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nid, 10))
	}
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
	// Revoking access must also stop the user's existing forwarding on that node;
	// otherwise the rules keep running (and keep billing) with no grant behind
	// them. Delete them and re-push the affected nodes so the kernel converges.
	affected, err := db.DeleteRulesForUserNode(s.DB, userID, nodeID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.apiDispatchFanout(affected)
	db.WriteAudit(s.DB, u.ID, "user.revoke_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	jsonOK(w, map[string]any{"ok": true, "removed_rule_nodes": len(affected)})
}

func (s *Server) apiBatchRevokeNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		NodeIDs []int64 `json:"node_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.NodeIDs) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择节点")
		return
	}
	var affected []int64
	for _, nid := range body.NodeIDs {
		if err := db.RevokeNode(s.DB, userID, nid); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		nodes, err := db.DeleteRulesForUserNode(s.DB, userID, nid)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		affected = append(affected, nodes...)
		db.WriteAudit(s.DB, u.ID, "user.revoke_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nid, 10))
	}
	s.apiDispatchFanout(affected)
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

func (s *Server) apiSetMaxForwards(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		MaxForwards int `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards < 0 {
		jsonErr(w, http.StatusBadRequest, "配额数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=? WHERE id=?`, body.MaxForwards, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_max_forwards", strconv.FormatInt(id, 10), strconv.Itoa(body.MaxForwards))
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
	// Re-dispatch so expiry (or extension) takes effect immediately in the kernel.
	if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
		for _, n := range nodes {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("expiry: re-dispatch node %d after setting user %d expiry: %v", n, id, err)
			}
		}
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetUserTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := db.ResetAllUserTraffic(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// If the user was disabled for exceeding quota, lift the ban so the
	// subsequent re-dispatch pushes their rules back to the kernel.
	if target, err := db.GetUserByID(s.DB, id); err == nil &&
		target.Disabled && target.DisableReason.Valid && target.DisableReason.String == "流量超额" {
		_ = db.SetUserDisabled(s.DB, id, false, "")
	}
	// re-dispatch rules on nodes that may have been excluded by per-node quota
	if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
		for _, n := range nodes {
			_ = s.dispatchToNode(n)
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_traffic", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpdateUserProfile(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		ExpiresAt        string  `json:"expires_at"`
		MaxForwards      int     `json:"max_forwards"`
		TrafficQuotaGB   float64 `json:"traffic_quota_gb"`
		TrafficResetDays int     `json:"traffic_reset_days"`
		AdminNote        string  `json:"admin_note"`
		BillingRate      float64 `json:"billing_rate"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards < 0 {
		jsonErr(w, http.StatusBadRequest, "配额数无效")
		return
	}
	if body.TrafficQuotaGB < 0 {
		jsonErr(w, http.StatusBadRequest, "流量配额无效")
		return
	}
	if body.TrafficResetDays < 0 {
		jsonErr(w, http.StatusBadRequest, "天数无效")
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

	trafficQuotaBytes := int64(body.TrafficQuotaGB * 1073741824)
	if trafficQuotaBytes < 0 {
		trafficQuotaBytes = 0
	}

	billingRate := body.BillingRate
	if billingRate <= 0 {
		billingRate = 1.0
	}
	if _, err := s.DB.Exec(
		`UPDATE users SET expires_at=?, max_forwards=?, traffic_quota_bytes=?, traffic_reset_days=?, admin_note=?, billing_rate=? WHERE id=?`,
		expiresAt, body.MaxForwards, trafficQuotaBytes, body.TrafficResetDays, strings.TrimSpace(body.AdminNote), billingRate, id,
	); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	db.WriteAudit(s.DB, u.ID, "user.update_profile", strconv.FormatInt(id, 10), "")
	if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
		for _, n := range nodes {
			_ = s.dispatchToNode(n)
		}
	}
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
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if target.Role == "admin" {
		jsonErr(w, http.StatusForbidden, "不支持重置管理员密码")
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
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if target.Role == "admin" {
		jsonErr(w, http.StatusForbidden, "不支持删除管理员")
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
	// A composite's online state is derived from its children, which may not be
	// in the user's granted set — resolve over the full node list, then project
	// the result onto the granted nodes so the dashboard shows accurate status.
	db.ResolveCompositeOnline(s.DB, nodes)
	onlineByID := make(map[int64]int, len(nodes))
	for _, n := range nodes {
		onlineByID[n.ID] = n.Online
	}
	for _, gn := range grantedNodes {
		if o, ok := onlineByID[gn.ID]; ok {
			gn.Online = o
		}
	}
	showRate, _ := db.GetSetting(s.DB, "show_rate_to_user")
	jsonOK(w, map[string]any{
		"user": apiUserFullView(u), "nodes": grantedNodes, "grants": grants,
		"rules": rules, "show_rate": showRate == "1",
	})
}

// applyEffectiveRoles overwrites each granted node's Roles with the grantee's
// effective role (grant override if set, else the node's own mask) so the
// my-side rule form filters entry/via candidates by what this user may actually
// do with the node. nodes and grants are index-aligned as ListNodesForUser
// returns them.
func applyEffectiveRoles(nodes []*db.Node, grants []*db.UserNode) {
	for i, n := range nodes {
		if i < len(grants) && grants[i] != nil {
			n.Roles = db.EffectiveNodeRoles(n.Roles, grants[i].Roles)
		}
	}
}

func (s *Server) apiMyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	db.FillRuleTraffic(s.DB, rules)
	idx := s.landingIndexFromDB(u.ID)
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	applyEffectiveRoles(grantedNodes, grants)
	// A user is granted the composite itself, not its hop children, so the
	// relay-stack resolver needs the full node list to see the child hosts;
	// copy the derived fields back onto the (narrower) granted set.
	allNodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeRelayStack(s.DB, allNodes)
	stackByID := buildMap(allNodes, func(n *db.Node) int64 { return n.ID })
	for _, n := range grantedNodes {
		if full := stackByID[n.ID]; full != nil {
			n.EntryRelayHost = full.EntryRelayHost
			n.EntryRelayHostV6 = full.EntryRelayHostV6
			n.ExitRelayHostV6 = full.ExitRelayHostV6
		}
	}
	// Attach each granted composite's member chain so the rule form can flatten
	// it in the live preview, and keep the all-nodes map for resolving saved
	// rules' physical hops (which may pass through non-granted composite members).
	db.ResolveCompositeHops(s.DB, grantedNodes)
	allByID := buildMap(allNodes, func(n *db.Node) int64 { return n.ID })
	grantedByID := buildMap(grantedNodes, func(n *db.Node) int64 { return n.ID })
	br := u.BillingRate
	if br <= 0 {
		br = 1
	}
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		item := s.buildRuleListItem(rl, "")
		item.classifyExit(idx, true)
		if n := grantedByID[rl.NodeID]; n != nil {
			item.RateMultiplier = n.RateMultiplier
		} else {
			item.RateMultiplier = 1
		}
		item.BillingRate = br
		views = append(views, item)
	}
	s.fillRuleChains(views, allByID)
	grantedSet := make(map[int64]bool)
	for _, n := range grantedNodes {
		grantedSet[n.ID] = true
	}
	allEdges, _ := db.ListAllNodeBindings(s.DB)
	edges := make([]*db.NodeBinding, 0, len(allEdges))
	for _, e := range allEdges {
		if grantedSet[e.UpstreamNodeID] && grantedSet[e.DownstreamNodeID] {
			edges = append(edges, e)
		}
	}
	showRate, _ := db.GetSetting(s.DB, "show_rate_to_user")
	jsonOK(w, map[string]any{
		"rules": views, "nodes": grantedNodes,
		"node_by_id": grantedByID, "show_rate": showRate == "1",
		"bindings": edges,
	})
}

func (s *Server) apiMyGetRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil || !rl.OwnerID.Valid || rl.OwnerID.Int64 != u.ID {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	db.FillRuleTraffic(s.DB, []*db.Rule{rl})
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	applyEffectiveRoles(grantedNodes, grants)
	// A user is granted the composite itself, not its hop children, so the
	// relay-stack resolver needs the full node list to see the child hosts;
	// copy the derived fields back onto the (narrower) granted set.
	allNodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeRelayStack(s.DB, allNodes)
	stackByID := buildMap(allNodes, func(n *db.Node) int64 { return n.ID })
	for _, n := range grantedNodes {
		if full := stackByID[n.ID]; full != nil {
			n.EntryRelayHost = full.EntryRelayHost
			n.EntryRelayHostV6 = full.EntryRelayHostV6
			n.ExitRelayHostV6 = full.ExitRelayHostV6
		}
	}
	db.ResolveCompositeHops(s.DB, grantedNodes)
	allByID := buildMap(allNodes, func(n *db.Node) int64 { return n.ID })
	grantedByID := buildMap(grantedNodes, func(n *db.Node) int64 { return n.ID })
	idx := s.landingIndexFromDB(u.ID)
	item := s.buildRuleListItem(rl, "")
	item.classifyExit(idx, true)
	if n := grantedByID[rl.NodeID]; n != nil {
		item.RateMultiplier = n.RateMultiplier
	} else {
		item.RateMultiplier = 1
	}
	item.BillingRate = u.BillingRate
	if item.BillingRate <= 0 {
		item.BillingRate = 1
	}
	views := []ruleListItem{item}
	s.fillRuleChains(views, allByID)
	item = views[0]
	showRate, _ := db.GetSetting(s.DB, "show_rate_to_user")
	jsonOK(w, map[string]any{
		"rule": item, "nodes": grantedNodes, "node_by_id": grantedByID,
		"show_rate": showRate == "1",
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
		// ExitMode is the exit-segment forwarding mode: the only hop of a
		// single-node rule, or the last hop of a composite chain (whose
		// inter-node hops take their modes from the node config). Mode is its
		// legacy alias honored for single-node rules only — see hopsForChain.
		Mode     string `json:"mode"`
		ExitMode string `json:"exit_mode"`
		// ViaNodeIDs is the ordered middle-layer path. A pointer tells "not
		// sent" apart from an explicit empty list; each via is authorized on
		// its own grant just like the entry node.
		ViaNodeIDs *[]int64 `json:"via_node_ids"`
		// EntryFamily selects the entry endpoint's IP family: "v4" (default),
		// "v6", or "both". Empty defaults to "v4".
		EntryFamily string `json:"entry_family"`
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
	entryFamily, err := normalizeEntryFamily(body.EntryFamily)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.NodeID == 0 {
		jsonErr(w, http.StatusBadRequest, "node_id 不能为空")
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

	// Each middle-layer node is authorized on its own grant before the chain
	// is validated, so a revoked via is rejected as forbidden rather than
	// falling through to the role/binding checks.
	vias := viasOf(body.ViaNodeIDs)
	for _, viaID := range vias {
		if _, gerr := db.GetNodeGrant(s.DB, u.ID, viaID); gerr != nil {
			jsonErr(w, http.StatusForbidden, "无权使用中间层节点")
			return
		}
	}

	hops, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode, s.grantRoleOverrides(u.ID))
	if derr != nil {
		jsonErr(w, http.StatusBadRequest, derr.Error())
		return
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
		NodeID:      body.NodeID,
		OwnerID:     nullInt64(u.ID),
		Name:        name,
		Proto:       proto,
		ExitHost:    exitHost,
		ExitPort:    exitPort,
		Comment:     strings.TrimSpace(body.Comment),
		EntryFamily: entryFamily,
		ViaNodeIDs:  vias,
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rl.ID = id

	entry, entryV6, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonRegenerateErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.user_create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "rule_id": id, "entry": entry, "entry_v6": entryV6})
}

// apiMyUpdateRule lets a user edit their own rule: name / proto / exit / comment
// and the entry node. Switching the node re-derives the chain (single -> one
// hop, composite -> sub-hops) and is gated by the grant on the target node and
// the same caps create enforces; keeping the node reuses the existing chain so
// the entry endpoint doesn't churn on a header-only edit.
func (s *Server) apiMyUpdateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		jsonErr(w, http.StatusForbidden, "用户已过期")
		return
	}
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
	var body struct {
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
		// ExitMode is the exit-segment forwarding mode (single hop or a
		// chain's last hop); empty keeps the current mode so clients that
		// don't send it can't silently reset a userspace exit back to kernel.
		// Mode is its legacy alias honored for single-node rules only — see
		// hopsForChain.
		Mode     string `json:"mode"`
		ExitMode string `json:"exit_mode"`
		// ViaNodeIDs is the ordered middle-layer path. A pointer tells "not
		// sent" (keep the stored path — old clients must not silently strip
		// layers) apart from an explicit empty list (clear the layers).
		ViaNodeIDs *[]int64 `json:"via_node_ids"`
		// EntryFamily selects the entry endpoint's IP family: "v4" (default),
		// "v6", or "both". Empty defaults to "v4".
		EntryFamily string `json:"entry_family"`
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
	entryFamily, err := normalizeEntryFamily(body.EntryFamily)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	entryID := rl.NodeID
	if body.NodeID > 0 {
		entryID = body.NodeID
	}
	// Absent via_node_ids keeps the stored path so a header-only edit can't
	// silently strip the middle layers; an explicit list (empty included)
	// replaces it.
	vias := rl.ViaNodeIDs
	if body.ViaNodeIDs != nil {
		vias = *body.ViaNodeIDs
	}
	// The grant on the target node authorizes the rule and carries the per-node
	// cap; a composite is the unit of authorization (granting it covers the
	// whole chain), so the check is on the selected node itself.
	grant, gerr := db.GetNodeGrant(s.DB, u.ID, entryID)
	if gerr != nil {
		jsonErr(w, http.StatusForbidden, "无权使用该节点")
		return
	}
	// Each middle-layer node is authorized on its own grant before the chain
	// is validated, so a revoked via is rejected as forbidden rather than
	// falling through to the role/binding checks.
	for _, viaID := range vias {
		if _, gerr := db.GetNodeGrant(s.DB, u.ID, viaID); gerr != nil {
			jsonErr(w, http.StatusForbidden, "无权使用中间层节点")
			return
		}
	}
	hops, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode, s.grantRoleOverrides(u.ID))
	if derr != nil {
		jsonErr(w, http.StatusBadRequest, derr.Error())
		return
	}
	// Same-entry edits without an explicit exit-segment mode keep the exit
	// hop's current mode (see the admin update handler for the rationale).
	explicit := body.ExitMode != "" || (!composite && body.Mode != "")
	if !explicit && entryID == rl.NodeID {
		if existing, _ := db.ListRuleHops(s.DB, id); len(existing) > 0 {
			hops[len(hops)-1].Mode = existing[len(existing)-1].Mode
		}
	}
	if body.EntryPort > 0 && len(hops) > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	oldHops, err := db.ListRuleHops(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Per-node cap only when moving to a different node — the rule already
	// counts against its current node, so a same-node edit can't exceed it.
	if entryID != rl.NodeID {
		if cnt, _ := db.CountRulesForUserNode(s.DB, u.ID, entryID); cnt+1 > grant.MaxForwards {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("超出该节点的转发上限（%d）", grant.MaxForwards))
			return
		}
	}
	// Global per-user quota, adjusted for this rule's existing hop count.
	if err := s.checkUserRuleQuota(u, len(hops), len(oldHops)); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	rl.NodeID = entryID
	rl.ViaNodeIDs = vias
	rl.Name, rl.Proto, rl.ExitHost, rl.ExitPort = name, proto, exitHost, exitPort
	rl.Comment = strings.TrimSpace(body.Comment)
	// Absent entry_family keeps the stored family — only an explicit value
	// changes it, mirroring how an empty mode keeps the exit segment.
	if entryFamily != "" {
		rl.EntryFamily = entryFamily
	}
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
	entry, entryV6, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		jsonRegenerateErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "rule.user_save", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "entry": entry, "entry_v6": entryV6})
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

func (s *Server) apiSetAdminNote(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		AdminNote string `json:"admin_note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET admin_note=? WHERE id=?`, strings.TrimSpace(body.AdminNote), id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_admin_note", strconv.FormatInt(id, 10), "")
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

// apiUserFullView includes quota/traffic/expiry fields for /api/me and admin detail.
func apiUserFullView(u *db.User) map[string]any {
	m := apiUserView(u)
	m["disabled"] = u.Disabled
	m["max_forwards"] = u.MaxForwards
	m["traffic_quota_bytes"] = u.TrafficQuotaBytes
	m["traffic_used_bytes"] = u.TrafficUsedBytes
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 != 0 {
		m["expires_at"] = u.ExpiresAt.Int64
	} else {
		m["expires_at"] = nil
	}
	if u.DisableReason.Valid {
		m["disable_reason"] = u.DisableReason.String
	} else {
		m["disable_reason"] = nil
	}
	m["landing_sub_url"] = u.LandingSubURL
	m["landing_uris"] = u.LandingURIs
	// Lets the user-side nav decide whether to show the landing-nodes entry
	// without an extra round-trip.
	m["has_landing_source"] = hasLandingSource(u)
	m["traffic_reset_days"] = u.TrafficResetDays
	m["last_traffic_reset_at"] = u.LastTrafficResetAt
	m["admin_note"] = u.AdminNote
	m["billing_rate"] = u.BillingRate
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

// isValidRelayHost checks that a string is a valid IPv4 literal or hostname.
// relay_host (the data-plane v4 address) and relay_host_v6 are family-typed
// fields; an IPv6 literal here would look identical to a v4-only node's
// address to anything reading relay_host without also checking family,
// silently reintroducing the mixed-family bug this field split was meant to
// fix. IPv6 belongs exclusively in relay_host_v6.
func isValidRelayHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	return resolver.IsHostname(host)
}

// isValidRelayHostV6 checks that a string is a valid IPv6 literal (not a
// v4-mapped one, which belongs in relay_host instead).
func isValidRelayHostV6(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

func (s *Server) apiSetPerNodeMaxForwards(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		MaxForwards int `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards < 1 {
		jsonErr(w, http.StatusBadRequest, "规则数上限至少为 1")
		return
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET max_forwards=? WHERE user_id=? AND node_id=?`,
		body.MaxForwards, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_max_forwards", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d max=%d", nodeID, body.MaxForwards))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetPerNodeQuota(w http.ResponseWriter, r *http.Request) {
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
	if _, err := s.DB.Exec(`UPDATE user_nodes SET traffic_quota_bytes=? WHERE user_id=? AND node_id=?`,
		body.TrafficQuotaBytes, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_quota", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d bytes=%d", nodeID, body.TrafficQuotaBytes))
	jsonOK(w, map[string]any{"ok": true})
}

// apiSetPerNodeRateLimit sets the grant's shared rate limit (MB/s, 0 =
// unlimited) and re-dispatches every node carrying the grant's rule hops so
// the data plane picks up the new shaping.
func (s *Server) apiSetPerNodeRateLimit(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.RateLimitMBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "限速不能为负")
		return
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`,
		body.RateLimitMBytes, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	affected, _ := db.RulesAffectedByNode(s.DB, userID, nodeID)
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_rate_limit", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d mbytes=%d", nodeID, body.RateLimitMBytes))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetPerNodeTraffic(w http.ResponseWriter, r *http.Request) {
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
	if err := db.ResetUserNodeTraffic(s.DB, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// re-dispatch rules that may have been excluded due to this node's quota
	affected, _ := db.RulesAffectedByNode(s.DB, userID, nodeID)
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_node_traffic", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetResetDays(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TrafficResetDays int `json:"traffic_reset_days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficResetDays < 0 {
		jsonErr(w, http.StatusBadRequest, "天数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET traffic_reset_days=? WHERE id=?`, body.TrafficResetDays, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_reset_days", strconv.FormatInt(id, 10), strconv.Itoa(body.TrafficResetDays))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiTokenInfo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	ruleCount, _ := db.CountRulesForUser(s.DB, u.ID)

	nodeViews := make([]map[string]any, 0, len(grantedNodes))
	for i, n := range grantedNodes {
		g := grants[i]
		nRules, _ := db.CountRulesForUserNode(s.DB, u.ID, n.ID)
		nodeViews = append(nodeViews, map[string]any{
			"name":            n.Name,
			"rule_count":      nRules,
			"rate_multiplier": n.RateMultiplier,
			"unidirectional":  n.Unidirectional,
			"traffic_used":    g.TrafficUsedBytes,
			"traffic_quota":   g.TrafficQuotaBytes,
		})
	}

	var expiresAt any
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 != 0 {
		expiresAt = u.ExpiresAt.Int64
	}

	jsonOK(w, map[string]any{
		"username":              u.Username,
		"traffic_used":          u.TrafficUsedBytes,
		"traffic_quota":         u.TrafficQuotaBytes,
		"traffic_reset_days":    u.TrafficResetDays,
		"last_traffic_reset_at": u.LastTrafficResetAt,
		"expires_at":            expiresAt,
		"rule_count":            ruleCount,
		"max_forwards":          u.MaxForwards,
		"billing_rate":          u.BillingRate,
		"nodes":                 nodeViews,
	})
}

func (s *Server) apiMyGetToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := db.GetAPITokenByUser(s.DB, u.ID)
	if err != nil {
		jsonOK(w, map[string]any{"has_token": false})
		return
	}
	var lastUsed any
	if t.LastUsedAt.Valid {
		lastUsed = t.LastUsedAt.Int64
	}
	jsonOK(w, map[string]any{
		"has_token":    true,
		"token_prefix": t.TokenPrefix,
		"disabled":     t.Disabled,
		"created_at":   t.CreatedAt,
		"last_used_at": lastUsed,
	})
}

func (s *Server) apiMyCreateToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	token, err := db.CreateAPIToken(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusConflict, "已存在 Token，请先删除后重新创建")
		return
	}
	jsonOK(w, map[string]any{"token": token})
}

func (s *Server) apiMyDeleteToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if err := db.DeleteAPIToken(s.DB, u.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, "删除失败")
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiMyRefreshToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	token, err := db.RefreshAPIToken(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "无 Token 可刷新")
		return
	}
	jsonOK(w, map[string]any{"token": token})
}

func (s *Server) apiMyToggleToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	disabled, err := db.ToggleAPIToken(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "Token 不存在")
		return
	}
	jsonOK(w, map[string]any{"disabled": disabled})
}

func (s *Server) apiBatchApplyGrants(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		UserIDs []int64 `json:"user_ids"`
		Grants  []struct {
			NodeName          string `json:"node_name"`
			MaxForwards       int    `json:"max_forwards"`
			TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
			RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
		} `json:"grants"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.UserIDs) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择目标用户")
		return
	}
	if len(body.Grants) == 0 {
		jsonErr(w, http.StatusBadRequest, "请提供授权节点")
		return
	}
	names := make([]string, len(body.Grants))
	for i, g := range body.Grants {
		names[i] = g.NodeName
	}
	nameToID, err := db.NodeIDsByNames(s.DB, names)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var skipped []string
	var granted int
	for _, g := range body.Grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			skipped = append(skipped, g.NodeName)
			continue
		}
		mf := g.MaxForwards
		if mf <= 0 {
			mf = 10
		}
		for _, uid := range body.UserIDs {
			if err := db.GrantNode(s.DB, uid, nid, mf, g.TrafficQuotaBytes); err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			mb := g.RateLimitMBytes
			if mb < 0 {
				mb = 0
			}
			if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`, mb, uid, nid); err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			db.WriteAudit(s.DB, u.ID, "user.grant_node", strconv.FormatInt(uid, 10), strconv.FormatInt(nid, 10))
			granted++
		}
	}

	// batch grants usually precede rule creation (no-op fanout), but changing
	// an existing grant's rate limit must take effect on already-active rules.
	affected := map[int64]bool{}
	for _, g := range body.Grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			continue
		}
		for _, uid := range body.UserIDs {
			ns, err := db.RulesAffectedByNode(s.DB, uid, nid)
			if err != nil {
				continue
			}
			for _, n := range ns {
				affected[n] = true
			}
		}
	}
	nodeIDs := make([]int64, 0, len(affected))
	for n := range affected {
		nodeIDs = append(nodeIDs, n)
	}
	s.apiDispatchFanout(nodeIDs)

	jsonOK(w, map[string]any{"ok": true, "granted": granted, "skipped_nodes": skipped})
}
