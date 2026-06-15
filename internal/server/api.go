package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/wsproto"
)

// --- JSON helpers ---

func jsonOK(w http.ResponseWriter, data any) {
	if m, ok := data.(map[string]any); ok {
		ensureNonNilSlices(m)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ensureNonNilSlices replaces nil slices with empty slices so JSON
// serialization produces [] instead of null.
func ensureNonNilSlices(m map[string]any) {
	for k, v := range m {
		if v == nil {
			continue
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			m[k] = []any{}
		}
	}
}

// --- API auth middleware ---

func (s *Server) requireAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			jsonErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		u, err := db.GetSessionUser(s.DB, c.Value)
		if err != nil || u == nil {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
			jsonErr(w, http.StatusUnauthorized, "会话已过期")
			return
		}
		if u.Disabled {
			jsonErr(w, http.StatusForbidden, "账号已被禁用")
			return
		}
		ctx := r.Context()
		ctx = withUser(ctx, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- Dispatcher helpers for JSON handlers (no flash cookies) ---

func (s *Server) apiDispatch(nodeID int64) error {
	if s.Dispatcher == nil {
		return nil
	}
	return s.dispatchToNode(nodeID)
}

func (s *Server) apiDispatchFanout(nodeIDs []int64) {
	for _, n := range nodeIDs {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("api dispatch node %d: %v", n, err)
		}
	}
}

// --- Auth endpoints ---

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	u, err := db.GetUserByUsername(s.DB, body.Username)
	if err != nil {
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(body.Password)) != nil {
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "账号已被禁用")
		return
	}
	token, err := db.CreateSession(s.DB, u.ID, sessionTTL)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, MaxAge: int(sessionTTL.Seconds()),
	})
	db.WriteAudit(s.DB, u.ID, "login", "", "")
	jsonOK(w, map[string]any{"user": apiUserView(u)})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = db.DeleteSession(s.DB, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	jsonOK(w, map[string]any{"user": apiUserFullView(u)})
}

func (s *Server) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(body.OldPassword)) != nil {
		jsonErr(w, http.StatusBadRequest, "原密码不正确")
		return
	}
	if len(body.NewPassword) < 6 {
		jsonErr(w, http.StatusBadRequest, "新密码至少 6 位")
		return
	}
	if body.NewPassword == body.OldPassword {
		jsonErr(w, http.StatusBadRequest, "新密码与原密码相同")
		return
	}
	hash, err := HashPassword(body.NewPassword)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "哈希失败")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, u.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur, _ := r.Cookie(sessionCookie)
	if cur != nil {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, u.ID, cur.Value)
	}
	db.WriteAudit(s.DB, u.ID, "user.change_password", "", "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- Dashboard ---

func (s *Server) apiDashboard(w http.ResponseWriter, r *http.Request) {
	nodes, _ := db.ListNodes(s.DB)
	tunnels, _ := db.ListTunnels(s.DB)
	forwards, _ := db.ListForwards(s.DB)
	users, _ := db.ListUsers(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"nodes": nodes, "tunnels": tunnels,
		"forwards": forwards, "users": users,
		"node_by_id": nodeByID,
	})
}

// --- Nodes ---

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	jsonOK(w, map[string]any{
		"nodes": nodes, "panel_url": panelURL,
		"server_version": serverVersion(),
	})
}

func (s *Server) apiCreateNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		jsonErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	n, err := db.CreateNode(s.DB, body.Name, "", strings.TrimSpace(body.Secret))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.create", strconv.FormatInt(n.ID, 10), body.Name)
	_ = s.apiDispatch(n.ID)
	jsonOK(w, map[string]any{"node": n})
}

func (s *Server) apiGetNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	n, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	forwards, _ := db.ListForwardsByNode(s.DB, n.ID)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	jsonOK(w, map[string]any{
		"node": n, "forwards": forwards, "panel_url": panelURL,
		"panel_url_configured": panelURL != "",
		"server_version":       serverVersion(),
	})
}

func (s *Server) apiRenameNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		jsonErr(w, http.StatusBadRequest, "名称不能为空")
		return
	}
	if err := db.RenameNode(s.DB, id, body.Name); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.rename", strconv.FormatInt(id, 10), body.Name)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	var body struct {
		RelayHost string `json:"relay_host"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	host := strings.TrimSpace(body.RelayHost)
	if host != "" && net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		jsonErr(w, http.StatusBadRequest, "中继地址须为 IPv4 或域名")
		return
	}
	if err := db.UpdateNodeRelayHost(s.DB, id, host); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	chains, _ := db.ChainsReferencingNode(s.DB, id)
	if len(chains) > 0 {
		s.apiRewireChains(chains)
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResyncNode(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.dispatchToNode(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiUpgradeNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	loadSelfBinary()
	if selfBinaryErr != nil {
		jsonErr(w, http.StatusInternalServerError, selfBinaryErr.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	if err := s.Hub.SendUpgrade(id, wsproto.Upgrade{
		Version: serverVersion(), SHA256: selfBinarySHA,
		Size: int64(len(selfBinaryBytes)), DownloadAt: panelURL + "/v1/binary",
	}); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade", strconv.FormatInt(id, 10), serverVersion())
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	affectedChains, _ := db.ChainsReferencingNode(s.DB, id)
	if err := db.DeleteNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.delete", strconv.FormatInt(id, 10), "")
	s.apiRewireChains(affectedChains)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResyncAllNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if err := s.dispatchToNode(n.ID); err != nil {
			fail++
		} else {
			ok++
		}
	}
	jsonOK(w, map[string]any{"ok": true, "synced": ok, "failed": fail})
}

func (s *Server) apiUpgradeAllNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	loadSelfBinary()
	if selfBinaryErr != nil {
		jsonErr(w, http.StatusInternalServerError, selfBinaryErr.Error())
		return
	}
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	if panelURL == "" {
		panelURL = "https://" + r.Host
	}
	upgrade := wsproto.Upgrade{
		Version: serverVersion(), SHA256: selfBinarySHA,
		Size: int64(len(selfBinaryBytes)), DownloadAt: panelURL + "/v1/binary",
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if n.AgentVersion == serverVersion() {
			continue
		}
		if err := s.Hub.SendUpgrade(n.ID, upgrade); err != nil {
			fail++
		} else {
			ok++
		}
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade_all", "", fmt.Sprintf("ok=%d fail=%d", ok, fail))
	jsonOK(w, map[string]any{"ok": true, "upgraded": ok, "failed": fail})
}

// --- Settings ---

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
	jsonOK(w, map[string]any{"panel_url": panelURL})
}

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		PanelURL string `json:"panel_url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := db.SetSetting(s.DB, "panel_url", strings.TrimSpace(body.PanelURL)); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "settings.panel_url", strings.TrimSpace(body.PanelURL), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- Tunnels ---

func (s *Server) apiListTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := db.ListTunnels(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{"tunnels": tunnels, "nodes": nodes, "node_by_id": nodeByID})
}

func (s *Server) apiCreateTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name            string `json:"name"`
		NodeID          int64  `json:"node_id"`
		ProtoMask       string `json:"proto_mask"`
		PortStart       int    `json:"port_start"`
		PortEnd         int    `json:"port_end"`
		TargetCIDRAllow string `json:"target_cidr_allow"`
		BandwidthMbps   int    `json:"bandwidth_mbps"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || body.NodeID == 0 || body.PortStart < 1 || body.PortEnd < body.PortStart || body.PortEnd > 65535 {
		jsonErr(w, http.StatusBadRequest, "字段不完整或端口段无效")
		return
	}
	if body.ProtoMask != "tcp" && body.ProtoMask != "udp" && body.ProtoMask != "tcp+udp" {
		jsonErr(w, http.StatusBadRequest, "协议必须为 tcp、udp 或 tcp+udp")
		return
	}
	cidr := strings.TrimSpace(body.TargetCIDRAllow)
	if cidr == "" {
		cidr = "0.0.0.0/0"
	}
	if err := validateCIDRList(cidr); err != nil {
		jsonErr(w, http.StatusBadRequest, "CIDR 无效: "+err.Error())
		return
	}
	id, err := db.CreateTunnel(s.DB, &db.Tunnel{
		Name: body.Name, NodeID: body.NodeID, ProtoMask: body.ProtoMask,
		PortStart: body.PortStart, PortEnd: body.PortEnd,
		TargetCIDRAllow: cidr, BandwidthMbps: body.BandwidthMbps,
	})
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "tunnel.create", strconv.FormatInt(id, 10), body.Name)
	t, _ := db.GetTunnel(s.DB, id)
	jsonOK(w, map[string]any{"tunnel": t})
}

func (s *Server) apiDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	t, _ := db.GetTunnel(s.DB, id)
	if n, err := db.CountForwardsByTunnel(s.DB, id); err == nil && n > 0 {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("通道仍被 %d 条转发占用", n))
		return
	}
	if err := db.DeleteTunnel(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "tunnel.delete", strconv.FormatInt(id, 10), "")
	if t != nil {
		_ = s.apiDispatch(t.NodeID)
	}
	jsonOK(w, map[string]any{"ok": true})
}

// --- Forwards ---

func (s *Server) apiListForwards(w http.ResponseWriter, r *http.Request) {
	allForwards, _ := db.ListForwards(s.DB)
	nodes, _ := db.ListNodes(s.DB)
	hopInfo, _ := db.ChainHopInfoMap(s.DB)
	ownerByID, _ := db.UsersByID(s.DB)
	combos, _ := db.ListTunnelCombos(s.DB)

	tab := r.URL.Query().Get("tab")
	if tab != "chain" {
		tab = "normal"
	}
	owner := r.URL.Query().Get("owner")
	if owner != "all" {
		owner = "admin"
	}
	var filtered []*db.Forward
	for _, f := range allForwards {
		if tab == "chain" && !f.ChainID.Valid {
			continue
		}
		if tab == "normal" && f.ChainID.Valid {
			continue
		}
		if owner == "admin" && f.OwnerID.Valid {
			continue
		}
		filtered = append(filtered, f)
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"forwards": filtered, "nodes": nodes, "node_by_id": nodeByID,
		"hop_info": hopInfo, "owner_by_id": ownerByID, "combos": combos,
	})
}

func (s *Server) apiCreateForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		NodeID    any    `json:"node_id"`
		ComboID   int64  `json:"combo_id"`
		Proto     string `json:"proto"`
		Mode      string `json:"mode"`
		ListenPort int   `json:"listen_port"`
		Exit      string `json:"exit"`
		Comment   string `json:"comment"`
		ChainName string `json:"chain_name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	// combo path
	nodeStr := fmt.Sprintf("%v", body.NodeID)
	if strings.HasPrefix(nodeStr, "combo:") || body.ComboID > 0 {
		var comboID int64
		if body.ComboID > 0 {
			comboID = body.ComboID
		} else {
			comboID, _ = strconv.ParseInt(nodeStr[6:], 10, 64)
		}
		s.apiCreateForwardFromCombo(w, r, u, comboID, body.Proto, body.Exit, body.ChainName)
		return
	}

	nodeID, _ := toInt64(body.NodeID)
	if nodeID == 0 {
		jsonErr(w, http.StatusBadRequest, "node_id 无效")
		return
	}
	targetIP, targetPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	mode := strings.TrimSpace(body.Mode)
	if !validMode(mode) {
		jsonErr(w, http.StatusBadRequest, "无效的转发模式")
		return
	}
	occupied, err := db.OccupiedPortsOnNode(s.DB, nodeID, proto, 0)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	listenPort := body.ListenPort
	if listenPort == 0 {
		listenPort = db.PickFreePort(db.ChainPortMin, db.ChainPortMax, occupied)
		if listenPort == 0 {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("端口段 %d-%d 内已无可用端口", db.ChainPortMin, db.ChainPortMax))
			return
		}
	} else {
		if listenPort < 1 || listenPort > 65535 {
			jsonErr(w, http.StatusBadRequest, "监听端口非法")
			return
		}
		if occupied[listenPort] {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("端口 %d 已被占用", listenPort))
			return
		}
	}
	f := &db.Forward{
		NodeID: nodeID, Proto: proto, ListenPort: listenPort,
		TargetIP: targetIP, TargetPort: targetPort,
		Comment: strings.TrimSpace(body.Comment), Mode: mode,
	}
	testRule := nft.Rule{Proto: proto, SrcPort: listenPort, DestPort: targetPort, Mode: mode}
	if resolver.IsHostname(targetIP) {
		testRule.DestHost = targetIP
	} else {
		testRule.DestIP = targetIP
	}
	if err := nft.Validate(testRule); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.create", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", nodeID, proto, listenPort, targetIP, targetPort))
	_ = s.apiDispatch(nodeID)
	jsonOK(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) apiCreateForwardFromCombo(w http.ResponseWriter, r *http.Request, u *db.User, comboID int64, proto, exit, chainName string) {
	if comboID == 0 {
		jsonErr(w, http.StatusBadRequest, "组合通道 ID 无效")
		return
	}
	combo, err := db.GetTunnelCombo(s.DB, comboID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "组合通道不存在")
		return
	}
	comboHops, err := db.ListComboHops(s.DB, comboID)
	if err != nil || len(comboHops) == 0 {
		jsonErr(w, http.StatusBadRequest, "组合通道为空")
		return
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto != "tcp" && proto != "udp" {
		jsonErr(w, http.StatusBadRequest, "协议须为 tcp 或 udp")
		return
	}
	exitHost, exitPort, err := parseExit(exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	chainName = strings.TrimSpace(chainName)
	if chainName == "" {
		chainName = combo.Name + "-" + exitHost
	}
	hops := make([]db.HopInput, 0, len(comboHops))
	for _, ch := range comboHops {
		tun, err := db.GetTunnel(s.DB, ch.TunnelID)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("组合通道内的通道 %d 不存在", ch.TunnelID))
			return
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, Mode: ch.Mode})
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	c := &db.Chain{Name: chainName, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create_from_combo", strconv.FormatInt(id, 10), chainName)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "chain_id": id, "entry": entry})
}

func (s *Server) apiGetForward(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	f, err := db.GetForward(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "转发不存在")
		return
	}
	nodes, _ := db.ListNodes(s.DB)
	jsonOK(w, map[string]any{"forward": f, "nodes": nodes})
}

func (s *Server) apiUpdateForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		NodeID     int64  `json:"node_id"`
		Proto      string `json:"proto"`
		Mode       string `json:"mode"`
		ListenPort int    `json:"listen_port"`
		TargetIP   string `json:"target_ip"`
		TargetPort int    `json:"target_port"`
		Comment    string `json:"comment"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	mode := strings.TrimSpace(body.Mode)
	targetIP := strings.TrimSpace(body.TargetIP)
	comment := strings.TrimSpace(body.Comment)
	if !validMode(mode) {
		jsonErr(w, http.StatusBadRequest, "无效的转发模式")
		return
	}
	testRule := nft.Rule{Proto: proto, SrcPort: body.ListenPort, DestPort: body.TargetPort, Mode: mode}
	if resolver.IsHostname(targetIP) {
		testRule.DestHost = targetIP
	} else {
		testRule.DestIP = targetIP
	}
	if err := nft.Validate(testRule); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	oldNodeID, err := db.UpdateForwardByID(s.DB, id, body.NodeID, proto, body.ListenPort, targetIP, body.TargetPort, comment, mode)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.edit", strconv.FormatInt(id, 10),
		fmt.Sprintf("node=%d %s/%d→%s:%d", body.NodeID, proto, body.ListenPort, targetIP, body.TargetPort))
	_ = s.apiDispatch(body.NodeID)
	if oldNodeID != body.NodeID {
		_ = s.apiDispatch(oldNodeID)
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodeID, err := db.DeleteForward(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.delete", strconv.FormatInt(id, 10), "")
	_ = s.apiDispatch(nodeID)
	jsonOK(w, map[string]any{"ok": true})
}

// --- Chains ---

func (s *Server) apiListChains(w http.ResponseWriter, r *http.Request) {
	chains, _ := db.ListAllChains(s.DB)
	nodes, _ := db.ListNodes(s.DB)
	users, _ := db.UsersByID(s.DB)
	views := make([]map[string]any, 0, len(chains))
	for _, c := range chains {
		v := s.buildChainView(c)
		oname := ""
		if c.OwnerID.Valid {
			if u := users[c.OwnerID.Int64]; u != nil {
				oname = u.Username
			}
		}
		views = append(views, map[string]any{
			"chain": c, "owner_name": oname,
			"path": v.Path, "entry": v.Entry,
			"entry_node_id": v.EntryNodeID,
		})
	}
	jsonOK(w, map[string]any{"chains": views, "nodes": nodes})
}

func (s *Server) apiCreateChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Hops      []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || (proto != "tcp" && proto != "udp") {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp 或 udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Hops) == 0 {
		jsonErr(w, http.StatusBadRequest, "至少添加一个节点")
		return
	}
	hops := make([]db.HopInput, len(body.Hops))
	for i, h := range body.Hops {
		hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
	}
	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	c := &db.Chain{Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"chain": c, "entry": entry})
}

func (s *Server) apiGetChain(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "链路不存在")
		return
	}
	hops, _ := db.ListChainHops(s.DB, id)
	forwards, _ := db.ListForwardsByChain(s.DB, id)
	fwByNode := make(map[int64]*db.Forward, len(forwards))
	for _, f := range forwards {
		fwByNode[f.NodeID] = f
	}
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"chain": c, "hops": hops,
		"fw_by_node": fwByNode, "nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiUpdateChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "链路不存在")
		return
	}
	var body struct {
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Hops      []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || (proto != "tcp" && proto != "udp") {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp 或 udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Hops) == 0 {
		jsonErr(w, http.StatusBadRequest, "至少添加一个节点")
		return
	}
	hops := make([]db.HopInput, len(body.Hops))
	for i, h := range body.Hops {
		hops[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode}
	}
	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	c.Name, c.Proto, c.ExitHost, c.ExitPort = name, proto, exitHost, exitPort
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if err := db.UpdateChainHeader(tx, c); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.save", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "entry": entry})
}

func (s *Server) apiDeleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.delete", strconv.FormatInt(id, 10), "")
	s.apiDispatchFanout(nodes)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiReallocateHop(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	pos, _ := strconv.Atoi(chi.URLParam(r, "pos"))
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "链路不存在")
		return
	}
	hops, err := db.ListChainHops(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pos < 0 || pos >= len(hops) {
		jsonErr(w, http.StatusBadRequest, "跳序号非法")
		return
	}
	var body struct {
		Port int `json:"port"`
	}
	_ = decodeJSON(r, &body)

	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	var avoid map[int64]int
	if body.Port > 0 {
		inputs[pos].DesiredPort = body.Port
	} else {
		avoid = map[int64]int{hops[pos].NodeID: hops[pos].ListenPort}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	_, affected, err := db.RegenerateChain(tx, c, inputs, avoid)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.reallocate", strconv.FormatInt(id, 10), strconv.Itoa(pos))
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true})
}

// --- Combos ---

func (s *Server) apiListCombos(w http.ResponseWriter, r *http.Request) {
	combos, _ := db.ListTunnelCombos(s.DB)
	tunnels, _ := db.ListTunnels(s.DB)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	tunnelByID := buildMap(tunnels, func(t *db.Tunnel) int64 { return t.ID })

	views := make([]map[string]any, 0, len(combos))
	for _, c := range combos {
		hops, _ := db.ListComboHops(s.DB, c.ID)
		names := make([]string, 0, len(hops))
		for _, h := range hops {
			t := tunnelByID[h.TunnelID]
			if t == nil {
				names = append(names, fmt.Sprintf("#%d", h.TunnelID))
				continue
			}
			n := nodeByID[t.NodeID]
			if n != nil {
				names = append(names, n.Name)
			} else {
				names = append(names, t.Name)
			}
		}
		views = append(views, map[string]any{
			"combo": c, "hops": hops,
			"path": strings.Join(names, " → "),
		})
	}
	jsonOK(w, map[string]any{"combos": views, "tunnels": tunnels, "node_by_id": nodeByID})
}

func (s *Server) apiCreateCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Name string `json:"name"`
		Hops []struct {
			TunnelID int64  `json:"tunnel_id"`
			Mode     string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "名称必填")
		return
	}
	if len(body.Hops) < 2 {
		jsonErr(w, http.StatusBadRequest, "组合通道至少需要 2 个通道")
		return
	}
	hops := make([]db.TunnelComboHop, len(body.Hops))
	for i, h := range body.Hops {
		mode := h.Mode
		if mode == "" {
			mode = "userspace"
		}
		hops[i] = db.TunnelComboHop{TunnelID: h.TunnelID, Mode: mode}
	}
	id, err := db.CreateTunnelCombo(s.DB, name, hops)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "combo.create", strconv.FormatInt(id, 10), name)
	jsonOK(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) apiUpdateCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Name string `json:"name"`
		Hops []struct {
			TunnelID int64  `json:"tunnel_id"`
			Mode     string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "名称必填")
		return
	}
	if len(body.Hops) < 2 {
		jsonErr(w, http.StatusBadRequest, "组合通道至少需要 2 个通道")
		return
	}
	hops := make([]db.TunnelComboHop, len(body.Hops))
	for i, h := range body.Hops {
		mode := h.Mode
		if mode == "" {
			mode = "userspace"
		}
		hops[i] = db.TunnelComboHop{TunnelID: h.TunnelID, Mode: mode}
	}
	if err := db.UpdateTunnelCombo(s.DB, id, name, hops); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "combo.save", strconv.FormatInt(id, 10), name)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := db.DeleteTunnelCombo(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "combo.delete", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- Users ---

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := db.ListUsers(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"users": users})
}

func (s *Server) apiGetUser(w http.ResponseWriter, r *http.Request) {
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
	tunnels, grants, _ := db.ListTunnelsForUser(s.DB, id)
	allTunnels, _ := db.ListTunnels(s.DB)
	forwards, _ := db.ListForwardsForUser(s.DB, id)
	combos, comboGrants, _ := db.ListCombosForUser(s.DB, id)
	allCombos, _ := db.ListTunnelCombos(s.DB)
	jsonOK(w, map[string]any{
		"user": target, "tunnels": tunnels,
		"grants": grants, "all_tunnels": allTunnels,
		"combos": combos, "combo_grants": comboGrants,
		"all_combos": allCombos, "forwards": forwards,
	})
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	var body struct {
		Username          string `json:"username"`
		Password          string `json:"password"`
		Role              string `json:"role"`
		MaxForwards       int    `json:"max_forwards"`
		TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
		ExpiresAt         string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" || body.Password == "" {
		jsonErr(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}
	role := body.Role
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		jsonErr(w, http.StatusBadRequest, "角色须为 admin 或 user")
		return
	}
	hash, err := HashPassword(body.Password)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := db.CreateUser(s.DB, username, hash, role)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Set quota fields if provided
	maxFwd := body.MaxForwards
	if maxFwd <= 0 {
		maxFwd = 100
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
		maxFwd, body.TrafficQuotaBytes, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw := strings.TrimSpace(body.ExpiresAt); raw != "" {
		if et, err := time.Parse("2006-01-02", raw); err == nil {
			s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, et.Unix(), id)
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.create", strconv.FormatInt(id, 10), username)
	created, _ := db.GetUserByID(s.DB, id)
	jsonOK(w, map[string]any{"user": created})
}

func (s *Server) apiGrantTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TunnelID    int64 `json:"tunnel_id"`
		MaxForwards int   `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards <= 0 {
		body.MaxForwards = 10
	}
	if err := db.GrantTunnel(s.DB, userID, body.TunnelID, body.MaxForwards); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.grant", strconv.FormatInt(userID, 10), strconv.FormatInt(body.TunnelID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiRevokeTunnel(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	tunnelID, err := urlParamInt64(r, "tunnelID")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad tunnel id")
		return
	}
	if err := db.RevokeTunnel(s.DB, userID, tunnelID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.revoke", strconv.FormatInt(userID, 10), strconv.FormatInt(tunnelID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiGrantCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		ComboID     int64 `json:"combo_id"`
		MaxForwards int   `json:"max_forwards"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.MaxForwards <= 0 {
		body.MaxForwards = 10
	}
	if err := db.GrantCombo(s.DB, userID, body.ComboID, body.MaxForwards); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.grant_combo", strconv.FormatInt(userID, 10), strconv.FormatInt(body.ComboID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiRevokeCombo(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	comboID, err := urlParamInt64(r, "comboID")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad combo id")
		return
	}
	if err := db.RevokeCombo(s.DB, userID, comboID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.revoke_combo", strconv.FormatInt(userID, 10), strconv.FormatInt(comboID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetUserQuota(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficQuotaBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "字节数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, body.TrafficQuotaBytes, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_quota_bytes", strconv.FormatInt(id, 10), strconv.FormatInt(body.TrafficQuotaBytes, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetUserExpiry(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	var expiresAt int64
	raw := strings.TrimSpace(body.ExpiresAt)
	if raw != "" {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "日期格式无效（需 YYYY-MM-DD）")
			return
		}
		expiresAt = t.Unix()
	}
	if _, err := s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, expiresAt, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_expiry", strconv.FormatInt(id, 10), raw)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetUserTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := db.ResetUserTraffic(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_traffic", strconv.FormatInt(id, 10), "")
	nodes, _ := db.DistinctUserNodes(s.DB, id)
	s.apiDispatchFanout(nodes)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiToggleUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
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
	if target.ID == u.ID {
		jsonErr(w, http.StatusBadRequest, "不能禁用自己")
		return
	}
	willDisable := !target.Disabled
	reason := ""
	if willDisable {
		reason = "管理员手动禁用"
	}
	if err := db.SetUserDisabled(s.DB, id, willDisable, reason); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if willDisable {
		if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
			s.apiDispatchFanout(nodes)
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.toggle", strconv.FormatInt(id, 10), fmt.Sprintf("disabled=%v", willDisable))
	jsonOK(w, map[string]any{"ok": true, "disabled": willDisable})
}

func (s *Server) apiResetUserPassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	newPw := db.RandToken(8)
	hash, err := HashPassword(newPw)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	db.WriteAudit(s.DB, u.ID, "user.reset_password", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true, "new_password": newPw})
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if id == u.ID {
		jsonErr(w, http.StatusBadRequest, "不能删除自己")
		return
	}
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	affected, err := db.DeleteForwardsForUser(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.DeleteUser(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.apiDispatchFanout(affected)
	db.WriteAudit(s.DB, u.ID, "user.delete", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}

// --- My (user self-service) endpoints ---

func (s *Server) apiMyDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tunnels, grants, _ := db.ListTunnelsForUser(s.DB, u.ID)
	forwards, _ := db.ListForwardsForUser(s.DB, u.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"user": apiUserFullView(u), "tunnels": tunnels, "grants": grants,
		"forwards": forwards, "nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiMyListForwards(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	tunnels, grants, _ := db.ListTunnelsForUser(s.DB, u.ID)
	forwards, _ := db.ListForwardsForUser(s.DB, u.ID)
	nodes, _ := db.ListNodes(s.DB)
	hopInfo, _ := db.ChainHopInfoMap(s.DB)
	combos, _, _ := db.ListCombosForUser(s.DB, u.ID)

	tab := r.URL.Query().Get("tab")
	if tab != "chain" {
		tab = "normal"
	}
	var filtered []*db.Forward
	for _, f := range forwards {
		if tab == "chain" && f.ChainID.Valid {
			filtered = append(filtered, f)
		} else if tab == "normal" && !f.ChainID.Valid {
			filtered = append(filtered, f)
		}
	}
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	tunnelByID := buildMap(tunnels, func(t *db.Tunnel) int64 { return t.ID })
	jsonOK(w, map[string]any{
		"user": apiUserFullView(u), "forwards": filtered, "tunnels": tunnels, "grants": grants,
		"hop_info": hopInfo, "combos": combos, "nodes": nodes,
		"node_by_id": nodeByID, "tunnel_by_id": tunnelByID,
	})
}

func (s *Server) apiMyCreateForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		jsonErr(w, http.StatusForbidden, "用户已过期")
		return
	}

	var body struct {
		TunnelID   any    `json:"tunnel_id"`
		ComboID    int64  `json:"combo_id"`
		Proto      string `json:"proto"`
		Mode       string `json:"mode"`
		ListenPort int    `json:"listen_port"`
		Exit       string `json:"exit"`
		Comment    string `json:"comment"`
		ChainName  string `json:"chain_name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	// combo path
	tunnelStr := fmt.Sprintf("%v", body.TunnelID)
	if strings.HasPrefix(tunnelStr, "combo:") || body.ComboID > 0 {
		var comboID int64
		if body.ComboID > 0 {
			comboID = body.ComboID
		} else {
			comboID, _ = strconv.ParseInt(tunnelStr[6:], 10, 64)
		}
		s.apiUserCreateForwardFromCombo(w, r, u, comboID, body.Proto, body.Exit, body.ChainName)
		return
	}

	tunnelID, _ := toInt64(body.TunnelID)
	if tunnelID == 0 {
		jsonErr(w, http.StatusBadRequest, "tunnel_id 无效")
		return
	}
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	mode := strings.TrimSpace(body.Mode)
	if !validMode(mode) {
		jsonErr(w, http.StatusBadRequest, "无效的转发模式")
		return
	}
	if mode == "userspace" && proto == "udp" {
		jsonErr(w, http.StatusBadRequest, "UDP 不支持用户态转发")
		return
	}

	grant, err := db.GetGrant(s.DB, u.ID, tunnelID)
	if err != nil {
		jsonErr(w, http.StatusForbidden, "无权使用该通道")
		return
	}
	tunnel, err := db.GetTunnel(s.DB, tunnelID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "通道不存在")
		return
	}

	targetIP, targetPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	occupied, err := db.OccupiedPortsOnNode(s.DB, tunnel.NodeID, proto, 0)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	listenPort := body.ListenPort
	if listenPort == 0 {
		listenPort = db.PickFreePort(tunnel.PortStart, tunnel.PortEnd, occupied)
		if listenPort == 0 {
			jsonErr(w, http.StatusConflict, fmt.Sprintf("通道 %d-%d 内已无可用端口", tunnel.PortStart, tunnel.PortEnd))
			return
		}
	} else if occupied[listenPort] {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("端口 %d 已被占用", listenPort))
		return
	}
	if err := validateAgainstTunnel(tunnel, proto, listenPort, targetIP, targetPort); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	totalCount, _ := db.CountForwardsForUser(s.DB, u.ID)
	if totalCount >= u.MaxForwards {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("已达用户最大转发数（%d）", u.MaxForwards))
		return
	}
	tunCount, _ := db.CountForwardsForUserTunnel(s.DB, u.ID, tunnelID)
	if tunCount >= grant.MaxForwards {
		jsonErr(w, http.StatusConflict, fmt.Sprintf("已达该通道最大转发数（%d）", grant.MaxForwards))
		return
	}

	f := &db.Forward{
		NodeID: tunnel.NodeID, Proto: proto, ListenPort: listenPort,
		TargetIP: targetIP, TargetPort: targetPort,
		Comment: strings.TrimSpace(body.Comment), Mode: mode,
	}
	f.OwnerID = sql.NullInt64{Int64: u.ID, Valid: true}
	f.TunnelID = sql.NullInt64{Int64: tunnel.ID, Valid: true}
	id, err := db.CreateForward(s.DB, f)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.user_create", strconv.FormatInt(id, 10),
		fmt.Sprintf("user=%d tunnel=%d %s/%d→%s:%d", u.ID, tunnel.ID, proto, listenPort, targetIP, targetPort))
	_ = s.apiDispatch(tunnel.NodeID)
	jsonOK(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) apiUserCreateForwardFromCombo(w http.ResponseWriter, r *http.Request, u *db.User, comboID int64, proto, exit, chainName string) {
	if comboID == 0 {
		jsonErr(w, http.StatusBadRequest, "组合通道 ID 无效")
		return
	}
	if _, err := db.GetComboGrant(s.DB, u.ID, comboID); err != nil {
		jsonErr(w, http.StatusForbidden, "无权使用该组合通道")
		return
	}
	combo, err := db.GetTunnelCombo(s.DB, comboID)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "组合通道不存在")
		return
	}
	comboHops, err := db.ListComboHops(s.DB, comboID)
	if err != nil || len(comboHops) == 0 {
		jsonErr(w, http.StatusBadRequest, "组合通道为空")
		return
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto != "tcp" && proto != "udp" {
		jsonErr(w, http.StatusBadRequest, "协议须为 tcp 或 udp")
		return
	}
	exitHost, exitPort, err := parseExit(exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	chainName = strings.TrimSpace(chainName)
	if chainName == "" {
		chainName = combo.Name + "-" + exitHost
	}
	hops := make([]db.HopInput, 0, len(comboHops))
	for _, ch := range comboHops {
		tun, err := db.GetTunnel(s.DB, ch.TunnelID)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("组合通道内的通道 %d 不存在", ch.TunnelID))
			return
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, TunnelID: nullInt64(ch.TunnelID), Mode: ch.Mode})
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	c := &db.Chain{OwnerID: nullInt64(u.ID), Name: chainName, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.user_create_from_combo", strconv.FormatInt(id, 10), chainName)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "chain_id": id, "entry": entry})
}

func (s *Server) apiMyDeleteForward(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	f, err := db.GetForward(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "转发不存在")
		return
	}
	if !f.OwnerID.Valid || f.OwnerID.Int64 != u.ID {
		jsonErr(w, http.StatusForbidden, "无权操作该转发")
		return
	}
	nodeID, err := db.DeleteForward(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "forward.user_delete", strconv.FormatInt(id, 10), "")
	_ = s.apiDispatch(nodeID)
	jsonOK(w, map[string]any{"ok": true})
}

// --- User chains ---

func (s *Server) apiMyListChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	chains, _ := db.ListChainsByUser(s.DB, u.ID)
	views := make([]map[string]any, 0, len(chains))
	for _, c := range chains {
		v := s.buildChainView(c)
		views = append(views, map[string]any{
			"chain": c,
			"path": v.Path, "entry": v.Entry, "entry_node_id": v.EntryNodeID,
		})
	}
	tunnels, _, _ := db.ListTunnelsForUser(s.DB, u.ID)
	combos, _, _ := db.ListCombosForUser(s.DB, u.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
	jsonOK(w, map[string]any{
		"chains": views, "tunnels": tunnels,
		"combos": combos, "nodes": nodes, "node_by_id": nodeByID,
	})
}

func (s *Server) apiMyCreateChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u.Disabled {
		jsonErr(w, http.StatusForbidden, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		jsonErr(w, http.StatusForbidden, "用户已过期")
		return
	}
	var body struct {
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Hops      []struct {
			TunnelID int64  `json:"tunnel_id"`
			Mode     string `json:"mode"`
		} `json:"hops"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	proto := strings.ToLower(strings.TrimSpace(body.Proto))
	if name == "" || (proto != "tcp" && proto != "udp") {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp 或 udp")
		return
	}
	exitHost, exitPort, err := parseExit(body.Exit)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Hops) == 0 {
		jsonErr(w, http.StatusBadRequest, "至少添加一个通道")
		return
	}

	// Build hop inputs, verifying tunnel grants
	hops := make([]db.HopInput, 0, len(body.Hops))
	var lastTunnel *db.Tunnel
	for _, h := range body.Hops {
		if _, err := db.GetGrant(s.DB, u.ID, h.TunnelID); err != nil {
			jsonErr(w, http.StatusForbidden, fmt.Sprintf("无权使用通道 %d", h.TunnelID))
			return
		}
		tun, err := db.GetTunnel(s.DB, h.TunnelID)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("通道 %d 不存在", h.TunnelID))
			return
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, TunnelID: nullInt64(h.TunnelID), Mode: h.Mode})
		lastTunnel = tun
	}
	if body.EntryPort > 0 {
		hops[0].DesiredPort = body.EntryPort
	}
	if err := exitAllowedByTunnel(lastTunnel, exitHost); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkUserChainQuota(u, hops, 0); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	c := &db.Chain{OwnerID: nullInt64(u.ID), Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.user_create", strconv.FormatInt(id, 10), name)
	s.apiDispatchFanout(affected)
	jsonOK(w, map[string]any{"ok": true, "chain_id": id, "entry": entry})
}

func (s *Server) apiMyDeleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	c, err := db.GetChain(s.DB, id)
	if err != nil || !c.OwnerID.Valid || c.OwnerID.Int64 != u.ID {
		jsonErr(w, http.StatusForbidden, "无权操作该链路")
		return
	}
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.user_delete", strconv.FormatInt(id, 10), "")
	s.apiDispatchFanout(nodes)
	jsonOK(w, map[string]any{"ok": true})
}

// --- Helpers ---

// withUser injects *db.User into ctx (same key as requireAuth).
func withUser(ctx context.Context, u *db.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// apiUserView returns the minimal user representation for login response.
func apiUserView(u *db.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
	}
}

// apiUserFullView includes quota/traffic/expiry fields for /api/me.
func apiUserFullView(u *db.User) map[string]any {
	m := apiUserView(u)
	m["max_forwards"] = u.MaxForwards
	m["traffic_quota_bytes"] = u.TrafficQuotaBytes
	m["traffic_used_bytes"] = u.TrafficUsedBytes
	if u.ExpiresAt.Valid {
		m["expires_at"] = u.ExpiresAt.Int64
	} else {
		m["expires_at"] = nil
	}
	if u.DisableReason.Valid {
		m["disable_reason"] = u.DisableReason.String
	} else {
		m["disable_reason"] = nil
	}
	return m
}

// apiRewireChains regenerates affected chains and dispatches (no flash cookies).
func (s *Server) apiRewireChains(chainIDs []int64) {
	seen := map[int64]bool{}
	var affected []int64
	for _, cid := range chainIDs {
		aff, err := s.regenerateChainByID(cid)
		if err != nil {
			log.Printf("api rewire chain %d: %v", cid, err)
			continue
		}
		for _, n := range aff {
			if !seen[n] {
				seen[n] = true
				affected = append(affected, n)
			}
		}
	}
	s.apiDispatchFanout(affected)
}

// toInt64 converts various JSON number representations to int64.
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}
