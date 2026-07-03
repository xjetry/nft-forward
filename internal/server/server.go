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
	s := &Server{DB: d, Hub: hub, Dispatcher: disp, Landing: landing.NewFetcher(), loginLimiter: newLoginLimiter(), stopExpiry: make(chan struct{}), stopCycle: make(chan struct{}), stopLandingSync: make(chan struct{})}
	hub.OnTrafficUpdate = func(userID int64, nodeID int64) {
		s.enforcePerNodeQuota(userID, nodeID)
		s.enforceUserQuota(userID)
		s.enforceExitQuota(userID)
	}
	hub.Redispatch = s.redispatchNodes
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

// cycleResetEnforcer periodically checks every user's traffic reset window.
// When the window rolls over it re-enables any user who was disabled for
// exceeding quota and re-dispatches their nodes, restoring their rules to
// the kernel for the fresh cycle.
//
// This covers users who are globally disabled (all rules already removed from
// the kernel) and therefore receive no traffic — without this goroutine they
// would never reach the cycle-reset check in applyCounters.
//
// The re-push runs unconditionally after a reset, not only for re-enabled
// users: per-grant quota exclusions are evaluated at push time only, so a
// suppressed rule stays dead until something re-pushes even when the user was
// never globally disabled.
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
				if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
					if err := db.SetUserDisabled(s.DB, u.ID, false, ""); err != nil {
						log.Printf("cycle: re-enable user %d: %v", u.ID, err)
						continue
					}
				}
				// Quota exclusions are evaluated at push time only; a fresh
				// cycle must re-push or suppressed rules stay dead.
				if nodes, err := db.DistinctUserNodes(s.DB, u.ID); err == nil {
					for _, n := range nodes {
						if err := s.dispatchToNode(n); err != nil {
							log.Printf("cycle: re-dispatch node %d for user %d: %v", n, u.ID, err)
						}
					}
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
		nodes, ok := s.resolveLandingExits(u, false)
		if !ok {
			continue
		}
		s.syncLandingExits(u, nodes)
	}
}

// enforceUserQuota disables a user that has reached its traffic quota and
// re-pushes every node it had rule hops on so ActiveRuleHopsForPush (which
// excludes disabled users) removes them from the kernel. Quota 0 = unlimited.
func (s *Server) enforceUserQuota(userID int64) {
	u, err := db.GetUserByID(s.DB, userID)
	if err != nil {
		log.Printf("quota: load user %d: %v", userID, err)
		return
	}
	if u.Disabled || u.TrafficQuotaBytes <= 0 || u.TrafficUsedBytes < u.TrafficQuotaBytes {
		return
	}
	if err := db.SetUserDisabled(s.DB, userID, true, "流量超额"); err != nil {
		log.Printf("quota: disable user %d: %v", userID, err)
		return
	}
	log.Printf("user %d disabled: traffic quota reached (%d/%d bytes)", userID, u.TrafficUsedBytes, u.TrafficQuotaBytes)
	nodes, err := db.DistinctUserNodes(s.DB, userID)
	if err != nil {
		log.Printf("quota: user %d nodes: %v", userID, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("quota: re-dispatch node %d after disabling user %d: %v", n, userID, err)
		}
	}
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
			r.Get("/nodes/{id}/hops", s.apiListNodeHops)
			r.Post("/nodes/{id}/hops", s.apiUpdateNodeHops)
			r.Post("/nodes/{id}/roles", s.apiUpdateNodeRolesMask)
			r.Get("/nodes/{id}/bindings", s.apiListNodeBindings)
			r.Post("/nodes/{id}/bindings", s.apiUpdateNodeBindings)
			r.Get("/node-bindings", s.apiListAllNodeBindings)

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

	// Public API (token auth)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.requireTokenAuth)
		r.Get("/info", s.apiTokenInfo)
	})

	r.NotFound(spaHandler().ServeHTTP)

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
