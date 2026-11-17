package db

import (
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// machineColumns 是 nodes 表上属于机器的列，与 Machine 的字段一一对应
// （TestMachineColumnsCoverTheMachineType 对照个数）。
var machineColumns = []string{
	"owner_id", "address", "secret", "relay_host", "relay_host_v6", "relay_host_declared", "relay_host_v6_declared",
	"online", "agent_version", "agent_sha", "last_seen", "last_apply_at", "last_error", "last_warning", "port_range",
	"last_upgrade_at", "last_upgrade_version", "last_upgrade_status", "last_upgrade_error", "unidirectional", "no_direct_exit",
}

// Machine 加了字段而这里没加对应的列，下面两个测试就会漏掉它。
func TestMachineColumnsCoverTheMachineType(t *testing.T) {
	if n := reflect.TypeOf(Machine{}).NumField(); n != len(machineColumns) {
		t.Fatalf("Machine 有 %d 个字段，machineColumns 列了 %d 列", n, len(machineColumns))
	}
}

// setMachineColumns 列出节点 id 上取值不为空、不为 0 的机器列。
func setMachineColumns(t *testing.T, d *sql.DB, id int64) []string {
	t.Helper()
	var set []string
	for _, c := range machineColumns {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id=? AND `+c+` IS NOT NULL AND CAST(`+c+` AS TEXT) NOT IN ('', '0')`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			set = append(set, c)
		}
	}
	sort.Strings(set)
	return set
}

// 机器列的每个写入口都拒绝组合；节点不存在与节点是组合是两种不同的失败，调用方要能
// 分清。组合行上的机器列因此一个都写不进去。
func TestMachineWritersRefuseComposites(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	phys := createTestNode(t, d, "p")
	comp := createTestComposite(t, d, "c", phys).ID

	writers := map[string]func(id int64) error{
		"relay_host":             func(id int64) error { return UpdateNodeRelayHost(d, id, "1.2.3.4") },
		"relay_host_v6":          func(id int64) error { return UpdateNodeRelayHostV6(d, id, "2001:db8::1") },
		"relay_host_declared":    func(id int64) error { return SetNodeRelayHostDeclared(d, id, true) },
		"relay_host_v6_declared": func(id int64) error { return SetNodeRelayHostV6Declared(d, id, true) },
		"port_range":             func(id int64) error { return UpdateNodePortRange(d, id, "30000-30010") },
		"owner":                  func(id int64) error { return UpdateNodeOwner(d, id, &uid) },
		"online":                 func(id int64) error { return MarkNodeOnline(d, id, "v1.0.0", "sha", "203.0.113.9") },
		"offline":                func(id int64) error { return MarkNodeOffline(d, id) },
		"applied":                func(id int64) error { return MarkNodeApplied(d, id, "skipped") },
		"dispatch_error":         func(id int64) error { return MarkNodeDispatchError(d, id, "boom") },
		"upgrade_result":         func(id int64) error { return RecordUpgradeResult(d, id, "v1.0.0", "acked", "") },
		"unidirectional":         func(id int64) error { return UpdateNodeUnidirectional(d, id, true) },
		"no_direct_exit":         func(id int64) error { return UpdateNodeNoDirectExit(d, id, true) },
	}
	names := make([]string, 0, len(writers))
	for name := range writers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		write := writers[name]
		if err := write(comp); !errors.Is(err, ErrPhysicalNodeSetting) {
			t.Errorf("%s：组合应返回 ErrPhysicalNodeSetting，实际 %v", name, err)
		}
		if err := write(phys); err != nil {
			t.Errorf("%s：物理节点应成功，实际 %v", name, err)
		}
		if err := write(999999); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s：不存在的节点应返回 sql.ErrNoRows，实际 %v", name, err)
		}
	}
	if set := setMachineColumns(t, d, comp); len(set) > 0 {
		t.Errorf("组合行上不应有机器列的值：%s", strings.Join(set, ", "))
	}
	if set := setMachineColumns(t, d, phys); len(set) == 0 {
		t.Error("物理节点上的写入应生效")
	}
}

// 成员只属于组合：给机器写成员返回 ErrNotComposite，给不存在的节点写返回 sql.ErrNoRows，
// 都不写入。
func TestMemberWriterRefusesMachines(t *testing.T) {
	d := openTestDB(t)
	a := createTestNode(t, d, "a")
	b := createTestNode(t, d, "b")
	if err := CreateNodeHops(d, a, []NodeHop{{Position: 0, HopNodeID: b}}); !errors.Is(err, ErrNotComposite) {
		t.Errorf("机器：应返回 ErrNotComposite，实际 %v", err)
	}
	if err := CreateNodeHops(d, 999999, []NodeHop{{Position: 0, HopNodeID: b}}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("不存在的节点：应返回 sql.ErrNoRows，实际 %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_hops`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("被拒绝的写入不应留下成员，实际 %d 条", n)
	}
}

// 建组合时成员写不进去（这里是认不出的转发模式），节点行也不留下。
func TestCreateCompositeNodeLeavesNothingWhenMembersFail(t *testing.T) {
	d := openTestDB(t)
	a := createTestNode(t, d, "a")
	if _, err := CreateCompositeNode(d, "combo", LineSpec{}, []NodeHop{{HopNodeID: a, Mode: "relay"}}); err == nil {
		t.Fatal("认不出的成员转发模式应让建组合失败")
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name='combo'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("建组合失败后不应留下节点行，实际 %d 行", n)
	}
}

// 建一台机器时给的机器属性与线路属性一次写全；组合行上不写机器列。
func TestCreateWritesEachKindsOwnAttributes(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	m, err := CreateMachineNode(d, "edge", MachineSpec{Secret: "tok", OwnerID: &uid, PortRange: "30000-30010", Unidirectional: true},
		LineSpec{RateMultiplier: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	if m.NodeType != "remote" || m.Secret != "tok" || m.OwnerID == nil || *m.OwnerID != uid || m.PortRange != "30000-30010" ||
		!m.Unidirectional || m.RateMultiplier != 2.5 || m.Roles != NodeRoleEntry|NodeRoleVia {
		t.Errorf("机器的属性没有按给的写入：%+v", m)
	}
	plain, err := CreateNode(d, "plain", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Secret == "" || plain.PortRange != DefaultPortRange || plain.OwnerID != nil || plain.RateMultiplier != 1.0 {
		t.Errorf("没给的机器属性应取默认值（随机凭据、默认端口范围、无属主、倍率 1）：%+v", plain)
	}
	c, err := CreateCompositeNode(d, "combo", LineSpec{RateMultiplier: 3}, []NodeHop{{HopNodeID: m.ID}, {HopNodeID: plain.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeType != "composite" || c.RateMultiplier != 3 || c.Roles != NodeRoleEntry|NodeRoleVia {
		t.Errorf("组合的线路属性没有按给的写入：%+v", c)
	}
	if set := setMachineColumns(t, d, c.ID); len(set) > 0 {
		t.Errorf("组合行上不应有机器列的值：%s", strings.Join(set, ", "))
	}
}

// 组合行上的机器列不再被写入之前，旧版本建组合时给它随机凭据与默认端口范围，各个写接口
// 也接受组合。迁移把组合行上的机器列一律清空，物理节点不动。
func TestMigrationClearsMachineAttributesOnComposites(t *testing.T) {
	const migration = "0050_composite_rows_carry_no_machine.sql"
	d := openTestDB(t)
	uid := createTestUser(t, d)
	phys := createTestNode(t, d, "p")
	comp := createTestComposite(t, d, "c", phys).ID
	for _, id := range []int64{phys, comp} {
		if _, err := d.Exec(`UPDATE nodes SET owner_id=?, address='203.0.113.1', secret='tok', relay_host='10.0.0.1',
			relay_host_v6='2001:db8::1', relay_host_declared=1, relay_host_v6_declared=1, online=1, agent_version='v1',
			agent_sha='sha', last_seen=1, last_apply_at=1, last_error='e', last_warning='w', port_range='10001-20000',
			last_upgrade_at=1, last_upgrade_version='v1', last_upgrade_status='error', last_upgrade_error='e',
			unidirectional=1, no_direct_exit=1 WHERE id=?`, uid, id); err != nil {
			t.Fatal(err)
		}
	}
	if set := setMachineColumns(t, d, comp); len(set) != len(machineColumns) {
		t.Fatalf("前置条件：组合行上的机器列应全部有值，实际 %v", set)
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

	if set := setMachineColumns(t, d, comp); len(set) > 0 {
		t.Errorf("迁移后组合行上的机器列应清空，仍有值：%s", strings.Join(set, ", "))
	}
	if set := setMachineColumns(t, d, phys); len(set) != len(machineColumns) {
		t.Errorf("迁移不应动物理节点，实际有值的机器列 %v", set)
	}
}
