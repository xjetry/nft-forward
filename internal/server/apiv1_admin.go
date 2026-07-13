package server

import (
	"net/http"
	"strings"
	"time"

	"nft-forward/internal/db"
)

// v1RequireUser writes a 404 and returns false when no user with id exists, so
// admin write handlers reject bad ids before mutating.
func (s *Server) v1RequireUser(w http.ResponseWriter, id int64) bool {
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		v1Err(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return false
	}
	return true
}

// v1AdminCreateUser provisions a user and, unless issue_token is explicitly
// false, mints their token in the same call — the closed-loop onboarding an
// agent uses to hand off a ready-to-use API consumer. The plaintext token is
// returned once.
func (s *Server) v1AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	var body struct {
		Username          string `json:"username"`
		Password          string `json:"password"`
		Role              string `json:"role"`
		MaxForwards       int    `json:"max_forwards"`
		TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
		ExpiresAt         int64  `json:"expires_at"`
		IssueToken        *bool  `json:"issue_token"`
		TokenScope        string `json:"token_scope"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	password := body.Password
	if strings.TrimSpace(password) == "" {
		// API-only consumers authenticate by token, not password. Give them a
		// random, unguessable one rather than an empty/known secret.
		password = db.RandToken(16)
	}
	user, aerr := s.provisionUser(admin.ID, provisionParams{
		Username: body.Username, Password: password, Role: body.Role,
		MaxForwards: body.MaxForwards, TrafficQuotaBytes: body.TrafficQuotaBytes,
		ExpiresAtUnix: body.ExpiresAt,
	})
	if aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	out := map[string]any{"user": toV1User(user)}
	issue := body.IssueToken == nil || *body.IssueToken // default true
	if issue {
		scope := db.NormalizeTokenScope(body.TokenScope)
		tok, _, err := db.IssueUserToken(s.DB, user.ID, scope)
		if err != nil {
			v1Err(w, http.StatusInternalServerError, codeInternal, "建 token 失败")
			return
		}
		out["token"] = tok
		out["token_scope"] = scope
	}
	v1OK(w, out)
}

// v1AdminMintUserToken mints or rotates the target user's single token. Because
// api_tokens.user_id is UNIQUE, re-minting for a user who already has one
// rotates it (rotated=true); the plaintext is returned once.
func (s *Server) v1AdminMintUserToken(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	_ = decodeJSON(r, &body)
	scope := db.NormalizeTokenScope(body.Scope)
	tok, rotated, err := db.IssueUserToken(s.DB, id, scope)
	if err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, "铸 token 失败")
		return
	}
	prefix := ""
	if t, e := db.GetAPITokenByUser(s.DB, id); e == nil {
		prefix = t.TokenPrefix
	}
	v1OK(w, map[string]any{"token": tok, "scope": scope, "token_prefix": prefix, "rotated": rotated})
}

// v1AdminGetUserToken returns token metadata (never the plaintext) so an admin
// can inspect a user's API state.
func (s *Server) v1AdminGetUserToken(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	t, err := db.GetAPITokenByUser(s.DB, id)
	if err != nil {
		v1OK(w, map[string]any{"has_token": false})
		return
	}
	var lastUsed *int64
	if t.LastUsedAt.Valid {
		lu := t.LastUsedAt.Int64
		lastUsed = &lu
	}
	v1OK(w, map[string]any{
		"has_token": true, "token_prefix": t.TokenPrefix, "scope": t.Scope,
		"disabled": t.Disabled, "created_at": t.CreatedAt, "last_used_at": lastUsed,
	})
}

// v1AdminDeleteUserToken revokes a user's token.
func (s *Server) v1AdminDeleteUserToken(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if err := db.DeleteAPIToken(s.DB, id); err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, "删除失败")
		return
	}
	v1OK(w, map[string]any{"deleted": true})
}

func (s *Server) v1AdminSetUserQuota(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setUserQuota(admin.ID, id, body.TrafficQuotaBytes); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminSetMaxForwards(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		MaxForwards int `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setUserMaxForwards(admin.ID, id, body.MaxForwards); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminSetExpiry(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		ExpiresAt int64 `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setUserExpiry(admin.ID, id, body.ExpiresAt); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminGrantNode(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeId")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad node id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		MaxForwards       int   `json:"max_forwards"`
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	_ = decodeJSON(r, &body)
	if aerr := s.grantUserNode(admin.ID, id, nodeID, body.MaxForwards, body.TrafficQuotaBytes); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"granted": true})
}

func (s *Server) v1AdminRevokeNode(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeId")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad node id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	removed, aerr := s.revokeUserNode(admin.ID, id, nodeID)
	if aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"removed": true, "removed_rule_nodes": removed})
}

func (s *Server) v1AdminSetPerNodeQuota(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeId")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad node id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setPerNodeQuota(admin.ID, id, nodeID, body.TrafficQuotaBytes); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminSetPerNodeRate(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeId")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad node id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setPerNodeRateLimit(admin.ID, id, nodeID, body.RateLimitMBytes); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminSetEnabled(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if !s.v1RequireUser(w, id) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	if aerr := s.setUserEnabled(admin.ID, id, body.Enabled); aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	v1OK(w, map[string]any{"updated": true})
}

func (s *Server) v1AdminBatchApplyGrants(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
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
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	specs := make([]batchGrantSpec, len(body.Grants))
	for i, g := range body.Grants {
		specs[i] = batchGrantSpec{NodeName: g.NodeName, MaxForwards: g.MaxForwards, TrafficQuotaBytes: g.TrafficQuotaBytes, RateLimitMBytes: g.RateLimitMBytes}
	}
	granted, skipped, aerr := s.batchApplyGrants(admin.ID, body.UserIDs, specs)
	if aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	if skipped == nil {
		skipped = []string{}
	}
	v1OK(w, map[string]any{"granted": granted, "skipped_nodes": skipped})
}

func (s *Server) v1AdminResyncNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if err := s.dispatchToNode(id); err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	v1OK(w, map[string]any{"resynced": true})
}

func (s *Server) v1AdminResyncAllNodes(w http.ResponseWriter, r *http.Request) {
	synced, failed, err := s.resyncAllNodes()
	if err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	v1OK(w, map[string]any{"synced": synced, "failed": failed})
}

// v1LandingExit is the frozen public shape of one landing-exit ledger row:
// exactly {user_id, host, port, protocol, present, used_bytes, updated_at}. The
// db row carries more (name/name_override/quota_bytes) which stays internal.
type v1LandingExit struct {
	UserID    int64  `json:"user_id"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Protocol  string `json:"protocol"`
	Present   bool   `json:"present"`
	UsedBytes int64  `json:"used_bytes"`
	UpdatedAt int64  `json:"updated_at"`
}

// v1LandingUsage exposes the cluster-wide landing-exit ledger so a downstream
// orchestrator (e.g. sing-board) can pull consumption for every user without
// walking each user's node grants individually.
func (s *Server) v1LandingUsage(w http.ResponseWriter, r *http.Request) {
	exits, err := db.ListAllLandingExits(s.DB)
	if err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	out := make([]v1LandingExit, 0, len(exits))
	for _, e := range exits {
		out = append(out, v1LandingExit{
			UserID: e.UserID, Host: e.Host, Port: e.Port, Protocol: e.Protocol,
			Present: e.Present, UsedBytes: e.UsedBytes, UpdatedAt: e.UpdatedAt,
		})
	}
	v1OK(w, out)
}

// v1Usage returns a one-shot billing snapshot: per-user rollup, per-node raw
// traffic + rule counts, and cluster totals. Read scope suffices — it's pure
// observability. Node traffic uses the raw per-node counter (the same figure
// the admin aggregate view shows), not per-rule attribution.
func (s *Server) v1Usage(w http.ResponseWriter, r *http.Request) {
	users, _ := db.ListUsers(s.DB)
	db.FillUserRuleCounts(s.DB, users)
	userViews := make([]map[string]any, 0, len(users))
	for _, u := range users {
		var exp *int64
		if u.ExpiresAt.Valid && u.ExpiresAt.Int64 != 0 {
			e := u.ExpiresAt.Int64
			exp = &e
		}
		userViews = append(userViews, map[string]any{
			"user_id": u.ID, "username": u.Username, "role": u.Role, "disabled": u.Disabled,
			"traffic_used_bytes": u.TrafficUsedBytes, "traffic_quota_bytes": u.TrafficQuotaBytes,
			"rule_count": u.RuleCount, "expires_at": exp,
		})
	}
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, nodes)
	rawByNode, _ := db.NodeRawTraffic(s.DB)
	rcByNode, _ := db.RuleCountByNode(s.DB)
	online := 0
	nodeViews := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if n.Online == 1 {
			online++
		}
		nodeViews = append(nodeViews, map[string]any{
			"node_id": n.ID, "name": n.Name, "node_type": n.NodeType,
			"rate_multiplier": n.RateMultiplier, "traffic_bytes": rawByNode[n.ID], "rule_count": rcByNode[n.ID],
		})
	}
	userCount, _ := db.CountUsers(s.DB)
	ruleCount, _ := db.CountAllRules(s.DB)
	totalBytes, _ := db.TotalRuleTrafficBytes(s.DB)
	v1OK(w, map[string]any{
		"generated_at": time.Now().Unix(),
		"totals": map[string]any{
			"users": userCount, "nodes_total": len(nodes), "nodes_online": online,
			"rules": ruleCount, "traffic_bytes": totalBytes,
		},
		"users": userViews,
		"nodes": nodeViews,
	})
}
