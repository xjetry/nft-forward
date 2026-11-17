package db

import (
	"reflect"
	"testing"

	"nft-forward/internal/chain"
)

func TestNodeSeatsAndGrantSeats(t *testing.T) {
	e, v, x := chain.SeatEntry, chain.SeatVia, chain.SeatExit
	nodeCases := []struct {
		stored       chain.Seat
		noDirectExit bool
		want         chain.Seat
	}{
		{chain.SeatEntry, false, e | x},
		{chain.SeatVia, false, v | x},
		{chain.SeatEntry | chain.SeatVia, false, e | v | x},
		{chain.SeatEntry | chain.SeatVia, true, e | v},
		{0, false, x},
		{0, true, 0},
		// 列里混进了链尾那一位：读出来不算数，链尾资格只看 no_direct_exit。
		{e | x, true, e},
	}
	for _, c := range nodeCases {
		if got := NodeSeats(c.stored, c.noDirectExit); got != c.want {
			t.Errorf("NodeSeats(%d, %v) = %b，期望 %b", c.stored, c.noDirectExit, got, c.want)
		}
	}

	grantCases := []struct {
		stored    chain.Seat
		want      chain.Seat
		overrides bool
	}{
		{0, 0, false}, // 不覆盖
		{chain.SeatEntry, e, true},
		{chain.SeatVia, v, true},
		{chain.SeatEntry | chain.SeatVia, e | v, true},
		{e | x, e, true}, // 链尾那一位不属于覆盖
	}
	for _, c := range grantCases {
		got, overrides := GrantSeats(c.stored)
		if got != c.want || overrides != c.overrides {
			t.Errorf("GrantSeats(%d) = %b, %v；期望 %b, %v", c.stored, got, overrides, c.want, c.overrides)
		}
	}
}

// 链路推导判定入口/中转资格时用的是“节点自己的资格，被 GrantSeats 给出的覆盖整体取
// 代”（chain 对 Request.SeatOverrides 的处理），接口展示的是 EffectiveSeats。两者对
// 节点与授权上每一种取值都必须给出相同的入口/中转资格，否则页面上显示能用的位置建规则
// 时会被拒，或者反过来。
func TestEffectiveSeatsMatchWhatChainDerivationUses(t *testing.T) {
	for node := chain.Seat(0); node <= SettableSeats; node++ {
		for grant := chain.Seat(0); grant <= SettableSeats; grant++ {
			derivation := NodeSeats(node, false) & SettableSeats
			if seats, overrides := GrantSeats(grant); overrides {
				derivation = seats
			}
			if shown := EffectiveSeats(node, grant); shown != derivation {
				t.Errorf("节点 %b、授权 %b：链路推导用的是 %b，EffectiveSeats 给出 %b", node, grant, derivation, shown)
			}
		}
	}
}

func TestLoadTopology(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "a", "", "")
	b, _ := CreateNode(d, "b", "", "")
	c, _ := CreateNode(d, "c", "", "")
	combo := createTestComposite(t, d, "combo")
	if err := UpdateNodeSeats(d, a.ID, chain.SeatEntry); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeSeats(d, b.ID, chain.SeatVia); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeNoDirectExit(d, b.ID, true); err != nil {
		t.Fatal(err)
	}
	// 成员故意倒着写入，读出来必须按 position 排。
	if err := CreateNodeHops(d, combo.ID, []NodeHop{
		{Position: 1, HopNodeID: c.ID, Mode: "kernel"},
		{Position: 0, HopNodeID: a.ID, Mode: "userspace"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindingsForDownstream(d, b.ID, []NodeBinding{{UpstreamNodeID: a.ID, Mode: "kernel"}}); err != nil {
		t.Fatal(err)
	}

	topo, err := LoadTopology(d)
	if err != nil {
		t.Fatal(err)
	}
	wantNodes := map[int64]chain.Node{
		a.ID:     {ID: a.ID, Name: "a", Seats: chain.SeatEntry | chain.SeatExit},
		b.ID:     {ID: b.ID, Name: "b", Seats: chain.SeatVia},
		c.ID:     {ID: c.ID, Name: "c", Seats: NodeSeats(c.Seats, false)},
		combo.ID: {ID: combo.ID, Name: "combo", Composite: true, Seats: NodeSeats(combo.Seats, false)},
	}
	if !reflect.DeepEqual(topo.Nodes, wantNodes) {
		t.Errorf("节点：得到 %+v\n期望 %+v", topo.Nodes, wantNodes)
	}
	wantMembers := map[int64][]chain.Member{
		combo.ID: {{NodeID: a.ID, Mode: "userspace"}, {NodeID: c.ID, Mode: "kernel"}},
	}
	if !reflect.DeepEqual(topo.Members, wantMembers) {
		t.Errorf("成员：得到 %+v，期望 %+v", topo.Members, wantMembers)
	}
	wantBindings := map[chain.Edge]string{{Up: a.ID, Down: b.ID}: "kernel"}
	if !reflect.DeepEqual(topo.Bindings, wantBindings) {
		t.Errorf("绑定边：得到 %+v，期望 %+v", topo.Bindings, wantBindings)
	}

	// 读完之后连接已经还回去：紧接着的普通查询不应被上一个事务卡住。
	if _, err := ListNodes(d); err != nil {
		t.Fatalf("LoadTopology 之后的查询失败：%v", err)
	}
}

// 组合成员与绑定边上没填的 mode 入库时记成 userspace，认不出的值被拒绝且不留下
// 部分写入。
func TestInterNodeLegModeIsSettledWhenStored(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "a", "", "")
	b, _ := CreateNode(d, "b", "", "")
	combo := createTestComposite(t, d, "combo")

	if err := CreateNodeHops(d, combo.ID, []NodeHop{{Position: 0, HopNodeID: a.ID}, {Position: 1, HopNodeID: b.ID, Mode: "kernel"}}); err != nil {
		t.Fatal(err)
	}
	members, _ := ListNodeHops(d, combo.ID)
	if len(members) != 2 || members[0].Mode != "userspace" || members[1].Mode != "kernel" {
		t.Fatalf("成员 mode：没填的应记成 userspace、填了的原样，实际 %+v %+v", members[0], members[1])
	}
	if err := CreateNodeHops(d, combo.ID, []NodeHop{{Position: 2, HopNodeID: a.ID, Mode: "relay"}}); err == nil {
		t.Fatal("成员 mode 认不出时应被拒绝")
	}

	if err := ReplaceBindingsForDownstream(d, b.ID, []NodeBinding{{UpstreamNodeID: a.ID}}); err != nil {
		t.Fatal(err)
	}
	if edge, err := GetNodeBinding(d, a.ID, b.ID); err != nil || edge.Mode != "userspace" {
		t.Fatalf("下游侧写入：没填的边 mode 应记成 userspace，实际 %+v err=%v", edge, err)
	}
	if err := ReplaceBindingsForUpstream(d, b.ID, []NodeBinding{{DownstreamNodeID: a.ID}}); err != nil {
		t.Fatal(err)
	}
	if edge, err := GetNodeBinding(d, b.ID, a.ID); err != nil || edge.Mode != "userspace" {
		t.Fatalf("上游侧写入：没填的边 mode 应记成 userspace，实际 %+v err=%v", edge, err)
	}

	// 认不出的值：整次替换回滚，原有的边还在。
	if err := ReplaceBindingsForDownstream(d, b.ID, []NodeBinding{{UpstreamNodeID: combo.ID, Mode: "relay"}}); err == nil {
		t.Fatal("下游侧写入：边 mode 认不出时应被拒绝")
	}
	if err := ReplaceBindingsForUpstream(d, b.ID, []NodeBinding{{DownstreamNodeID: combo.ID, Mode: "relay"}}); err == nil {
		t.Fatal("上游侧写入：边 mode 认不出时应被拒绝")
	}
	edges, _ := ListAllNodeBindings(d)
	if len(edges) != 2 {
		t.Fatalf("被拒绝的替换不应改动已有的边，实际剩 %d 条：%+v", len(edges), edges)
	}
}
