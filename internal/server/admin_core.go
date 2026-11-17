package server

import (
	"database/sql"
	"errors"
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
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	hash, err := HashPassword(p.Password)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	maxFwd := p.MaxForwards
	if maxFwd <= 0 {
		maxFwd = 100
	}
	// The row and its settings are written in one transaction: a user that
	// exists without the expiry or quota it was created with could not be
	// repaired by retrying, because the retry would hit the username conflict.
	id, err := func() (int64, error) {
		tx, err := s.DB.Begin()
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		id, err := db.CreateUser(tx, username, hash, role)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
			maxFwd, p.TrafficQuotaBytes, id); err != nil {
			return 0, err
		}
		if p.ExpiresAtUnix != 0 {
			if _, err := tx.Exec(`UPDATE users SET expires_at=? WHERE id=?`, p.ExpiresAtUnix, id); err != nil {
				return 0, err
			}
		}
		if p.LandingSubURL != "" || p.AdminNote != "" {
			if _, err := tx.Exec(`UPDATE users SET landing_sub_url=?, admin_note=? WHERE id=?`,
				strings.TrimSpace(p.LandingSubURL), strings.TrimSpace(p.AdminNote), id); err != nil {
				return 0, err
			}
		}
		return id, tx.Commit()
	}()
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
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
	db.WriteAudit(s.DB, adminID, "user.set_expiry", strconv.FormatInt(userID, 10), strconv.FormatInt(expiresAtUnix, 10))
	// The expiry is stored; it reaches the nodes only through this
	// re-dispatch. The call is declarative, so repeating it retries.
	nodes, err := db.DistinctUserNodes(s.DB, userID)
	if err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "到期时间已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("expiry: re-dispatch node %d after setting user %d expiry: %v", n, userID, err)
		}
	}
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
	db.WriteAudit(s.DB, adminID, "user.set_node_rate_limit", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d mbytes=%d", nodeID, mbytes))
	// The cap is stored; what is left is telling the nodes. Not knowing which
	// nodes to tell is reported as a failure — the call is declarative, so
	// repeating it is safe and re-pushes.
	affected, err := db.RulesAffectedByNode(s.DB, userID, nodeID)
	if err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "限速已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	return nil
}

// batchGrantSpec is one node grant applied across a set of users by batchApplyGrants.
type batchGrantSpec struct {
	NodeName          string
	MaxForwards       int
	TrafficQuotaBytes int64
	RateLimitMBytes   int64
}

// batchApplyGrants writes the given grants for every listed user.
//
// The batch is a set of (user, node) pairs: a node named more than once keeps
// its last spec, a user listed more than once counts once. All pairs are
// written in one transaction (db.ApplyGrants), so the request either takes
// effect for every pair or for none.
//
// An existing grant has its caps overwritten unless keepExisting is set, in
// which case it is left untouched and counted in kept. Node names that match
// no node are returned in skipped — for the admin caller this is a
// convenience, not an enumeration oracle (admin may see all nodes). A user id
// that matches no user fails the whole request before anything is written.
func (s *Server) batchApplyGrants(adminID int64, userIDs []int64, grants []batchGrantSpec, keepExisting bool) (granted, kept int, skipped []string, aerr *adminError) {
	if len(userIDs) == 0 {
		return 0, 0, nil, &adminError{http.StatusBadRequest, codeValidation, "请选择目标用户"}
	}
	if len(grants) == 0 {
		return 0, 0, nil, &adminError{http.StatusBadRequest, codeValidation, "请提供授权节点"}
	}
	users := make([]int64, 0, len(userIDs))
	seenUser := map[int64]bool{}
	for _, uid := range userIDs {
		if seenUser[uid] {
			continue
		}
		seenUser[uid] = true
		if _, err := db.GetUserByID(s.DB, uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, 0, nil, &adminError{http.StatusBadRequest, codeValidation, fmt.Sprintf("用户 #%d 不存在", uid)}
			}
			return 0, 0, nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
		}
		users = append(users, uid)
	}
	names := make([]string, len(grants))
	for i, g := range grants {
		names[i] = g.NodeName
	}
	nameToID, err := db.NodeIDsByNames(s.DB, names)
	if err != nil {
		return 0, 0, nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	// One spec per node, the last one given; nodeOrder keeps first-seen order
	// so writes and audit rows come out in a stable sequence.
	specByNode := map[int64]batchGrantSpec{}
	var nodeOrder []int64
	for _, g := range grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			skipped = append(skipped, g.NodeName)
			continue
		}
		if _, seen := specByNode[nid]; !seen {
			nodeOrder = append(nodeOrder, nid)
		}
		specByNode[nid] = g
	}
	writes := make([]db.GrantWrite, 0, len(nodeOrder)*len(users))
	for _, nid := range nodeOrder {
		g := specByNode[nid]
		mf := g.MaxForwards
		if mf <= 0 {
			mf = defaultGrantMaxForwards
		}
		mb := g.RateLimitMBytes
		if mb < 0 {
			mb = 0
		}
		for _, uid := range users {
			writes = append(writes, db.GrantWrite{UserID: uid, NodeID: nid, MaxForwards: mf, TrafficQuotaBytes: g.TrafficQuotaBytes, RateLimitMBytes: mb})
		}
	}
	written, err := db.ApplyGrants(s.DB, writes, keepExisting)
	if err != nil {
		return 0, 0, skipped, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	// Only pairs that were actually written can change what a node enforces;
	// a kept grant has the same caps as before and needs no re-push.
	affected := map[int64]bool{}
	for i, w := range writes {
		if !written[i] {
			kept++
			continue
		}
		granted++
		db.WriteAudit(s.DB, adminID, "user.grant_node", strconv.FormatInt(w.UserID, 10), strconv.FormatInt(w.NodeID, 10))
		ns, err := db.RulesAffectedByNode(s.DB, w.UserID, w.NodeID)
		if err != nil {
			log.Printf("grants: list nodes to re-push for user %d node %d: %v", w.UserID, w.NodeID, err)
			continue
		}
		for _, n := range ns {
			affected[n] = true
		}
	}
	nodeIDs := make([]int64, 0, len(affected))
	for n := range affected {
		nodeIDs = append(nodeIDs, n)
	}
	s.apiDispatchFanout(nodeIDs)
	return granted, kept, skipped, nil
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

// landingSave is what saving a user's landing source reports besides success.
type landingSave struct {
	// User is the user as stored after the save.
	User *db.User
	// SubError says why the subscription could not be fetched; "" when it was
	// fetched or the source has no subscription.
	SubError string
}

// saveUserLandingSource stores a user's landing source — the subscription URL
// and, when uris is non-nil, the manual URIs (nil leaves them as stored) — and
// rematerializes the landing-exit ledger from it. An empty subURL clears the
// subscription; the same sync sweeps empty ledger rows and flips
// ledger-bearing ones to present=0. Shared by the SPA and /api/v1, which
// render the result in their own envelopes.
//
// A subscription that cannot be fetched does not fail the save: it may be
// unreachable now and fine an hour later, and the periodic sync pass will pick
// it up then. But the save is not complete either — the ledger stays on its
// previous snapshot — so the reason is returned in SubError for the caller to
// pass on.
func (s *Server) saveUserLandingSource(adminID, userID int64, subURL string, uris *string) (landingSave, *adminError) {
	subURL = strings.TrimSpace(subURL)
	if subURL != "" && !strings.HasPrefix(subURL, "http://") && !strings.HasPrefix(subURL, "https://") {
		return landingSave{}, &adminError{http.StatusBadRequest, codeValidation, "订阅地址须以 http:// 或 https:// 开头"}
	}
	u, err := db.GetUserByID(s.DB, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return landingSave{}, &adminError{http.StatusNotFound, codeNotFound, "用户不存在"}
	}
	if err != nil {
		return landingSave{}, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	// The only setter writes both source columns; when the caller does not
	// supply the manual URIs, the stored ones are written back unchanged.
	storeURIs := u.LandingURIs
	if uris != nil {
		storeURIs = strings.TrimSpace(*uris)
	}
	if err := db.SetUserLandingSource(s.DB, userID, subURL, storeURIs); err != nil {
		return landingSave{}, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.set_landing", strconv.FormatInt(userID, 10), subURL)
	// Re-read before resolving: SyncUserLandingExits discards results whose
	// source snapshot no longer matches the users row, so the freshly written
	// values must flow into the sync.
	target, err := db.GetUserByID(s.DB, userID)
	if err != nil {
		return landingSave{}, &adminError{http.StatusInternalServerError, codeInternal, "来源已保存，但没能读回用户以刷新落地节点: " + err.Error()}
	}
	saved := landingSave{User: target}
	if nodes, err := s.resolveLandingExits(target, true); err != nil {
		saved.SubError = err.Error()
	} else {
		s.syncLandingExits(target, nodes)
	}
	return saved, nil
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
	db.WriteAudit(s.DB, adminID, "user.toggle", strconv.FormatInt(userID, 10), fmt.Sprintf("disabled=%v", !enabled))
	// The state is stored; the user's rules leave or return to the nodes only
	// through this re-dispatch. The call is declarative, so repeating it
	// retries.
	nodes, err := db.DistinctUserNodes(s.DB, userID)
	if err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "状态已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	s.apiDispatchFanout(nodes)
	return nil
}
