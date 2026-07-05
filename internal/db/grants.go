package db

import "database/sql"

type NodeHop struct {
	NodeID            int64   `json:"node_id"`
	Position          int     `json:"position"`
	HopNodeID         int64   `json:"hop_node_id"`
	Mode              string  `json:"mode"`
	TrafficMultiplier float64 `json:"traffic_multiplier"`
}

type UserNode struct {
	UserID      int64 `json:"user_id"`
	NodeID      int64 `json:"node_id"`
	MaxForwards int   `json:"max_forwards"`
	// TrafficQuotaBytes is the per-grant quota override; 0 means fall back to the global user quota.
	TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64 `json:"traffic_used_bytes"`
	// RateLimitMBytes is the per-grant shared rate limit in MB/s; all of the
	// user's rules on this node share one bucket. 0 = unlimited (unlike the
	// quota, there is no user-level fallback).
	RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	// Roles overrides the node's role mask for this user: 0 = inherit the
	// node's roles, non-zero = use this mask (entry/via combination). Lets a
	// node be an entry for one grantee while staying via-only for others,
	// independent of the node's global roles.
	Roles     int64 `json:"roles"`
	GrantedAt int64 `json:"granted_at"`
}

// CreateNodeHops inserts ordered hops for a composite node.
func CreateNodeHops(d DBTX, nodeID int64, hops []NodeHop) error {
	for _, h := range hops {
		mult := h.TrafficMultiplier
		if mult < 0 {
			mult = 0
		}
		if _, err := d.Exec(`INSERT INTO node_hops(node_id, position, hop_node_id, mode, traffic_multiplier) VALUES (?,?,?,?,?)`,
			nodeID, h.Position, h.HopNodeID, h.Mode, mult); err != nil {
			return err
		}
	}
	return nil
}

func scanNodeHop(r rowScanner) (*NodeHop, error) {
	h := &NodeHop{}
	if err := r.Scan(&h.NodeID, &h.Position, &h.HopNodeID, &h.Mode, &h.TrafficMultiplier); err != nil {
		return nil, err
	}
	return h, nil
}

func ListNodeHops(d *sql.DB, nodeID int64) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode, traffic_multiplier FROM node_hops WHERE node_id=? ORDER BY position`, scanNodeHop, nodeID)
}

// ListAllNodeHops returns every composite node's hops in one query, ordered so
// callers can group by node_id without an N+1 fan-out.
func ListAllNodeHops(d *sql.DB) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode, traffic_multiplier FROM node_hops ORDER BY node_id, position`, scanNodeHop)
}

func DeleteNodeHops(d DBTX, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM node_hops WHERE node_id=?`, nodeID)
	return err
}

// GrantNode grants a user access to a node with a max_forwards limit. If the
// grant already exists, only max_forwards is updated (idempotent).
func GrantNode(d *sql.DB, userID, nodeID int64, maxForwards int, trafficQuotaBytes int64) error {
	_, err := d.Exec(`INSERT INTO user_nodes(user_id, node_id, max_forwards, traffic_quota_bytes, granted_at) VALUES (?,?,?,?,?)
		ON CONFLICT(user_id, node_id) DO UPDATE SET max_forwards=excluded.max_forwards, traffic_quota_bytes=excluded.traffic_quota_bytes`,
		userID, nodeID, maxForwards, trafficQuotaBytes, now())
	return err
}

func RevokeNode(d *sql.DB, userID, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

func GetNodeGrant(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	row := d.QueryRow(`SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, roles, granted_at FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	g := &UserNode{}
	if err := row.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

func scanUserNode(r rowScanner) (*UserNode, error) {
	g := &UserNode{}
	if err := r.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

// ListNodesForUser returns the nodes a user has been granted access to, along
// with the grant metadata. Both slices are aligned by index.
func ListNodesForUser(d *sql.DB, userID int64) ([]*Node, []*UserNode, error) {
	rows, err := d.Query(`
		SELECT `+nodeColsQualified+`,
		       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.roles, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []*Node
	var grants []*UserNode
	for rows.Next() {
		n := &Node{}
		g := &UserNode{UserID: userID}
		var disabled, unidirectional, relayHostDeclared, relayHostV6Declared, noDirectExit int
		var localMigratedAt, lastSeen sql.NullInt64
		var agentVersion sql.NullString
		var ownerID sql.NullInt64
		var luVersion, luStatus, luError sql.NullString
		if err := rows.Scan(
			&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
			&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
			&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
			&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
			&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
			&n.SortOrder, &n.RateMultiplier, &unidirectional,
			&relayHostDeclared, &relayHostV6Declared, &n.Roles, &noDirectExit,
			&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt,
		); err != nil {
			return nil, nil, err
		}
		n.LastUpgradeVersion = luVersion.String
		n.LastUpgradeStatus = luStatus.String
		n.LastUpgradeError = luError.String
		n.Disabled = disabled == 1
		n.Unidirectional = unidirectional == 1
		n.NoDirectExit = noDirectExit == 1
		n.RelayHostDeclared = relayHostDeclared == 1
		n.RelayHostV6Declared = relayHostV6Declared == 1
		if ownerID.Valid {
			v := ownerID.Int64
			n.OwnerID = &v
		}
		if localMigratedAt.Valid {
			v := localMigratedAt.Int64
			n.LocalMigratedAt = &v
		}
		if lastSeen.Valid {
			v := lastSeen.Int64
			n.LastSeen = &v
		}
		if agentVersion.Valid {
			n.AgentVersion = agentVersion.String
		}
		g.NodeID = n.ID
		nodes = append(nodes, n)
		grants = append(grants, g)
	}
	return nodes, grants, rows.Err()
}

// CountRulesForUserNode counts rules owned by a user on a specific node.
func CountRulesForUserNode(d *sql.DB, userID, nodeID int64) (int, error) {
	return count(d, `SELECT COUNT(*) FROM rules WHERE owner_id=? AND node_id=?`, userID, nodeID)
}

// ListUsersForNode returns grants for a specific node with user info.
func ListUsersForNode(d *sql.DB, nodeID int64) ([]struct {
	UserID            int64  `json:"user_id"`
	Username          string `json:"username"`
	MaxForwards       int    `json:"max_forwards"`
	TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
	RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
	Roles             int64  `json:"roles"`
	GrantedAt         int64  `json:"granted_at"`
}, error) {
	rows, err := d.Query(`SELECT g.user_id, u.username, g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.roles, g.granted_at
		FROM user_nodes g JOIN users u ON u.id = g.user_id
		WHERE g.node_id = ? ORDER BY g.granted_at`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		UserID            int64  `json:"user_id"`
		Username          string `json:"username"`
		MaxForwards       int    `json:"max_forwards"`
		TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
		TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
		RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
		Roles             int64  `json:"roles"`
		GrantedAt         int64  `json:"granted_at"`
	}
	for rows.Next() {
		var r struct {
			UserID            int64  `json:"user_id"`
			Username          string `json:"username"`
			MaxForwards       int    `json:"max_forwards"`
			TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
			TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
			RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
			Roles             int64  `json:"roles"`
			GrantedAt         int64  `json:"granted_at"`
		}
		if err := rows.Scan(&r.UserID, &r.Username, &r.MaxForwards, &r.TrafficQuotaBytes, &r.TrafficUsedBytes, &r.RateLimitMBytes, &r.Roles, &r.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ListAllGrants(d *sql.DB) ([]*UserNode, error) {
	return queryAll(d, `SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, roles, granted_at FROM user_nodes`, scanUserNode)
}

// CheckNodeAccess validates that a user has access to a node. Node owners
// automatically bypass the grant check.
func CheckNodeAccess(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	var ownerID sql.NullInt64
	_ = d.QueryRow(`SELECT owner_id FROM nodes WHERE id = ?`, nodeID).Scan(&ownerID)
	if ownerID.Valid && ownerID.Int64 == userID {
		return &UserNode{UserID: userID, NodeID: nodeID, MaxForwards: 9999, TrafficQuotaBytes: 0, TrafficUsedBytes: 0}, nil
	}
	return GetNodeGrant(d, userID, nodeID)
}

// GrantShape carries the shaping identity and limit of one rate-limited grant.
// GrantID is the user_nodes rowid: stable across upserts, so agents can use it
// as the connmark-backed shaping group id. user_nodes has a composite primary
// key (no INTEGER PRIMARY KEY), so it is an ordinary rowid table — after a
// revoke deletes the highest-numbered row, SQLite may reuse that rowid for
// the next insert. A revoke+regrant is therefore not guaranteed to produce a
// new group id: on that rare reuse, a connection that outlives the revoke
// keeps being classified into the tc class the reused id now names, spending
// bandwidth against the new grant's bucket until the connection ends, after
// which the misclassification self-heals.
type GrantShape struct {
	GrantID         int64
	RateLimitMBytes int64
}

// GrantShapes returns every rate-limited grant keyed by {user_id, node_id}.
// Unlimited grants (rate 0) are omitted so callers can treat absence as "no
// shaping".
func GrantShapes(d *sql.DB) (map[[2]int64]GrantShape, error) {
	rows, err := d.Query(`SELECT rowid, user_id, node_id, rate_limit_mbytes FROM user_nodes WHERE rate_limit_mbytes > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]GrantShape{}
	for rows.Next() {
		var gs GrantShape
		var uid, nid int64
		if err := rows.Scan(&gs.GrantID, &uid, &nid, &gs.RateLimitMBytes); err != nil {
			return nil, err
		}
		out[[2]int64{uid, nid}] = gs
	}
	return out, rows.Err()
}
