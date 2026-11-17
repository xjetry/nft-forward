package db

import (
	"fmt"
	"reflect"
	"testing"
)

// hopColumnCount 报告 rule_hops 表里名叫 name 的列有几个（0 或 1）。
func hopColumnCount(t *testing.T, d DBTX, name string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('rule_hops') WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 跳行上“这一跳属于哪个逻辑节点”的那一列叫 logical_node_id。它原来叫 via_node_id，
// 与规则表头的 via_node_ids（入口之后的中转层，不含入口）只差一个 s，指的却是另一
// 件事：入口那一段的跳也有它。迁移只改列名：每一行的值原样留下，旧名字不再存在。
func TestMigrationRenamesTheHopLogicalNodeColumn(t *testing.T) {
	const migration = "0054_rule_hop_logical_node.sql"
	d := openTestDB(t)
	if n := hopColumnCount(t, d, "logical_node_id"); n != 1 {
		t.Fatalf("跑完全部迁移的库里 rule_hops 应有 logical_node_id 列，实际 %d 个", n)
	}
	if n := hopColumnCount(t, d, "via_node_id"); n != 0 {
		t.Fatalf("跑完全部迁移的库里 rule_hops 不应再有 via_node_id 列，实际 %d 个", n)
	}

	a, b, c := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"), relayNode(t, d, "c", "10.0.0.3")
	k := compositeOver(t, d, "k", b, c)
	ruleID := chainRule(t, d, "r", a)
	if _, err := regenerateHops(t, d, ruleID, hop(a, a), hop(b, k), hop(c, k)); err != nil {
		t.Fatal(err)
	}
	want := []int64{a.ID, k.ID, k.ID}
	logicalNodes := func(column string) []int64 {
		t.Helper()
		got, err := queryInt64s(d, fmt.Sprintf(`SELECT %s FROM rule_hops WHERE rule_id=? ORDER BY position`, column), ruleID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := logicalNodes("logical_node_id"); !reflect.DeepEqual(got, want) {
		t.Fatalf("前置条件：各跳所属的逻辑节点应是 %v，实际 %v", want, got)
	}

	// 回到迁移之前的形状，再重放这一个迁移。
	mustExec(t, d, `ALTER TABLE rule_hops RENAME COLUMN logical_node_id TO via_node_id`)
	if got := logicalNodes("via_node_id"); !reflect.DeepEqual(got, want) {
		t.Fatalf("前置条件：改回旧列名后各跳的值应仍是 %v，实际 %v", want, got)
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

	if n := hopColumnCount(t, d, "via_node_id"); n != 0 {
		t.Errorf("重放迁移后 rule_hops 不应再有 via_node_id 列，实际 %d 个", n)
	}
	if n := hopColumnCount(t, d, "logical_node_id"); n != 1 {
		t.Fatalf("重放迁移后 rule_hops 应有 logical_node_id 列，实际 %d 个", n)
	}
	if got := logicalNodes("logical_node_id"); !reflect.DeepEqual(got, want) {
		t.Errorf("重放迁移后各跳所属的逻辑节点应原样是 %v，实际 %v", want, got)
	}
}
