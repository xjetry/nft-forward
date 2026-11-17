package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/accounting"
	"nft-forward/internal/db"
	"nft-forward/internal/landing"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/rulechange"
)

type Server struct {
	DB  *sql.DB
	Hub *Hub
	// Rules 是规则写入的唯一入口，与 Hub 用的是同一个：面板上的接口与节点发来的命令
	// 过同一套判定。
	Rules   *rulechange.Changes
	Landing *landing.Fetcher
	// dispatch 让对同一个节点的下发串行、合并；所有下发都经 dispatchToNode 走它。
	dispatch        *nodeDispatchQueue
	loginLimiter    *loginLimiter
	tokenLimiter    *tokenLimiter
	stopExpiry      chan struct{}
	stopCycle       chan struct{}
	stopLandingSync chan struct{}
	// nodeRolesMu serializes apiSetNodeRoles' read-modify-write of the
	// "node_roles" settings row.
	nodeRolesMu sync.Mutex
}

func New(d *sql.DB) (*Server, error) {
	s, err := newServer(d)
	if err != nil {
		return nil, err
	}
	go s.expiryEnforcer()
	go s.cycleResetEnforcer()
	go s.landingSyncEnforcer()
	return s, nil
}

// newServer builds a server that answers requests but runs none of the
// periodic work New starts. Everything it does to the database is then the
// doing of the requests it is given, which is what a test that counts or
// fails individual statements needs.
func newServer(d *sql.DB) (*Server, error) {
	if _, err := EnsureSelfNode(d); err != nil {
		return nil, fmt.Errorf("ensure self node: %w", err)
	}
	// 下面新建的 Hub 连接表是空的，nodes.online 是它的镜像，所以先整体清零
	// （理由见 db.MarkAllNodesOffline）。
	if err := db.MarkAllNodesOffline(d); err != nil {
		return nil, fmt.Errorf("mark nodes offline: %w", err)
	}
	hub := NewHub(d)
	s := &Server{DB: d, Hub: hub, Rules: hub.Rules, Landing: landing.NewFetcher(), loginLimiter: newLoginLimiter(), tokenLimiter: newTokenLimiter(), stopExpiry: make(chan struct{}), stopCycle: make(chan struct{}), stopLandingSync: make(chan struct{})}
	s.dispatch = newNodeDispatchQueue(s.dispatchRound)
	hub.OnTrafficUpdate = func(userID int64) {
		s.enforcePerNodeQuota(userID)
		s.enforceUserQuota(userID)
		s.enforceExitQuota(userID)
	}
	hub.Redispatch = s.redispatchNodes
	hub.OnRelayHostChange = func(nodeID int64) {
		if err := s.rewireRulesReferencingNode(nodeID); err != nil {
			log.Printf("hub: node %d changed its relay host: rewiring the rules that reference it: %v", nodeID, err)
		}
	}
	return s, nil
}

// expiryEnforcer 定时重推承载着已到期用户规则的节点，让这些规则撤出数据面。
//
// 到期是压制条件里唯一不由写入触发的输入：时间走过到期时刻时没有任何入口被调用，
// 只能靠定时检查。它不记“哪个用户已经处理过”，所以只要还有到期且未停用的用户
// 挂着规则，每一轮都会重推他们的节点（内容不变的重推在节点侧不动数据面）。
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
			s.dispatchNodes("账号到期", nodes)
		}
	}
}

// cycleResetEnforcer 定期检查每个用户的流量重置周期，周期翻转时把用量账本归零并
// 重新下发该用户的节点，让因配额用尽被排除的规则在新周期恢复转发。
//
// 计数入账路径（accounting.Record）里也有同样的周期检查，但它只在该用户有流量
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
				reset, err := accounting.ResetCycleIfDue(s.DB, u)
				if err != nil {
					log.Printf("cycle: check reset for user %d: %v", u.ID, err)
					continue
				}
				if !reset {
					continue
				}
				if err := s.redispatchUser(u.ID, "流量周期重置"); err != nil {
					log.Printf("cycle: user %d was reset but its nodes could not be listed for re-dispatch: %v", u.ID, err)
				}
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
		nodes, err := s.resolveLandingExits(u, false)
		if err != nil {
			log.Printf("landing: sync pass resolve user %d: %v", u.ID, err)
			continue
		}
		s.refreshLandingLedger(u, nodes)
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
	if err := s.redispatchUser(userID, "全局流量配额用尽"); err != nil {
		log.Printf("quota: user %d is over quota but its nodes could not be listed for re-dispatch: %v", userID, err)
	}
}

// enforcePerNodeQuota 在一批计数入账后调用：对 userID 每条配额用尽的授权，重新下发
// 经由它的规则所在的节点，由 db.ActiveRuleHopsForPush 把这些规则排除出去。只动
// 用尽的授权涉及的节点，其余节点不受打扰。
func (s *Server) enforcePerNodeQuota(userID int64) {
	exceeded, err := db.NodesExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: per-node check user %d: %v", userID, err)
		return
	}
	for _, grantNode := range exceeded {
		if err := s.redispatchUserGrant(userID, grantNode, "授权流量配额用尽"); err != nil {
			log.Printf("quota: user %d grant %d is used up but its nodes could not be listed for re-dispatch: %v", userID, grantNode, err)
		}
	}
}

// enforceExitQuota 在一批计数入账后调用：对 userID 每个配额用尽的落地出口，重新下发
// 指向它的规则所在的节点。
func (s *Server) enforceExitQuota(userID int64) {
	exceeded, err := db.ExitsExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: exit check user %d: %v", userID, err)
		return
	}
	for _, k := range exceeded {
		if err := s.redispatchUserExit(userID, k.Host, k.Port, "落地出口流量配额用尽"); err != nil {
			log.Printf("quota: user %d exit %s:%d is used up but its nodes could not be listed for re-dispatch: %v", userID, k.Host, k.Port, err)
		}
	}
}

// buildRules converts panel-side RuleHop rows into kernel-side nft.Rule
// values. Lookup tables are preloaded in bulk so the conversion is
// O(ruleHops) with no per-row queries.
//
// 任何一张查找表读不出来，构建就失败。构建结果是节点被要求安装的东西，三张表各供
// 其中一部分：规则表给一跳它的规则 id、规则名与属主，跳数表给 HopCount，授权表给
// 限速分组与速率。缺了哪一张，规则集仍会列出每一跳，只是少了那一部分。
func buildRules(d *sql.DB, ruleHops []*db.RuleHop) ([]nft.Rule, error) {
	ruleMap, err := db.RulesByID(d)
	if err != nil {
		return nil, err
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
	hopCounts, err := db.RuleHopCounts(d, ruleIDs)
	if err != nil {
		return nil, err
	}
	shapes, err := db.GrantShapes(d)
	if err != nil {
		return nil, err
	}

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
	return rules, nil
}

// computeRev 给一份下发的规则载荷算出版本号。rev 标识的是载荷本身：节点把收下的
// rev 与规则在同一次落盘里存下（daemon 的 SetPanelRuleset），重连握手时报回来；
// 面板此刻要发的载荷算出同一个 rev，就说明节点存着的面板段与这份载荷逐字段相同，
// 不再下发（reconcileOnConnect）。
//
// 所以 rev 覆盖载荷的每个字段，包括数据面不读的规则名、备注、跳数。节点存着这些
// 值并且在用：TUI 显示规则名与备注，本机发起的编辑把表单里的备注带回面板，daemon
// 按跳数决定编辑走哪条命令（handleUpdatePanelRule）。它们在节点离线期间变了，重连
// 时 rev 不同、补发一次，节点上的副本才不会停在旧值上。
//
// “要不要下发”与“数据面要不要重放”是两个问题。后者由节点收到载荷后自己按数据面
// 投影判断（nft.SameDataplane）：只变了数据面不读的字段时，它存下新值、不动数据面。
//
// 同一组规则两次算出的 rev 相同，要求 buildRules 两次给出的顺序相同；顺序来自
// db.ActiveRuleHopsForPush，它按（监听端口，协议）排成全序。
func computeRev(rules []nft.Rule) string {
	h := sha256.New()
	b, _ := json.Marshal(rules)
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
			r.Post("/sub-fetch", s.apiSubFetch)
			r.Get("/node-roles", s.apiGetNodeRoles)
		})

		// Admin routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("admin"))

			r.Get("/dashboard", s.apiDashboard)
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
