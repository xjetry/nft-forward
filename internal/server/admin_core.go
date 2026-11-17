package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// msgNoGrant answers a change to a grant the user does not have. It covers a
// user or a node that does not exist as well: either way there is no such
// (user, node) row to change.
const msgNoGrant = "该用户没有这个节点的授权"

// adminLookupErr is lookupErr as an adminError: the caller's status, code and
// message when the row does not exist, an internal error carrying err
// otherwise. err is not nil.
func adminLookupErr(err error, status int, code, msg string) *adminError {
	if errors.Is(err, sql.ErrNoRows) {
		return &adminError{status, code, msg}
	}
	return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
}

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
	ExpiresAt         sql.NullInt64 // 由 parseExpiryDate / parseExpiryUnix 给出
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
	// The user is read back in the same transaction for the same reason — a
	// create that cannot report what it created is not committed.
	created, err := func() (*db.User, error) {
		tx, err := s.DB.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		id, err := db.CreateUser(tx, username, hash, role)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
			maxFwd, p.TrafficQuotaBytes, id); err != nil {
			return nil, err
		}
		if p.ExpiresAt.Valid {
			if _, err := tx.Exec(`UPDATE users SET expires_at=? WHERE id=?`, p.ExpiresAt, id); err != nil {
				return nil, err
			}
		}
		if p.LandingSubURL != "" || p.AdminNote != "" {
			if _, err := tx.Exec(`UPDATE users SET landing_sub_url=?, admin_note=? WHERE id=?`,
				strings.TrimSpace(p.LandingSubURL), strings.TrimSpace(p.AdminNote), id); err != nil {
				return nil, err
			}
		}
		created, err := db.GetUserByID(tx, id)
		if err != nil {
			return nil, err
		}
		return created, tx.Commit()
	}()
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	db.WriteAudit(s.DB, adminID, "user.create", strconv.FormatInt(created.ID, 10), username)
	return created, nil
}

// setUserQuota sets the user's absolute traffic quota (bytes) and re-dispatches
// their nodes: the quota is a condition of the push predicate, so raising it
// restores the user's rules and lowering it below current usage withdraws them.
// Shared by SPA and v1; declarative, so a repeated call converges on the same
// state.
func (s *Server) setUserQuota(adminID, userID, bytes int64) *adminError {
	if bytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "字节数无效"}
	}
	if err := db.UpdateRow(s.DB, `UPDATE users SET traffic_quota_bytes=? WHERE id=?`, bytes, userID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, "用户不存在")
	}
	db.WriteAudit(s.DB, adminID, "user.set_quota_bytes", strconv.FormatInt(userID, 10), strconv.FormatInt(bytes, 10))
	// 配额已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	if err := s.redispatchUser(userID, "设置全局流量配额"); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "配额已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	return nil
}

// setUserMaxForwards sets the user's absolute max forward-rule count.
func (s *Server) setUserMaxForwards(adminID, userID int64, n int) *adminError {
	if n < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "配额数无效"}
	}
	if err := db.UpdateRow(s.DB, `UPDATE users SET max_forwards=? WHERE id=?`, n, userID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, "用户不存在")
	}
	db.WriteAudit(s.DB, adminID, "user.set_max_forwards", strconv.FormatInt(userID, 10), strconv.Itoa(n))
	return nil
}

// 到期时间进入系统只有下面两个入口，各对应一种线上写法。二者的返回值只有两种：
// NULL（sql.NullInt64 的零值，不设到期），或一个正的 unix 秒。users.expires_at 的
// 三条写入语句（provisionUser、setUserExpiry、apiUpdateUserProfile 里各一条）绑定的
// 都是它们的返回值；建用户的 INSERT（db.CreateUser）不写这一列，列没有默认值，留下
// 的是 NULL。所以库里不会有 0 或负数——到期判定（db.User.Expired 与下发用的 SQL）
// 只把 NULL 当作永不到期，0 在它那里是一个早已过去的时刻。
//
// “不设到期”是放宽，两个入口都只在调用方明说时才给：页面是空串，/api/v1 是 0。
// 认不出的、或落不到一个正时刻上的取值一律拒绝，不归到“不设到期”里去。

// parseExpiryDate 解析页面送来的到期日：YYYY-MM-DD，取 UTC 0 点；空串表示不设到期。
// 1970-01-01 及更早的日期换不成一个正的 unix 秒，拒绝。
func parseExpiryDate(raw string) (sql.NullInt64, *adminError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sql.NullInt64{}, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return sql.NullInt64{}, &adminError{http.StatusBadRequest, codeValidation, "日期格式无效（需 YYYY-MM-DD）"}
	}
	if t.Unix() <= 0 {
		return sql.NullInt64{}, &adminError{http.StatusBadRequest, codeValidation, "到期日无效"}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}, nil
}

// parseExpiryUnix 解析 /api/v1 送来的到期时刻：unix 秒，0 表示不设到期（这个接口的
// 约定）。负数不是一个到期时刻，拒绝。
func parseExpiryUnix(v int64) (sql.NullInt64, *adminError) {
	if v < 0 {
		return sql.NullInt64{}, &adminError{http.StatusBadRequest, codeValidation, "到期时间无效"}
	}
	return sql.NullInt64{Int64: v, Valid: v > 0}, nil
}

// unixOrNil 把可空的 unix 时刻换成对外的写法：一个数，或 null。
func unixOrNil(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// setUserExpiry 设置用户的到期时间（at 由 parseExpiryDate / parseExpiryUnix 给出，
// NULL 表示永不到期），并重新下发他的节点，让变化立即到节点上。
func (s *Server) setUserExpiry(adminID, userID int64, at sql.NullInt64) *adminError {
	if err := db.UpdateRow(s.DB, `UPDATE users SET expires_at=? WHERE id=?`, at, userID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, "用户不存在")
	}
	db.WriteAudit(s.DB, adminID, "user.set_expiry", strconv.FormatInt(userID, 10), strconv.FormatInt(at.Int64, 10))
	// 到期时间已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	if err := s.redispatchUser(userID, "设置到期时间"); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "到期时间已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	return nil
}

// grantUserNodes 确保用户在每个节点上都有给定上限的授权（经 db.GrantNodes 建或改），
// 要么全部写入、要么一条都不写。maxForwards<=0 取默认值。
//
// 每条授权的两端都先查一遍，“没有这个用户”“没有这个节点”由这两次查找回答。
// user_nodes 上的外键同样会拒绝这样的写入，但这里不去读一次失败的写入的原因：它
// 会被当作内部错误原样上报。
//
// db.GrantNode 对已存在的授权是覆盖写，配额也在其中，所以这里可能改动已有授权的
// 配额：写完要重新下发经过这些授权的规则。
func (s *Server) grantUserNodes(adminID, userID int64, nodeIDs []int64, maxForwards int, quotaBytes int64) *adminError {
	if maxForwards <= 0 {
		maxForwards = defaultGrantMaxForwards
	}
	if _, err := db.GetUserByID(s.DB, userID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, "用户不存在")
	}
	for _, nid := range nodeIDs {
		if _, err := db.GetNode(s.DB, nid); err != nil {
			return adminLookupErr(err, http.StatusNotFound, codeNotFound, fmt.Sprintf("节点 #%d 不存在", nid))
		}
	}
	if err := db.GrantNodes(s.DB, userID, nodeIDs, maxForwards, quotaBytes); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	for _, nid := range nodeIDs {
		db.WriteAudit(s.DB, adminID, "user.grant_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nid, 10))
	}
	// 授权已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	for _, nid := range nodeIDs {
		if err := s.redispatchUserGrant(userID, nid, "授权节点"); err != nil {
			return &adminError{http.StatusInternalServerError, codeInternal, "授权已保存，但没能确定需要重新下发的节点: " + err.Error()}
		}
	}
	return nil
}

// revokeUserNode removes the grant AND tears down the rules that ran behind it,
// re-dispatching the affected nodes so forwarding (and billing) stops. Returns
// the number of physical nodes re-pushed.
func (s *Server) revokeUserNode(adminID, userID, nodeID int64) (int, *adminError) {
	affected, err := db.RevokeGrants(s.DB, []db.GrantKey{{UserID: userID, NodeID: nodeID}})
	if err != nil {
		return 0, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	s.dispatchNodes("规则变更", affected)
	db.WriteAudit(s.DB, adminID, "user.revoke_node", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	return len(affected), nil
}

// setPerNodeQuota sets a grant's absolute traffic quota (bytes) on user_nodes
// and re-dispatches the nodes carrying rules that run through the grant, since
// the grant's quota is a condition of the push predicate.
func (s *Server) setPerNodeQuota(adminID, userID, nodeID, bytes int64) *adminError {
	if bytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "字节数无效"}
	}
	if err := db.UpdateRow(s.DB, `UPDATE user_nodes SET traffic_quota_bytes=? WHERE user_id=? AND node_id=?`,
		bytes, userID, nodeID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, msgNoGrant)
	}
	db.WriteAudit(s.DB, adminID, "user.set_node_quota", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d bytes=%d", nodeID, bytes))
	// 配额已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	if err := s.redispatchUserGrant(userID, nodeID, "设置授权流量配额"); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "配额已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	return nil
}

// setPerNodeRateLimit sets a grant's shared rate cap (MB/s, 0 = unlimited) and
// re-dispatches every node carrying the grant's rule hops so shaping updates.
func (s *Server) setPerNodeRateLimit(adminID, userID, nodeID, mbytes int64) *adminError {
	if mbytes < 0 {
		return &adminError{http.StatusBadRequest, codeValidation, "限速不能为负"}
	}
	if err := db.UpdateRow(s.DB, `UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`,
		mbytes, userID, nodeID); err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, msgNoGrant)
	}
	db.WriteAudit(s.DB, adminID, "user.set_node_rate_limit", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d mbytes=%d", nodeID, mbytes))
	// 限速已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	if err := s.redispatchUserGrant(userID, nodeID, "设置授权限速"); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "限速已保存，但没能确定需要重新下发的节点: " + err.Error()}
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
			return 0, 0, nil, adminLookupErr(err, http.StatusBadRequest, codeValidation, fmt.Sprintf("用户 #%d 不存在", uid))
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
	var affectedErr error
	for i, w := range writes {
		if !written[i] {
			kept++
			continue
		}
		granted++
		db.WriteAudit(s.DB, adminID, "user.grant_node", strconv.FormatInt(w.UserID, 10), strconv.FormatInt(w.NodeID, 10))
		ns, err := db.RulesAffectedByNode(s.DB, w.UserID, w.NodeID)
		if err != nil {
			// The grants are stored. The nodes found for the other pairs are
			// still re-pushed below; this pair's are reported as not found.
			if affectedErr == nil {
				affectedErr = err
			}
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
	s.dispatchNodes("规则变更", nodeIDs)
	if affectedErr != nil {
		return granted, kept, skipped, &adminError{http.StatusInternalServerError, codeInternal, "授权已保存，但没能确定需要重新下发的节点: " + affectedErr.Error()}
	}
	return granted, kept, skipped, nil
}

// errNotAMachine 是对组合发起只有机器才能执行的操作（下发规则、推送升级）时的错误。
var errNotAMachine = errors.New("组合节点不跑 agent：它承载的规则由成员节点执行，请对成员节点操作")

// requireMachine 是“这个操作只对机器有意义”的判定：下发与升级都要经过一个 agent
// 连接，组合没有自己的连接，对它下发或升级只会把失败记在组合行上。
func requireMachine(n *db.Node) error {
	if n.NodeType == "composite" {
		return errNotAMachine
	}
	return nil
}

// resyncNode 把一台机器此刻应有的规则集重新下发给它，SPA 与 /api/v1 共用。下发失败
// 就是这次请求的错误，按“是谁的失败”回答（pushErrAnswer）。
func (s *Server) resyncNode(id int64) *adminError {
	n, err := db.GetNode(s.DB, id)
	if err != nil {
		return adminLookupErr(err, http.StatusNotFound, codeNotFound, "节点不存在")
	}
	if err := requireMachine(n); err != nil {
		return &adminError{http.StatusBadRequest, codeValidation, err.Error()}
	}
	if err := s.dispatchToNode(id); err != nil {
		status, code := pushErrAnswer(err)
		return &adminError{status, code, err.Error()}
	}
	return nil
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
		if requireMachine(n) != nil {
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
	if err != nil {
		return landingSave{}, adminLookupErr(err, http.StatusNotFound, codeNotFound, "用户不存在")
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
	nodes, err := s.resolveLandingExits(target, true)
	if err != nil {
		saved.SubError = err.Error()
		return saved, nil
	}
	// Bringing the ledger up to date is part of what saving a source means
	// here, so a sync that fails is reported — as a failure that says the
	// source itself is stored.
	if err := s.syncLandingExits(target, nodes); err != nil {
		return landingSave{}, &adminError{http.StatusInternalServerError, codeInternal, "来源已保存，但没能刷新落地出口: " + err.Error()}
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
	// 状态已经落库，要经这次重新下发才到节点上。调用是声明式的，重复一次就是重试。
	if err := s.redispatchUser(userID, "启用/停用账号"); err != nil {
		return &adminError{http.StatusInternalServerError, codeInternal, "状态已保存，但没能确定需要重新下发的节点: " + err.Error()}
	}
	return nil
}
