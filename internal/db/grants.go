package db

import "database/sql"

type NodeHop struct {
	NodeID    int64  `json:"node_id"`
	Position  int    `json:"position"`
	HopNodeID int64  `json:"hop_node_id"`
	Mode      string `json:"mode"`
}

type UserNode struct {
	UserID      int64 `json:"user_id"`
	NodeID      int64 `json:"node_id"`
	MaxForwards int   `json:"max_forwards"`
	GrantedAt   int64 `json:"granted_at"`
}

// CreateNodeHops inserts ordered hops for a composite node.
func CreateNodeHops(d DBTX, nodeID int64, hops []NodeHop) error {
	for _, h := range hops {
		if _, err := d.Exec(`INSERT INTO node_hops(node_id, position, hop_node_id, mode) VALUES (?,?,?,?)`,
			nodeID, h.Position, h.HopNodeID, h.Mode); err != nil {
			return err
		}
	}
	return nil
}

func scanNodeHop(r rowScanner) (*NodeHop, error) {
	h := &NodeHop{}
	if err := r.Scan(&h.NodeID, &h.Position, &h.HopNodeID, &h.Mode); err != nil {
		return nil, err
	}
	return h, nil
}

func ListNodeHops(d *sql.DB, nodeID int64) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode FROM node_hops WHERE node_id=? ORDER BY position`, scanNodeHop, nodeID)
}

func DeleteNodeHops(d DBTX, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM node_hops WHERE node_id=?`, nodeID)
	return err
}

// GrantNode grants a user access to a node with a max_forwards limit. If the
// grant already exists, only max_forwards is updated (idempotent).
func GrantNode(d *sql.DB, userID, nodeID int64, maxForwards int) error {
	_, err := d.Exec(`INSERT INTO user_nodes(user_id, node_id, max_forwards, granted_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id, node_id) DO UPDATE SET max_forwards=excluded.max_forwards`,
		userID, nodeID, maxForwards, now())
	return err
}

func RevokeNode(d *sql.DB, userID, nodeID int64) error {
	_, err := d.Exec(`DELETE FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

func GetNodeGrant(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	row := d.QueryRow(`SELECT user_id, node_id, max_forwards, granted_at FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	g := &UserNode{}
	if err := row.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

func scanUserNode(r rowScanner) (*UserNode, error) {
	g := &UserNode{}
	if err := r.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}

// ListNodesForUser returns the nodes a user has been granted access to, along
// with the grant metadata. Both slices are aligned by index.
func ListNodesForUser(d *sql.DB, userID int64) ([]*Node, []*UserNode, error) {
	rows, err := d.Query(`
		SELECT `+nodeCols+`,
		       g.max_forwards, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []*Node
	var grants []*UserNode
	for rows.Next() {
		n := &Node{}
		g := &UserNode{UserID: userID}
		var disabled int
		var localMigratedAt, lastSeen sql.NullInt64
		var agentVersion sql.NullString
		var ownerID sql.NullInt64
		if err := rows.Scan(
			&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
			&n.RelayHost, &n.Online, &agentVersion,
			&lastSeen, &n.LastApplyAt, &n.LastError,
			&disabled, &localMigratedAt, &n.CreatedAt,
			&g.MaxForwards, &g.GrantedAt,
		); err != nil {
			return nil, nil, err
		}
		n.Disabled = disabled == 1
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

func ListAllGrants(d *sql.DB) ([]*UserNode, error) {
	return queryAll(d, `SELECT user_id, node_id, max_forwards, granted_at FROM user_nodes`, scanUserNode)
}

// CheckNodeAccess validates that a user has access to a node. Node owners
// automatically bypass the grant check.
func CheckNodeAccess(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	var ownerID sql.NullInt64
	_ = d.QueryRow(`SELECT owner_id FROM nodes WHERE id = ?`, nodeID).Scan(&ownerID)
	if ownerID.Valid && ownerID.Int64 == userID {
		return &UserNode{UserID: userID, NodeID: nodeID, MaxForwards: 9999}, nil
	}
	return GetNodeGrant(d, userID, nodeID)
}
