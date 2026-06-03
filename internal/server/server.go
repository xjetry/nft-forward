package server

import (
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

	"github.com/dustin/go-humanize"
	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/wsproto"
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
		"add":   func(a, b int) int { return a + b },
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
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{DB: d, Hub: hub, Dispatcher: disp, tmpl: tmpl}
	// The Hub accumulates tenant usage but owns no policy; the Server decides
	// what a quota breach means and drives the re-dispatch, keeping the Hub a
	// pure transport that never imports the dispatch path.
	hub.OnTrafficUpdate = s.enforceTenantQuota
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

// buildRules converts panel-side Forward rows into kernel-side nft.Rule
// values, stamping per-rule bandwidth from the owning tunnel (forwards
// without a tunnel are unmetered admin-mode rules).
func buildRules(d *sql.DB, forwards []*db.Forward) []nft.Rule {
	tunnels := map[int64]*db.Tunnel{}
	rules := make([]nft.Rule, 0, len(forwards))
	for _, f := range forwards {
		bw := 0
		if f.TunnelID.Valid {
			t, ok := tunnels[f.TunnelID.Int64]
			if !ok {
				t, _ = db.GetTunnel(d, f.TunnelID.Int64)
				if t != nil {
					tunnels[f.TunnelID.Int64] = t
				}
			}
			if t != nil {
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
func computeRev(rules []nft.Rule) string {
	h := sha256.New()
	b, _ := json.Marshal(rules)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(logRequests)
	r.Get("/login", s.getLogin)
	r.Post("/login", s.postLogin)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	r.HandleFunc("/v1/agents", s.Hub.ServeWS)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/logout", s.logout)
		r.Get("/", s.dashboard)
		r.Get("/change-password", s.getChangePassword)
		r.Post("/change-password", s.postChangePassword)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth, s.requireRole("admin"))
		r.Get("/nodes", s.listNodes)
		r.Post("/nodes", s.createNode)
		r.Get("/nodes/{id}", s.showNode)
		r.Post("/nodes/{id}/delete", s.deleteNode)
		r.Post("/nodes/{id}/resync", s.resyncNode)
		r.Post("/nodes/{id}/import-tui", s.handleImportTuiSnapshot)

		r.Post("/nodes/{id}/relay-host", s.setNodeRelayHost)

		r.Post("/settings", s.saveSettings)

		r.Get("/chains", s.listChains)
		r.Get("/chains/new", s.newChain)
		r.Post("/chains", s.createChain)
		r.Get("/chains/{id}", s.showChain)
		r.Post("/chains/{id}", s.saveChain)
		r.Post("/chains/{id}/delete", s.deleteChain)
		r.Post("/chains/{id}/hops/{pos}/reallocate", s.reallocateHop)

		r.Get("/forwards", s.listForwards)
		r.Post("/forwards", s.createForward)
		r.Post("/forwards/{id}/delete", s.deleteForward)

		r.Get("/tunnels", s.listTunnels)
		r.Post("/tunnels", s.createTunnel)
		r.Post("/tunnels/{id}/delete", s.deleteTunnel)

		r.Get("/tenants", s.listTenants)
		r.Post("/tenants", s.createTenant)
		r.Get("/tenants/{id}", s.showTenant)
		r.Post("/tenants/{id}/delete", s.deleteAdminTenant)
		r.Post("/tenants/{id}/toggle", s.toggleTenant)
		r.Post("/tenants/{id}/reset-traffic", s.resetTenantTraffic)
		r.Post("/tenants/{id}/quota-bytes", s.setTenantQuotaBytes)
		r.Post("/tenants/{id}/grants", s.grantTenantTunnel)
		r.Post("/tenants/{id}/grants/{tunnelID}/delete", s.revokeTenantTunnel)
		r.Post("/tenants/{id}/users", s.createTenantUser)

		r.Get("/users", s.listUsers)
		r.Post("/users/{id}/toggle", s.toggleUser)
		r.Post("/users/{id}/reset-password", s.resetUserPassword)
		r.Post("/users/{id}/delete", s.deleteUserHandler)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth, s.requireRole("tenant"))
		r.Get("/my", s.tenantDashboard)
		r.Get("/my/forwards", s.tenantListForwards)
		r.Post("/my/forwards", s.tenantCreateForward)
		r.Post("/my/forwards/{id}/delete", s.tenantDeleteForward)

		r.Get("/my/chains", s.tenantListChains)
		r.Get("/my/chains/new", s.tenantNewChain)
		r.Post("/my/chains", s.tenantCreateChain)
		r.Post("/my/chains/{id}/delete", s.tenantDeleteChain)
	})

	return r
}

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Role == "tenant" {
		http.Redirect(w, r, "/my", http.StatusSeeOther)
		return
	}
	nodes, _ := db.ListNodes(s.DB)
	forwards, _ := db.ListForwards(s.DB)
	tenants, _ := db.ListTenants(s.DB)
	tunnels, _ := db.ListTunnels(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "dashboard.html", map[string]any{
		"User":     u,
		"Nodes":    nodes,
		"Forwards": forwards,
		"Tenants":  tenants,
		"Tunnels":  tunnels,
		"NodeByID": nodeByID,
	})
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	s.render(w, "nodes.html", map[string]any{
		"User":     u,
		"Nodes":    nodes,
		"PanelURL": panelURL,
		"Flash":    flashFromCookie(w, r),
	})
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	if name == "" {
		setFlash(w, "name 不能为空")
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	// Remote nodes dial the panel in reverse (WebSocket, matched by token), so
	// the panel never stores a control-plane address for them.
	n, err := db.CreateNode(s.DB, name, "", secret)
	if err != nil {
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), name)
	s.dispatchAfterMutation(w, n.ID, "节点创建")
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", n.ID), http.StatusSeeOther)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	panelURL := strings.TrimSpace(r.FormValue("panel_url"))
	if err := db.SetSetting(s.DB, "panel_url", panelURL); err != nil {
		setFlash(w, "保存失败: "+err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "settings.panel_url", panelURL, "")
	setFlash(w, "面板地址已保存")
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) showNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	n, err := db.GetNode(s.DB, id)
	if err != nil {
		http.Error(w, "节点不存在", http.StatusNotFound)
		return
	}
	forwards, _ := db.ListForwardsByNode(s.DB, n.ID)
	tuiSnapJSON, ts, _ := db.GetTuiSnapshot(s.DB, n.ID)
	var tuiSnap []wsproto.Forward
	if tuiSnapJSON != "" {
		_ = json.Unmarshal([]byte(tuiSnapJSON), &tuiSnap)
	}
	age := ""
	if ts != nil {
		age = humanize.Time(time.Unix(*ts, 0))
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	panelConfigured := panelURL != ""
	if !panelConfigured {
		// Fall back to the host the admin is browsing on, so the install command
		// is copy-pasteable even before panel_url is set.
		panelURL = "https://" + r.Host
	}
	s.render(w, "node_detail.html", map[string]any{
		"User":               u,
		"Node":               n,
		"Forwards":           forwards,
		"TuiSnapshot":        tuiSnap,
		"TuiSnapshotAge":     age,
		"PanelURL":           panelURL,
		"PanelURLConfigured": panelConfigured,
		"Flash":              flashFromCookie(w, r),
	})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	// Capture chains routing through this node before the delete cascades their
	// hop rows away; their upstream hops materialize this node's relay_host and
	// must be re-wired (around the gap) afterward.
	affectedChains, _ := db.ChainsReferencingNode(s.DB, id)
	if err := db.DeleteNode(s.DB, id); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
	s.rewireChainsAfterNodeChange(w, affectedChains, "节点删除，链路重连")
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) resyncNode(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err := s.dispatchToNode(id); err != nil {
		setFlash(w, "重新同步失败: "+err.Error())
	} else {
		setFlash(w, "已触发重新同步")
	}
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
}

func (s *Server) listForwards(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	forwards, _ := db.ListForwards(s.DB)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "forwards.html", map[string]any{
		"User":     u,
		"Forwards": forwards,
		"Nodes":    nodes,
		"NodeByID": nodeByID,
		"Flash":    flashFromCookie(w, r),
	})
}

func (s *Server) createForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodeID, _ := strconv.ParseInt(r.FormValue("node_id"), 10, 64)
	listenPort, _ := strconv.Atoi(r.FormValue("listen_port"))
	targetPort, _ := strconv.Atoi(r.FormValue("target_port"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	comment := strings.TrimSpace(r.FormValue("comment"))
	mode := strings.TrimSpace(r.FormValue("mode"))

	f := &db.Forward{
		NodeID:     nodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
		Mode:       mode,
	}
	testRule := nft.Rule{
		Proto:    proto,
		SrcPort:  listenPort,
		DestPort: targetPort,
		Mode:     mode,
	}
	if resolver.IsHostname(targetIP) {
		testRule.DestHost = targetIP
	} else {
		testRule.DestIP = targetIP
	}
	if err := nft.Validate(testRule); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	occupied, err := db.OccupiedPortsOnNode(s.DB, nodeID, proto, 0)
	if err != nil {
		setFlash(w, "端口检查失败: "+err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	if occupied[listenPort] {
		setFlash(w, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort))
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.create", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", nodeID, proto, listenPort, targetIP, targetPort))
	s.dispatchAfterMutation(w, nodeID, "转发新增")
	http.Redirect(w, r, "/forwards", http.StatusSeeOther)
}

func (s *Server) deleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	nodeID, err := db.DeleteForward(s.DB, id)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterMutation(w, nodeID, "转发删除")
	http.Redirect(w, r, "/forwards", http.StatusSeeOther)
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
