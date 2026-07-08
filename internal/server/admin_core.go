package server

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

// adminError carries a v1-mappable failure out of a shared admin mutation core.
// The cores are called by BOTH the SPA (/api) and public (/api/v1) handlers, so
// they must not write to a ResponseWriter themselves — each surface renders the
// error in its own envelope.
type adminError struct {
	status int
	code   string
	msg    string
}

func (e *adminError) Error() string { return e.msg }

// writeAdminErrV1 renders an adminError in the /api/v1 envelope.
func writeAdminErrV1(w http.ResponseWriter, e *adminError) {
	v1Err(w, e.status, e.code, e.msg)
}

// provisionParams are the normalized inputs to provisionUser. Callers decode
// their own wire shape (SPA takes an expires_at date string; v1 takes unix) and
// hand the core already-normalized values.
type provisionParams struct {
	Username          string
	Password          string // caller ensures non-empty (generate a random one for API-only users)
	Role              string
	MaxForwards       int
	TrafficQuotaBytes int64
	ExpiresAtUnix     int64 // 0 = no expiry
	LandingSubURL     string
	AdminNote         string
}

// provisionUser creates a user with billing/quota defaults and audits it. A
// duplicate username is a conflict (not a 500) so the v1 surface can report it
// cleanly. Shared by apiCreateUser (SPA) and v1AdminCreateUser.
func (s *Server) provisionUser(adminID int64, p provisionParams) (*db.User, *adminError) {
	username := strings.TrimSpace(p.Username)
	if username == "" || p.Password == "" {
		return nil, &adminError{http.StatusBadRequest, codeValidation, "用户名和密码不能为空"}
	}
	role := p.Role
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		return nil, &adminError{http.StatusBadRequest, codeValidation, "角色须为 admin 或 user"}
	}
	if _, err := db.GetUserByUsername(s.DB, username); err == nil {
		return nil, &adminError{http.StatusConflict, codeConflict, "用户名已存在"}
	}
	hash, err := HashPassword(p.Password)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	id, err := db.CreateUser(s.DB, username, hash, role)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	maxFwd := p.MaxForwards
	if maxFwd <= 0 {
		maxFwd = 100
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
		maxFwd, p.TrafficQuotaBytes, id); err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	if p.ExpiresAtUnix != 0 {
		s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, p.ExpiresAtUnix, id)
	}
	if p.LandingSubURL != "" || p.AdminNote != "" {
		s.DB.Exec(`UPDATE users SET landing_sub_url=?, admin_note=? WHERE id=?`,
			strings.TrimSpace(p.LandingSubURL), strings.TrimSpace(p.AdminNote), id)
	}
	db.WriteAudit(s.DB, adminID, "user.create", strconv.FormatInt(id, 10), username)
	created, err := db.GetUserByID(s.DB, id)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	return created, nil
}

// setUserQuota sets the user's absolute traffic quota (bytes). Shared by SPA and
// v1; declarative, so a repeated call is a no-op.
func (s *Server) setUserQuota(adminID, userID, bytes int64) *adminError {
	if bytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "字节数无效"}
	}
	if _, err := s.DB.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, bytes, userID); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.set_quota_bytes", strconv.FormatInt(userID, 10), strconv.FormatInt(bytes, 10))
	return nil
}

// setUserMaxForwards sets the user's absolute max forward-rule count.
func (s *Server) setUserMaxForwards(adminID, userID int64, n int) *adminError {
	if n < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "配额数无效"}
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=? WHERE id=?`, n, userID); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.set_max_forwards", strconv.FormatInt(userID, 10), strconv.Itoa(n))
	return nil
}

// setUserExpiry sets the user's absolute expiry (unix seconds; 0 = no expiry)
// and re-dispatches their nodes so the change lands in the kernel immediately.
func (s *Server) setUserExpiry(adminID, userID, expiresAtUnix int64) *adminError {
	if _, err := s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, expiresAtUnix, userID); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	if nodes, err := db.DistinctUserNodes(s.DB, userID); err == nil {
		for _, n := range nodes {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("expiry: re-dispatch node %d after setting user %d expiry: %v", n, userID, err)
			}
		}
	}
	db.WriteAudit(s.DB, adminID, "user.set_expiry", strconv.FormatInt(userID, 10), strconv.FormatInt(expiresAtUnix, 10))
	return nil
}

// grantUserNode ensures the user is granted the node with the given caps
// (create-or-update via db.GrantNode). maxForwards<=0 falls back to the default.
func (s *Server) grantUserNode(adminID, userID, nodeID int64, maxForwards int, quotaBytes int64) *adminError {
	if maxForwards <= 0 {
		maxForwards = defaultGrantMaxForwards
	}
	if err := db.GrantNode(s.DB, userID, nodeID, maxForwards, quotaBytes); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.grant_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	return nil
}

// revokeUserNode removes the grant AND tears down the rules that ran behind it,
// re-dispatching the affected nodes so forwarding (and billing) stops. Returns
// the number of physical nodes re-pushed.
func (s *Server) revokeUserNode(adminID, userID, nodeID int64) (int, *adminError) {
	if err := db.RevokeNode(s.DB, userID, nodeID); err != nil {
		return 0, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	affected, err := db.DeleteRulesForUserNode(s.DB, userID, nodeID)
	if err != nil {
		return 0, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	s.apiDispatchFanout(affected)
	db.WriteAudit(s.DB, adminID, "user.revoke_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	return len(affected), nil
}

// setPerNodeQuota sets a grant's absolute traffic quota (bytes) on user_nodes.
func (s *Server) setPerNodeQuota(adminID, userID, nodeID, bytes int64) *adminError {
	if bytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "字节数无效"}
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET traffic_quota_bytes=? WHERE user_id=? AND node_id=?`,
		bytes, userID, nodeID); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.set_node_quota", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d bytes=%d", nodeID, bytes))
	return nil
}

// setPerNodeRateLimit sets a grant's shared rate cap (MB/s, 0 = unlimited) and
// re-dispatches every node carrying the grant's rule hops so shaping updates.
func (s *Server) setPerNodeRateLimit(adminID, userID, nodeID, mbytes int64) *adminError {
	if mbytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "限速不能为负"}
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`,
		mbytes, userID, nodeID); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	affected, _ := db.RulesAffectedByNode(s.DB, userID, nodeID)
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	db.WriteAudit(s.DB, adminID, "user.set_node_rate_limit", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d mbytes=%d", nodeID, mbytes))
	return nil
}

// batchGrantSpec is one node grant applied across a set of users by batchApplyGrants.
type batchGrantSpec struct {
	NodeName          string
	MaxForwards       int
	TrafficQuotaBytes int64
	RateLimitMBytes   int64
}

// batchApplyGrants grants each named node to every listed user (with per-grant
// caps + rate limit) and re-dispatches nodes whose active rules are affected.
// Node names that don't resolve are skipped and reported — for the admin caller
// this is a convenience, not an enumeration oracle (admin may see all nodes).
func (s *Server) batchApplyGrants(adminID int64, userIDs []int64, grants []batchGrantSpec) (int, []string, *adminError) {
	if len(userIDs) == 0 {
		return 0, nil, &adminError{http.StatusBadRequest, codeValidation, "请选择目标用户"}
	}
	if len(grants) == 0 {
		return 0, nil, &adminError{http.StatusBadRequest, codeValidation, "请提供授权节点"}
	}
	names := make([]string, len(grants))
	for i, g := range grants {
		names[i] = g.NodeName
	}
	nameToID, err := db.NodeIDsByNames(s.DB, names)
	if err != nil {
		return 0, nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	var skipped []string
	var granted int
	for _, g := range grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			skipped = append(skipped, g.NodeName)
			continue
		}
		mf := g.MaxForwards
		if mf <= 0 {
			mf = 10
		}
		for _, uid := range userIDs {
			if err := db.GrantNode(s.DB, uid, nid, mf, g.TrafficQuotaBytes); err != nil {
				return granted, skipped, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
			}
			mb := g.RateLimitMBytes
			if mb < 0 {
				mb = 0
			}
			if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`, mb, uid, nid); err != nil {
				return granted, skipped, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
			}
			db.WriteAudit(s.DB, adminID, "user.grant_node", strconv.FormatInt(uid, 10), strconv.FormatInt(nid, 10))
			granted++
		}
	}
	affected := map[int64]bool{}
	for _, g := range grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			continue
		}
		for _, uid := range userIDs {
			ns, err := db.RulesAffectedByNode(s.DB, uid, nid)
			if err != nil {
				continue
			}
			for _, n := range ns {
				affected[n] = true
			}
		}
	}
	nodeIDs := make([]int64, 0, len(affected))
	for n := range affected {
		nodeIDs = append(nodeIDs, n)
	}
	s.apiDispatchFanout(nodeIDs)
	return granted, skipped, nil
}

// resyncAllNodes re-pushes kernel state to every non-composite node, tolerating
// per-node dispatch failures (a disconnected node counts as failed, not fatal).
// Composite nodes have no agent of their own and are skipped.
func (s *Server) resyncAllNodes() (int, int, error) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		return 0, 0, err
	}
	var synced, failed int
	for _, n := range nodes {
		if n.NodeType == "composite" {
			continue
		}
		if e := s.dispatchToNode(n.ID); e != nil {
			failed++
		} else {
			synced++
		}
	}
	return synced, failed, nil
}

// setUserEnabled sets the user's enabled state (declarative replacement for the
// SPA toggle). Disabling drops the user's rules from their nodes; enabling
// restores them — either way we re-dispatch. An admin may not disable itself.
func (s *Server) setUserEnabled(adminID, userID int64, enabled bool) *adminError {
	if !enabled && userID == adminID {
		return &adminError{http.StatusBadRequest, codeValidation, "不能禁用自己"}
	}
	reason := ""
	if !enabled {
		reason = "管理员手动禁用"
	}
	if err := db.SetUserDisabled(s.DB, userID, !enabled, reason); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	if nodes, err := db.DistinctUserNodes(s.DB, userID); err == nil {
		s.apiDispatchFanout(nodes)
	}
	db.WriteAudit(s.DB, adminID, "user.toggle", strconv.FormatInt(userID, 10), fmt.Sprintf("disabled=%v", !enabled))
	return nil
}
