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

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

type Server struct {
	DB         *sql.DB
	Hub        *Hub
	Dispatcher *Dispatcher
}

func New(d *sql.DB) (*Server, error) {
	if _, err := EnsureSelfNode(d); err != nil {
		return nil, fmt.Errorf("ensure self node: %w", err)
	}
	hub := NewHub(d)
	disp := &Dispatcher{DB: d, Hub: hub}
	s := &Server{DB: d, Hub: hub, Dispatcher: disp}
	hub.OnTrafficUpdate = s.enforceUserQuota
	hub.Redispatch = s.redispatchNodes
	return s, nil
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
	if err := s.Dispatcher.Dispatch(nodeID, rules, rev); err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	_ = db.MarkNodeApplied(s.DB, nodeID)
	return nil
}

// dispatchAfterMutation wraps the common "CRUD-handler dispatches to a
// node and wants to surface failure to the admin doing the mutation"
// pattern.
func (s *Server) dispatchAfterMutation(w http.ResponseWriter, nodeID int64, action string) {
	if err := s.dispatchToNode(nodeID); err != nil {
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到节点失败：%v", action, err))
		log.Printf("dispatch node %d (%s): %v", nodeID, action, err)
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

		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth)
			r.Get("/me", s.apiMe)
			r.Post("/change-password", s.apiChangePassword)
			r.Get("/probe", s.probeEndpoint)
			r.Get("/probe-chain", s.probeChainEndpoint)
			r.Get("/dashboard", s.apiDashboard)
		})

		// Admin routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("admin"))

			r.Get("/nodes", s.apiListNodes)
			r.Post("/nodes", s.apiCreateNode)
			r.Get("/nodes/{id}", s.apiGetNode)
			r.Post("/nodes/{id}/rename", s.apiRenameNode)
			r.Post("/nodes/{id}/relay-host", s.apiSetNodeRelayHost)
			r.Post("/nodes/{id}/port-range", s.apiUpdateNodePortRange)
			r.Post("/nodes/{id}/resync", s.apiResyncNode)
			r.Post("/nodes/{id}/upgrade", s.apiUpgradeNode)
			r.Delete("/nodes/{id}", s.apiDeleteNode)
			r.Post("/nodes/{id}/toggle", s.apiToggleNode)
			r.Post("/nodes/{id}/hidden", s.apiToggleNodeHidden)
			r.Post("/nodes/{id}/owner", s.apiUpdateNodeOwner)
			r.Post("/nodes/resync-all", s.apiResyncAllNodes)
			r.Post("/nodes/upgrade-all", s.apiUpgradeAllNodes)
			r.Get("/nodes/{id}/hops", s.apiListNodeHops)
			r.Post("/nodes/{id}/hops", s.apiUpdateNodeHops)

			r.Get("/settings", s.apiGetSettings)
			r.Post("/settings", s.apiSaveSettings)

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
			r.Post("/users/{id}/quota", s.apiSetUserQuota)
			r.Post("/users/{id}/expiry", s.apiSetUserExpiry)
			r.Post("/users/{id}/reset-traffic", s.apiResetUserTraffic)

			r.Get("/users", s.apiListUsers)
			r.Post("/users/{id}/toggle", s.apiToggleUser)
			r.Post("/users/{id}/reset-password", s.apiResetUserPassword)
			r.Delete("/users/{id}", s.apiDeleteUser)
		})

		// User routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("user"))
			r.Get("/my", s.apiMyDashboard)
			r.Get("/my/rules", s.apiMyListRules)
			r.Post("/my/rules", s.apiMyCreateRule)
			r.Delete("/my/rules/{id}", s.apiMyDeleteRule)
		})
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
