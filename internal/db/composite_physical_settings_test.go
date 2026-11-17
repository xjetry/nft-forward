package db

import (
	"database/sql"
	"testing"
)

// createTestComposite 经 CreateCompositeNode 建一个组合，members 是成员的节点 id，按
// 次序、转发模式取默认值。不给成员时建出一个没有成员的组合，供需要自己写成员的测试用。
func createTestComposite(t *testing.T, d *sql.DB, name string, members ...int64) *Node {
	t.Helper()
	hops := make([]NodeHop, len(members))
	for i, m := range members {
		hops[i] = NodeHop{HopNodeID: m}
	}
	n, err := CreateCompositeNode(d, name, LineSpec{}, hops)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func physicalSettings(t *testing.T, d *sql.DB, id int64) (unidirectional, noDirectExit int) {
	t.Helper()
	if err := d.QueryRow(`SELECT unidirectional, no_direct_exit FROM nodes WHERE id=?`, id).Scan(&unidirectional, &noDirectExit); err != nil {
		t.Fatal(err)
	}
	return
}

// 写入口开始拒绝组合节点之前，页面允许在组合节点上打开这两个开关。迁移把这些从未
// 生效过的值清掉，物理节点的设置不动。
func TestMigrationClearsPhysicalSettingsOnComposites(t *testing.T) {
	const migration = "0044_composite_drops_physical_settings.sql"
	d := openTestDB(t)
	phys := createTestNode(t, d, "p")
	comp := createTestComposite(t, d, "c", phys).ID
	// 直接改库，模拟写入口拒绝组合之前留下的值。
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
