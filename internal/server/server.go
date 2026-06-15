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
// re-pushes every node it had forwards on so ActiveForwardsForPush (which
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
// forwards DB and dispatches it via the Hub (or unix socket for the
// self-node). Called after admin CRUD on forwards/tunnels/users.
//
// The outcome is reflected on the nodes row so the panel UI can show
// "已同步 / 错误" without each handler having to write that itself:
// success stamps last_apply_at and clears last_error; failure stamps
// last_error while preserving last_apply_at (so admins can read both
// "last successful apply was at T" and "but the most recent attempt
// failed with msg"). DB-write failures of these status columns are
// swallowed because the dispatch error is the load-bearing signal we
// owe the caller.
func (s *Server) dispatchToNode(nodeID int64) error {
	forwards, err := db.ActiveForwardsForPush(s.DB, nodeID)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	rules := buildRules(s.DB, forwards)
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
// pattern. action is a short Chinese label describing what was just
// mutated (e.g. "转发新增"); on failure it becomes part of the flash
// message so the admin sees both what they did and why the kernel
// didn't catch up. Background / non-handler call sites should invoke
// dispatchToNode directly and log.
func (s *Server) dispatchAfterMutation(w http.ResponseWriter, nodeID int64, action string) {
	if err := s.dispatchToNode(nodeID); err != nil {
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到节点失败：%v", action, err))
		log.Printf("dispatch node %d (%s): %v", nodeID, action, err)
	}
}

// dispatchAfterFanout dispatches to every node touched by a user-scope
// mutation (e.g. user toggle/delete affects every node that ran the
// user's forwards). Per-node errors are aggregated into a single flash
// because the flash cookie holds only one message; per-node detail still
// lands in last_error on each affected nodes row.
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
		sort.Strings(failed) // deterministic flash ordering
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到 %d 个节点失败（%s）",
			action, len(failed), strings.Join(failed, "；")))
	}
}

// redispatchNodes re-pushes kernel state to every node a background (WS-driven)
// chain mutation touched. Dispatches run off the caller's goroutine: this is
// invoked from the hub read loop, and dispatchToNode blocks up to the apply-ack
// timeout per node — blocking the read loop would stall the originating agent's
// connection (even pings). The chain mutation is already committed; dispatch is
// best-effort and per-node failures land in last_error.
func (s *Server) redispatchNodes(nodeIDs []int64) {
	for _, n := range nodeIDs {
		go func(n int64) {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("dispatch node %d (链式变更): %v", n, err)
			}
		}(n)
	}
}

// buildRules converts panel-side Forward rows into kernel-side nft.Rule
// values, stamping per-rule bandwidth from the owning tunnel (forwards
// without a tunnel are unmetered admin-mode rules). Lookup tables are
// preloaded in bulk so the conversion is O(forwards) with no per-row queries.
func buildRules(d *sql.DB, forwards []*db.Forward) []nft.Rule {
	tunnels, _ := db.TunnelsByID(d)
	if tunnels == nil {
		tunnels = map[int64]*db.Tunnel{}
	}
	chains, _ := db.ChainsByID(d)
	if chains == nil {
		chains = map[int64]*db.Chain{}
	}
	users, _ := db.UsersByID(d)
	if users == nil {
		users = map[int64]*db.User{}
	}
	rules := make([]nft.Rule, 0, len(forwards))
	for _, f := range forwards {
		bw := 0
		if f.TunnelID.Valid {
			if t := tunnels[f.TunnelID.Int64]; t != nil {
				bw = t.BandwidthMbps
			}
		}
		rule := nft.Rule{
			Proto:         f.Proto,
			SrcPort:       f.ListenPort,
			DestPort:      f.TargetPort,
			Comment:       f.Comment,
			BandwidthMbps: bw,
			Mode:          f.Mode,
		}
		if f.ChainID.Valid {
			rule.ChainID = f.ChainID.Int64
			if c := chains[f.ChainID.Int64]; c != nil {
				rule.ChainName = c.Name
			}
		}
		if f.OwnerID.Valid {
			if u := users[f.OwnerID.Int64]; u != nil {
				rule.TenantName = u.Username
			}
		}
		if resolver.IsHostname(f.TargetIP) {
			rule.DestHost = f.TargetIP
		} else {
			rule.DestIP = f.TargetIP
		}
		rules = append(rules, rule)
	}
	return rules
}

// computeRev returns a stable hash of the ruleset so a reconnecting
// agent whose last_applied_rev matches can be skipped. Determinism
// hinges on ActiveForwardsForPush returning rows in a stable order
// (it sorts by listen_port).
//
// Chain metadata is panel-side display info, not part of the data plane;
// exclude it so a chain rename does not force a redundant re-apply on
// reconnecting nodes.
func computeRev(rules []nft.Rule) string {
	bare := make([]nft.Rule, len(rules))
	for i, r := range rules {
		r.ChainID = 0
		r.ChainName = ""
		r.TenantName = ""
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
			r.Post("/nodes/{id}/resync", s.apiResyncNode)
			r.Post("/nodes/{id}/upgrade", s.apiUpgradeNode)
			r.Delete("/nodes/{id}", s.apiDeleteNode)
			r.Post("/nodes/resync-all", s.apiResyncAllNodes)
			r.Post("/nodes/upgrade-all", s.apiUpgradeAllNodes)

			r.Get("/settings", s.apiGetSettings)
			r.Post("/settings", s.apiSaveSettings)

			r.Get("/tunnels", s.apiListTunnels)
			r.Post("/tunnels", s.apiCreateTunnel)
			r.Delete("/tunnels/{id}", s.apiDeleteTunnel)

			r.Get("/forwards", s.apiListForwards)
			r.Post("/forwards", s.apiCreateForward)
			r.Get("/forwards/{id}", s.apiGetForward)
			r.Put("/forwards/{id}", s.apiUpdateForward)
			r.Delete("/forwards/{id}", s.apiDeleteForward)

			r.Get("/chains", s.apiListChains)
			r.Post("/chains", s.apiCreateChain)
			r.Get("/chains/{id}", s.apiGetChain)
			r.Put("/chains/{id}", s.apiUpdateChain)
			r.Delete("/chains/{id}", s.apiDeleteChain)
			r.Post("/chains/{id}/hops/{pos}/reallocate", s.apiReallocateHop)

			r.Get("/combos", s.apiListCombos)
			r.Post("/combos", s.apiCreateCombo)
			r.Put("/combos/{id}", s.apiUpdateCombo)
			r.Delete("/combos/{id}", s.apiDeleteCombo)

			r.Get("/users/{id}", s.apiGetUser)
			r.Post("/users", s.apiCreateUser)
			r.Post("/users/{id}/grants", s.apiGrantTunnel)
			r.Delete("/users/{id}/grants/{tunnelID}", s.apiRevokeTunnel)
			r.Post("/users/{id}/combo-grants", s.apiGrantCombo)
			r.Delete("/users/{id}/combo-grants/{comboID}", s.apiRevokeCombo)
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
			r.Get("/my/forwards", s.apiMyListForwards)
			r.Post("/my/forwards", s.apiMyCreateForward)
			r.Delete("/my/forwards/{id}", s.apiMyDeleteForward)
			r.Get("/my/chains", s.apiMyListChains)
			r.Post("/my/chains", s.apiMyCreateChain)
			r.Delete("/my/chains/{id}", s.apiMyDeleteChain)
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
