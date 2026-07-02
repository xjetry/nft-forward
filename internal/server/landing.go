package server

import (
	"fmt"
	"log"
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

// resolveLandingExits resolves the user's admin-assigned landing set for
// materialization. Unlike landingNodesFor — display-oriented, silently
// degrading to manual URIs when the subscription fetch fails — it reports
// failure, because syncing a partial set would flip the subscription's exits
// to present=0 and shift billing classification on a network blip.
func (s *Server) resolveLandingExits(u *db.User, force bool) ([]landing.Node, bool) {
	var nodes []landing.Node
	if uris := strings.TrimSpace(u.LandingURIs); uris != "" {
		nodes = append(nodes, landing.ParseURIs(strings.Split(uris, "\n"))...)
	}
	if url := strings.TrimSpace(u.LandingSubURL); url != "" {
		subNodes, err := s.Landing.Subscription(url, force)
		if err != nil {
			return nil, false
		}
		nodes = append(nodes, subNodes...)
	}
	return nodes, true
}

// dedupLandingNodes keeps the first node per host:port — manual URIs precede
// subscription nodes, so they win a collision — as the materialized shape.
func dedupLandingNodes(nodes []landing.Node) []landing.Node {
	seen := make(map[string]bool, len(nodes))
	out := make([]landing.Node, 0, len(nodes))
	for _, n := range nodes {
		key := net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// syncLandingExits materializes a successfully resolved landing set and
// re-pushes any exits whose push-exclusion state flipped with presence —
// excluded rules generate no traffic, so no counters-driven path would ever
// revive them.
func (s *Server) syncLandingExits(u *db.User, nodes []landing.Node) {
	deduped := dedupLandingNodes(nodes)
	exits := make([]db.LandingExitInput, 0, len(deduped))
	for _, n := range deduped {
		exits = append(exits, db.LandingExitInput{Host: n.Host, Port: n.Port, Name: n.Name, Protocol: n.Protocol, URI: n.URI})
	}
	flipped, synced, err := db.SyncUserLandingExits(s.DB, u.ID, exits, u.LandingSubURL, u.LandingURIs)
	if err != nil {
		log.Printf("landing: sync exits for user %d: %v", u.ID, err)
		return
	}
	if !synced || len(flipped) == 0 {
		return
	}
	go func() {
		for _, k := range flipped {
			s.redispatchUserExit(u.ID, k.Host, k.Port)
		}
	}()
}

// redispatchUserExit re-pushes every node carrying rules that exit to the
// given landing exit so a changed ledger/quota state reaches the data plane.
func (s *Server) redispatchUserExit(userID int64, host string, port int) {
	nodes, err := db.NodesForUserExit(s.DB, userID, host, port)
	if err != nil {
		log.Printf("landing: nodes for user %d exit %s:%d: %v", userID, host, port, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("landing: re-dispatch node %d for exit %s:%d: %v", n, host, port, err)
		}
	}
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
	// Return a fresh preview so the admin sees what the source resolved to,
	// and materialize it while the resolution is known-good.
	target, _ := db.GetUserByID(s.DB, id)
	if nodes, ok := s.resolveLandingExits(target, true); ok {
		s.syncLandingExits(target, nodes)
	}
	nodes := s.landingNodesFor(target, false)
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

// apiListUserLandingExits returns the user's materialized landing-exit set
// for the admin quota card. ?refresh=1 re-resolves the source first; a failed
// resolution silently serves the existing snapshot.
func (s *Server) apiListUserLandingExits(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		if nodes, ok := s.resolveLandingExits(target, true); ok {
			s.syncLandingExits(target, nodes)
		}
	}
	exits, err := db.ListUserLandingExits(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"exits": exits})
}

// exitBody is the shared request shape addressing one exit row.
type exitBody struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	QuotaBytes int64  `json:"quota_bytes"`
}

func (s *Server) apiSetLandingExitQuota(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.QuotaBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "字节数无效")
		return
	}
	updated, present, err := db.SetUserLandingExitQuota(s.DB, id, body.Host, body.Port, body.QuotaBytes)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	// A lowered quota may start excluding immediately; a raised/cleared one
	// lifts the exclusion. Residual rows sit outside the exclusion — no push.
	if present {
		go s.redispatchUserExit(id, body.Host, body.Port)
	}
	db.WriteAudit(s.DB, u.ID, "user.set_exit_quota", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d bytes=%d", body.Host, body.Port, body.QuotaBytes))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetLandingExitTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	updated, present, err := db.ResetUserLandingExitTraffic(s.DB, id, body.Host, body.Port)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	if present {
		go s.redispatchUserExit(id, body.Host, body.Port)
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_exit_traffic", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d", body.Host, body.Port))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteLandingExit(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	status, err := db.DeleteUserLandingExit(s.DB, id, body.Host, body.Port)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch status {
	case "notfound":
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	case "present":
		jsonErr(w, http.StatusConflict, "在册出口由同步维护，不可删除")
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.delete_exit", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d", body.Host, body.Port))
	jsonOK(w, map[string]any{"ok": true})
}
