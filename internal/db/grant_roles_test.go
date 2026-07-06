package db

import "testing"

// A new grant inherits (roles = 0) so existing behavior is unchanged.
func TestGrantRolesDefaultInherit(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != 0 {
		t.Fatalf("new grant roles = %d, want 0 (inherit)", g.Roles)
	}
}

// Every user_nodes read path must return the same override — one misaligned
// scan would silently shift the grant columns.
func TestGrantRolesReadAlignment(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)
	if _, err := d.Exec(`UPDATE user_nodes SET roles=1 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != 1 {
		t.Fatalf("GetNodeGrant roles = %d, want 1", g.Roles)
	}

	_, grants, err := ListNodesForUser(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Roles != 1 {
		t.Fatalf("ListNodesForUser roles = %v, want [1]", grants)
	}

	users, err := ListUsersForNode(d, nid)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Roles != 1 {
		t.Fatalf("ListUsersForNode roles = %v, want 1", users)
	}
}

func TestEffectiveNodeRoles(t *testing.T) {
	// grant 0 inherits the node mask
	if got := EffectiveNodeRoles(NodeRoleVia, 0); got != NodeRoleVia {
		t.Fatalf("inherit = %d, want %d", got, NodeRoleVia)
	}
	// override may add a bit the node lacks (via node opened as entry)
	if got := EffectiveNodeRoles(NodeRoleVia, NodeRoleEntry); got != NodeRoleEntry {
		t.Fatalf("override-add = %d, want %d", got, NodeRoleEntry)
	}
	// override may drop a bit the node has (entry+via node narrowed to via)
	if got := EffectiveNodeRoles(NodeRoleEntry|NodeRoleVia, NodeRoleVia); got != NodeRoleVia {
		t.Fatalf("override-narrow = %d, want %d", got, NodeRoleVia)
	}
}

func TestSetGrantRoles(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	if err := SetGrantRoles(d, uid, nid, NodeRoleEntry); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != NodeRoleEntry {
		t.Fatalf("roles = %d, want %d", g.Roles, NodeRoleEntry)
	}
}
