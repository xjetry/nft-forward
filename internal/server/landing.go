package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
	"nft-forward/internal/landing"
)

// landingNodesFor resolves a user's admin-assigned landing nodes by merging the
// manually pasted URIs with the nodes parsed from the subscription URL (if any).
// Manual URIs come first so they win on a host:port collision in landingIndex.
// The user's own URIs are deliberately not handled here — they live only in the
// browser (localStorage) and never reach the server, so the user's node info is
// not exposed. force bypasses the subscription cache.
func (s *Server) landingNodesFor(u *db.User, force bool) []landing.Node {
	var nodes []landing.Node
	if uris := strings.TrimSpace(u.LandingURIs); uris != "" {
		nodes = append(nodes, landing.ParseURIs(strings.Split(uris, "\n"))...)
	}
	if url := strings.TrimSpace(u.LandingSubURL); url != "" {
		if subNodes, err := s.Landing.Subscription(url, force); err == nil {
			nodes = append(nodes, subNodes...)
		}
	}
	return nodes
}

// landingIndex keys landing nodes by "host:port" for exit classification. The
// first node for a key wins, so manual URIs take precedence over subscription.
func landingIndex(nodes []landing.Node) map[string]landing.Node {
	m := make(map[string]landing.Node, len(nodes))
	for _, n := range nodes {
		key := net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
		if _, ok := m[key]; !ok {
			m[key] = n
		}
	}
	return m
}

// hasDynamicSource reports whether the user has a subscription URL (a refresh
// button only makes sense then; a manual-only source is static).
func hasDynamicSource(u *db.User) bool { return strings.TrimSpace(u.LandingSubURL) != "" }

// hasLandingSource reports whether the user has any admin-assigned landing
// source (subscription or URIs). The user's own browser-local URIs are tracked
// client-side and don't factor in here.
func hasLandingSource(u *db.User) bool {
	return hasDynamicSource(u) || strings.TrimSpace(u.LandingURIs) != ""
}

// apiSetUserLanding saves a user's landing-node source (subscription URL and/or
// manual URIs). Admin only.
func (s *Server) apiSetUserLanding(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		LandingSubURL string `json:"landing_sub_url"`
		LandingURIs   string `json:"landing_uris"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	subURL := strings.TrimSpace(body.LandingSubURL)
	uris := strings.TrimSpace(body.LandingURIs)
	if subURL != "" && !strings.HasPrefix(subURL, "http://") && !strings.HasPrefix(subURL, "https://") {
		jsonErr(w, http.StatusBadRequest, "订阅地址须以 http:// 或 https:// 开头")
		return
	}
	if err := db.SetUserLandingSource(s.DB, id, subURL, uris); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_landing", strconv.FormatInt(id, 10), subURL)
	// Return a fresh preview so the admin sees what the source resolved to.
	target, _ := db.GetUserByID(s.DB, id)
	nodes := s.landingNodesFor(target, true)
	jsonOK(w, map[string]any{"ok": true, "landing_nodes": nodes})
}

// apiSubFetch proxies a subscription URL fetch so the browser avoids CORS.
func (s *Server) apiSubFetch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	url := strings.TrimSpace(body.URL)
	if url == "" || (!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://")) {
		jsonErr(w, http.StatusBadRequest, "订阅地址须以 http:// 或 https:// 开头")
		return
	}
	nodes, err := s.Landing.Subscription(url, true)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "获取订阅失败: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"nodes": nodes})
}

// apiMyLandingNodes returns the current user's landing nodes for the create-rule
// picker and the landing-nodes nav page. ?refresh=1 bypasses the subscription
// cache.
func (s *Server) apiMyLandingNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	force := r.URL.Query().Get("refresh") == "1"
	nodes := s.landingNodesFor(u, force)
	jsonOK(w, map[string]any{
		"nodes":       nodes,
		"has_source":  hasLandingSource(u),
		"has_dynamic": hasDynamicSource(u),
	})
}
