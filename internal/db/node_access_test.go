package db

import (
	"database/sql"
	"testing"
)

// “用户能否使用这个节点”只有一个答案：管理员能，其余人要有授权；只是节点的属主
// 不算。
func TestNodeAccess(t *testing.T) {
	d := openTestDB(t)
	adminID, _ := CreateUser(d, "root", "hash", "admin")
	admin, _ := GetUserByID(d, adminID)
	user, _ := GetUserByID(d, createTestUser(t, d))
	granted := createTestNode(t, d, "granted")
	owned := createTestNode(t, d, "owned")
	other := createTestNode(t, d, "other")
	if err := GrantNode(d, user.ID, granted, 7, 0); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeOwner(d, owned, &user.ID); err != nil {
		t.Fatal(err)
	}

	for _, nid := range []int64{granted, owned, other} {
		g, ok, err := NodeAccess(d, admin, nid)
		if err != nil || !ok || g != nil {
			t.Errorf("管理员能用任何节点且没有授权行：节点 %d 得到 grant=%v ok=%v err=%v", nid, g, ok, err)
		}
	}
	g, ok, err := NodeAccess(d, user, granted)
	if err != nil || !ok || g == nil || g.MaxForwards != 7 {
		t.Errorf("有授权：应放行并带回那条授权，得到 grant=%+v ok=%v err=%v", g, ok, err)
	}
	if _, ok, err := NodeAccess(d, user, owned); err != nil || ok {
		t.Errorf("只是属主、没有授权：不应放行，得到 ok=%v err=%v", ok, err)
	}
	if _, ok, err := NodeAccess(d, user, other); err != nil || ok {
		t.Errorf("没有授权：不应放行，得到 ok=%v err=%v", ok, err)
	}

	// 读库失败不是放行。
	d.Close()
	if _, ok, err := NodeAccess(d, user, granted); err == nil || ok {
		t.Errorf("读库失败应报错且不放行，得到 ok=%v err=%v", ok, err)
	}
}

// 迁移 0047 把“属主隐含访问权”的旧口径落成数据：非管理员属主得到一条授权，没有属主
// 的机器归第一个管理员，空的规则名被补上。在一个停在迁移之前形状的库上重放它。
func TestMigrationTurnsImpliedOwnerAccessIntoGrants(t *testing.T) {
	const migration = "0047_node_owner_is_identity_only.sql"
	d := openTestDB(t)
	adminID, _ := CreateUser(d, "root", "hash", "admin")
	uid := createTestUser(t, d)
	ownedBare := createTestNode(t, d, "owned-bare")
	ownedGranted := createTestNode(t, d, "owned-granted")
	adminOwned := createTestNode(t, d, "admin-owned")
	ownerless := createTestNode(t, d, "ownerless")
	composite := createTestNode(t, d, "composite")
	mustExec(t, d, `UPDATE nodes SET node_type='composite' WHERE id=?`, composite)
	for _, nid := range []int64{ownedBare, ownedGranted} {
		if err := UpdateNodeOwner(d, nid, &uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := UpdateNodeOwner(d, adminOwned, &adminID); err != nil {
		t.Fatal(err)
	}
	if err := GrantNode(d, uid, ownedGranted, 3, 0); err != nil {
		t.Fatal(err)
	}
	rule := func(name, comment, host string, port int) int64 {
		res, err := d.Exec(`INSERT INTO rules(node_id, owner_id, name, proto, exit_host, exit_port, comment, created_at) VALUES (?,?,?,?,?,?,?,0)`,
			ownedBare, uid, name, "tcp", host, port, comment)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	named := rule("kept", "备注", "8.8.8.8", 443)
	byComment := rule("", " 解析 ", "8.8.8.8", 53)
	byV4 := rule("  ", "", "8.8.4.4", 80)
	byV6 := rule("", "", "2001:db8::1", 443)

	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}
	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	maxForwards := func(nodeID int64) (int, bool) {
		var n int
		err := d.QueryRow(`SELECT max_forwards FROM user_nodes WHERE user_id=? AND node_id=?`, uid, nodeID).Scan(&n)
		if err == sql.ErrNoRows {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return n, true
	}
	if n, ok := maxForwards(ownedBare); !ok || n != 9999 {
		t.Errorf("非管理员属主、原先没有授权：应得到一条授权（条数上限 9999），实际 %d 存在=%v", n, ok)
	}
	if n, ok := maxForwards(ownedGranted); !ok || n != 3 {
		t.Errorf("已有的授权应原样保留（条数上限 3），实际 %d 存在=%v", n, ok)
	}
	var grants int
	if err := d.QueryRow(`SELECT COUNT(*) FROM user_nodes`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 2 {
		t.Errorf("只应补出一条授权（管理员属主不需要），授权表应共 2 行，实际 %d", grants)
	}

	owner := func(nodeID int64) sql.NullInt64 {
		var o sql.NullInt64
		if err := d.QueryRow(`SELECT owner_id FROM nodes WHERE id=?`, nodeID).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := owner(ownerless); !o.Valid || o.Int64 != adminID {
		t.Errorf("没有属主的机器应归第一个管理员 %d，实际 %+v", adminID, o)
	}
	if o := owner(composite); o.Valid {
		t.Errorf("组合不是机器，不应有属主，实际 %+v", o)
	}
	if o := owner(ownedBare); !o.Valid || o.Int64 != uid {
		t.Errorf("已有属主的机器不应被改，实际 %+v", o)
	}

	want := map[int64]string{named: "kept", byComment: "解析", byV4: "8.8.4.4:80", byV6: "[2001:db8::1]:443"}
	for id, name := range want {
		rl, err := GetRule(d, id)
		if err != nil {
			t.Fatal(err)
		}
		if rl.Name != name {
			t.Errorf("规则 %d 的名字应为 %q，实际 %q", id, name, rl.Name)
		}
	}
}
