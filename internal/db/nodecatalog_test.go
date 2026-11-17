package db

import (
	"reflect"
	"testing"
)

func catalogOf(nodes []*Node, hops []*NodeHop, raw map[int64]int64) *NodeCatalog {
	return NewNodeCatalog(nodes, hops, raw)
}

func memberHop(comp, child int64, mode string) *NodeHop {
	return &NodeHop{NodeID: comp, HopNodeID: child, Mode: mode}
}

func TestCatalogCompositeOnline(t *testing.T) {
	mk := func(id int64, typ string, online int, disabled bool) *Node {
		return &Node{ID: id, NodeType: typ, Online: online, Disabled: disabled}
	}
	hop := func(comp, child int64) *NodeHop { return memberHop(comp, child, "") }

	tests := []struct {
		name  string
		nodes []*Node
		hops  []*NodeHop
		want  map[int64]bool
	}{
		{
			name:  "all children online => composite online",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 1, false), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]bool{3: true, 1: true},
		},
		{
			name:  "one child offline => composite offline",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 0, false), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]bool{3: false, 2: false},
		},
		{
			// 停用的成员算离线；停用的物理节点自己的 online 仍是它的列值。
			name:  "disabled child counts as offline",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 1, true), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]bool{3: false, 2: true},
		},
		{
			// 组合自己的 online 列不参与判定。
			name:  "composite with no children => offline",
			nodes: []*Node{mk(3, "composite", 1, false)},
			want:  map[int64]bool{3: false},
		},
		{
			name: "multiple composites resolved independently",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 0, false),
				mk(3, "composite", 0, false), mk(4, "composite", 0, false),
			},
			hops: []*NodeHop{hop(3, 1), hop(4, 1), hop(4, 2)},
			want: map[int64]bool{3: true, 4: false},
		},
		{
			name: "nested composite online when every deep leaf online",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 1, false), mk(3, "remote", 1, false),
				mk(8, "composite", 0, false), mk(9, "composite", 0, false),
			},
			hops: []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			want: map[int64]bool{8: true, 9: true},
		},
		{
			name: "nested composite offline when a deep leaf offline",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 0, false), mk(3, "remote", 1, false),
				mk(8, "composite", 0, false), mk(9, "composite", 0, false),
			},
			hops: []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			want: map[int64]bool{8: false, 9: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := catalogOf(tt.nodes, tt.hops, nil)
			for id, want := range tt.want {
				if got := c.Derive(c.Node(id)).Online; got != want {
					t.Errorf("node %d online = %v, want %v", id, got, want)
				}
			}
		})
	}
}

func TestCatalogCompositeRelayStack(t *testing.T) {
	mk := func(id int64, typ, v4, v6 string) *Node {
		return &Node{ID: id, NodeType: typ, RelayHost: v4, RelayHostV6: v6}
	}
	hop := func(comp, child int64) *NodeHop { return memberHop(comp, child, "") }

	tests := []struct {
		name                                string
		nodes                               []*Node
		hops                                []*NodeHop
		id                                  int64
		wantEntry, wantEntryV6, wantExitV6 string
	}{
		{
			name:      "single hop composite mirrors that node",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1"), mk(9, "composite", "", "")},
			hops:      []*NodeHop{hop(9, 1)},
			id:        9,
			wantEntry: "10.0.0.1", wantEntryV6: "2001:db8::1", wantExitV6: "2001:db8::1",
		},
		{
			name:      "multi-hop: entry from first, exit v6 from last",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", ""), mk(2, "remote", "10.0.0.2", "2001:db8::2"), mk(9, "composite", "", "")},
			hops:      []*NodeHop{hop(9, 1), hop(9, 2)},
			id:        9,
			wantEntry: "10.0.0.1", wantExitV6: "2001:db8::2",
		},
		{
			name:  "composite with no hops stays empty",
			nodes: []*Node{mk(9, "composite", "", "")},
			id:    9,
		},
		{
			// 物理节点的地址就在它自己的行上，派生值里没有。
			name:  "physical node has no derived stack",
			nodes: []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1")},
			id:    1,
		},
		{
			name: "nested composite: entry from deep first leaf, exit v6 from deep last leaf",
			nodes: []*Node{
				mk(1, "remote", "10.0.0.1", ""), mk(2, "remote", "10.0.0.2", "2001:db8::2"), mk(3, "remote", "10.0.0.3", "2001:db8::3"),
				mk(8, "composite", "", ""), mk(9, "composite", "", ""),
			},
			hops:      []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			id:        9,
			wantEntry: "10.0.0.1", wantExitV6: "2001:db8::3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := catalogOf(tt.nodes, tt.hops, nil)
			d := c.Derive(c.Node(tt.id))
			if d.EntryRelayHost != tt.wantEntry || d.EntryRelayHostV6 != tt.wantEntryV6 || d.ExitRelayHostV6 != tt.wantExitV6 {
				t.Errorf("stack = %q/%q/%q, want %q/%q/%q", d.EntryRelayHost, d.EntryRelayHostV6, d.ExitRelayHostV6,
					tt.wantEntry, tt.wantEntryV6, tt.wantExitV6)
			}
		})
	}
}

func TestCatalogCompositeMembers(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "hk-a", NodeType: "remote"},
		{ID: 2, Name: "hk-b", NodeType: "remote"},
		{ID: 9, Name: "combo", NodeType: "composite"},
		{ID: 7, Name: "solo", NodeType: "remote"},
	}
	c := catalogOf(nodes, []*NodeHop{memberHop(9, 1, "kernel"), memberHop(9, 2, "userspace")}, nil)
	want := []NodeMember{
		{NodeID: 1, Name: "hk-a", NodeType: "remote", Mode: "kernel"},
		{NodeID: 2, Name: "hk-b", NodeType: "remote", Mode: "userspace"},
	}
	if got := c.Derive(c.Node(9)).Members; !reflect.DeepEqual(got, want) {
		t.Errorf("composite members = %+v, want %+v", got, want)
	}
	if got := c.Derive(c.Node(7)).Members; got != nil {
		t.Errorf("single node solo should have no members, got %+v", got)
	}
}

// A composite whose member is itself a composite flattens recursively to the
// physical leaf chain — inner composites are expanded away, never shown as
// members, and each leaf keeps the mode of the innermost composite that owns it.
func TestCatalogCompositeMembersNested(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "a", NodeType: "remote"},
		{ID: 2, Name: "b", NodeType: "remote"},
		{ID: 3, Name: "x", NodeType: "remote"},
		{ID: 8, Name: "inner", NodeType: "composite"},
		{ID: 9, Name: "outer", NodeType: "composite"},
	}
	c := catalogOf(nodes, []*NodeHop{
		memberHop(9, 8, "kernel"),    // outer -> inner (mode dormant: inner is a black box)
		memberHop(9, 3, "userspace"), // outer -> x
		memberHop(8, 1, "kernel"),    // inner -> a
		memberHop(8, 2, "userspace"), // inner -> b
	}, nil)
	if got := c.Derive(c.Node(8)).Members; len(got) != 2 || got[0].NodeID != 1 || got[1].NodeID != 2 {
		t.Fatalf("inner members = %+v, want [a b]", got)
	}
	want := []NodeMember{
		{NodeID: 1, Name: "a", NodeType: "remote", Mode: "kernel"},
		{NodeID: 2, Name: "b", NodeType: "remote", Mode: "userspace"},
		{NodeID: 3, Name: "x", NodeType: "remote", Mode: "userspace"},
	}
	if got := c.Derive(c.Node(9)).Members; !reflect.DeepEqual(got, want) {
		t.Errorf("outer members = %+v, want %+v", got, want)
	}
}

func TestCatalogRawTraffic(t *testing.T) {
	mk := func(id int64, typ string) *Node { return &Node{ID: id, NodeType: typ} }
	hop := func(comp, child int64) *NodeHop { return memberHop(comp, child, "") }
	tests := []struct {
		name    string
		nodes   []*Node
		hops    []*NodeHop
		raw     map[int64]int64
		id      int64
		want    int64
		present bool
	}{
		{"composite takes entry physical child's raw", []*Node{mk(1, "remote"), mk(2, "remote"), mk(9, "composite")},
			[]*NodeHop{hop(9, 1), hop(9, 2)}, map[int64]int64{1: 100, 2: 200}, 9, 100, true},
		{"entry never forwarded resolves to 0", []*Node{mk(1, "remote"), mk(9, "composite")},
			[]*NodeHop{hop(9, 1)}, map[int64]int64{}, 9, 0, true},
		{"composite with no hops stays absent", []*Node{mk(9, "composite")}, nil, map[int64]int64{}, 9, 0, false},
		// outer(9) = [inner(8), x(3)]; inner(8) = [a(1), b(2)] => entry is a(1)
		{"nested composite takes deep first physical leaf",
			[]*Node{mk(1, "remote"), mk(2, "remote"), mk(3, "remote"), mk(8, "composite"), mk(9, "composite")},
			[]*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)}, map[int64]int64{1: 42, 2: 7, 3: 999}, 9, 42, true},
		{"physical node keeps its own raw", []*Node{mk(1, "remote"), mk(9, "composite")},
			[]*NodeHop{hop(9, 1)}, map[int64]int64{1: 500}, 1, 500, true},
		{"physical node that never reported stays absent", []*Node{mk(1, "remote")}, nil, map[int64]int64{}, 1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := catalogOf(tt.nodes, tt.hops, tt.raw).RawTrafficForDisplay()[tt.id]
			if got != tt.want || ok != tt.present {
				t.Errorf("raw[%d] = %d (present=%v), want %d (present=%v)", tt.id, got, ok, tt.want, tt.present)
			}
		})
	}
	// 用量快照只认节点自己上报的计数：组合恒为 0，物理节点就是自己的累计。
	c := catalogOf([]*Node{mk(1, "remote"), mk(9, "composite")}, []*NodeHop{hop(9, 1)}, map[int64]int64{1: 500})
	if c.OwnRawBytes(9) != 0 || c.OwnRawBytes(1) != 500 {
		t.Errorf("own raw: composite %d (want 0), physical %d (want 500)", c.OwnRawBytes(9), c.OwnRawBytes(1))
	}
}

// 用户侧只拿得到被授权的节点，而组合的成员通常不在授权之列。目录按库里的全量拓扑
// 展开：拿一行来自授权查询的组合去 Derive，嵌套的组合照样展开到物理节点、成员的名字
// 照样解析得出。
func TestCatalogDerivesAGrantedCompositeOverTheWholeTopology(t *testing.T) {
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
	uid := createTestUser(t, d)
	if err := GrantNode(d, uid, outer.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	granted, _, err := ListNodesForUser(d, uid)
	if err != nil || len(granted) != 1 {
		t.Fatalf("授权查询：%+v err=%v", granted, err)
	}
	c, err := LoadNodeCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	want := []NodeMember{
		{NodeID: a.ID, Name: "a", NodeType: "remote", Mode: "kernel"},
		{NodeID: b.ID, Name: "b", NodeType: "remote", Mode: "userspace"},
		{NodeID: x.ID, Name: "x", NodeType: "remote", Mode: "userspace"},
	}
	if got := c.Derive(granted[0]).Members; !reflect.DeepEqual(got, want) {
		t.Fatalf("成员链：得到 %+v\n期望 %+v", got, want)
	}
}

// 展不开的组合（这里是成员关系成环）没有任何派生属性：离线、没有成员链、没有出入口
// 地址、展示用的原始流量里没有它；同一份目录里展得开的组合不受影响。
func TestUnflattenableCompositeHasNoDerivedAttributes(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "a", NodeType: "remote", Online: 1, RelayHost: "10.0.0.1", RelayHostV6: "2001:db8::1"},
		{ID: 7, Name: "loop-a", NodeType: "composite"},
		{ID: 8, Name: "loop-b", NodeType: "composite"},
		{ID: 9, Name: "ok", NodeType: "composite"},
	}
	c := catalogOf(nodes, []*NodeHop{
		{NodeID: 7, Position: 0, HopNodeID: 1}, {NodeID: 7, Position: 1, HopNodeID: 8},
		{NodeID: 8, Position: 0, HopNodeID: 7},
		{NodeID: 9, Position: 0, HopNodeID: 1},
	}, map[int64]int64{1: 500})
	raw := c.RawTrafficForDisplay()
	for _, id := range []int64{7, 8} {
		if d := c.Derive(c.Node(id)); !reflect.DeepEqual(d, NodeDerived{}) {
			t.Errorf("成环的组合 %d 不应有派生属性，实际 %+v", id, d)
		}
		if _, written := raw[id]; written {
			t.Errorf("成环的组合 %d 不应出现在原始流量里", id)
		}
	}
	ok := c.Derive(c.Node(9))
	if !ok.Online || len(ok.Members) != 1 || ok.EntryRelayHost != "10.0.0.1" || ok.ExitRelayHostV6 != "2001:db8::1" || raw[9] != 500 {
		t.Errorf("展得开的组合应照常得到派生属性，实际 %+v raw=%d", ok, raw[9])
	}
}
