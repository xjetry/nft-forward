package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/accounting"
	"nft-forward/internal/db"
	"nft-forward/internal/landing"
)

// landingNodesFor resolves a user's admin-assigned landing nodes by merging the
// manually pasted URIs with the nodes parsed from the subscription URL (if any).
// Manual URIs come first so they win on a host:port collision downstream
// (e.g. in dedupLandingNodes).
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
func (s *Server) resolveLandingExits(u *db.User, force bool) ([]landing.Node, error) {
	var nodes []landing.Node
	if uris := strings.TrimSpace(u.LandingURIs); uris != "" {
		nodes = append(nodes, landing.ParseURIs(strings.Split(uris, "\n"))...)
	}
	if url := strings.TrimSpace(u.LandingSubURL); url != "" {
		subNodes, err := s.Landing.Subscription(url, force)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, subNodes...)
	}
	return nodes, nil
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
// re-pushes the nodes of any exits whose push-exclusion state flipped with
// presence — excluded rules generate no traffic, so no counters-driven path
// would ever revive them. The nodes come out of the sync's own transaction
// (db.SyncUserLandingExits); only pushing them is left to do afterwards. An
// error means the ledger was not brought up to date.
func (s *Server) syncLandingExits(u *db.User, nodes []landing.Node) error {
	deduped := dedupLandingNodes(nodes)
	exits := make([]db.LandingExitInput, 0, len(deduped))
	for _, n := range deduped {
		exits = append(exits, db.LandingExitInput{Host: n.Host, Port: n.Port, Name: n.Name, Protocol: n.Protocol, URI: n.URI})
	}
	repush, synced, err := db.SyncUserLandingExits(s.DB, u.ID, exits, u.LandingSubURL, u.LandingURIs)
	if err != nil {
		return err
	}
	if synced {
		s.redispatchNodes(repush)
	}
	return nil
}

// refreshLandingLedger is syncLandingExits for the callers that sync in
// passing — a page that resolved the landing set anyway, the periodic pass.
// Nothing was asked of them that a failed sync leaves undone: the sync is one
// transaction (db.SyncUserLandingExits), so the ledger stays on its previous
// snapshot, and the next sync makes up for it. So the failure is logged and
// not reported.
func (s *Server) refreshLandingLedger(u *db.User, nodes []landing.Node) {
	if err := s.syncLandingExits(u, nodes); err != nil {
		log.Printf("landing: sync exits for user %d: %v", u.ID, err)
	}
}

// landingIndexFromDB builds the exit-classification index from the user's
// materialized landing set — the same table that drives metering and push
// exclusion, so the badge can never disagree with billing. No subscription
// fetch happens on this path.
func (s *Server) landingIndexFromDB(userID int64) (map[string]landing.Node, error) {
	exits, err := db.PresentLandingExitsForUser(s.DB, userID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]landing.Node, len(exits))
	for _, e := range exits {
		key := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		if _, ok := m[key]; !ok {
			m[key] = landing.Node{Name: e.Name, Protocol: e.Protocol, Host: e.Host, Port: e.Port, URI: e.URI}
		}
	}
	return m, nil
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
	saved, aerr := s.saveUserLandingSource(u.ID, id, body.LandingSubURL, &body.LandingURIs)
	if aerr != nil {
		jsonErr(w, aerr.status, aerr.msg)
		return
	}
	// A fresh preview, so the admin sees what the source resolved to. When the
	// subscription could not be fetched (sub_error), the preview holds the
	// manual URIs only.
	jsonOK(w, map[string]any{
		"ok": true, "landing_nodes": s.landingNodesFor(saved.User, false), "sub_error": saved.SubError,
	})
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

// myLandingNodeView is a landing node plus its exit-ledger fields for the
// user's landing page.
type myLandingNodeView struct {
	landing.Node
	QuotaBytes int64 `json:"quota_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	Exceeded   bool  `json:"exceeded"`
}

// apiMyLandingNodes returns the current user's landing nodes for the
// create-rule picker and the landing-nodes nav page. ?refresh=1 bypasses the
// subscription cache. A failed resolution serves the last materialized
// snapshot (stale=true) — billing classification runs on the snapshot, so the
// list the user sees should match it rather than silently shrink.
func (s *Server) apiMyLandingNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	force := r.URL.Query().Get("refresh") == "1"
	nodes, err := s.resolveLandingExits(u, force)
	stale := false
	if err == nil {
		s.refreshLandingLedger(u, nodes)
	} else {
		stale = true
		present, err := db.PresentLandingExitsForUser(s.DB, u.ID)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		nodes = make([]landing.Node, 0, len(present))
		for _, e := range present {
			nodes = append(nodes, landing.Node{Name: e.Name, Protocol: e.Protocol, Host: e.Host, Port: e.Port, URI: e.URI})
		}
	}
	exits, err := db.ListUserLandingExits(s.DB, u.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ledger := map[string]*db.LandingExit{}
	for _, e := range exits {
		ledger[net.JoinHostPort(e.Host, strconv.Itoa(e.Port))] = e
	}
	views := make([]myLandingNodeView, 0, len(nodes))
	for _, n := range nodes {
		v := myLandingNodeView{Node: n}
		if e := ledger[net.JoinHostPort(n.Host, strconv.Itoa(n.Port))]; e != nil {
			v.QuotaBytes = e.QuotaBytes
			v.UsedBytes = e.UsedBytes
			v.Exceeded = e.QuotaBytes > 0 && e.UsedBytes >= e.QuotaBytes
			// An admin rename must reach the client the user actually
			// imports, not just this list — rewrite the URI's display name
			// too. A URI the rewriter can't handle is served unchanged so
			// copying keeps working.
			if e.NameOverride != "" {
				v.Node.Name = e.NameOverride
				if rewritten, err := landing.RewriteName(v.Node.URI, e.NameOverride); err == nil {
					v.Node.URI = rewritten
				}
			}
		}
		views = append(views, v)
	}
	jsonOK(w, map[string]any{
		"nodes":       views,
		"has_source":  hasLandingSource(u),
		"has_dynamic": hasDynamicSource(u),
		"stale":       stale,
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
		jsonLookupErr(w, err, http.StatusNotFound, "用户不存在")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		// A refresh that cannot reach the subscription leaves the ledger on
		// its last snapshot; the ledger below is still the truth to show.
		if nodes, err := s.resolveLandingExits(target, true); err != nil {
			log.Printf("landing: refresh exits for user %d: %v", target.ID, err)
		} else {
			s.refreshLandingLedger(target, nodes)
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
	db.WriteAudit(s.DB, u.ID, "user.set_exit_quota", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d bytes=%d", body.Host, body.Port, body.QuotaBytes))
	// A lowered quota may start excluding immediately; a raised/cleared one
	// lifts the exclusion. Residual rows sit outside the exclusion — no push.
	if present {
		if err := s.redispatchUserExit(id, body.Host, body.Port, "设置落地出口流量配额"); err != nil {
			jsonErr(w, http.StatusInternalServerError, "配额已保存，但没能确定需要重新下发的节点: "+err.Error())
			return
		}
	}
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
	updated, present, err := accounting.ResetExit(s.DB, id, body.Host, body.Port)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_exit_traffic", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d", body.Host, body.Port))
	if present {
		if err := s.redispatchUserExit(id, body.Host, body.Port, "重置落地出口流量"); err != nil {
			jsonErr(w, http.StatusInternalServerError, "用量已重置，但没能确定需要重新下发的节点: "+err.Error())
			return
		}
	}
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

// apiRenameLandingExit sets or clears a display-name override on one exit.
// The override outlives subscription syncs and is what the user-facing list
// and copied URIs show; an empty name falls back to the parsed one.
func (s *Server) apiRenameLandingExit(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	updated, err := db.SetUserLandingExitName(s.DB, id, body.Host, body.Port, name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.rename_exit", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d name=%q", body.Host, body.Port, name))
	jsonOK(w, map[string]any{"ok": true})
}
