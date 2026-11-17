package chain

import (
	"reflect"
	"strings"
	"testing"
)

// 测试拓扑。物理节点：
//
//	1 e1   入口            2 e2   入口（不许做链尾）
//	3 v1   中转            4 v2   中转
//	5 x    既非入口也非中转
//
// 组合：
//
//	10 C1 = [1(userspace) → 3(kernel)]          入口 + 中转
//	11 C2 = [10 → 4(userspace)]                 入口；嵌套了 C1
//	12 CT = [4(kernel) → 2(userspace)]          中转；最后一个成员不许做链尾
//	13 C0 = [1(userspace)]                      入口；只有一个成员
//
// 绑定边（上游 → 下游）：1→3 userspace，3→4 kernel，1→10 kernel，10→4 kernel，
// 1→12 userspace，12→3 userspace。
func testTopology() Topology {
	both := SeatEntry | SeatVia | SeatExit
	return Topology{
		Nodes: map[int64]Node{
			1:  {ID: 1, Name: "e1", Seats: SeatEntry | SeatExit},
			2:  {ID: 2, Name: "e2", Seats: SeatEntry},
			3:  {ID: 3, Name: "v1", Seats: SeatVia | SeatExit},
			4:  {ID: 4, Name: "v2", Seats: SeatVia | SeatExit},
			5:  {ID: 5, Name: "x", Seats: SeatExit},
			10: {ID: 10, Name: "C1", Composite: true, Seats: both},
			11: {ID: 11, Name: "C2", Composite: true, Seats: SeatEntry | SeatExit},
			12: {ID: 12, Name: "CT", Composite: true, Seats: SeatVia | SeatExit},
			13: {ID: 13, Name: "C0", Composite: true, Seats: SeatEntry | SeatExit},
		},
		Members: map[int64][]Member{
			10: {{NodeID: 1, Mode: ModeUserspace}, {NodeID: 3, Mode: ModeKernel}},
			11: {{NodeID: 10, Mode: ModeKernel}, {NodeID: 4, Mode: ModeUserspace}},
			12: {{NodeID: 4, Mode: ModeKernel}, {NodeID: 2, Mode: ModeUserspace}},
			13: {{NodeID: 1, Mode: ModeUserspace}},
		},
		Bindings: map[Edge]string{
			{Up: 1, Down: 3}:  ModeUserspace,
			{Up: 3, Down: 4}:  ModeKernel,
			{Up: 1, Down: 10}: ModeKernel,
			{Up: 10, Down: 4}: ModeKernel,
			{Up: 1, Down: 12}: ModeUserspace,
			{Up: 12, Down: 3}: ModeUserspace,
		},
	}
}

func hop(node int64, mode string, via int64) Hop {
	return Hop{NodeID: node, Mode: mode, ViaNodeID: via}
}

func TestFlatten(t *testing.T) {
	topo := testTopology()
	cases := []struct {
		name string
		node int64
		want []Hop
	}{
		{"物理节点是它自己这一跳，模式留空", 1, []Hop{hop(1, "", 1)}},
		{"组合按成员次序展开，各取组合里配的模式", 10, []Hop{hop(1, ModeUserspace, 10), hop(3, ModeKernel, 10)}},
		// 嵌套：C1 的两个成员取 C1 里配的模式（C2 给 C1 这个成员配的 kernel 不落到
		// 任何一跳上），所属逻辑节点一律是最外层的 C2。
		{"嵌套的组合透明展开", 11, []Hop{hop(1, ModeUserspace, 11), hop(3, ModeKernel, 11), hop(4, ModeUserspace, 11)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := topo.Flatten(c.node)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("得到 %+v，期望 %+v", got, c.want)
			}
		})
	}
}

func TestFlattenRefusesWhatItCannotExpand(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Topology)
		node    int64
		wantErr string
	}{
		{"节点不在拓扑里", func(*Topology) {}, 99, "节点不存在"},
		{"组合没有成员", func(tp *Topology) { tp.Members[10] = nil }, 10, "组合节点无子节点"},
		{"成员不在拓扑里", func(tp *Topology) { tp.Members[10] = []Member{{NodeID: 99}} }, 10, "子节点不存在"},
		{"成员关系成环", func(tp *Topology) { tp.Members[10] = []Member{{NodeID: 11}} }, 11, "组合节点存在环引用"},
		{"嵌套组合里的错误向外传", func(tp *Topology) { tp.Members[10] = nil }, 11, "组合节点无子节点"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			topo := testTopology()
			c.mutate(&topo)
			hops, err := topo.Flatten(c.node)
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("期望错误 %q，实际 hops=%+v err=%v", c.wantErr, hops, err)
			}
			if hops != nil {
				t.Fatalf("展开失败时不应返回部分结果，实际 %+v", hops)
			}
		})
	}
}

// 物理跳数的上限按整个逻辑节点累计，嵌套里的每一台都算；恰好等于上限可以，多一台
// 就拒绝。
func TestFlattenCapsPhysicalHopsAcrossNesting(t *testing.T) {
	build := func(leaves int) Topology {
		topo := Topology{Nodes: map[int64]Node{}, Members: map[int64][]Member{}}
		// 一半的物理节点放在内层组合 200 里，其余直接挂在外层组合 100 上。
		topo.Nodes[100] = Node{ID: 100, Name: "outer", Composite: true}
		topo.Nodes[200] = Node{ID: 200, Name: "inner", Composite: true}
		topo.Members[100] = []Member{{NodeID: 200}}
		for i := 1; i <= leaves; i++ {
			id := int64(i)
			topo.Nodes[id] = Node{ID: id, Name: "p"}
			owner := int64(100)
			if i%2 == 0 {
				owner = 200
			}
			topo.Members[owner] = append(topo.Members[owner], Member{NodeID: id, Mode: ModeKernel})
		}
		return topo
	}
	atCap := build(MaxFlattenedHops)
	if hops, err := atCap.Flatten(100); err != nil || len(hops) != MaxFlattenedHops {
		t.Fatalf("恰好 %d 台应能展开，实际 %d 台 err=%v", MaxFlattenedHops, len(hops), err)
	}
	over := build(MaxFlattenedHops + 1)
	if _, err := over.Flatten(100); err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("%d 台应因超过上限被拒绝，实际 err=%v", MaxFlattenedHops+1, err)
	}
}

func TestValidateMembers(t *testing.T) {
	topo := testTopology()
	cases := []struct {
		name    string
		root    int64
		members []int64
		wantErr string
	}{
		{"物理成员", 10, []int64{1, 3, 4}, ""},
		{"嵌一个不会绕回来的组合", 11, []int64{10, 12}, ""},
		{"新建组合可以引用任何现有组合", 0, []int64{10, 11, 12}, ""},
		{"引用自己", 10, []int64{1, 10}, "组合节点不能引用自己"},
		{"成员不存在", 10, []int64{1, 99}, "子节点 99 不存在"},
		// C2 的成员里有 C1，再让 C1 引用 C2 就成环；错误里写出绕回来的路径。
		{"经嵌套绕回自己", 10, []int64{11}, "组合节点存在环引用：C2 → C1"},
		// 物理节点 1 是组合 C1 的成员；给它写成员，报的是“它不是组合”，而不是从 C1
		// 绕回它的环。
		{"给物理节点写成员", 1, []int64{10}, "非组合节点：只有组合节点有成员"},
		{"不存在的节点", 99, []int64{1}, "非组合节点：只有组合节点有成员"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := topo.ValidateMembers(c.root, c.members)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.wantErr {
				t.Fatalf("期望错误 %q，实际 %q", c.wantErr, got)
			}
		})
	}
}

func TestDeriveChains(t *testing.T) {
	topo := testTopology()
	cases := []struct {
		name string
		req  Request
		want []Hop
	}{
		{
			"裸单点：只有入口这一跳",
			Request{Entry: 1},
			[]Hop{hop(1, "", 1)},
		},
		{
			// 上一段是物理节点：衔接那条腿用绑定边 1→3 的模式。
			"物理入口接物理中转",
			Request{Entry: 1, Vias: []int64{3}},
			[]Hop{hop(1, ModeUserspace, 1), hop(3, "", 3)},
		},
		{
			"两层中转，各取各的绑定边",
			Request{Entry: 1, Vias: []int64{3, 4}},
			[]Hop{hop(1, ModeUserspace, 1), hop(3, ModeKernel, 3), hop(4, "", 4)},
		},
		{
			// 组合做入口：段内的腿用组合里配的；最后一个成员是链尾，它配的模式不生效。
			"组合入口，没有中转",
			Request{Entry: 10},
			[]Hop{hop(1, ModeUserspace, 10), hop(3, "", 10)},
		},
		{
			// 上一段是组合：衔接那条腿用组合给最后一个成员配的 kernel——这里它与绑定边
			// 10→4 的模式恰好相同，TestDeriveJunctionAfterCompositeIgnoresTheBindingMode
			// 把两者区分开。
			"组合入口接物理中转",
			Request{Entry: 10, Vias: []int64{4}},
			[]Hop{hop(1, ModeUserspace, 10), hop(3, ModeKernel, 10), hop(4, "", 4)},
		},
		{
			// 物理入口接组合中转，再接物理中转。1→12 是绑定边的 userspace；12→3 这条
			// 腿由组合 CT 的最后一个成员发起，用 CT 给它配的 userspace。
			"组合在链中间：它自己决定怎么送出去",
			Request{Entry: 1, Vias: []int64{12, 3}},
			[]Hop{hop(1, ModeUserspace, 1), hop(4, ModeKernel, 12), hop(2, ModeUserspace, 12), hop(3, "", 3)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := topo.Derive(c.req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("得到 %+v\n期望 %+v", got, c.want)
			}
		})
	}
}

// 上一段是组合时，衔接那条腿用的是组合里配的模式而不是绑定边的：把两者配成不同的
// 值，结果必须跟着组合走。
func TestDeriveJunctionAfterCompositeIgnoresTheBindingMode(t *testing.T) {
	topo := testTopology()
	topo.Members[10][1].Mode = ModeUserspace          // C1 给最后一个成员配的
	topo.Bindings[Edge{Up: 10, Down: 4}] = ModeKernel // 绑定边上配的
	hops, err := topo.Derive(Request{Entry: 10, Vias: []int64{4}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hops[1]; got != hop(3, ModeUserspace, 10) {
		t.Fatalf("组合最后一个成员发起的那条腿应用组合里配的 userspace，实际 %+v", got)
	}
}

func TestDeriveRefusals(t *testing.T) {
	cases := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{"入口不存在", Request{Entry: 99}, "节点不存在"},
		{"入口没有入口资格", Request{Entry: 3}, "节点 v1 不是入口"},
		{"中转层不存在", Request{Entry: 1, Vias: []int64{99}}, "中转节点不存在"},
		{"中转层没有中转资格", Request{Entry: 1, Vias: []int64{5}}, "节点 x 不是中转"},
		{"中转层没有绑定到上一层", Request{Entry: 1, Vias: []int64{4}}, "中转 v2 未绑定到所选上游"},
		// 绑定边有方向：3→4 存在，4→3 不存在。
		{"绑定边不能反着用", Request{Entry: 1, Vias: []int64{3, 4, 3}}, "中转 v1 未绑定到所选上游"},
		{"链尾的物理节点不许直接转发", Request{Entry: 2}, "节点 e2 禁止直接转发，必须在其后选择线路层"},
		// 逻辑节点 CT 自己有链尾资格，但判定看的是它展开后的最后一台 e2。
		{"组合收尾时看它的最后一个物理成员", Request{Entry: 1, Vias: []int64{12}}, "节点 e2 禁止直接转发，必须在其后选择线路层"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hops, err := testTopology().Derive(c.req)
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("期望错误 %q，实际 hops=%+v err=%v", c.wantErr, hops, err)
			}
		})
	}
}

// 不许做链尾的节点只要不在链尾就可以用：e2 做入口、后面接了中转层，链就成立。
func TestDeriveAllowsANoExitNodeAwayFromTheTail(t *testing.T) {
	topo := testTopology()
	topo.Bindings[Edge{Up: 2, Down: 3}] = ModeKernel
	hops, err := topo.Derive(Request{Entry: 2, Vias: []int64{3}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Hop{hop(2, ModeKernel, 2), hop(3, "", 3)}
	if !reflect.DeepEqual(hops, want) {
		t.Fatalf("得到 %+v，期望 %+v", hops, want)
	}
}

// 入口/中转资格可以按授权覆盖，覆盖值整体取代节点自身的；链尾资格不受覆盖影响。
func TestDeriveSeatOverrides(t *testing.T) {
	topo := testTopology()
	// v1 自身只是中转。授权把它覆盖成入口：可以做入口了，同时不再是中转。
	asEntry := map[int64]Seat{3: SeatEntry}
	if _, err := topo.Derive(Request{Entry: 3, SeatOverrides: asEntry}); err != nil {
		t.Fatalf("授权把 v1 覆盖成入口后应能做入口：%v", err)
	}
	if _, err := topo.Derive(Request{Entry: 1, Vias: []int64{3}, SeatOverrides: asEntry}); err == nil || err.Error() != "节点 v1 不是中转" {
		t.Fatalf("覆盖值里没有中转，v1 不应再能做中转，实际 err=%v", err)
	}
	// e2 不许做链尾；覆盖里即使写上 SeatExit 也不改变这一点。
	_, err := topo.Derive(Request{Entry: 2, SeatOverrides: map[int64]Seat{2: SeatEntry | SeatExit}})
	if err == nil || !strings.Contains(err.Error(), "禁止直接转发") {
		t.Fatalf("链尾资格不可按授权覆盖，实际 err=%v", err)
	}
}

// 出口那条腿的模式：显式指定 > 规则现有的 > 留空。
func TestDeriveExitModePrecedence(t *testing.T) {
	topo := testTopology()
	k, u := ModeKernel, ModeUserspace
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"都没给：留空", Request{Entry: 1}, ""},
		{"显式指定盖过现有的", Request{Entry: 1, Exit: ExitChoice{Explicit: u, Current: k}}, u},
		{"单点没指定时沿用现有的", Request{Entry: 1, Exit: ExitChoice{Current: u}}, u},
		{"有中转层、没指定时沿用现有的", Request{Entry: 1, Vias: []int64{3}, Exit: ExitChoice{Current: u}}, u},
		{"有中转层、都没给：留空", Request{Entry: 1, Vias: []int64{3}}, ""},
		{"组合入口都没给：留空", Request{Entry: 10}, ""},
		{"单成员组合入口都没给：留空", Request{Entry: 13}, ""},
		{"组合入口显式指定：盖掉组合给最后一个成员配的", Request{Entry: 10, Exit: ExitChoice{Explicit: u}}, u},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hops, err := topo.Derive(c.req)
			if err != nil {
				t.Fatal(err)
			}
			if got := hops[len(hops)-1].Mode; got != c.want {
				t.Fatalf("出口模式应为 %q，实际 %q", c.want, got)
			}
		})
	}
}

// 同一条取舍规则也用在规则已落库的链上（不重新推导、只改出口模式的编辑）。
func TestExitChoiceApplyOnAStoredChain(t *testing.T) {
	k, u := ModeKernel, ModeUserspace
	cases := []struct {
		name   string
		hops   []Hop
		choice ExitChoice
		want   string
	}{
		{"单点没指定时保持现状", []Hop{hop(1, u, 1)}, ExitChoice{Current: u}, u},
		{"单点显式指定盖过现状", []Hop{hop(1, k, 1)}, ExitChoice{Explicit: u, Current: k}, u},
		{"多跳链没指定时保持现状", []Hop{hop(1, k, 1), hop(3, u, 3)}, ExitChoice{Current: u}, u},
		{"显式指定对任何链都生效", []Hop{hop(1, u, 10), hop(3, k, 10)}, ExitChoice{Explicit: u, Current: k}, u},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := append([]Hop(nil), c.hops...)
			c.choice.Apply(c.hops)
			if got := c.hops[len(c.hops)-1].Mode; got != c.want {
				t.Fatalf("出口模式应为 %q，实际 %q", c.want, got)
			}
			if !reflect.DeepEqual(c.hops[:len(c.hops)-1], before[:len(before)-1]) {
				t.Fatalf("只应改最后一跳，实际 %+v（原 %+v）", c.hops, before)
			}
		})
	}
	ExitChoice{Explicit: u}.Apply(nil) // 空链不应 panic
}

func TestCheckExitOnAHandBuiltChain(t *testing.T) {
	topo := testTopology()
	if err := topo.CheckExit([]Hop{hop(2, "", 2), hop(3, "", 3)}); err != nil {
		t.Fatalf("链尾是可以直接转发的节点，不应拒绝：%v", err)
	}
	if err := topo.CheckExit([]Hop{hop(1, "", 1), hop(2, "", 2)}); err == nil {
		t.Fatal("链尾是不许直接转发的节点，应拒绝")
	}
	if err := topo.CheckExit(nil); err != nil {
		t.Fatalf("空链不在这里报错：%v", err)
	}
	if err := topo.CheckExit([]Hop{hop(99, "", 99)}); err != nil {
		t.Fatalf("节点不在快照里不在这里报错：%v", err)
	}
}

func TestInterNodeLegMode(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", ModeUserspace, false},
		{"  ", ModeUserspace, false},
		{"kernel", ModeKernel, false},
		{" Userspace ", ModeUserspace, false},
		{"relay", "", true},
	}
	for _, c := range cases {
		got, err := InterNodeLegMode(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("InterNodeLegMode(%q) = %q, %v；期望 %q，出错=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestEnforceModes(t *testing.T) {
	k, u := ModeKernel, ModeUserspace
	cases := []struct {
		name    string
		proto   string
		entryV6 bool
		in      []string
		want    []string
	}{
		{"空与认不出的按 kernel", "tcp", false, []string{"", "relay", u}, []string{k, k, u}},
		{"udp 每一跳都是 kernel", "udp", false, []string{u, "", u}, []string{k, k, k}},
		{"tcp+udp 不强制", "tcp+udp", false, []string{u, k}, []string{u, k}},
		{"入口对外提供 v6 时入口那一跳是 userspace", "tcp", true, []string{k, k}, []string{u, k}},
		{"入口对外提供 v6：单跳", "tcp", true, []string{""}, []string{u}},
		{"空链", "tcp", true, nil, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := append([]string(nil), c.in...)
			got := EnforceModes(c.proto, c.entryV6, c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
			if !reflect.DeepEqual(c.in, in) {
				t.Fatalf("不应改动传入的切片，实际变成 %v", c.in)
			}
		})
	}
}
