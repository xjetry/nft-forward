package db

import "testing"

// 升级时，入口跳上记着的加权流量原样搬进规则行的 weighted_bytes，入口跳自己的
// 逐跳计数从 0 重新开始；其余跳本来就是折算流量，不动。
func TestRuleWeightedBytesMigrationMovesEntryTotals(t *testing.T) {
	const migration = "0049_rule_weighted_bytes.sql"
	d := openTestDB(t)
	entry := createTestNode(t, d, "e")
	next := createTestNode(t, d, "m")

	// 回到迁移之前：rules 上没有这一列，加权流量记在入口跳上。
	if _, err := d.Exec(`ALTER TABLE rules DROP COLUMN weighted_bytes`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}
	twoHops := func(port int, entryTotal, nextTotal int64) (ruleID int64) {
		t.Helper()
		res, err := d.Exec(`INSERT INTO rules(node_id, name, proto, exit_host, exit_port, created_at) VALUES (?,?,?,?,?,0)`,
			entry, "r", "tcp", "8.8.8.8", 443)
		if err != nil {
			t.Fatal(err)
		}
		ruleID, _ = res.LastInsertId()
		for pos, h := range []struct {
			node  int64
			total int64
		}{{entry, entryTotal}, {next, nextTotal}} {
			if _, err := d.Exec(`INSERT INTO rule_hops(rule_id, position, node_id, proto, listen_port, target_host, target_port, total_bytes) VALUES (?,?,?,?,?,?,?,?)`,
				ruleID, pos, h.node, "tcp", port, "8.8.8.8", 443, h.total); err != nil {
				t.Fatal(err)
			}
		}
		return ruleID
	}
	billed := twoHops(20001, 3000, 1000)
	idle := twoHops(20002, 0, 50)

	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	one := func(q string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := d.QueryRow(q, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	for _, c := range []struct {
		what      string
		got, want int64
	}{
		{"有流量的规则：规则的加权流量", one(`SELECT weighted_bytes FROM rules WHERE id=?`, billed), 3000},
		{"有流量的规则：入口跳逐跳计数", one(`SELECT total_bytes FROM rule_hops WHERE rule_id=? AND position=0`, billed), 0},
		{"有流量的规则：第二跳逐跳计数", one(`SELECT total_bytes FROM rule_hops WHERE rule_id=? AND position=1`, billed), 1000},
		{"入口跳为 0 的规则：规则的加权流量", one(`SELECT weighted_bytes FROM rules WHERE id=?`, idle), 0},
		{"入口跳为 0 的规则：第二跳逐跳计数", one(`SELECT total_bytes FROM rule_hops WHERE rule_id=? AND position=1`, idle), 50},
	} {
		if c.got != c.want {
			t.Errorf("%s: 期望 %d，实际 %d", c.what, c.want, c.got)
		}
	}
	rl, err := GetRule(d, billed)
	if err != nil {
		t.Fatal(err)
	}
	if rl.WeightedBytes != 3000 {
		t.Errorf("GetRule 应读出迁移搬过来的加权流量 3000，实际 %d", rl.WeightedBytes)
	}
}
