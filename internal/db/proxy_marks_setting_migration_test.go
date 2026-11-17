package db

import "testing"

// 落地代理的标记表存在 settings 的一行里，键叫 proxy_marks。它原来叫 node_roles。迁移
// 只改键：整张表的 JSON 原样留下（包括按位存之前写下的字符串值），旧键不再存在。
func TestMigrationRenamesTheProxyMarksSetting(t *testing.T) {
	const migration = "0057_proxy_marks_setting.sql"
	const table = `{"vless:a:443":3,"ss:old:1":"landing"}`
	d := openTestDB(t)

	// 回到迁移之前的形状：标记表在旧键下。
	if err := SetSetting(d, "node_roles", table); err != nil {
		t.Fatal(err)
	}
	// 旁边一行别的设置，迁移不该碰它。
	if err := SetSetting(d, "panel_name", "面板"); err != nil {
		t.Fatal(err)
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

	if got, err := GetSetting(d, "proxy_marks"); err != nil || got != table {
		t.Errorf("迁移后标记表应在 proxy_marks 下、内容不变，实际 %q，err=%v", got, err)
	}
	var old int
	if err := d.QueryRow(`SELECT COUNT(*) FROM settings WHERE key='node_roles'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Errorf("迁移后不应再有 node_roles 这一行，实际 %d 行", old)
	}
	if got, err := GetSetting(d, "panel_name"); err != nil || got != "面板" {
		t.Errorf("别的设置不应被动到，panel_name 实际 %q，err=%v", got, err)
	}
}

// 还没有打过标记的库里没有这一行：迁移不新增行，也不报错。
func TestProxyMarksSettingMigrationIsANoOpWithoutTheRow(t *testing.T) {
	const migration = "0057_proxy_marks_setting.sql"
	d := openTestDB(t)
	count := func() int {
		t.Helper()
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM settings WHERE key IN ('node_roles','proxy_marks')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("前置条件：新库里不应有标记表这一行，实际 %d 行", n)
	}
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}
	if err := migrate(d); err != nil {
		t.Fatalf("没有这一行时迁移不应报错：%v", err)
	}
	if n := count(); n != 0 {
		t.Errorf("没有这一行时迁移不应新增行，实际 %d 行", n)
	}
}
