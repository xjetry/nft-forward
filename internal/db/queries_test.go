package db

import "testing"

func TestNodeLastWarningRoundTrip(t *testing.T) {
	d := openTestDB(t)

	n, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}

	if err := MarkNodeApplied(d, n.ID, "2 条规则的目标无法解析：端口 8080 → 4212"); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.LastWarning == "" {
		t.Fatal("last_warning should be set after MarkNodeApplied with warning")
	}
	if got.LastError.Valid {
		t.Fatal("last_error should be cleared on apply")
	}

	// 干净成功清 warning
	if err := MarkNodeApplied(d, n.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("last_warning should be cleared, got %q", got.LastWarning)
	}

	// 下发硬失败：置 error、清 warning
	_ = MarkNodeApplied(d, n.ID, "some warning")
	if err := MarkNodeDispatchError(d, n.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("dispatch error should clear warning, got %q", got.LastWarning)
	}
	if !got.LastError.Valid || got.LastError.String != "boom" {
		t.Fatalf("last_error = %+v, want boom", got.LastError)
	}
}

func TestNodeRelayHostDeclaredRoundTrip(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "n1", "https://p", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHostDeclared || n.RelayHostV6Declared {
		t.Fatalf("new node should start undeclared, got %+v", n)
	}

	if err := UpdateNodeRelayHost(d, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostDeclared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostV6Declared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be true after SetNodeRelayHostDeclared(true)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should be true after SetNodeRelayHostV6Declared(true)")
	}

	if err := SetNodeRelayHostDeclared(d, n.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be false after SetNodeRelayHostDeclared(false)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should remain true (only the v4 flag was cleared)")
	}
}

func TestNodeRolesRoundTrip(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "hk-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n.Roles != NodeRoleEntry {
		t.Fatalf("default roles want %d, got %d", NodeRoleEntry, n.Roles)
	}
	if err := UpdateNodeRoles(d, n.ID, NodeRoleEntry|NodeRoleVia); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.Roles != NodeRoleEntry|NodeRoleVia {
		t.Fatalf("roles want %d, got %d", NodeRoleEntry|NodeRoleVia, got.Roles)
	}
}

func TestListNodesForUserCarriesRoles(t *testing.T) {
	d := openTestDB(t)
	uid, _ := CreateUser(d, "testuser", "hash", "user")
	n, _ := CreateNode(d, "hk-1", "", "")
	_ = UpdateNodeRoles(d, n.ID, NodeRoleVia)
	if err := GrantNode(d, uid, n.ID, 5, 0); err != nil {
		t.Fatal(err)
	}
	nodes, _, err := ListNodesForUser(d, uid)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("want 1 node, err=%v n=%d", err, len(nodes))
	}
	if nodes[0].Roles != NodeRoleVia {
		t.Fatalf("roles want %d, got %d", NodeRoleVia, nodes[0].Roles)
	}
}

func TestRuleViaRoundTripAndHopProvenance(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "entry", "", "")
	b, _ := CreateNode(d, "mid", "", "")
	_ = UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = UpdateNodeRelayHost(d, b.ID, "2.2.2.2")

	r := &Rule{NodeID: a.ID, Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443,
		ViaNodeIDs: []int64{b.ID}}
	tx, _ := d.Begin()
	id, err := CreateRule(tx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	hops := []HopInput{
		{NodeID: a.ID, Mode: "userspace", ViaNodeID: a.ID},
		{NodeID: b.ID, Mode: "kernel", ViaNodeID: b.ID},
	}
	if _, _, _, err := RegenerateRule(tx, r, hops, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, _ := GetRule(d, id)
	if len(got.ViaNodeIDs) != 1 || got.ViaNodeIDs[0] != b.ID {
		t.Fatalf("via round-trip failed: %+v", got.ViaNodeIDs)
	}
	rh, _ := ListRuleHops(d, id)
	if len(rh) != 2 || rh[0].ViaNodeID != a.ID || rh[1].ViaNodeID != b.ID {
		t.Fatalf("hop provenance wrong: %+v", rh)
	}
}
