package db

import (
	"database/sql"
	"reflect"
	"strconv"
	"testing"
)

// ruleHeaderPath 读规则表头里的逻辑路径：入口节点与其后的中转层。
func ruleHeaderPath(t *testing.T, d *sql.DB, ruleID int64) (int64, []int64) {
	t.Helper()
	r, err := GetRule(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	vias := r.ViaNodeIDs
	if vias == nil {
		vias = []int64{}
	}
	return r.NodeID, vias
}

// compositeOver 建一个由 members 依次组成的组合节点。
func compositeOver(t *testing.T, d *sql.DB, name string, members ...*Node) *Node {
	t.Helper()
	hops := make([]NodeHop, len(members))
	for i, m := range members {
		hops[i] = NodeHop{HopNodeID: m.ID, Mode: "kernel"}
	}
	n, err := CreateCompositeNode(d, name, LineSpec{}, hops)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// regenerateHops 在一个事务里按 hops 重新生成规则的跳；成功才提交。返回传给
// RegenerateRule 的那一份规则（它会被改写）与错误。
func regenerateHops(t *testing.T, d *sql.DB, ruleID int64, hops ...HopInput) (*Rule, error) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r, err := GetRule(tx, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RegenerateRule(tx, r, hops, nil); err != nil {
		return r, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return r, nil
}

func hop(n, logical *Node) HopInput {
	return HopInput{NodeID: n.ID, Mode: "kernel", LogicalNodeID: logical.ID}
}

// 规则表头里的逻辑路径由它的跳行决定：跳按所属的逻辑节点（rule_hops.logical_node_id）分成
// 连续的段，第一段的逻辑节点是入口（rules.node_id），其余各段依次是中转层
// （rules.via_node_ids）。RegenerateRule 是唯一写跳行的地方，表头在同一处写。
func TestRuleHeaderFollowsItsHops(t *testing.T) {
	d := openTestDB(t)
	a, b, c := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"), relayNode(t, d, "c", "10.0.0.3")
	k := compositeOver(t, d, "k", b, c)
	ruleID := chainRule(t, d, "r", a)

	for _, tc := range []struct {
		name  string
		hops  []HopInput
		entry int64
		vias  []int64
	}{
		{"入口是单点，其后是一个组合展开的两跳", []HopInput{hop(a, a), hop(b, k), hop(c, k)}, a.ID, []int64{k.ID}},
		{"整条链是一个组合展开的", []HopInput{hop(b, k), hop(c, k)}, k.ID, []int64{}},
		{"每一跳自成一段", []HopInput{hop(a, a), hop(b, b), hop(c, c)}, a.ID, []int64{b.ID, c.ID}},
		{"只剩一跳", []HopInput{hop(c, c)}, c.ID, []int64{}},
	} {
		r, err := regenerateHops(t, d, ruleID, tc.hops...)
		if err != nil {
			t.Fatalf("%s：%v", tc.name, err)
		}
		entry, vias := ruleHeaderPath(t, d, ruleID)
		if entry != tc.entry || !reflect.DeepEqual(vias, tc.vias) {
			t.Errorf("%s：表头是入口 %d、中转层 %v，期望入口 %d、中转层 %v", tc.name, entry, vias, tc.entry, tc.vias)
		}
		got := r.ViaNodeIDs
		if got == nil {
			got = []int64{}
		}
		if r.NodeID != tc.entry || !reflect.DeepEqual(got, tc.vias) {
			t.Errorf("%s：传进去的规则应同步成入口 %d、中转层 %v，实际入口 %d、中转层 %v", tc.name, tc.entry, tc.vias, r.NodeID, got)
		}
	}

	// 下面两种链写不成表头，拒绝；表头与跳都保持上一次成功时的样子。
	wantEntry, wantVias := ruleHeaderPath(t, d, ruleID)
	wantHops := hopLines(t, d, ruleID)
	for _, tc := range []struct {
		name string
		hops []HopInput
	}{
		{"同一个逻辑节点的跳被别的段隔开", []HopInput{hop(b, k), hop(a, a), hop(c, k)}},
		{"跳所属的逻辑节点不存在", []HopInput{{NodeID: a.ID, Mode: "kernel", LogicalNodeID: 987654}}},
	} {
		if _, err := regenerateHops(t, d, ruleID, tc.hops...); !IsChainError(err) {
			t.Errorf("%s：应被当作搭不成的链拒绝，实际 err=%v", tc.name, err)
		}
		entry, vias := ruleHeaderPath(t, d, ruleID)
		if entry != wantEntry || !reflect.DeepEqual(vias, wantVias) || !reflect.DeepEqual(hopLines(t, d, ruleID), wantHops) {
			t.Errorf("%s：被拒绝后规则不应变化", tc.name)
		}
	}
}

// UpdateRuleHeader 只写发起方填写的字段。传进来的规则上带着别的入口与中转层，也不会
// 被写进去：那两列只跟着跳行走，改它们的办法是用新的跳重新生成。
func TestUpdateRuleHeaderLeavesTheLogicalPathAlone(t *testing.T) {
	d := openTestDB(t)
	a, b, c := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"), relayNode(t, d, "c", "10.0.0.3")
	ruleID := chainRule(t, d, "r", a, b)

	r, err := GetRule(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	r.Name, r.NodeID, r.ViaNodeIDs = "renamed", c.ID, []int64{a.ID}
	if err := UpdateRuleHeader(d, r); err != nil {
		t.Fatal(err)
	}

	got, err := GetRule(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Errorf("名称应改成 renamed，实际 %q", got.Name)
	}
	if entry, vias := ruleHeaderPath(t, d, ruleID); entry != a.ID || !reflect.DeepEqual(vias, []int64{b.ID}) {
		t.Errorf("逻辑路径不应被改动：表头是入口 %d、中转层 %v，期望入口 %d、中转层 %v", entry, vias, a.ID, []int64{b.ID})
	}
}

// 删掉规则中间的一台节点，规则在剩下的跳上重新生成，表头里的中转层跟着少掉它。
func TestDeletingAHopNodeRemovesItFromTheRuleHeader(t *testing.T) {
	d := openTestDB(t)
	a, b, c := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"), relayNode(t, d, "c", "10.0.0.3")
	ruleID := chainRule(t, d, "r", a, b, c)
	if entry, vias := ruleHeaderPath(t, d, ruleID); entry != a.ID || !reflect.DeepEqual(vias, []int64{b.ID, c.ID}) {
		t.Fatalf("前置：表头应是入口 %d、中转层 %v，实际入口 %d、中转层 %v", a.ID, []int64{b.ID, c.ID}, entry, vias)
	}

	if _, err := DeleteNodeAndRewire(d, b.ID); err != nil {
		t.Fatal(err)
	}

	if entry, vias := ruleHeaderPath(t, d, ruleID); entry != a.ID || !reflect.DeepEqual(vias, []int64{c.ID}) {
		t.Errorf("删掉中间节点后表头应是入口 %d、中转层 %v，实际入口 %d、中转层 %v", a.ID, []int64{c.ID}, entry, vias)
	}
}

// 迁移 0053 按跳行重算每条规则表头里的逻辑路径。重算前后一致的规则不变；第一跳所属
// 的逻辑节点已经不存在的规则不动（rules.node_id 有外键，写不进去）。
func TestMigrationRecomputesRuleHeadersFromHops(t *testing.T) {
	const migration = "0053_rule_header_follows_hops.sql"
	d := openTestDB(t)
	a, b, c, x := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"),
		relayNode(t, d, "c", "10.0.0.3"), relayNode(t, d, "x", "10.0.0.9")
	k := compositeOver(t, d, "k", b, c)

	// 每条规则先按 hops 建好，再把表头改回迁移之前可能留下的样子。
	type seeded struct {
		name      string
		hops      []HopInput
		oldEntry  int64
		oldVias   string
		wantEntry int64
		wantVias  []int64
		id        int64
	}
	rules := []*seeded{
		{name: "每跳自成一段，表头没记中转层", hops: []HopInput{hop(a, a), hop(b, b), hop(c, c)},
			oldEntry: a.ID, oldVias: "[]", wantEntry: a.ID, wantVias: []int64{b.ID, c.ID}},
		{name: "整条链属于一个组合，表头记的是第一台物理节点", hops: []HopInput{hop(b, k), hop(c, k)},
			oldEntry: b.ID, oldVias: "[]", wantEntry: k.ID, wantVias: []int64{}},
		{name: "表头的入口是请求另给的节点", hops: []HopInput{hop(a, a), hop(b, b)},
			oldEntry: x.ID, oldVias: "[]", wantEntry: a.ID, wantVias: []int64{b.ID}},
		{name: "表头本来就与跳一致", hops: []HopInput{hop(a, a), hop(b, k), hop(c, k)},
			oldEntry: a.ID, oldVias: "[" + strconv.FormatInt(k.ID, 10) + "]", wantEntry: a.ID, wantVias: []int64{k.ID}},
	}
	for _, r := range rules {
		r.id = chainRule(t, d, r.name, a)
		if _, err := regenerateHops(t, d, r.id, r.hops...); err != nil {
			t.Fatalf("%s：%v", r.name, err)
		}
		mustExec(t, d, `UPDATE rules SET node_id=?, via_node_ids=? WHERE id=?`, r.oldEntry, r.oldVias, r.id)
	}
	// 第一跳所属的逻辑节点不存在：跳行直接改出来（RegenerateRule 不收这样的链）。
	dangling := chainRule(t, d, "第一跳的逻辑节点已不存在", a, b)
	mustExec(t, d, `UPDATE rule_hops SET logical_node_id=987654 WHERE rule_id=? AND position=0`, dangling)
	mustExec(t, d, `UPDATE rules SET node_id=?, via_node_ids='[]' WHERE id=?`, a.ID, dangling)
	// 没有跳的规则。
	var hopless int64
	{
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		hopless, err = CreateRule(tx, &Rule{NodeID: x.ID, Name: "没有跳", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, ViaNodeIDs: []int64{a.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// 回到迁移 0053 之前的形状：那时跳行上的这一列还叫 via_node_id（0054 才改名），
	// 0053 的 SQL 写的是这个名字。两个迁移的登记都删掉，migrate 按序重放它们。
	mustExec(t, d, `ALTER TABLE rule_hops RENAME COLUMN logical_node_id TO via_node_id`)
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version IN (?, ?)`, migration, "0054_rule_hop_logical_node.sql")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("前置条件：迁移 %s 与改列名的迁移应都已登记，删除了 %d 行", migration, n)
	}
	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	for _, r := range rules {
		if entry, vias := ruleHeaderPath(t, d, r.id); entry != r.wantEntry || !reflect.DeepEqual(vias, r.wantVias) {
			t.Errorf("%s：迁移后表头是入口 %d、中转层 %v，期望入口 %d、中转层 %v", r.name, entry, vias, r.wantEntry, r.wantVias)
		}
	}
	if entry, vias := ruleHeaderPath(t, d, dangling); entry != a.ID || len(vias) != 0 {
		t.Errorf("第一跳的逻辑节点已不存在的规则不应被改动：表头是入口 %d、中转层 %v", entry, vias)
	}
	if entry, vias := ruleHeaderPath(t, d, hopless); entry != x.ID || !reflect.DeepEqual(vias, []int64{a.ID}) {
		t.Errorf("没有跳的规则不应被改动：表头是入口 %d、中转层 %v", entry, vias)
	}
}
