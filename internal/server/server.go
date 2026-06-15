package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Server struct {
	DB         *sql.DB
	Hub        *Hub
	Dispatcher *Dispatcher
	tmpl       *template.Template
}

func New(d *sql.DB) (*Server, error) {
	if _, err := EnsureSelfNode(d); err != nil {
		return nil, fmt.Errorf("ensure self node: %w", err)
	}
	hub := NewHub(d)
	disp := &Dispatcher{DB: d, Hub: hub}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		// unix renders a unix-seconds timestamp. It accepts both the legacy
		// sql.NullInt64 columns and the agent-dialer *int64 columns so a
		// single helper covers every timestamp field templates display.
		"unix": func(v any) string {
			switch t := v.(type) {
			case sql.NullInt64:
				if !t.Valid {
					return "—"
				}
				return fmtUnix(t.Int64)
			case *int64:
				if t == nil {
					return "—"
				}
				return fmtUnix(*t)
			default:
				return "—"
			}
		},
		"nullstr": func(s sql.NullString) string {
			if !s.Valid {
				return ""
			}
			return s.String
		},
		"upper": strings.ToUpper,
		"add": func(a, b int) int { return a + b },
		"date": func(ts int64) string {
			return time.Unix(ts, 0).Format("2006-01-02")
		},
		"dateInput": func(ts int64) string {
			return time.Unix(ts, 0).Format("2006-01-02")
		},
		"expired": func(ts int64) bool {
			return ts > 0 && ts < time.Now().Unix()
		},
		"sub": func(a, b int) int { return a - b },
		"pages": func(total, cur int) []int {
			var out []int
			for i := 1; i <= total; i++ {
				out = append(out, i)
			}
			return out
		},
		"pageURL": func(extra string, page int) template.URL {
			return template.URL(extra + fmt.Sprintf("page=%d", page))
		},
		"div": func(a, b int64) int64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"pct": func(used, total int64) int {
			if total <= 0 {
				return 0
			}
			p := int(used * 100 / total)
			if p > 100 {
				p = 100
			}
			return p
		},
		"mkmap": func(pairs ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		"fmtBytes": func(b int64) string {
			switch {
			case b < 1024:
				return fmt.Sprintf("%d B", b)
			case b < 1024*1024:
				return fmt.Sprintf("%.1f KB", float64(b)/1024)
			case b < 1024*1024*1024:
				return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
			default:
				return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
			}
		},
		"timeAgo": func(v any) string {
			var ts int64
			switch t := v.(type) {
			case sql.NullInt64:
				if !t.Valid {
					return "—"
				}
				ts = t.Int64
			case *int64:
				if t == nil {
					return "—"
				}
				ts = *t
			default:
				return "—"
			}
			d := time.Since(time.Unix(ts, 0))
			switch {
			case d < time.Minute:
				return "刚刚"
			case d < time.Hour:
				return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d 小时前", int(d.Hours()))
			default:
				return fmt.Sprintf("%d 天前", int(d.Hours()/24))
			}
		},
		"firstChar": func(s string) string {
			if s == "" {
				return "?"
			}
			return strings.ToUpper(s[:1])
		},
		"serverVersion": serverVersion,
		"sidebarCounts": func() map[string]int {
			var nodes, tunnels, tenants, forwards, chains, combos int
			d.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodes)
			d.QueryRow("SELECT COUNT(*) FROM tunnels").Scan(&tunnels)
			d.QueryRow("SELECT COUNT(*) FROM tenants").Scan(&tenants)
			d.QueryRow("SELECT COUNT(*) FROM forwards WHERE chain_id IS NULL").Scan(&forwards)
			d.QueryRow("SELECT COUNT(*) FROM chains WHERE tenant_id IS NULL").Scan(&chains)
			d.QueryRow("SELECT COUNT(*) FROM tunnel_combos").Scan(&combos)
			return map[string]int{
				"nodes": nodes, "tunnels": tunnels, "tenants": tenants,
				"forwards": forwards, "chains": chains, "combos": combos,
			}
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{DB: d, Hub: hub, Dispatcher: disp, tmpl: tmpl}
	// The Hub accumulates tenant usage but owns no policy; the Server decides
	// what a quota breach means and drives the re-dispatch, keeping the Hub a
	// pure transport that never imports the dispatch path.
	hub.OnTrafficUpdate = s.enforceTenantQuota
	hub.Redispatch = s.redispatchNodes
	return s, nil
}

// enforceTenantQuota disables a tenant that has reached its traffic quota and
// re-pushes every node it had forwards on so ActiveForwardsForPush (which
// excludes disabled tenants) removes them from the kernel. Quota 0 = unlimited.
func (s *Server) enforceTenantQuota(tenantID int64) {
	t, err := db.GetTenant(s.DB, tenantID)
	if err != nil {
		log.Printf("quota: load tenant %d: %v", tenantID, err)
		return
	}
	if t.Disabled || t.TrafficQuotaBytes <= 0 || t.TrafficUsedBytes < t.TrafficQuotaBytes {
		return
	}
	if err := db.SetTenantDisabled(s.DB, tenantID, true, "流量超额"); err != nil {
		log.Printf("quota: disable tenant %d: %v", tenantID, err)
		return
	}
	log.Printf("tenant %d disabled: traffic quota reached (%d/%d bytes)", tenantID, t.TrafficUsedBytes, t.TrafficQuotaBytes)
	nodes, err := db.DistinctTenantNodes(s.DB, tenantID)
	if err != nil {
		log.Printf("quota: tenant %d nodes: %v", tenantID, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("quota: re-dispatch node %d after disabling tenant %d: %v", n, tenantID, err)
		}
	}
}

// dispatchToNode builds the panel-segment ruleset for nodeID from the
// forwards DB and dispatches it via the Hub (or unix socket for the
// self-node). Called after admin CRUD on forwards/tunnels/tenants.
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

// dispatchAfterFanout dispatches to every node touched by a tenant-scope
// mutation (e.g. tenant toggle/delete affects every node that ran the
// tenant's forwards). Per-node errors are aggregated into a single flash
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
	tenants, _ := db.TenantsByID(d)
	if tenants == nil {
		tenants = map[int64]*db.Tenant{}
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
		if f.TenantID.Valid {
			if tn := tenants[f.TenantID.Int64]; tn != nil {
				rule.TenantName = tn.Name
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

			r.Get("/tenants", s.apiListTenants)
			r.Post("/tenants", s.apiCreateTenant)
			r.Get("/tenants/{id}", s.apiGetTenant)
			r.Delete("/tenants/{id}", s.apiDeleteTenant)
			r.Post("/tenants/{id}/toggle", s.apiToggleTenant)
			r.Post("/tenants/{id}/reset-traffic", s.apiResetTenantTraffic)
			r.Post("/tenants/{id}/quota", s.apiSetTenantQuota)
			r.Post("/tenants/{id}/expiry", s.apiSetTenantExpiry)
			r.Post("/tenants/{id}/grants", s.apiGrantTunnel)
			r.Delete("/tenants/{id}/grants/{tunnelID}", s.apiRevokeTunnel)
			r.Post("/tenants/{id}/combo-grants", s.apiGrantCombo)
			r.Delete("/tenants/{id}/combo-grants/{comboID}", s.apiRevokeCombo)
			r.Post("/tenants/{id}/users", s.apiCreateTenantUser)

			r.Get("/users", s.apiListUsers)
			r.Post("/users/{id}/toggle", s.apiToggleUser)
			r.Post("/users/{id}/reset-password", s.apiResetUserPassword)
			r.Delete("/users/{id}", s.apiDeleteUser)
		})

		// Tenant routes
		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIAuth, s.requireRole("tenant"))
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

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Role == "tenant" {
		http.Redirect(w, r, "/my", http.StatusSeeOther)
		return
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		log.Printf("dashboard: list nodes: %v", err)
	}
	forwards, err := db.ListForwards(s.DB)
	if err != nil {
		log.Printf("dashboard: list forwards: %v", err)
	}
	tenants, err := db.ListTenants(s.DB)
	if err != nil {
		log.Printf("dashboard: list tenants: %v", err)
	}
	tunnels, err := db.ListTunnels(s.DB)
	if err != nil {
		log.Printf("dashboard: list tunnels: %v", err)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	s.render(w, "dashboard.html", map[string]any{
		"User":     u,
		"Nodes":    nodes,
		"Forwards": forwards,
		"Tenants":  tenants,
		"Tunnels":  tunnels,
		"NodeByID": nodeByID,
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func fmtUnix(t int64) string {
	if t == 0 {
		return "—"
	}
	return strconv.FormatInt(t, 10) // raw seconds; template can prettify later
}
