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

	"golang.org/x/crypto/bcrypt"

	"nft-forward/internal/accounting"
	"nft-forward/internal/chain"
	"nft-forward/internal/db"
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
		// Only "no such session" ends the session. A lookup that failed says
		// nothing about the cookie, so it is neither cleared nor answered with
		// 401 — the client would take that as having been logged out.
		if errors.Is(err, sql.ErrNoRows) {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
			jsonErr(w, http.StatusUnauthorized, "会话已过期")
			return
		}
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
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
			v1Err(w, http.StatusUnauthorized, codeUnauthorized, "缺少 API Token")
			return
		}
		u, t, err := db.GetUserByAPIToken(s.DB, token)
		// As with sessions: only "no such token" is a 401. A caller told its
		// token is invalid has reason to discard it.
		if errors.Is(err, sql.ErrNoRows) {
			v1Err(w, http.StatusUnauthorized, codeUnauthorized, "无效的 API Token")
			return
		}
		if err != nil {
			v1Err(w, http.StatusInternalServerError, codeInternal, err.Error())
			return
		}
		if t.Disabled {
			v1Err(w, http.StatusForbidden, codeForbidden, "Token 已停用")
			return
		}
		if u.Disabled {
			v1Err(w, http.StatusForbidden, codeForbidden, "账号已被禁用")
			return
		}
		if s.tokenLimiter != nil && !s.tokenLimiter.allow(t.ID) {
			v1Err(w, http.StatusTooManyRequests, codeRateLimited, "请求过于频繁，请稍后再试")
			return
		}
		// last_used_at is only worth a write when it has gone stale; a polling
		// agent would otherwise issue one UPDATE per request.
		if s.tokenLimiter == nil || s.tokenLimiter.shouldTouch(t.ID) {
			// The timestamp is bookkeeping about the token, not part of
			// authenticating the request, so failing to store it does not fail
			// the request — but it is reported.
			if err := db.TouchAPITokenUsage(s.DB, t.ID); err != nil {
				log.Printf("api token %d: record last use: %v", t.ID, err)
			}
		}
		ctx := withToken(withUser(r.Context(), u), t)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
		if err := db.DeleteSession(s.DB, c.Value); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiBranding(w http.ResponseWriter, r *http.Request) {
	panelName, err := db.GetSetting(s.DB, "panel_name")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"panel_name": panelName})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	panelName, err := db.GetSetting(s.DB, "panel_name")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	// Keep the session this request came in on; every other session of the
	// user ends together with the old password (db.SetPassword).
	keep := ""
	if cur, err := r.Cookie(sessionCookie); err == nil {
		keep = cur.Value
	}
	if err := db.SetPassword(s.DB, u.ID, hash, keep); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
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
	existing, err := db.GetUserByUsername(s.DB, username)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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

// apiDashboard 是管理员仪表盘，挂在管理员路由下：它给的是全部节点的管理员视图，以及
// 规则与用户的总数。用户自己的概览是 apiMyDashboard（/api/my）。
func (s *Server) apiDashboard(w http.ResponseWriter, r *http.Request) {
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The dashboard only shows aggregates over rules/users, so compute them
	// server-side instead of shipping the full rules and users arrays (plus a
	// node_by_id map the UI never read) on every load.
	ruleCount, err := db.CountAllRules(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ruleCountByNode, err := db.RuleCountByNode(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	totalBytes, err := db.TotalRuleTrafficBytes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	userCount, err := db.CountUsers(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"nodes":              adminNodesOf(cat, cat.Nodes()),
		"node_raw_traffic":   cat.RawTrafficForDisplay(),
		"rule_count":         ruleCount,
		"rule_count_by_node": ruleCountByNode,
		"total_bytes":        totalBytes,
		"user_count":         userCount,
	})
}

// --- Nodes ---

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	panelName, err := db.GetSetting(s.DB, "panel_name")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	showRate, err := db.GetSetting(s.DB, "show_rate_to_user")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"nodes": adminNodesOf(cat, cat.Nodes()), "panel_url": panelURL, "panel_name": panelName,
		"node_raw_traffic":     cat.RawTrafficForDisplay(),
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

	// 线路属性两种节点都有；机器属性只给机器，成员只给组合。只对另一种节点有意义的
	// 字段直接拒绝，不悄悄丢掉。请求在任何写入之前校验完，被拒绝的请求不留下节点行。
	line := db.LineSpec{RateMultiplier: body.RateMultiplier}
	switch strings.TrimSpace(body.NodeType) {
	case "composite":
		if body.Secret != "" || body.PortRange != "" || body.Unidirectional {
			jsonErr(w, http.StatusBadRequest, "组合节点不跑 agent，没有接入凭据、端口范围与计费方向：这些按成员节点各自的设置")
			return
		}
		if len(body.Hops) < 2 {
			jsonErr(w, http.StatusBadRequest, "组合节点至少需要 2 个子节点")
			return
		}
		childIDs := make([]int64, len(body.Hops))
		members := make([]db.NodeHop, len(body.Hops))
		for i, h := range body.Hops {
			childIDs[i] = h.NodeID
			mode, err := chain.InterNodeLegMode(h.Mode)
			if err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			members[i] = db.NodeHop{HopNodeID: h.NodeID, Mode: mode}
		}
		if status, err := s.validateCompositeMembers(0, childIDs); err != nil {
			jsonErr(w, status, err.Error())
			return
		}
		n, err := db.CreateCompositeNode(s.DB, body.Name, line, members)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		db.WriteAudit(s.DB, u.ID, "node.create_composite", strconv.FormatInt(n.ID, 10), body.Name)
		s.grantInitialUsers(u.ID, n.ID, body.UserIDs)
		s.writeAdminNode(w, n.ID)
	case "", "remote":
		if len(body.Hops) > 0 {
			jsonErr(w, http.StatusBadRequest, "只有组合节点有成员；建组合请指定 node_type 为 composite")
			return
		}
		if body.PortRange != "" {
			if err := db.ValidatePortRange(body.PortRange); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// 属主是建它的管理员：这台机器的节点侧命令以属主的名义执行。
		n, err := db.CreateMachineNode(s.DB, body.Name, db.MachineSpec{
			Secret: strings.TrimSpace(body.Secret), OwnerID: &u.ID,
			PortRange: body.PortRange, Unidirectional: body.Unidirectional,
		}, line)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), body.Name)
		s.grantInitialUsers(u.ID, n.ID, body.UserIDs)
		s.dispatchNodes("新建节点", []int64{n.ID})
		s.writeAdminNode(w, n.ID)
	default:
		jsonErr(w, http.StatusBadRequest, "node_type 须为 remote 或 composite")
	}
}

// writeAdminNode 回 {"node": 节点 id 的管理员视图}，给写完节点之后的响应用。
func (s *Server) writeAdminNode(w http.ResponseWriter, id int64) {
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := cat.Node(id)
	if n == nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	jsonOK(w, map[string]any{"node": adminNodeOf(cat, n)})
}

func (s *Server) apiGetNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := cat.Node(id)
	if n == nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	// The detail page treats each list below as the node's complete state: an
	// empty rule list reads as "nothing runs here", an empty user list as "no
	// one is granted this node", and a composite's member list seeds its
	// editor. A failed query must therefore fail the request; served as an
	// empty list it would be indistinguishable from a true empty one.
	ruleHops, err := db.ListRuleHopsByNode(s.DB, n.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n.NodeType == "composite" {
		compositeHops, err := db.ListRuleHopsByCompositeNode(s.DB, n.ID)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		ruleHops = append(ruleHops, compositeHops...)
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

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
			var ownerID sql.NullInt64
			// Each lookup is for the rule of a hop listed just above, so a
			// row is expected; any error — a rule deleted in between included
			// — fails the request, which is a read and can be repeated.
			err := s.DB.QueryRow(`SELECT COUNT(*) FROM rule_hops WHERE rule_id=?`, rh.RuleID).Scan(&m.hopCount)
			if err == nil {
				err = s.DB.QueryRow(`SELECT COALESCE(n.node_type,'single'), r.node_id FROM rules r JOIN nodes n ON n.id=r.node_id WHERE r.id=?`, rh.RuleID).Scan(&m.nodeType, &m.nodeID)
			}
			if err == nil {
				err = s.DB.QueryRow(`SELECT owner_id FROM rules WHERE id=?`, rh.RuleID).Scan(&ownerID)
			}
			if err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if ownerID.Valid {
				m.ownerID = ownerID.Int64
				// An owner that no longer exists has no name to show; any
				// other failure is not an answer about the owner.
				owner, err := db.GetUserByID(s.DB, ownerID.Int64)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					jsonErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				if err == nil {
					m.ownerName = owner.Username
				}
			}
			metaCache[rh.RuleID] = m
		}
	}
	for i, rh := range ruleHops {
		m := metaCache[rh.RuleID]
		views[i] = ruleHopView{RuleHop: rh, TotalHops: m.hopCount, NodeType: m.nodeType, RuleNodeID: m.nodeID, OwnerID: m.ownerID, OwnerName: m.ownerName}
	}

	grantedUsers, err := db.ListUsersForNode(s.DB, n.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Composite nodes that directly list this node as a member hop, so the detail
	// page can show where this node is referenced. Only direct referrers: nesting
	// is resolved on the composite's own page, not transitively here. A query
	// failure degrades to an empty list rather than blocking the node detail.
	referrers, err := db.CompositeReferrers(s.DB, n.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type compositeRefView struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		NodeType string `json:"node_type"`
	}
	referencedBy := make([]compositeRefView, 0, len(referrers))
	for _, c := range referrers {
		referencedBy = append(referencedBy, compositeRefView{ID: c.ID, Name: c.Name, NodeType: c.NodeType})
	}
	resp := map[string]any{
		"node":      adminNodeDetailView{adminNodeView: adminNodeOf(cat, n), Secret: n.Secret},
		"rule_hops": views, "panel_url": panelURL,
		"panel_url_configured":     panelURL != "",
		"latest_agent_version":     serverVersion(),
		"upgrade":                  deriveUpgradeStatus(n, serverVersion(), time.Now()),
		"granted_users":            grantedUsers,
		"referenced_by_composites": referencedBy,
	}

	// Include node_hops if composite, enriched with each child's name.
	if n.NodeType == "composite" {
		nodeHops, err := db.ListNodeHops(s.DB, n.ID)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := make([]nodeHopView, len(nodeHops))
		for i, h := range nodeHops {
			nm := ""
			if nd := cat.Node(h.HopNodeID); nd != nil {
				nm = nd.Name
			}
			views[i] = nodeHopView{NodeHop: h, NodeName: nm}
		}
		resp["node_hops"] = views
		// Candidate members for the composite editor: any node except this
		// composite itself (a self-reference is refused). Nesting is allowed, so
		// other composites are eligible members too — chain.Topology.ValidateMembers on
		// the server is the authoritative cycle guard.
		candidates := make([]adminNodeView, 0, len(cat.Nodes()))
		for _, nd := range cat.Nodes() {
			if nd.ID != n.ID {
				candidates = append(candidates, adminNodeOf(cat, nd))
			}
		}
		resp["single_nodes"] = candidates
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
// belongs to the rule (see chain.ExitChoice), so the stored last-hop mode stays
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
	type hopUpdate struct {
		NodeID int64  `json:"node_id"`
		Mode   string `json:"mode"`
	}
	var body struct {
		Hops []hopUpdate `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
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
	// 先判定 id 是不是组合（ValidateMembers），再看成员个数：给机器写成员的请求报的是
	// “非组合节点”。
	if status, err := s.validateCompositeMembers(id, childIDs); err != nil {
		jsonErr(w, status, err.Error())
		return
	}
	if len(body.Hops) < 2 {
		jsonErr(w, http.StatusBadRequest, "组合节点至少需要 2 个子节点")
		return
	}
	hops := make([]db.NodeHop, len(body.Hops))
	for i, hu := range body.Hops {
		mode, err := chain.InterNodeLegMode(hu.Mode)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		hops[i] = db.NodeHop{NodeID: id, Position: i, HopNodeID: hu.NodeID, Mode: mode}
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
		mode, err := chain.InterNodeLegMode(b.Mode)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
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
			jsonErr(w, http.StatusBadRequest, "下游节点需先开启中转角色")
			return
		}
		mode, err := chain.InterNodeLegMode(b.Mode)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
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
		jsonMachineWriteErr(w, err, "组合节点没有自己的中继地址：入口取展开后第一台机器的中继地址，请在那台机器上设置")
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	// 地址已经落库，引用本节点的规则要经这次重新接线才用上它。原样再保存一次不改变
	// 结果，等于重试这次接线。
	if err := s.rewireRulesReferencingNode(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, "中继地址已保存，但没能确定需要重新接线的规则: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// rewireRulesReferencingNode 重新生成所有引用 nodeID 的规则，并下发受影响的节点。
//
// relay_host 是其他节点把流量送到本节点时用的数据面地址，建规则时被抄进上游跳的
// rule_hops.target_host。所以改写 relay_host 的每一条路径都要经过这里：管理员接口
// apiSetNodeRelayHost 直接调用，节点 hello 里的声明与回填经 Hub.OnRelayHostChange
// 调用。
//
// 返回值只有一种含义：没能读出哪些规则引用了这个节点。这时地址已经改了，却没有任何
// 规则跟着重接，调用方必须如实上报，不能当作“没有规则引用它”。单条规则重接失败、
// 单个节点下发失败不在返回值里（见 rulechange.Changes.Rewire、dispatchNodes）。
func (s *Server) rewireRulesReferencingNode(nodeID int64) error {
	ruleIDs, err := db.RulesReferencingNode(s.DB, nodeID)
	if err != nil {
		return err
	}
	if len(ruleIDs) > 0 {
		s.apiRewireRules(ruleIDs)
	}
	return nil
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
		cnt, err := db.CountV6EntryRulesOnNode(s.DB, id)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cnt > 0 {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("该节点上有 %d 条使用 v6 入口的规则，需先将它们的入口类型改为 v4 才能清除 IPv6 中继地址", cnt))
			return
		}
	}
	if err := db.UpdateNodeRelayHostV6(s.DB, id, host); err != nil {
		jsonMachineWriteErr(w, err, "组合节点没有自己的 IPv6 中继地址：入口与出口取展开后首尾两台机器的地址，请在那两台机器上设置")
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host_v6", strconv.FormatInt(id, 10), host)
	jsonOK(w, map[string]any{"ok": true})
}

// jsonMachineWriteErr 报告一次机器属性写入（db.updateMachine）的失败：目标是组合时回
// 400 与 compositeMsg，节点不存在回 404，其余回 500。
func jsonMachineWriteErr(w http.ResponseWriter, err error, compositeMsg string) {
	switch {
	case errors.Is(err, db.ErrPhysicalNodeSetting):
		jsonErr(w, http.StatusBadRequest, compositeMsg)
	case errors.Is(err, sql.ErrNoRows):
		jsonErr(w, http.StatusNotFound, "节点不存在")
	default:
		jsonErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) apiUpdateNodePortRange(w http.ResponseWriter, r *http.Request) {
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
		jsonMachineWriteErr(w, err, "组合节点不支持设置端口范围：监听端口在成员节点上分配，按各成员节点的端口范围")
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
		jsonMachineWriteErr(w, err, "组合节点没有自己的计费方向：按上报流量的成员节点各自的设置计费，请在成员节点上设置")
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
		jsonMachineWriteErr(w, err, "组合节点不能设置“禁止直接转发”：能否作为链尾由组合里最后一个成员节点决定，请在该成员节点上设置")
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
	if aerr := s.resyncNode(id); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
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
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	// 推送目标恒为面板自身版本：loadAgentArtifact 只取 serverVersion() 对应 release
	// 的产物，各架构的产物版本号相同。
	version := serverVersion()
	err = s.upgradeNode(node, panelURL)
	if errors.Is(err, errNotAMachine) {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Record the dispatch outcome so the node detail can surface a silent
	// failure later (an acked upgrade whose version never takes).
	status, errText := "acked", ""
	if err != nil {
		status, errText = "error", err.Error()
	}
	if err := db.RecordUpgradeResult(s.DB, id, version, status, errText); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade", strconv.FormatInt(id, 10), version)
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
	// Deleting a node FK-cascades its member rows out of every composite that
	// references it, silently altering those composites' definitions. Warn first:
	// unless the client confirms, return the affected composites instead of
	// deleting. Applies to both physical and composite nodes — a composite can be
	// nested inside another composite.
	// A failed lookup must stop the delete: read as "no composite refers to
	// it", it would skip the very confirmation this check exists for.
	referrers, err := db.CompositeReferrers(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(referrers) > 0 && r.URL.Query().Get("confirm") != "1" {
		affected := make([]map[string]any, len(referrers))
		for i, ref := range referrers {
			affected[i] = map[string]any{"id": ref.ID, "name": ref.Name}
		}
		jsonOK(w, map[string]any{"needs_confirm": true, "affected_composites": affected})
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
		s.dispatchNodes("规则变更", rerun)
		jsonOK(w, map[string]any{"ok": true})
		return
	}
	// A physical node: re-wire the rules it participated in, splicing it out of
	// each chain (its hop rows FK-cascade away on delete, and rewire
	// re-materializes the surviving hops onto their new neighbors).
	// Looked up before the delete and required to succeed: once the node is
	// gone its hop rows are too, and the rules that need re-wiring could no
	// longer be found.
	affectedRules, err := db.RulesReferencingNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	// 节点启停是压制条件的输入：经过它的规则随之整条撤下或恢复，链上的每个节点都要
	// 重推，不只是它自己。启停此时已经落库；读不出要通知哪些节点时如实回答。这个接口
	// 是翻转（db.ToggleNode），再请求一次是把状态翻回去，不是重试这次下发。
	if err := s.redispatchThroughNode(id, "节点启停"); err != nil {
		jsonErr(w, http.StatusInternalServerError, "节点启停已保存，但没能确定需要重新下发的节点: "+err.Error())
		return
	}
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
		jsonMachineWriteErr(w, err, "组合节点没有属主：属主是机器的节点侧命令的执行身份，组合不跑 agent")
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
	synced, failed, err := s.resyncAllNodes()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "synced": synced, "failed": failed})
}

func (s *Server) apiUpgradeAllNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	all, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var nodes []*db.Node
	for _, n := range all {
		if requireMachine(n) != nil || n.Disabled {
			continue
		}
		nodes = append(nodes, n)
	}
	// 先把本批在线节点用得到的产物取齐，任何一个取不到就整批失败、一个都不推。
	// 产物取不到（dev 构建、GitHub 不可达、release 缺资产）是面板侧的故障，不是
	// 某个节点的故障；前端对本接口只在请求失败时报错，若把它摊成逐节点失败，
	// 界面上会是一句“已发起升级”而实际一台都没升。
	// 离线、或架构不在 agentAssets 里的节点不参与这一步——那是节点级结果，由下面
	// 的逐节点推送各自记账，不该拦住其他节点。
	for _, n := range nodes {
		arch, online := s.Hub.NodeArch(n.ID)
		if !online {
			continue
		}
		if _, err := agentAssetName(arch); err != nil {
			continue
		}
		if _, err := s.loadAgentArtifact(arch); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	version := serverVersion()
	var ok, fail int
	for _, n := range nodes {
		err := s.upgradeNode(n, panelURL)
		status, errText := "acked", ""
		if err != nil {
			status, errText = "error", err.Error()
			fail++
		} else {
			ok++
		}
		if err := db.RecordUpgradeResult(s.DB, n.ID, version, status, errText); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade_all", "", fmt.Sprintf("ok=%d fail=%d", ok, fail))
	jsonOK(w, map[string]any{"ok": true, "upgraded": ok, "failed": fail})
}

// --- Settings ---

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	panelName, err := db.GetSetting(s.DB, "panel_name")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	showRate, err := db.GetSetting(s.DB, "show_rate_to_user")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	poolSizeStr, err := db.GetSetting(s.DB, "pool_size")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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

// loadNodeRoles reads the global node-role table (the "node_roles" settings
// row). Values stored before the role became a bitmask — the strings
// "landing" / "direct" — are read as their bit; a row that is not valid JSON
// is an error rather than an empty table, because the caller of a write would
// otherwise replace the whole table with its own change.
func (s *Server) loadNodeRoles() (map[string]int, error) {
	val, err := db.GetSetting(s.DB, "node_roles")
	if err != nil {
		return nil, err
	}
	roles := map[string]int{}
	if val == "" {
		return roles, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(val), &raw); err != nil {
		return nil, fmt.Errorf("node_roles setting: %w", err)
	}
	for k, v := range raw {
		var n int
		if err := json.Unmarshal(v, &n); err == nil {
			if n &= roleMask; n != 0 {
				roles[k] = n
			}
			continue
		}
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
	return roles, nil
}

func (s *Server) apiGetNodeRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.loadNodeRoles()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"roles": roles})
}

// apiSetNodeRoles turns role bits on or off for a set of node keys and
// returns the resulting table.
//
// The table is one settings row shared by every admin, so a write is a
// read-modify-write of the whole row. It is expressed as a change (these
// keys, these bits, on or off) and applied here under nodeRolesMu: two admins
// editing different nodes, or the same node's two roles, both take effect
// whatever order their requests arrive in. This handler is the row's only
// writer.
func (s *Server) apiSetNodeRoles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
		Role int      `json:"role"`
		On   bool     `json:"on"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Keys) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择节点")
		return
	}
	if body.Role == 0 || body.Role&^roleMask != 0 {
		jsonErr(w, http.StatusBadRequest, "无效的用途")
		return
	}
	s.nodeRolesMu.Lock()
	defer s.nodeRolesMu.Unlock()
	roles, err := s.loadNodeRoles()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, k := range body.Keys {
		if k == "" {
			continue
		}
		next := roles[k] &^ body.Role
		if body.On {
			next = roles[k] | body.Role
		}
		if next == 0 {
			delete(roles, k)
		} else {
			roles[k] = next
		}
	}
	b, err := json.Marshal(roles)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.SetSetting(s.DB, "node_roles", string(b)); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "roles": roles})
}

// --- Rules ---

func (s *Server) apiListRules(w http.ResponseWriter, r *http.Request) {
	var rules []*db.Rule
	var err error
	if raw := r.URL.Query().Get("owner_ids"); raw != "" {
		var ids []int64
		for _, part := range strings.Split(raw, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		rules, err = db.ListRulesByOwnerIDs(s.DB, ids)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		rules, err = db.ListAllRules(s.DB)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	allUsers, err := db.ListUsers(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	userList := make([]map[string]any, 0, len(allUsers))
	for _, u := range allUsers {
		userList = append(userList, map[string]any{"id": u.ID, "username": u.Username})
	}
	items, err := s.ruleItems(rules, cat, false)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"rules": items,
		"nodes": adminNodesOf(cat, cat.Nodes()),
		"users": userList,
	})
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
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hops, err := db.ListRuleHops(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := s.ruleItems([]*db.Rule{rl}, cat, true)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes := adminNodesOf(cat, cat.Nodes())
	jsonOK(w, map[string]any{
		"rule": items[0], "hops": hops,
		"nodes": nodes, "node_by_id": adminNodesByID(nodes),
	})
}

// --- Users ---

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := db.ListUsers(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.FillUserRuleCounts(s.DB, users); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	grantedNodes, grants, err := db.ListNodesForUser(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules, err := db.ListRulesByUser(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The landing_nodes preview doubles as a sync point: any successful
	// resolution keeps the materialized set fresh without waiting for the
	// background pass.
	landingPreview, err := s.resolveLandingExits(target, false)
	if err == nil {
		s.syncLandingExits(target, landingPreview)
	} else {
		landingPreview = s.landingNodesFor(target, false)
	}
	jsonOK(w, map[string]any{
		"user": apiUserFullView(target), "nodes": adminNodesOf(cat, grantedNodes),
		"grants": grants, "all_nodes": adminNodesOf(cat, cat.Nodes()),
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
	var expiresAt int64
	if raw := strings.TrimSpace(body.ExpiresAt); raw != "" {
		if et, err := time.Parse("2006-01-02", raw); err == nil {
			expiresAt = et.Unix()
		}
	}
	user, aerr := s.provisionUser(u.ID, provisionParams{
		Username: body.Username, Password: body.Password, Role: body.Role,
		MaxForwards: body.MaxForwards, TrafficQuotaBytes: body.TrafficQuotaBytes,
		ExpiresAtUnix: expiresAt, LandingSubURL: body.LandingSubURL, AdminNote: body.AdminNote,
	})
	if aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	jsonOK(w, map[string]any{"user": user})
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
	ids := body.NodeIDs
	if len(ids) == 0 && body.NodeID != 0 {
		ids = []int64{body.NodeID}
	}
	if len(ids) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择节点")
		return
	}
	for _, nid := range ids {
		if aerr := s.grantUserNode(u.ID, userID, nid, body.MaxForwards, body.TrafficQuotaBytes); aerr != nil {
			jsonErr(w, aerr.status, aerr.msg)
			return
		}
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
	removed, aerr := s.revokeUserNode(u.ID, userID, nodeID)
	if aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "removed_rule_nodes": removed})
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
	s.dispatchNodes("规则变更", affected)
	jsonOK(w, map[string]any{"ok": true})
}

// apiBatchUpdateNodeGrants applies one node's grant-roster diff in a single
// request: grants every user in add_user_ids (default per-node quota) and
// revokes every user in remove_user_ids. Revoking also deletes the removed
// users' rules on this node — same contract as apiRevokeNode — and the fanout
// is dispatched once at the end. This replaces the node detail page's roster
// editor issuing N sequential requests that could partially fail with no
// indication of which users were actually applied.
func (s *Server) apiBatchUpdateNodeGrants(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodeID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		AddUserIDs    []int64 `json:"add_user_ids"`
		RemoveUserIDs []int64 `json:"remove_user_ids"`
		MaxForwards   int     `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.AddUserIDs) == 0 && len(body.RemoveUserIDs) == 0 {
		jsonErr(w, http.StatusBadRequest, "没有需要变更的授权")
		return
	}
	if body.MaxForwards <= 0 {
		body.MaxForwards = defaultGrantMaxForwards
	}
	// Additions first: a user appearing in both lists ends up revoked, which
	// matches the roster editor's semantics (the final list is authoritative).
	granted := 0
	for _, uid := range body.AddUserIDs {
		if err := db.GrantNode(s.DB, uid, nodeID, body.MaxForwards, 0); err != nil {
			jsonErr(w, http.StatusInternalServerError,
				fmt.Sprintf("已新增 %d/%d 个授权后失败: %s", granted, len(body.AddUserIDs), err.Error()))
			return
		}
		db.WriteAudit(s.DB, u.ID, "user.grant_node", strconv.FormatInt(uid, 10), strconv.FormatInt(nodeID, 10))
		granted++
	}
	// db.GrantNode 对已有授权是覆盖写：名单里若有已授权的用户，他这条授权的配额被
	// 写回 0（不限量），此前因授权配额用尽被排除的规则要恢复下发。
	var affected []int64
	for _, uid := range body.AddUserIDs {
		if nodes, err := db.RulesAffectedByNode(s.DB, uid, nodeID); err == nil {
			affected = append(affected, nodes...)
		}
	}
	revoked := 0
	for _, uid := range body.RemoveUserIDs {
		if err := db.RevokeNode(s.DB, uid, nodeID); err != nil {
			jsonErr(w, http.StatusInternalServerError,
				fmt.Sprintf("已移除 %d/%d 个授权后失败: %s", revoked, len(body.RemoveUserIDs), err.Error()))
			return
		}
		nodes, err := db.DeleteRulesForUserNode(s.DB, uid, nodeID)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		affected = append(affected, nodes...)
		db.WriteAudit(s.DB, u.ID, "user.revoke_node", strconv.FormatInt(uid, 10), strconv.FormatInt(nodeID, 10))
		revoked++
	}
	s.dispatchNodes("规则变更", affected)
	jsonOK(w, map[string]any{"ok": true, "granted": granted, "revoked": revoked})
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
	if aerr := s.setUserQuota(u.ID, id, body.TrafficQuotaBytes); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
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
	if aerr := s.setUserMaxForwards(u.ID, id, body.MaxForwards); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
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
	if raw := strings.TrimSpace(body.ExpiresAt); raw != "" {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "日期格式无效（需 YYYY-MM-DD）")
			return
		}
		expiresAt = t.Unix()
	}
	if aerr := s.setUserExpiry(u.ID, id, expiresAt); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
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
	if err := accounting.ResetUser(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_traffic", strconv.FormatInt(id, 10), "")
	// 用量归零后，此前因全局配额、授权配额或落地出口配额用尽被排除的规则都应恢复，
	// 要经这次重新下发才到节点上。再重置一次不改变结果，等于重试这次下发。
	if err := s.redispatchUser(id, "重置流量"); err != nil {
		jsonErr(w, http.StatusInternalServerError, "流量已重置，但没能确定需要重新下发的节点: "+err.Error())
		return
	}
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
	// 这里一次改了到期时间与全局配额，两者都是下发判定的条件，要经这次重新下发才到
	// 节点上。原样再保存一次不改变结果，等于重试这次下发。
	if err := s.redispatchUser(id, "修改用户资料"); err != nil {
		jsonErr(w, http.StatusInternalServerError, "资料已保存，但没能确定需要重新下发的节点: "+err.Error())
		return
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
	// Flip: a currently-disabled user becomes enabled and vice versa.
	enabled := target.Disabled
	if aerr := s.setUserEnabled(u.ID, id, enabled); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "disabled": !enabled})
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
	if err := db.SetPassword(s.DB, id, hash, ""); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	s.dispatchNodes("规则变更", affected)
	db.WriteAudit(s.DB, u.ID, "user.delete", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- My (user self-service) endpoints ---

func (s *Server) apiMyDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	// 节点带的是这个用户授权生效后的角色（myNodeOf），与规则表单筛选入口、中转层时
	// 用的是同一个值，不是节点自身的掩码。
	nodes, grants, _, err := s.grantedNodes(u)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules, err := db.ListRulesByUser(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	showRate, err := db.GetSetting(s.DB, "show_rate_to_user")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"user": apiUserFullView(u), "nodes": nodes, "grants": grants,
		"rules": rules, "show_rate": showRate == "1",
	})
}

func (s *Server) apiMyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, _, cat, err := s.grantedNodes(u)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules, err := db.ListRulesByUser(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 级联选择中转层用的绑定边：两端都在用户的授权里才给。
	grantedSet := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		grantedSet[n.ID] = true
	}
	allEdges, err := db.ListAllNodeBindings(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	edges := make([]*db.NodeBinding, 0, len(allEdges))
	for _, e := range allEdges {
		if grantedSet[e.UpstreamNodeID] && grantedSet[e.DownstreamNodeID] {
			edges = append(edges, e)
		}
	}
	showRate, err := db.GetSetting(s.DB, "show_rate_to_user")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := s.ruleItems(rules, cat, true)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"rules": items, "nodes": nodes,
		"node_by_id": myNodesByID(nodes), "show_rate": showRate == "1",
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
	nodes, _, cat, err := s.grantedNodes(u)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	showRate, err := db.GetSetting(s.DB, "show_rate_to_user")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := s.ruleItems([]*db.Rule{rl}, cat, true)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"rule": items[0], "nodes": nodes,
		"node_by_id": myNodesByID(nodes), "show_rate": showRate == "1",
	})
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

// withUser 把登录用户放进请求上下文，userFromCtx 用同一个键取。会话与 token 两种
// 认证（requireAPIAuth、requireTokenAuth）都经它。
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
	m["over_quota"] = u.OverQuota
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
	if aerr := s.setPerNodeQuota(u.ID, userID, nodeID, body.TrafficQuotaBytes); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
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
	if aerr := s.setPerNodeRateLimit(u.ID, userID, nodeID, body.RateLimitMBytes); aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetPerNodeRoles(w http.ResponseWriter, r *http.Request) {
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
		Roles int64 `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// 0 = inherit the node mask; any other value must be a legal entry/via
	// combination. This only changes what the grantee may do with the node, so
	// there is no need to constrain it to a subset of the node's own mask.
	if body.Roles&^(db.NodeRoleEntry|db.NodeRoleVia) != 0 {
		jsonErr(w, http.StatusBadRequest, "roles invalid")
		return
	}
	if err := db.SetGrantRoles(s.DB, userID, nodeID, body.Roles); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_roles", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d roles=%d", nodeID, body.Roles))
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
	if err := accounting.ResetGrant(s.DB, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_node_traffic", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	// 这条授权的用量归零后，此前因它的配额用尽被排除的规则应恢复，要经这次重新下发才
	// 到节点上。再重置一次不改变结果，等于重试这次下发。
	if err := s.redispatchUserGrant(userID, nodeID, "重置授权流量"); err != nil {
		jsonErr(w, http.StatusInternalServerError, "流量已重置，但没能确定需要重新下发的节点: "+err.Error())
		return
	}
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

func (s *Server) apiMyGetToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := db.GetAPITokenByUser(s.DB, u.ID)
	if errors.Is(err, sql.ErrNoRows) {
		jsonOK(w, map[string]any{"has_token": false})
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var lastUsed any
	if t.LastUsedAt.Valid {
		lastUsed = t.LastUsedAt.Int64
	}
	jsonOK(w, map[string]any{
		"has_token":    true,
		"token_prefix": t.TokenPrefix,
		"scope":        t.Scope,
		"disabled":     t.Disabled,
		"created_at":   t.CreatedAt,
		"last_used_at": lastUsed,
	})
}

func (s *Server) apiMyCreateToken(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	// Scope is optional and defaults to the least-privileged read; a caller opts
	// into write power explicitly. An unrecognized value normalizes to read
	// rather than failing, so an old client that omits it stays read-only.
	var body struct {
		Scope string `json:"scope"`
	}
	_ = decodeJSON(r, &body)
	scope := db.NormalizeTokenScope(body.Scope)
	token, err := db.CreateAPIToken(s.DB, u.ID, scope)
	if err != nil {
		jsonErr(w, http.StatusConflict, "已存在 Token，请先删除后重新创建")
		return
	}
	jsonOK(w, map[string]any{"token": token, "scope": scope})
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
		// OnlyMissing leaves a (user, node) pair that already has a grant
		// untouched instead of overwriting its caps; such pairs are counted
		// in the response's "kept".
		OnlyMissing bool `json:"only_missing"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	specs := make([]batchGrantSpec, len(body.Grants))
	for i, g := range body.Grants {
		specs[i] = batchGrantSpec{NodeName: g.NodeName, MaxForwards: g.MaxForwards, TrafficQuotaBytes: g.TrafficQuotaBytes, RateLimitMBytes: g.RateLimitMBytes}
	}
	granted, kept, skipped, aerr := s.batchApplyGrants(u.ID, body.UserIDs, specs, body.OnlyMissing)
	if aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "granted": granted, "kept": kept, "skipped_nodes": skipped})
}
