package db

import (
	"database/sql"
	"errors"
	"testing"
)

func markComposite(t *testing.T, d *sql.DB, id int64) {
	t.Helper()
	if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func physicalSettings(t *testing.T, d *sql.DB, id int64) (unidirectional, noDirectExit int) {
	t.Helper()
	if err := d.QueryRow(`SELECT unidirectional, no_direct_exit FROM nodes WHERE id=?`, id).Scan(&unidirectional, &noDirectExit); err != nil {
		t.Fatal(err)
	}
	return
}

// 两个写函数是这两列仅有的写入口，组合节点在这里被拒绝；节点不存在与节点是组合
// 是两种不同的失败，调用方要能分清。
func TestPhysicalSettingWritersRefuseComposite(t *testing.T) {
	d := openTestDB(t)
	phys := createTestNode(t, d, "p")
	comp := createTestNode(t, d, "c")
	markComposite(t, d, comp)

	writers := map[string]func(id int64) error{
		"unidirectional": func(id int64) error { return UpdateNodeUnidirectional(d, id, true) },
		"no_direct_exit": func(id int64) error { return UpdateNodeNoDirectExit(d, id, true) },
	}
	for name, write := range writers {
		if err := write(comp); !errors.Is(err, ErrPhysicalNodeSetting) {
			t.Errorf("%s: 组合节点应返回 ErrPhysicalNodeSetting，实际 %v", name, err)
		}
		if err := write(phys); err != nil {
			t.Errorf("%s: 物理节点应成功，实际 %v", name, err)
		}
		if err := write(999999); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s: 不存在的节点应返回 sql.ErrNoRows，实际 %v", name, err)
		}
	}
	if u, n := physicalSettings(t, d, comp); u != 0 || n != 0 {
		t.Errorf("组合节点上不应写入：unidirectional=%d no_direct_exit=%d", u, n)
	}
	if u, n := physicalSettings(t, d, phys); u != 1 || n != 1 {
		t.Errorf("物理节点上应写入：unidirectional=%d no_direct_exit=%d", u, n)
	}
}

// 写入口开始拒绝组合节点之前，页面允许在组合节点上打开这两个开关。迁移把这些从未
// 生效过的值清掉，物理节点的设置不动。
func TestMigrationClearsPhysicalSettingsOnComposites(t *testing.T) {
	const migration = "0044_composite_drops_physical_settings.sql"
	d := openTestDB(t)
	phys := createTestNode(t, d, "p")
	comp := createTestNode(t, d, "c")
	markComposite(t, d, comp)
	if _, err := d.Exec(`UPDATE nodes SET unidirectional=1, no_direct_exit=1 WHERE id IN (?,?)`, phys, comp); err != nil {
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

	if u, n := physicalSettings(t, d, comp); u != 0 || n != 0 {
		t.Errorf("迁移后组合节点上应清零：unidirectional=%d no_direct_exit=%d", u, n)
	}
	if u, n := physicalSettings(t, d, phys); u != 1 || n != 1 {
		t.Errorf("迁移不应动物理节点：unidirectional=%d no_direct_exit=%d", u, n)
	}
}
