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
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

var urlParse = url.Parse

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
		"unix": func(i sql.NullInt64) string {
			if !i.Valid {
				return "—"
			}
			return fmtUnix(i.Int64)
		},
		"nullstr": func(s sql.NullString) string {
			if !s.Valid {
				return ""
			}
			return s.String
		},
		"upper":   strings.ToUpper,
		"add":     func(a, b int) int { return a + b },
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
		"port": func(addr string) string {
			// Pull the port out of a "http(s)://host:port" address so the
			// node install command's `--listen` matches whatever port the
			// admin configured when adding the node.
			u, err := urlParse(addr)
			if err != nil {
				return "7878"
			}
			if p := u.Port(); p != "" {
				return p
			}
			if u.Scheme == "https" {
				return "443"
			}
			return "80"
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{DB: d, Hub: hub, Dispatcher: disp, tmpl: tmpl}, nil
}

// dispatchToNode builds the panel-segment ruleset for nodeID from the
// forwards DB and dispatches it via the Hub (or unix socket for the
// self-node). Called after admin CRUD on forwards/tunnels/tenants.
func (s *Server) dispatchToNode(nodeID int64) error {
	forwards, err := db.ActiveForwardsForPush(s.DB, nodeID)
	if err != nil {
		return err
	}
	rules := buildRules(s.DB, forwards)
	rev := computeRev(rules)
	return s.Dispatcher.Dispatch(nodeID, rules, rev)
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
	s.render(w, "dashboard.html", map[string]any{
		"User":     u,
		"Nodes":    nodes,
		"Forwards": forwards,
		"Tenants":  tenants,
		"Tunnels":  tunnels,
	})
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "nodes.html", map[string]any{
		"User":  u,
		"Nodes": nodes,
		"Flash": flashFromCookie(w, r),
	})
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	address := strings.TrimSpace(r.FormValue("address"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	if name == "" || address == "" {
		setFlash(w, "name 和 address 不能为空")
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		address = "http://" + address
	}
	n, err := db.CreateNode(s.DB, name, address, secret)
	if err != nil {
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), name)
	if err := s.dispatchToNode(n.ID); err != nil {
		log.Printf("dispatch node %d: %v", n.ID, err)
	}
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", n.ID), http.StatusSeeOther)
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
	s.render(w, "node_detail.html", map[string]any{
		"User":     u,
		"Node":     n,
		"Forwards": forwards,
		"Flash":    flashFromCookie(w, r),
	})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err := db.DeleteNode(s.DB, id); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
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
	s.render(w, "forwards.html", map[string]any{
		"User":     u,
		"Forwards": forwards,
		"Nodes":    nodes,
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

	f := &db.Forward{
		NodeID:     nodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
	}
	testRule := nft.Rule{
		Proto:    proto,
		SrcPort:  listenPort,
		DestPort: targetPort,
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
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.create", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", nodeID, proto, listenPort, targetIP, targetPort))
	if err := s.dispatchToNode(nodeID); err != nil {
		log.Printf("dispatch node %d: %v", nodeID, err)
	}
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
	if err := s.dispatchToNode(nodeID); err != nil {
		log.Printf("dispatch node %d: %v", nodeID, err)
	}
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
