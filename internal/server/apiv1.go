package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

// The /api/v1 surface is the stable, token-authenticated contract for
// programmatic / AI-agent callers. It is deliberately separate from /api (the
// session-only SPA surface): /api may change shape with the frontend, /api/v1
// must not. Every response here uses its own slim DTOs — never the SPA payloads —
// and a fixed envelope, so the two contracts evolve independently.

// tokenKey stores the authenticated *db.APIToken in the request context. It must
// be a distinct ctxKey value from userKey (which is 0, iota in auth.go).
const tokenKey ctxKey = 1

func withToken(ctx context.Context, t *db.APIToken) context.Context {
	return context.WithValue(ctx, tokenKey, t)
}

func tokenFromCtx(ctx context.Context) *db.APIToken {
	v, _ := ctx.Value(tokenKey).(*db.APIToken)
	return v
}

// Stable machine-readable error codes for /api/v1. The message stays
// human-readable (Chinese is fine); agents branch on the code.
const (
	codeUnauthorized  = "unauthorized"
	codeForbidden     = "forbidden"
	codeScopeRequired = "scope_required"
	codeRateLimited   = "rate_limited"
	codeValidation    = "validation"
	codeNotFound      = "not_found"
	codeConflict      = "conflict"
	codeDuplicateHop  = "duplicate_chain_node"
	codeInternal      = "internal"
)

// v1OK writes the standard success envelope: {"data": ...}.
func v1OK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// v1Err writes the standard error envelope: {"error":{"code","message"}}.
func v1Err(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// v1RequireRole is the /api/v1 role gate: same check as requireRole but emitting
// the v1 error envelope instead of plain text.
func (s *Server) v1RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := userFromCtx(r.Context())
			if u == nil || u.Role != role {
				v1Err(w, http.StatusForbidden, codeForbidden, "权限不足")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireScope gates a route on the token's scope. A readwrite requirement is met
// only by a readwrite token; a read requirement is met by either.
func (s *Server) requireScope(want string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := tokenFromCtx(r.Context())
			if t == nil || !scopeSatisfies(t.Scope, want) {
				v1Err(w, http.StatusForbidden, codeScopeRequired, "该操作需要 readwrite 权限的 Token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// scopeSatisfies reports whether a token scope of have covers a want requirement.
func scopeSatisfies(have, want string) bool {
	if want == db.TokenScopeReadWrite {
		return have == db.TokenScopeReadWrite
	}
	return have == db.TokenScopeRead || have == db.TokenScopeReadWrite
}

// registerV1Routes mounts the token-authenticated public API. All routes sit
// behind requireTokenAuth (which also rate-limits and stamps last_used); role and
// scope gates layer on top per group.
func (s *Server) registerV1Routes(r chi.Router) {
	r.Use(s.requireTokenAuth)

	// Self + shared diagnostics: any authenticated token, read scope.
	r.Get("/info", s.v1Info)
	r.Get("/probe", s.v1Probe)
	r.Get("/probe-chain", s.v1ProbeChain)

	// User self-service reads.
	r.Group(func(r chi.Router) {
		r.Use(s.v1RequireRole("user"))
		r.Get("/my/nodes", s.v1MyListNodes)
		r.Get("/my/rules", s.v1MyListRules)
		r.Get("/my/rules/{id}", s.v1MyGetRule)
	})

	// User self-service writes: readwrite scope on top of the user role.
	r.Group(func(r chi.Router) {
		r.Use(s.v1RequireRole("user"), s.requireScope(db.TokenScopeReadWrite))
		r.Post("/my/rules", s.v1MyCreateRule)
		r.Put("/my/rules/{id}", s.v1MyUpdateRule)
		r.Delete("/my/rules/{id}", s.v1MyDeleteRule)
	})

	// Admin reads: cluster observability.
	r.Group(func(r chi.Router) {
		r.Use(s.v1RequireRole("admin"))
		r.Get("/nodes", s.v1ListNodes)
		r.Get("/users", s.v1ListUsers)
		r.Get("/dashboard", s.v1Dashboard)
		r.Get("/users/{id}/token", s.v1AdminGetUserToken)
	})

	// Admin writes: readwrite scope on top of the admin role. All mutations are
	// declarative (PUT/DELETE absolute values), so retries are naturally
	// idempotent — no Idempotency-Key needed.
	r.Group(func(r chi.Router) {
		r.Use(s.v1RequireRole("admin"), s.requireScope(db.TokenScopeReadWrite))
		r.Post("/users", s.v1AdminCreateUser)
		r.Post("/users/{id}/token", s.v1AdminMintUserToken)
		r.Delete("/users/{id}/token", s.v1AdminDeleteUserToken)
	})
}

// --- DTOs ---

// v1Node is the slim node shape for the public API. It never carries the node
// secret (unlike the admin SPA detail view) and omits SPA-only display fields.
type v1Node struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	NodeType       string  `json:"node_type"`
	Online         bool    `json:"online"`
	Disabled       bool    `json:"disabled"`
	AgentVersion   string  `json:"agent_version,omitempty"`
	Roles          int64   `json:"roles"`
	RateMultiplier float64 `json:"rate_multiplier"`
	Unidirectional bool    `json:"unidirectional"`
	NoDirectExit   bool    `json:"no_direct_exit"`
	PortRange      string  `json:"port_range,omitempty"`
	LastSeen       *int64  `json:"last_seen,omitempty"`
}

func toV1Node(n *db.Node) v1Node {
	return v1Node{
		ID: n.ID, Name: n.Name, NodeType: n.NodeType,
		Online: n.Online == 1, Disabled: n.Disabled, AgentVersion: n.AgentVersion,
		Roles: n.Roles, RateMultiplier: n.RateMultiplier, Unidirectional: n.Unidirectional,
		NoDirectExit: n.NoDirectExit, PortRange: n.PortRange, LastSeen: n.LastSeen,
	}
}

func toV1Nodes(nodes []*db.Node) []v1Node {
	out := make([]v1Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toV1Node(n))
	}
	return out
}

// v1User is the slim user shape for the admin read API. pw_hash never leaves the
// server (json:"-" on the field), and only billing/quota-relevant fields ship.
type v1User struct {
	ID                int64   `json:"id"`
	Username          string  `json:"username"`
	Role              string  `json:"role"`
	Disabled          bool    `json:"disabled"`
	MaxForwards       int     `json:"max_forwards"`
	RuleCount         int     `json:"rule_count"`
	TrafficUsedBytes  int64   `json:"traffic_used_bytes"`
	TrafficQuotaBytes int64   `json:"traffic_quota_bytes"`
	TrafficResetDays  int     `json:"traffic_reset_days"`
	ExpiresAt         *int64  `json:"expires_at"`
	BillingRate       float64 `json:"billing_rate"`
}

func toV1User(u *db.User) v1User {
	var exp *int64
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 != 0 {
		e := u.ExpiresAt.Int64
		exp = &e
	}
	return v1User{
		ID: u.ID, Username: u.Username, Role: u.Role, Disabled: u.Disabled,
		MaxForwards: u.MaxForwards, RuleCount: u.RuleCount,
		TrafficUsedBytes: u.TrafficUsedBytes, TrafficQuotaBytes: u.TrafficQuotaBytes,
		TrafficResetDays: u.TrafficResetDays, ExpiresAt: exp, BillingRate: u.BillingRate,
	}
}

// v1RuleChainHop is one physical hop of a rule's flattened chain.
type v1RuleChainHop struct {
	NodeID   int64  `json:"node_id"`
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
}

// v1Rule is the slim rule shape: the persisted fields plus the resolved entry
// endpoint(s) and the flattened physical chain, without the SPA's landing-URI /
// billing-display extras.
type v1Rule struct {
	ID               int64            `json:"id"`
	Name             string           `json:"name"`
	NodeID           int64            `json:"node_id"`
	Proto            string           `json:"proto"`
	Entry            string           `json:"entry"`
	EntryV6          string           `json:"entry_v6,omitempty"`
	EntryFamily      string           `json:"entry_family"`
	Exit             string           `json:"exit"`
	ExitMode         string           `json:"exit_mode"`
	Comment          string           `json:"comment,omitempty"`
	ViaNodeIDs       []int64          `json:"via_node_ids"`
	Chain            []v1RuleChainHop `json:"chain,omitempty"`
	TrafficUsedBytes int64            `json:"traffic_used_bytes"`
	CreatedAt        int64            `json:"created_at"`
}

func toV1Rule(it ruleListItem) v1Rule {
	chain := make([]v1RuleChainHop, 0, len(it.Chain))
	for _, c := range it.Chain {
		chain = append(chain, v1RuleChainHop{NodeID: c.NodeID, Name: c.Name, NodeType: c.NodeType})
	}
	via := it.ViaNodeIDs
	if via == nil {
		via = []int64{}
	}
	return v1Rule{
		ID: it.ID, Name: it.Name, NodeID: it.NodeID, Proto: it.Proto,
		Entry: it.Entry, EntryV6: it.EntryV6, EntryFamily: it.EntryFamily,
		Exit: it.Exit, ExitMode: it.ExitMode, Comment: it.Comment,
		ViaNodeIDs: via, Chain: chain, TrafficUsedBytes: it.TotalBytes, CreatedAt: it.CreatedAt,
	}
}

// v1RulesFromDB resolves a batch of rules into their slim DTOs, filling traffic
// and the flattened chain (resolved over all nodes so composite members show).
func (s *Server) v1RulesFromDB(rules []*db.Rule) []v1Rule {
	db.FillRuleTraffic(s.DB, rules)
	allNodes, _ := db.ListNodes(s.DB)
	allByID := buildMap(allNodes, func(n *db.Node) int64 { return n.ID })
	items := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		items = append(items, s.buildRuleListItem(rl, ""))
	}
	s.fillRuleChains(items, allByID)
	out := make([]v1Rule, 0, len(items))
	for _, it := range items {
		out = append(out, toV1Rule(it))
	}
	return out
}

// --- Handlers: self + shared ---

func (s *Server) v1Info(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	ruleCount, _ := db.CountRulesForUser(s.DB, u.ID)

	nodeViews := make([]map[string]any, 0, len(grantedNodes))
	for i, n := range grantedNodes {
		g := grants[i]
		nRules, _ := db.CountRulesForUserNode(s.DB, u.ID, n.ID)
		nodeViews = append(nodeViews, map[string]any{
			"id":              n.ID,
			"name":            n.Name,
			"rule_count":      nRules,
			"rate_multiplier": n.RateMultiplier,
			"unidirectional":  n.Unidirectional,
			"traffic_used":    g.TrafficUsedBytes,
			"traffic_quota":   g.TrafficQuotaBytes,
		})
	}

	var expiresAt *int64
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 != 0 {
		e := u.ExpiresAt.Int64
		expiresAt = &e
	}
	t := tokenFromCtx(r.Context())
	scope := ""
	if t != nil {
		scope = t.Scope
	}
	v1OK(w, map[string]any{
		"username":              u.Username,
		"role":                  u.Role,
		"scope":                 scope,
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

func (s *Server) v1Probe(w http.ResponseWriter, r *http.Request) {
	res, status := s.runProbe(r.Context(), r.URL.Query().Get("target"), r.URL.Query().Get("node"), r.URL.Query().Get("proto"))
	if status != http.StatusOK {
		v1Err(w, status, probeStatusCode(status), res.Error)
		return
	}
	v1OK(w, res)
}

func (s *Server) v1ProbeChain(w http.ResponseWriter, r *http.Request) {
	res, status := s.runProbeChain(r.Context(), chainRuleIDParam(r))
	if status != http.StatusOK {
		v1Err(w, status, probeStatusCode(status), res.Error)
		return
	}
	v1OK(w, res)
}

// probeStatusCode maps a probe's transport-level HTTP status onto a stable code.
// A probe that completed (200) but found the target unreachable is not an error
// here — it flows through v1OK with the result's own ok=false.
func probeStatusCode(status int) string {
	switch status {
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusBadRequest:
		return codeValidation
	default:
		return codeInternal
	}
}

// --- Handlers: user self-service ---

func (s *Server) v1MyListNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	applyEffectiveRoles(grantedNodes, grants)
	// A composite's online state derives from its children, which may be outside
	// the granted set; resolve over all nodes then project onto the granted ones.
	allNodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, allNodes)
	onlineByID := make(map[int64]int, len(allNodes))
	for _, n := range allNodes {
		onlineByID[n.ID] = n.Online
	}
	for _, n := range grantedNodes {
		if o, ok := onlineByID[n.ID]; ok {
			n.Online = o
		}
	}
	v1OK(w, toV1Nodes(grantedNodes))
}

func (s *Server) v1MyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	v1OK(w, s.v1RulesFromDB(rules))
}

func (s *Server) v1MyGetRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil || !rl.OwnerID.Valid || rl.OwnerID.Int64 != u.ID {
		v1Err(w, http.StatusNotFound, codeNotFound, "规则不存在")
		return
	}
	views := s.v1RulesFromDB([]*db.Rule{rl})
	v1OK(w, views[0])
}

func (s *Server) v1MyCreateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		NodeID       int64     `json:"node_id"`
		NodeName     string    `json:"node_name"`
		Name         string    `json:"name"`
		Proto        string    `json:"proto"`
		Exit         string    `json:"exit"`
		EntryPort    int       `json:"entry_port"`
		Comment      string    `json:"comment"`
		Mode         string    `json:"mode"`
		ExitMode     string    `json:"exit_mode"`
		ViaNodeIDs   *[]int64  `json:"via_node_ids"`
		ViaNodeNames *[]string `json:"via_node_names"`
		EntryFamily  string    `json:"entry_family"`
		DryRun       bool      `json:"dry_run"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	dryRun := body.DryRun || r.URL.Query().Get("dry_run") == "1"

	nodeID, vias, merr := s.resolveRuleNodeRefs(body.NodeID, body.NodeName, body.ViaNodeIDs, body.ViaNodeNames)
	if merr != nil {
		writeRuleMutationV1(w, merr)
		return
	}

	// Idempotency: a retried create with the same key replays the original rule
	// instead of allocating a second entry port and dispatching again. Only real
	// (non-dry-run) creates are recorded/replayed.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !dryRun && idemKey != "" {
		if rid, ok, _ := db.LookupIdempotentRule(s.DB, u.ID, idemKey); ok {
			if rl, err := db.GetRule(s.DB, rid); err == nil && rl.OwnerID.Valid && rl.OwnerID.Int64 == u.ID {
				views := s.v1RulesFromDB([]*db.Rule{rl})
				v1OK(w, map[string]any{"rule": views[0], "idempotent_replay": true})
				return
			}
		}
	}

	out, merr := s.createUserRule(u, userRuleParams{
		NodeID: nodeID, Name: body.Name, Proto: body.Proto, Exit: body.Exit,
		EntryPort: body.EntryPort, Comment: body.Comment, Mode: body.Mode,
		ExitMode: body.ExitMode, ViaNodeIDs: vias, EntryFamily: body.EntryFamily,
	}, dryRun)
	if merr != nil {
		writeRuleMutationV1(w, merr)
		return
	}
	if dryRun {
		v1OK(w, map[string]any{
			"dry_run":  true,
			"entry":    out.Entry,
			"entry_v6": out.EntryV6,
			"chain":    s.hopsChainPreview(out.Hops),
		})
		return
	}
	if idemKey != "" {
		_ = db.SaveIdempotentRule(s.DB, u.ID, idemKey, out.RuleID)
	}
	rl, err := db.GetRule(s.DB, out.RuleID)
	if err != nil {
		v1Err(w, http.StatusInternalServerError, codeInternal, "读取新建规则失败")
		return
	}
	views := s.v1RulesFromDB([]*db.Rule{rl})
	v1OK(w, map[string]any{"rule": views[0]})
}

func (s *Server) v1MyUpdateRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	var body struct {
		NodeID       int64     `json:"node_id"`
		NodeName     string    `json:"node_name"`
		Name         string    `json:"name"`
		Proto        string    `json:"proto"`
		Exit         string    `json:"exit"`
		EntryPort    int       `json:"entry_port"`
		Comment      string    `json:"comment"`
		Mode         string    `json:"mode"`
		ExitMode     string    `json:"exit_mode"`
		ViaNodeIDs   *[]int64  `json:"via_node_ids"`
		ViaNodeNames *[]string `json:"via_node_names"`
		EntryFamily  string    `json:"entry_family"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	// node_name resolves to node_id only when a node was actually named; leaving
	// both empty keeps the stored entry node (NodeID 0 in updateUserRule).
	nodeID, vias, merr := s.resolveRuleNodeRefs(body.NodeID, body.NodeName, body.ViaNodeIDs, body.ViaNodeNames)
	if merr != nil {
		writeRuleMutationV1(w, merr)
		return
	}
	entry, entryV6, merr := s.updateUserRule(u, id, updateUserRuleParams{
		NodeID: nodeID, Name: body.Name, Proto: body.Proto, Exit: body.Exit,
		EntryPort: body.EntryPort, Comment: body.Comment, Mode: body.Mode,
		ExitMode: body.ExitMode, ViaNodeIDs: vias, EntryFamily: body.EntryFamily,
	})
	if merr != nil {
		writeRuleMutationV1(w, merr)
		return
	}
	rl, err := db.GetRule(s.DB, id)
	if err != nil {
		v1OK(w, map[string]any{"entry": entry, "entry_v6": entryV6})
		return
	}
	views := s.v1RulesFromDB([]*db.Rule{rl})
	v1OK(w, map[string]any{"rule": views[0]})
}

func (s *Server) v1MyDeleteRule(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "bad id")
		return
	}
	if merr := s.deleteUserRule(u, id); merr != nil {
		writeRuleMutationV1(w, merr)
		return
	}
	v1OK(w, map[string]any{"deleted": true})
}

// --- Handlers: admin reads ---

func (s *Server) v1ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, nodes)
	v1OK(w, toV1Nodes(nodes))
}

func (s *Server) v1ListUsers(w http.ResponseWriter, r *http.Request) {
	users, _ := db.ListUsers(s.DB)
	db.FillUserRuleCounts(s.DB, users)
	out := make([]v1User, 0, len(users))
	for _, u := range users {
		out = append(out, toV1User(u))
	}
	v1OK(w, out)
}

func (s *Server) v1Dashboard(w http.ResponseWriter, r *http.Request) {
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeOnline(s.DB, nodes)
	online := 0
	for _, n := range nodes {
		if n.Online == 1 {
			online++
		}
	}
	ruleCount, _ := db.CountAllRules(s.DB)
	totalBytes, _ := db.TotalRuleTrafficBytes(s.DB)
	userCount, _ := db.CountUsers(s.DB)
	v1OK(w, map[string]any{
		"nodes_total":         len(nodes),
		"nodes_online":        online,
		"rule_count":          ruleCount,
		"user_count":          userCount,
		"total_traffic_bytes": totalBytes,
		"nodes":               toV1Nodes(nodes),
	})
}

// --- shared v1 rule helpers ---

// resolveRuleNodeRefs turns optional name references into ids for the id-based
// rule cores. node_name resolves the entry node only when node_id is unset;
// via_node_names, when present, resolves the whole ordered path (an explicit
// empty list clears the path). A named node that doesn't exist is a validation
// error rather than a silent skip.
func (s *Server) resolveRuleNodeRefs(nodeID int64, nodeName string, viaIDs *[]int64, viaNames *[]string) (int64, *[]int64, *ruleMutationError) {
	if nodeID == 0 && strings.TrimSpace(nodeName) != "" {
		id, merr := s.resolveNodeName(nodeName)
		if merr != nil {
			return 0, nil, merr
		}
		nodeID = id
	}
	vias := viaIDs
	// Explicit ids win; names are resolved only when ids weren't supplied.
	if viaIDs == nil && viaNames != nil {
		resolved := make([]int64, 0, len(*viaNames))
		for _, name := range *viaNames {
			id, merr := s.resolveNodeName(name)
			if merr != nil {
				return 0, nil, merr
			}
			resolved = append(resolved, id)
		}
		vias = &resolved
	}
	return nodeID, vias, nil
}

func (s *Server) resolveNodeName(name string) (int64, *ruleMutationError) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, &ruleMutationError{status: http.StatusBadRequest, msg: "节点名不能为空"}
	}
	m, err := db.NodeIDsByNames(s.DB, []string{name})
	if err != nil {
		return 0, &ruleMutationError{status: http.StatusInternalServerError, msg: err.Error()}
	}
	id, ok := m[name]
	if !ok {
		// A name that doesn't resolve is reported exactly like a node that exists
		// but wasn't granted (the GetNodeGrant path below returns the same 403):
		// NodeIDsByNames is unscoped, so a distinct "doesn't exist" answer would
		// let a token enumerate node names it was never authorized to see. A user
		// can only legitimately name nodes listed by /api/v1/my/nodes.
		return 0, &ruleMutationError{status: http.StatusForbidden, msg: "无权使用该节点"}
	}
	return id, nil
}

// hopsChainPreview resolves the physical hops of a would-be rule (dry run) to
// their node names for the preview chain.
func (s *Server) hopsChainPreview(hops []db.HopInput) []v1RuleChainHop {
	out := make([]v1RuleChainHop, 0, len(hops))
	for _, h := range hops {
		hop := v1RuleChainHop{NodeID: h.NodeID}
		if n, err := db.GetNode(s.DB, h.NodeID); err == nil {
			hop.Name = n.Name
			hop.NodeType = n.NodeType
		}
		out = append(out, hop)
	}
	return out
}

// writeRuleMutationV1 renders a rule create/edit failure in the v1 error
// envelope, mapping the shared error's status onto a stable code (and the
// duplicate-hop RegenerateRule failure onto 409 + duplicate_chain_node).
func writeRuleMutationV1(w http.ResponseWriter, e *ruleMutationError) {
	if e.regenerate {
		if errors.Is(e.cause, db.ErrDuplicateChainNode) {
			v1Err(w, http.StatusConflict, codeDuplicateHop, e.msg)
			return
		}
		v1Err(w, http.StatusBadRequest, codeValidation, e.msg)
		return
	}
	code := codeValidation
	switch e.status {
	case http.StatusForbidden:
		code = codeForbidden
	case http.StatusConflict:
		code = codeConflict
	case http.StatusNotFound:
		code = codeNotFound
	case http.StatusInternalServerError:
		code = codeInternal
	}
	v1Err(w, e.status, code, e.msg)
}
