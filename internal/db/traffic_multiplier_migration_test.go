package db

import (
	"fmt"
	"testing"
)

// 两个没有读者的倍率列被删掉：node_hops.traffic_multiplier 与 nodes.traffic_multiplier。
// 在带着这两列（存着旧库里的非零缺省值）的库上重放迁移，两列都消失，组合的成员与
// 节点的价格（rate_multiplier）原样保留。
func TestMigrationDropsTheRetiredTrafficMultiplierColumns(t *testing.T) {
	const migration = "0051_drop_retired_traffic_multiplier.sql"
	d := openTestDB(t)
	a := createTestNode(t, d, "a")
	b := createTestNode(t, d, "b")
	c := createTestComposite(t, d, "c", a, b)
	if err := UpdateNodeRateMultiplier(d, c.ID, 2.5); err != nil {
		t.Fatal(err)
	}
	tables := []string{"node_hops", "nodes"}
	retired := func(table string) int {
		t.Helper()
		var n int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name='traffic_multiplier'`, table)
		if err := d.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range tables {
		if n := retired(table); n != 0 {
			t.Fatalf("跑完全部迁移的库里 %s 不应有 traffic_multiplier 列，实际有 %d 个", table, n)
		}
	}

	// 回到迁移之前的形状，再重放这一个迁移。
	for _, table := range tables {
		mustExec(t, d, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0`, table))
		if n := retired(table); n != 1 {
			t.Fatalf("前置条件：%s 应已带回 traffic_multiplier 列，实际 %d 个", table, n)
		}
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
		if n := retired(table); n != 0 {
			t.Errorf("重放迁移后 %s 不应再有 traffic_multiplier 列，实际还有 %d 个", table, n)
		}
	}
	hops, err := ListNodeHops(d, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || hops[0].HopNodeID != a || hops[1].HopNodeID != b {
		t.Fatalf("组合的成员应原样保留为 [a b]，实际 %+v", hops)
	}
	got, err := GetNode(d, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateMultiplier != 2.5 {
		t.Fatalf("组合的 rate_multiplier 应原样保留为 2.5，实际 %v", got.RateMultiplier)
	}
}
