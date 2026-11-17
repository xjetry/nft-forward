package db

import "testing"

// columnCount 报告 table 表里名叫 name 的列有几个（0 或 1）。
func columnCount(t *testing.T, d DBTX, table, name string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 账号与授权上的条数上限数的是规则，列叫 max_rules。它原来叫 max_forwards——规则还叫
// “转发”时的名字。迁移只改列名：两张表上每一行的值原样留下，旧名字不再存在。
func TestMigrationRenamesTheRuleCapColumns(t *testing.T) {
	const migration = "0055_rule_cap_columns.sql"
	d := openTestDB(t)
	tables := []string{"users", "user_nodes"}
	for _, table := range tables {
		if n := columnCount(t, d, table, "max_rules"); n != 1 {
			t.Fatalf("跑完全部迁移的库里 %s 应有 max_rules 列，实际 %d 个", table, n)
		}
		if n := columnCount(t, d, table, "max_forwards"); n != 0 {
			t.Fatalf("跑完全部迁移的库里 %s 不应再有 max_forwards 列，实际 %d 个", table, n)
		}
	}

	uid := createTestUser(t, d)
	node := createTestNode(t, d, "n")
	mustExec(t, d, `UPDATE users SET max_rules=37 WHERE id=?`, uid)
	if err := GrantNode(d, uid, node, 5, 0); err != nil {
		t.Fatal(err)
	}
	caps := func(column string) (account, grant int) {
		t.Helper()
		if err := d.QueryRow(`SELECT `+column+` FROM users WHERE id=?`, uid).Scan(&account); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT `+column+` FROM user_nodes WHERE user_id=? AND node_id=?`, uid, node).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		return account, grant
	}

	// 回到迁移之前的形状，再重放这一个迁移。
	for _, table := range tables {
		mustExec(t, d, `ALTER TABLE `+table+` RENAME COLUMN max_rules TO max_forwards`)
	}
	if account, grant := caps("max_forwards"); account != 37 || grant != 5 {
		t.Fatalf("前置条件：改回旧列名后账号上限应是 37、授权上限应是 5，实际 %d、%d", account, grant)
	}
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

	for _, table := range tables {
		if n := columnCount(t, d, table, "max_forwards"); n != 0 {
			t.Errorf("重放迁移后 %s 不应再有 max_forwards 列，实际 %d 个", table, n)
		}
		if n := columnCount(t, d, table, "max_rules"); n != 1 {
			t.Errorf("重放迁移后 %s 应有 max_rules 列，实际 %d 个", table, n)
		}
	}
	if t.Failed() {
		return
	}
	if account, grant := caps("max_rules"); account != 37 || grant != 5 {
		t.Errorf("迁移只改列名，账号上限应仍是 37、授权上限应仍是 5，实际 %d、%d", account, grant)
	}

	// 读写这两列的代码用的是新名字：经它们写入、再读出，值对得上。
	if err := GrantNode(d, uid, node, 8, 0); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, node)
	if err != nil || g == nil || g.MaxRules != 8 {
		t.Errorf("GrantNode 写入的授权上限应能由 GetNodeGrant 读出 8，实际 %+v，err=%v", g, err)
	}
	u, err := GetUserByID(d, uid)
	if err != nil || u == nil || u.MaxRules != 37 {
		t.Errorf("GetUserByID 应读出账号上限 37，实际 %+v，err=%v", u, err)
	}
}
