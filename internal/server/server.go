package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/landing"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

type Server struct {
	DB              *sql.DB
	Hub             *Hub
	Dispatcher      *Dispatcher
	Landing         *landing.Fetcher
	loginLimiter    *loginLimiter
	tokenLimiter    *tokenLimiter
	stopExpiry      chan struct{}
	stopCycle       chan struct{}
	stopLandingSync chan struct{}
}

func New(d *sql.DB) (*Server, error) {
	if _, err := EnsureSelfNode(d); err != nil {
		return nil, fmt.Errorf("ensure self node: %w", err)
	}
	hub := NewHub(d)
	disp := &Dispatcher{DB: d, Hub: hub}
	s := &Server{DB: d, Hub: hub, Dispatcher: disp, Landing: landing.NewFetcher(), loginLimiter: newLoginLimiter(), tokenLimiter: newTokenLimiter(), stopExpiry: make(chan struct{}), stopCycle: make(chan struct{}), stopLandingSync: make(chan struct{})}
	hub.OnTrafficUpdate = func(userID int64, nodeID int64) {
		s.enforcePerNodeQuota(userID, nodeID)
		s.enforceUserQuota(userID)
		s.enforceExitQuota(userID)
	}
	hub.Redispatch = s.redispatchNodes
	hub.OnRelayHostChange = s.rewireRulesReferencingNode
	go s.expiryEnforcer()
	go s.cycleResetEnforcer()
	go s.landingSyncEnforcer()
	return s, nil
}

// expiryEnforcer periodically scans for users whose expires_at has passed
// and re-dispatches the affected nodes so their forwarding rules are
// removed from the kernel.
func (s *Server) expiryEnforcer() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopExpiry:
			return
		case <-ticker.C:
			nodes, err := db.ExpiredUserNodeIDs(s.DB)
			if err != nil {
				log.Printf("expiry: query expired-user nodes: %v", err)
				continue
			}
			for _, n := range nodes {
				if err := s.dispatchToNode(n); err != nil {
					log.Printf("expiry: re-dispatch node %d: %v", n, err)
				}
			}
		}
	}
}

// cycleResetEnforcer 定期检查每个用户的流量重置周期，周期翻转时把用量账本归零并
// 重新下发该用户的节点，让因配额用尽被排除的规则在新周期恢复转发。
//
// 计数入账路径（Hub.applyCounters）里也有同样的周期检查，但它只在该用户有流量
// 上报时才走到；规则已全部撤出节点的用户不再产生计数，要靠这里的定时检查。
//
// 重置之后无条件重推：配额条件只在下发时求值（db.ActiveRuleHopsForPush），归零本身
// 不会让节点上的规则集变化。
func (s *Server) cycleResetEnforcer() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCycle:
			return
		case <-ticker.C:
			users, err := db.ListUsers(s.DB)
			if err != nil {
				log.Printf("cycle: list users: %v", err)
				continue
			}
			for _, u := range users {
				if u.TrafficResetDays <= 0 {
					continue
				}
				reset, err := db.CheckAndResetTrafficCycle(s.DB, u)
				if err != nil {
					log.Printf("cycle: check reset for user %d: %v", u.ID, err)
					continue
				}
				if !reset {
					continue
				}
				s.redispatchUser(u.ID, "流量周期重置")
			}
		}
	}
}

// landingSyncEnforcer keeps materialized landing-exit sets in step with
// subscription content when no page load resolves them. The first pass runs
// immediately and includes manual-URI users, backfilling existing deployments
// right after upgrade; the table then persists, so later restarts have no
// empty-set window.
func (s *Server) landingSyncEnforcer() {
	s.landingSyncPass(true)
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopLandingSync:
			return
		case <-ticker.C:
			s.landingSyncPass(false)
		}
	}
}

// landingSyncPass syncs every user with a landing source. includeManualOnly
// widens the pass to users without a subscription — their set only changes on
// save, so the periodic pass skips them.
func (s *Server) landingSyncPass(includeManualOnly bool) {
	users, err := db.ListUsers(s.DB)
	if err != nil {
		log.Printf("landing: sync pass list users: %v", err)
		return
	}
	for _, u := range users {
		if !hasLandingSource(u) {
			continue
		}
		if !includeManualOnly && !hasDynamicSource(u) {
			continue
		}
		nodes, ok := s.resolveLandingExits(u, false)
		if !ok {
			continue
		}
		s.syncLandingExits(u, nodes)
	}
}

// redispatchUser 重新下发承载 userID 规则跳的每一个节点。
//
// 一条规则下不下发由 db.ActiveRuleHopsForPush 在下发时按库里的当前值判定。凡是改了
// 这个用户的账号级状态（停用标志、到期时间、全局配额、全局已用量）的入口，改完
// 都调这里，让节点上的规则集立刻跟上；why 只用于日志。
func (s *Server) redispatchUser(userID int64, why string) {
	nodes, err := db.DistinctUserNodes(s.DB, userID)
	if err != nil {
		log.Printf("redispatch user %d (%s): list nodes: %v", userID, why, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("redispatch user %d (%s): node %d: %v", userID, why, n, err)
		}
	}
}

// redispatchUserGrant 重新下发 userID 经由授权节点 nodeID 的规则所在的节点。改动
// 这条授权的配额或已用量之后调用，理由同 redispatchUser。
func (s *Server) redispatchUserGrant(userID, nodeID int64, why string) {
	nodes, err := db.RulesAffectedByNode(s.DB, userID, nodeID)
	if err != nil {
		log.Printf("redispatch user %d grant %d (%s): list nodes: %v", userID, nodeID, why, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("redispatch user %d grant %d (%s): node %d: %v", userID, nodeID, why, n, err)
		}
	}
}

// enforceUserQuota 在一批计数入账后调用：用户的全局流量配额用尽时重新下发他的
// 节点，由 db.ActiveRuleHopsForPush 把他的规则排除出去。账号本身不停用，用户仍能
// 登录查看用量；配额调高或用量归零后，各自的入口重推即可恢复，这里不留任何需要
// 事后撤销的状态。
//
// 这里没有“已经处理过”的记号：只要用户处于用尽状态，每入账一批带他流量的计数就
// 重推一次他的节点（授权配额的 enforcePerNodeQuota 也是这样）。
func (s *Server) enforceUserQuota(userID int64) {
	u, err := db.GetUserByID(s.DB, userID)
	if err != nil {
		log.Printf("quota: load user %d: %v", userID, err)
		return
	}
	if !u.OverQuota {
		return
	}
	log.Printf("user %d reached traffic quota (%d/%d bytes): withdrawing rules", userID, u.TrafficUsedBytes, u.TrafficQuotaBytes)
	s.redispatchUser(userID, "全局流量配额用尽")
}

// enforcePerNodeQuota re-dispatches nodes affected by any per-node quota
// overrun for userID. Only nodes whose rules include a hop where quota > 0
// and used >= quota are targeted, so unrelated nodes are never churned.
func (s *Server) enforcePerNodeQuota(userID int64, nodeID int64) {
	exceeded, err := db.NodesExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: per-node check user %d: %v", userID, err)
		return
	}
	for _, excNode := range exceeded {
		affectedNodes, err := db.RulesAffectedByNode(s.DB, userID, excNode)
		if err != nil {
			log.Printf("quota: affected nodes for user %d node %d: %v", userID, excNode, err)
			continue
		}
		for _, n := range affectedNodes {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("quota: re-dispatch node %d after per-node quota user %d: %v", n, userID, err)
			}
		}
	}
}

// enforceExitQuota re-pushes the nodes carrying rules whose landing-exit
// ledger reached quota, so ActiveRuleHopsForPush drops exactly the rules
// pointed at the exhausted exit.
func (s *Server) enforceExitQuota(userID int64) {
	exceeded, err := db.ExitsExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: exit check user %d: %v", userID, err)
		return
	}
	for _, k := range exceeded {
		s.redispatchUserExit(userID, k.Host, k.Port)
	}
}

// dispatchToNode builds the panel-segment ruleset for nodeID from the
// rule_hops DB and dispatches it via the Hub (or unix socket for the
// self-node).
//
// The outcome is reflected on the nodes row so the panel UI can show
// sync status: success stamps last_apply_at and clears last_error;
// failure stamps last_error while preserving last_apply_at.
func (s *Server) dispatchToNode(nodeID int64) error {
	ruleHops, err := db.ActiveRuleHopsForPush(s.DB, nodeID)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	rules := buildRules(s.DB, ruleHops)
	rev := computeRev(rules)
	warning, err := s.Dispatcher.Dispatch(nodeID, rules, rev)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	_ = db.MarkNodeApplied(s.DB, nodeID, warning)
	return nil
}

// dispatchAfterMutation wraps the common "CRUD-handler dispatches to a
// node and wants to surface failure to the admin doing the mutation"
// pattern.
func (s *Server) dispatchAfterMutation(w http.ResponseWriter, nodeID int64, action string) {
	if err := s.dispatchToNode(nodeID); err != nil {
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到节点失败：%v", action, err))
		log.Printf("dispatch node %d (%s): %v", nodeID, action, err)
		return
	}
	if n, err := db.GetNode(s.DB, nodeID); err == nil && n.LastWarning != "" {
		setFlash(w, fmt.Sprintf("%s 已保存，但 %s", action, n.LastWarning))
	}
}

// dispatchAfterFanout dispatches to every node touched by a user-scope
// mutation. Per-node errors are aggregated into a single flash because
// the flash cookie holds only one message.
func (s *Server) dispatchAfterFanout(w http.ResponseWriter, nodeIDs []int64, action string) {
	type result struct {
		nodeID int64
		err    error
	}
	results := make(chan result, len(nodeIDs))
	var wg sync.WaitGroup
	for _, n := range nodeIDs {
		wg.Add(1)
		go func(nodeID int64) {
			defer wg.Done()
			results <- result{nodeID: nodeID, err: s.dispatchToNode(nodeID)}
		}(n)
	}
	wg.Wait()
	close(results)

	var failed []string
	for r := range results {
		if r.err != nil {
			log.Printf("dispatch node %d (%s): %v", r.nodeID, action, r.err)
			failed = append(failed, fmt.Sprintf("节点 %d: %v", r.nodeID, r.err))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到 %d 个节点失败（%s）",
			action, len(failed), strings.Join(failed, "；")))
	}
}

// redispatchNodes re-pushes kernel state to every node a background (WS-driven)
// rule mutation touched. Dispatches run off the caller's goroutine.
func (s *Server) redispatchNodes(nodeIDs []int64) {
	for _, n := range nodeIDs {
		go func(n int64) {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("dispatch node %d (规则变更): %v", n, err)
			}
		}(n)
	}
}

// buildRules converts panel-side RuleHop rows into kernel-side nft.Rule
// values. Lookup tables are preloaded in bulk so the conversion is
// O(ruleHops) with no per-row queries.
func buildRules(d *sql.DB, ruleHops []*db.RuleHop) []nft.Rule {
	ruleMap, _ := db.RulesByID(d)
	if ruleMap == nil {
		ruleMap = map[int64]*db.Rule{}
	}
	users, _ := db.UsersByID(d)
	if users == nil {
		users = map[int64]*db.User{}
	}

	// Collect distinct rule IDs for a single bulk hop-count query
	ruleIDSet := map[int64]bool{}
	for _, rh := range ruleHops {
		ruleIDSet[rh.RuleID] = true
	}
	ruleIDs := make([]int64, 0, len(ruleIDSet))
	for id := range ruleIDSet {
		ruleIDs = append(ruleIDs, id)
	}
	hopCounts, _ := db.RuleHopCounts(d, ruleIDs)
	if hopCounts == nil {
		hopCounts = map[int64]int{}
	}
	shapes, _ := db.GrantShapes(d)

	rules := make([]nft.Rule, 0, len(ruleHops))
	for _, rh := range ruleHops {
		rule := nft.Rule{
			Proto:    rh.Proto,
			SrcPort:  rh.ListenPort,
			DestPort: rh.TargetPort,
			Comment:  rh.Comment,
			Mode:     rh.Mode,
			HopCount: hopCounts[rh.RuleID],
		}
		if r := ruleMap[rh.RuleID]; r != nil {
			rule.RuleID = r.ID
			rule.RuleName = r.Name
			if r.OwnerID.Valid {
				if u := users[r.OwnerID.Int64]; u != nil {
					rule.OwnerName = u.Username
				}
				// Shaping follows the hop's logical segment's grant: the entry
				// segment reads the entry grant, a middle-layer segment reads the
				// layer grant — the same logical node its quota is tracked on.
				if gs, ok := shapes[[2]int64{r.OwnerID.Int64, rh.ViaNodeID}]; ok {
					rule.ShapeGroup = gs.GrantID
					rule.RateMBytes = int(gs.RateLimitMBytes)
					// Legacy mirror so pre-group agents still shape
					// (per rule, approximate): MB/s (2^20 bytes) → Mbit/s.
					rule.BandwidthMbps = int((gs.RateLimitMBytes*8388608 + 500000) / 1000000)
				}
			}
		}
		if resolver.IsHostname(rh.TargetHost) {
			rule.DestHost = rh.TargetHost
		} else {
			rule.DestIP = rh.TargetHost
		}
		rules = append(rules, rule)
	}
	return rules
}

// computeRev returns a stable hash of the ruleset so a reconnecting
// agent whose last_applied_rev matches can be skipped. Determinism
// hinges on ActiveRuleHopsForPush returning rows in a stable order
// (it sorts by listen_port).
//
// Rule metadata is panel-side display info, not part of the data plane;
// exclude it so a rule rename does not force a redundant re-apply on
// reconnecting nodes.
func computeRev(rules []nft.Rule) string {
	bare := make([]nft.Rule, len(rules))
	for i, r := range rules {
		r.RuleID = 0
		r.RuleName = ""
		r.OwnerName = ""
		bare[i] = r
	}
	h := sha256.New()
	b, _ := json.Marshal(bare)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(logRequests)
	r.Get("/healthz", s.healthz)
	r.HandleFunc("/v1/agents", s.Hub.ServeWS)
	r.Get("/v1/binary", s.serveBinary)

	// --- JSON API ---
	r.Route("/api", func(r chi.Router) {
		r.Post("/login", s.apiLogin)
		r.Post("/logout", s.apiLogout)
		r.Get("/branding", s.apiBranding)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth)
			r.Get("/me", s.apiMe)
			r.Post("/change-password", s.apiChangePassword)
			r.Get("/probe", s.probeEndpoint)
			r.Get("/probe-chain", s.probeChainEndpoint)
			r.Get("/dashboard", s.apiDashboard)
			r.Post("/sub-fetch", s.apiSubFetch)
			r.Get("/node-roles", s.apiGetNodeRoles)
		})

		// Admin routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("admin"))

			r.Get("/nodes", s.apiListNodes)
			r.Post("/nodes", s.apiCreateNode)
			r.Get("/nodes/{id}", s.apiGetNode)
			r.Post("/nodes/{id}/rename", s.apiRenameNode)
			r.Post("/nodes/{id}/relay-host", s.apiSetNodeRelayHost)
			r.Post("/nodes/{id}/relay-host-v6", s.apiSetNodeRelayHostV6)
			r.Post("/nodes/{id}/port-range", s.apiUpdateNodePortRange)
			r.Post("/nodes/{id}/resync", s.apiResyncNode)
			r.Post("/nodes/{id}/upgrade", s.apiUpgradeNode)
			r.Delete("/nodes/{id}", s.apiDeleteNode)
			r.Post("/nodes/{id}/toggle", s.apiToggleNode)
			r.Post("/nodes/{id}/owner", s.apiUpdateNodeOwner)
			r.Post("/nodes/reorder", s.apiReorderNodes)
			r.Post("/nodes/resync-all", s.apiResyncAllNodes)
			r.Post("/nodes/upgrade-all", s.apiUpgradeAllNodes)
			r.Post("/nodes/{id}/rate-multiplier", s.apiSetNodeRateMultiplier)
			r.Post("/nodes/{id}/unidirectional", s.apiSetNodeUnidirectional)
			r.Post("/nodes/{id}/no-direct-exit", s.apiSetNodeNoDirectExit)
			r.Get("/nodes/{id}/hops", s.apiListNodeHops)
			r.Post("/nodes/{id}/hops", s.apiUpdateNodeHops)
			r.Post("/nodes/{id}/roles", s.apiUpdateNodeRolesMask)
			r.Get("/nodes/{id}/bindings", s.apiListNodeBindings)
			r.Post("/nodes/{id}/bindings", s.apiUpdateNodeBindings)
			r.Get("/nodes/{id}/downstream-bindings", s.apiListNodeDownstreamBindings)
			r.Post("/nodes/{id}/downstream-bindings", s.apiUpdateNodeDownstreamBindings)
			r.Get("/node-bindings", s.apiListAllNodeBindings)
			r.Post("/nodes/{id}/grants/batch", s.apiBatchUpdateNodeGrants)

			r.Get("/settings", s.apiGetSettings)
			r.Post("/settings", s.apiSaveSettings)
			r.Post("/node-roles", s.apiSetNodeRoles)

			r.Get("/rules", s.apiListRules)
			r.Post("/rules", s.apiCreateRule)
			r.Get("/rules/{id}", s.apiGetRule)
			r.Put("/rules/{id}", s.apiUpdateRule)
			r.Delete("/rules/{id}", s.apiDeleteRule)
			r.Post("/rules/{id}/hops/{pos}/reallocate", s.apiReallocateRuleHop)

			r.Get("/users/{id}", s.apiGetUser)
			r.Post("/users", s.apiCreateUser)
			r.Post("/users/{id}/grants", s.apiGrantNode)
			r.Delete("/users/{id}/grants/{nodeID}", s.apiRevokeNode)
			r.Post("/users/{id}/grants/batch-revoke", s.apiBatchRevokeNodes)
			r.Post("/users/{id}/quota", s.apiSetUserQuota)
			r.Post("/users/{id}/max-forwards", s.apiSetMaxForwards)
			r.Post("/users/{id}/landing", s.apiSetUserLanding)
			r.Post("/users/{id}/expiry", s.apiSetUserExpiry)
			r.Post("/users/{id}/reset-traffic", s.apiResetUserTraffic)
			r.Post("/users/{id}/reset-days", s.apiSetResetDays)
			r.Post("/users/{id}/nodes/{nodeID}/max-forwards", s.apiSetPerNodeMaxForwards)
			r.Post("/users/{id}/nodes/{nodeID}/quota", s.apiSetPerNodeQuota)
			r.Post("/users/{id}/nodes/{nodeID}/rate-limit", s.apiSetPerNodeRateLimit)
			r.Post("/users/{id}/nodes/{nodeID}/roles", s.apiSetPerNodeRoles)
			r.Post("/users/{id}/nodes/{nodeID}/reset-traffic", s.apiResetPerNodeTraffic)
			r.Get("/users/{id}/landing-exits", s.apiListUserLandingExits)
			r.Post("/users/{id}/landing-exits/quota", s.apiSetLandingExitQuota)
			r.Post("/users/{id}/landing-exits/reset", s.apiResetLandingExitTraffic)
			r.Post("/users/{id}/landing-exits/delete", s.apiDeleteLandingExit)
			r.Post("/users/{id}/landing-exits/rename", s.apiRenameLandingExit)

			r.Get("/users", s.apiListUsers)
			r.Patch("/users/{id}/profile", s.apiUpdateUserProfile)
			r.Post("/users/{id}/admin-note", s.apiSetAdminNote)
			r.Post("/users/{id}/toggle", s.apiToggleUser)
			r.Post("/users/{id}/reset-password", s.apiResetUserPassword)
			r.Delete("/users/{id}", s.apiDeleteUser)
			r.Post("/grants/batch-apply", s.apiBatchApplyGrants)
		})

		// User routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("user"))
			r.Get("/my", s.apiMyDashboard)
			r.Post("/my/username", s.apiChangeUsername)
			r.Get("/my/landing-nodes", s.apiMyLandingNodes)
			r.Get("/my/rules", s.apiMyListRules)
			r.Get("/my/rules/{id}", s.apiMyGetRule)
			r.Post("/my/rules", s.apiMyCreateRule)
			r.Put("/my/rules/{id}", s.apiMyUpdateRule)
			r.Delete("/my/rules/{id}", s.apiMyDeleteRule)
		})

		// API token self-management is role-agnostic: an admin needs its own
		// token to reach the /api/v1 admin surface, so this can't sit under the
		// user-only group. Each handler keys on the session user's own id.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth)
			r.Get("/my/token", s.apiMyGetToken)
			r.Post("/my/token", s.apiMyCreateToken)
			r.Delete("/my/token", s.apiMyDeleteToken)
			r.Post("/my/token/refresh", s.apiMyRefreshToken)
			r.Post("/my/token/toggle", s.apiMyToggleToken)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth)
			r.Get("/ws/speed", s.apiSpeedWS)
		})
	})

	// Public API (token auth) — the stable programmatic / AI-agent surface.
	r.Route("/api/v1", s.registerV1Routes)

	r.NotFound(s.spaHandler().ServeHTTP)

	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.PingContext(r.Context()); err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"ok":true}`)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
