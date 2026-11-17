package db

import (
	"reflect"
	"testing"

	"nft-forward/internal/chain"
)

// chainsOf 用测试里给的节点与成员关系建一份拓扑，展开 nodes 里的组合。组合聚合函数
// 的测试用它准备输入，走的是与生产相同的展开（flattenComposites）。
func chainsOf(nodes []*Node, hops []*NodeHop) map[int64][]chain.Hop {
	topo := chain.Topology{Nodes: map[int64]chain.Node{}, Members: map[int64][]chain.Member{}}
	for _, n := range nodes {
		topo.Nodes[n.ID] = chain.Node{ID: n.ID, Name: n.Name, Composite: n.NodeType == "composite"}
	}
	for _, h := range hops {
		topo.Members[h.NodeID] = append(topo.Members[h.NodeID], chain.Member{NodeID: h.HopNodeID, Mode: h.Mode})
	}
	return flattenComposites(topo, nodes)
}

func TestNodeSeatsAndGrantSeats(t *testing.T) {
	e, v, x := chain.SeatEntry, chain.SeatVia, chain.SeatExit
	nodeCases := []struct {
		roles        int64
		noDirectExit bool
		want         chain.Seat
	}{
		{NodeRoleEntry, false, e | x},
		{NodeRoleVia, false, v | x},
		{NodeRoleEntry | NodeRoleVia, false, e | v | x},
		{NodeRoleEntry | NodeRoleVia, true, e | v},
		{0, false, x},
		{0, true, 0},
	}
	for _, c := range nodeCases {
		if got := NodeSeats(c.roles, c.noDirectExit); got != c.want {
			t.Errorf("NodeSeats(%d, %v) = %b，期望 %b", c.roles, c.noDirectExit, got, c.want)
		}
	}

	grantCases := []struct {
		roles     int64
		want      chain.Seat
		overrides bool
	}{
		{0, 0, false}, // 不覆盖
		{NodeRoleEntry, e, true},
		{NodeRoleVia, v, true},
		{NodeRoleEntry | NodeRoleVia, e | v, true},
	}
	for _, c := range grantCases {
		got, overrides := GrantSeats(c.roles)
		if got != c.want || overrides != c.overrides {
			t.Errorf("GrantSeats(%d) = %b, %v；期望 %b, %v", c.roles, got, overrides, c.want, c.overrides)
		}
	}
}

// GrantSeats 与 EffectiveNodeRoles 是同一条“授权怎样覆盖节点资格”的规则的两种表达：
// 前者给链路推导，后者给节点列表里展示的生效角色。两者对每一种掩码组合必须给出相同
// 的入口/中转资格。
func TestGrantSeatsAgreesWithEffectiveNodeRoles(t *testing.T) {
	entryVia := chain.SeatEntry | chain.SeatVia
	for nodeRoles := int64(0); nodeRoles <= 3; nodeRoles++ {
		for grantRoles := int64(0); grantRoles <= 3; grantRoles++ {
			viaSeats := NodeSeats(nodeRoles, false) & entryVia
			if seats, overrides := GrantSeats(grantRoles); overrides {
				viaSeats = seats
			}
			viaRoles := NodeSeats(EffectiveNodeRoles(nodeRoles, grantRoles), false) & entryVia
			if viaSeats != viaRoles {
				t.Errorf("节点掩码 %d、授权掩码 %d：GrantSeats 给出 %b，EffectiveNodeRoles 给出 %b", nodeRoles, grantRoles, viaSeats, viaRoles)
			}
		}
	}
}

func TestLoadTopology(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "a", "", "")
	b, _ := CreateNode(d, "b", "", "")
	c, _ := CreateNode(d, "c", "", "")
	combo, _ := CreateNode(d, "combo", "", "")
	markComposite(t, d, combo.ID)
	if err := UpdateNodeRoles(d, a.ID, NodeRoleEntry); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeRoles(d, b.ID, NodeRoleVia); err != nil {
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
		c.ID:     {ID: c.ID, Name: "c", Seats: NodeSeats(c.Roles, false)},
		combo.ID: {ID: combo.ID, Name: "combo", Composite: true, Seats: NodeSeats(combo.Roles, false)},
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
	combo, _ := CreateNode(d, "combo", "", "")
	markComposite(t, d, combo.ID)

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

// 用户侧只拿得到被授权的节点，而组合的成员通常不在授权之列。成员链的展开必须按库里
// 的全量拓扑来：嵌套的组合照样展开到物理节点，成员的名字照样解析得出。
func TestResolveCompositeHopsForAGrantedSubset(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "a", "", "")
	b, _ := CreateNode(d, "b", "", "")
	x, _ := CreateNode(d, "x", "", "")
	inner, _ := CreateNode(d, "inner", "", "")
	outer, _ := CreateNode(d, "outer", "", "")
	markComposite(t, d, inner.ID)
	markComposite(t, d, outer.ID)
	if err := CreateNodeHops(d, inner.ID, []NodeHop{{Position: 0, HopNodeID: a.ID, Mode: "kernel"}, {Position: 1, HopNodeID: b.ID, Mode: "userspace"}}); err != nil {
		t.Fatal(err)
	}
	if err := CreateNodeHops(d, outer.ID, []NodeHop{{Position: 0, HopNodeID: inner.ID, Mode: "kernel"}, {Position: 1, HopNodeID: x.ID, Mode: "userspace"}}); err != nil {
		t.Fatal(err)
	}

	all, err := ListNodes(d)
	if err != nil {
		t.Fatal(err)
	}
	// 用户只被授权了 outer 这一个节点。
	var granted []*Node
	for _, n := range all {
		if n.ID == outer.ID {
			cp := *n
			granted = append(granted, &cp)
		}
	}
	ResolveCompositeHops(d, granted, all)

	want := []NodeChildHop{
		{NodeID: a.ID, Name: "a", NodeType: "remote", Mode: "kernel"},
		{NodeID: b.ID, Name: "b", NodeType: "remote", Mode: "userspace"},
		{NodeID: x.ID, Name: "x", NodeType: "remote", Mode: "userspace"},
	}
	if !reflect.DeepEqual(granted[0].Hops, want) {
		t.Fatalf("成员链：得到 %+v\n期望 %+v", granted[0].Hops, want)
	}
}

// 展不开的组合（这里是成员关系成环）在四个聚合里表现一致：离线、没有成员链、没有
// 出入口地址、不写原始流量；同一批里展得开的组合不受影响。
func TestUnflattenableCompositeHasNoDerivedAttributes(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "a", NodeType: "remote", Online: 1, RelayHost: "10.0.0.1", RelayHostV6: "2001:db8::1"},
		{ID: 7, Name: "loop-a", NodeType: "composite"},
		{ID: 8, Name: "loop-b", NodeType: "composite"},
		{ID: 9, Name: "ok", NodeType: "composite"},
	}
	hops := []*NodeHop{
		{NodeID: 7, Position: 0, HopNodeID: 1}, {NodeID: 7, Position: 1, HopNodeID: 8},
		{NodeID: 8, Position: 0, HopNodeID: 7},
		{NodeID: 9, Position: 0, HopNodeID: 1},
	}
	chains := chainsOf(nodes, hops)
	raw := map[int64]int64{1: 500}

	resolveCompositeOnline(nodes, chains)
	resolveCompositeRelayStack(nodes, chains)
	resolveCompositeHops(nodes, chains, nodes)
	fillCompositeRawTraffic(nodes, chains, raw)

	for _, n := range nodes[1:3] {
		if n.Online != 0 || n.Hops != nil || n.EntryRelayHost != "" || n.EntryRelayHostV6 != "" || n.ExitRelayHostV6 != "" {
			t.Errorf("成环的组合 %s 不应有派生属性，实际 online=%d hops=%+v entry=%q/%q exit6=%q",
				n.Name, n.Online, n.Hops, n.EntryRelayHost, n.EntryRelayHostV6, n.ExitRelayHostV6)
		}
		if _, written := raw[n.ID]; written {
			t.Errorf("成环的组合 %s 不应写原始流量", n.Name)
		}
	}
	ok := nodes[3]
	if ok.Online != 1 || len(ok.Hops) != 1 || ok.EntryRelayHost != "10.0.0.1" || ok.ExitRelayHostV6 != "2001:db8::1" || raw[ok.ID] != 500 {
		t.Errorf("展得开的组合应照常得到派生属性，实际 online=%d hops=%+v entry=%q exit6=%q raw=%d",
			ok.Online, ok.Hops, ok.EntryRelayHost, ok.ExitRelayHostV6, raw[ok.ID])
	}
}
