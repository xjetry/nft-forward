// Package chain 推导一条转发链路由哪些物理节点、按什么次序、各用什么转发模式组成。
//
// 它不读库：输入是一份拓扑快照（Topology），输出是物理跳序列或拒绝原因。建规则与
// 改规则时落库的链、节点列表里组合的成员链与在线状态、探测走的链，用的都是这里
// 的同一份展开，不会各算各的。
//
// 词汇：
//
//   - 逻辑节点：规则引用的节点——入口节点，或中转层里的一层。它可以是一台物理
//     节点，也可以是一个组合。授权、配额、计价都记在逻辑节点上。
//   - 物理节点：跑 agent、真正转发流量的那一台。组合不是物理节点。
//   - 段：一个逻辑节点展开出的那一串物理跳。段里每一跳的 ViaNodeID 都是这个逻辑
//     节点。
//   - 腿：相邻两跳之间，或末跳到目标之间的一段转发。腿的转发模式记在发起它的那
//     一跳上（Hop.Mode）。
//   - 裸单点：只有一跳、并且这一跳就是规则引用的那个物理节点本身的链。
package chain

import (
	"errors"
	"fmt"
	"strings"
)

// 转发模式。kernel 是内核 DNAT，userspace 是用户态中继。
const (
	ModeKernel    = "kernel"
	ModeUserspace = "userspace"
)

// Seat 是一个节点在链里可以占的位置，可以按位组合。
//
// 三个位置按不同的身份判定，这是它们唯一的差别：入口与中转看的是规则引用的那个
// 逻辑节点，而且可以按授权覆盖（Request.SeatOverrides）；出口看的是链尾真正发起
// 出口那条腿的物理节点，不可覆盖——一个组合能不能收尾，取决于它展开后的最后一台
// 机器。
type Seat uint8

const (
	// SeatEntry：可以做链首，即规则的入口。
	SeatEntry Seat = 1 << iota
	// SeatVia：可以做中转层里的一层。
	SeatVia
	// SeatExit：可以做链尾，由它发起到目标的那条腿。
	SeatExit
)

// Node 是拓扑快照里的一个节点。
type Node struct {
	ID   int64
	Name string
	// Composite 为真表示这是组合：它自己不转发，由 Topology.Members 里的成员按序
	// 组成。
	Composite bool
	Seats     Seat
}

// Member 是组合的一个成员。Mode 是从这个成员出发的那条腿的转发模式；组合最后一个
// 成员的 Mode 只在组合处于链中间时生效（见 Derive）。
type Member struct {
	NodeID int64
	Mode   string
}

// Edge 是一条绑定边：下游节点 Down 可以接在上游节点 Up 之后做中转层。
type Edge struct{ Up, Down int64 }

// Topology 是推导所需的全部输入，由调用方一次读齐。
type Topology struct {
	Nodes map[int64]Node
	// Members：组合 id → 它的有序成员。成员可以又是组合。
	Members map[int64][]Member
	// Bindings：绑定边 → 这条边上衔接那条腿的转发模式。
	Bindings map[Edge]string
}

// Hop 是物理链上的一跳。
type Hop struct {
	NodeID int64
	// Mode 是从这一跳出发的那条腿的转发模式。空表示推导没有为它指定，由
	// EnforceModes 落成默认值。
	Mode string
	// ViaNodeID 是这一跳所属的逻辑节点。
	ViaNodeID int64
}

// MaxFlattenedHops 是一个逻辑节点展开后物理跳数的上限。嵌套可以让跳数成倍增长
// （C2=[C1,C1]、C3=[C2,C2]……展开成 2^n 台），而几十跳的转发链本身没有意义。成员
// 关系成环在写入时就被拒绝（ValidateMembers），所以展开总会结束；这个上限管的是
// 不成环但指数膨胀的嵌套。每一层至少贡献一跳，所以不需要另设深度上限。
const MaxFlattenedHops = 32

// Flatten 把逻辑节点 nodeID 展开成它的段。物理节点展开成它自己这一跳（Mode 留空）；
// 组合递归展开到物理节点为止——嵌套的组合是透明的，每一跳取“直接包含它的那个
// 组合”里配置的 Mode，ViaNodeID 则一律是最外层的 nodeID。
//
// 展开失败（节点不存在、组合没有成员、成员不存在、成环、超过 MaxFlattenedHops）
// 返回错误，不返回部分结果：一个展不开的逻辑节点不能承载规则，展示时也没有可信
// 的成员链可言。
func (t Topology) Flatten(nodeID int64) ([]Hop, error) {
	hops, _, err := t.segment(nodeID)
	return hops, err
}

// segment 是 Flatten 外加“nodeID 本身是不是组合”。Derive 在两段衔接处要按上一段
// 是不是组合来决定那条腿的模式。
func (t Topology) segment(nodeID int64) (hops []Hop, composite bool, err error) {
	n, ok := t.Nodes[nodeID]
	if !ok {
		return nil, false, errors.New("节点不存在")
	}
	if !n.Composite {
		return []Hop{{NodeID: nodeID, ViaNodeID: nodeID}}, false, nil
	}
	count := 0
	hops, err = t.flatten(nodeID, nodeID, map[int64]bool{}, &count)
	if err != nil {
		return nil, true, err
	}
	return hops, true, nil
}

// flatten 展开组合 compositeID。onPath 是当前递归路径上的组合，用来发现环：写入
// 时已拒绝成环，这里仍然检查，是为了对库里可能存在的陈旧数据也保证终止。count
// 在整次展开里累计物理跳数。
func (t Topology) flatten(compositeID, via int64, onPath map[int64]bool, count *int) ([]Hop, error) {
	if onPath[compositeID] {
		return nil, errors.New("组合节点存在环引用")
	}
	onPath[compositeID] = true
	defer delete(onPath, compositeID)

	members := t.Members[compositeID]
	if len(members) == 0 {
		return nil, errors.New("组合节点无子节点")
	}
	var hops []Hop
	for _, m := range members {
		child, ok := t.Nodes[m.NodeID]
		if !ok {
			return nil, errors.New("子节点不存在")
		}
		if child.Composite {
			sub, err := t.flatten(m.NodeID, via, onPath, count)
			if err != nil {
				return nil, err
			}
			hops = append(hops, sub...)
			continue
		}
		*count++
		if *count > MaxFlattenedHops {
			return nil, fmt.Errorf("组合展平后物理跳数超过上限 %d", MaxFlattenedHops)
		}
		hops = append(hops, Hop{NodeID: m.NodeID, Mode: m.Mode, ViaNodeID: via})
	}
	return hops, nil
}

// ValidateMembers 检查要写给组合 rootID 的成员列表：成员必须存在，并且不能让成员
// 关系成环。rootID 为 0 表示新建组合——此刻还没有节点能引用它，不可能绕回它。
// 组合里嵌组合是允许的，被拒绝的只有环：有环时 Flatten 无法终止。
func (t Topology) ValidateMembers(rootID int64, members []int64) error {
	for _, id := range members {
		if id == rootID {
			return errors.New("组合节点不能引用自己")
		}
		n, ok := t.Nodes[id]
		if !ok {
			return fmt.Errorf("子节点 %d 不存在", id)
		}
		if !n.Composite {
			continue
		}
		// 成员是组合：从它出发沿成员关系若能走到 rootID，写入后就成环。
		if path, reaches := t.memberPath(id, rootID, map[int64]bool{}); reaches {
			return fmt.Errorf("组合节点存在环引用：%s", strings.Join(path, " → "))
		}
	}
	return nil
}

// memberPath 沿成员关系从 from 找 target，找到时返回途经节点的名字。visited 防止
// 库里已有的环让搜索不终止。
func (t Topology) memberPath(from, target int64, visited map[int64]bool) ([]string, bool) {
	n, ok := t.Nodes[from]
	if !ok {
		return nil, false
	}
	if from == target {
		return []string{n.Name}, true
	}
	if !n.Composite || visited[from] {
		return nil, false
	}
	visited[from] = true
	for _, m := range t.Members[from] {
		if path, ok := t.memberPath(m.NodeID, target, visited); ok {
			return append([]string{n.Name}, path...), true
		}
	}
	return nil, false
}

// ExitChoice 是一次请求对“出口那条腿（链尾到目标）用什么转发模式”的各级意愿。
// 按 Explicit、Legacy、Current 的次序取第一个生效的；都没有时留空，由 EnforceModes
// 落成默认值。
type ExitChoice struct {
	// Explicit 是请求显式指定的出口模式。
	Explicit string
	// Legacy 是请求里的旧字段 mode。它只对裸单点链算作对出口的指定，别的链上一律
	// 忽略：仍在发送这个字段的客户端不应因此改掉组合或多层链的出口。
	Legacy string
	// Current 是这条规则现在的出口模式。调用方只在“这次编辑没有换入口”时填它：
	// 没有指定就沿用现状，免得不带出口模式的请求把 userspace 出口改回默认值。
	Current string
}

// Apply 按 c 决定链 hops 出口那条腿的模式，写到最后一跳上。链是推导出来的还是规则
// 已落库的快照，都用这同一条规则。
func (c ExitChoice) Apply(hops []Hop) {
	if len(hops) == 0 {
		return
	}
	mode := c.Explicit
	if mode == "" && bareSingle(hops) {
		mode = c.Legacy
	}
	if mode == "" {
		mode = c.Current
	}
	hops[len(hops)-1].Mode = mode
}

// bareSingle 报告 hops 是不是裸单点链。只看跳数不够：只有一个成员的组合展开后也
// 只有一跳，但那一跳的 ViaNodeID 是组合的 id、NodeID 是物理成员的 id，两者不同。
func bareSingle(hops []Hop) bool {
	return len(hops) == 1 && hops[0].ViaNodeID == hops[0].NodeID
}

// Request 是一次链路推导的输入：规则选了哪个入口、哪些中转层，以及对出口模式的
// 意愿。
type Request struct {
	// Entry 是入口逻辑节点。
	Entry int64
	// Vias 是有序的中转层，每一项是一个逻辑节点。
	Vias []int64
	// Exit 决定出口那条腿的转发模式。
	Exit ExitChoice
	// SeatOverrides 是按授权覆盖的位置：出现在这里的节点，它的入口/中转资格以这里
	// 为准，不看节点自身的。只有 SeatEntry 与 SeatVia 两位有意义。
	SeatOverrides map[int64]Seat
}

// seats 返回逻辑节点 n 在这次请求里的入口/中转资格。
func (r Request) seats(n Node) Seat {
	if ov, ok := r.SeatOverrides[n.ID]; ok {
		return ov
	}
	return n.Seats
}

// Derive 推导出规则的物理链：入口段，后面依次接每个中转层的段。
//
// 资格：入口必须有 SeatEntry；每个中转层必须有 SeatVia，并且有一条从上一层到它的
// 绑定边；链尾的物理节点必须有 SeatExit。这里是权威检查——界面上的选择器只是按
// 同样的条件做了筛选，授权本身不代表资格。授权与配额由调用方另行检查。
//
// 腿的转发模式：段内部的腿用组合里配置的；两段衔接的那条腿，上一段是物理节点时
// 用绑定边的模式，上一段是组合时用组合给自己最后一个成员配的模式（组合自己决定
// 怎么送出去，绑定边只用来判断能不能接）；出口那条腿由 req.Exit 决定。
func (t Topology) Derive(req Request) ([]Hop, error) {
	entry, ok := t.Nodes[req.Entry]
	if !ok {
		return nil, errors.New("节点不存在")
	}
	if req.seats(entry)&SeatEntry == 0 {
		return nil, fmt.Errorf("节点 %s 不是入口", entry.Name)
	}
	hops, prevComposite, err := t.segment(req.Entry)
	if err != nil {
		return nil, err
	}

	prev := req.Entry
	for _, viaID := range req.Vias {
		via, ok := t.Nodes[viaID]
		if !ok {
			return nil, errors.New("中转节点不存在")
		}
		if req.seats(via)&SeatVia == 0 {
			return nil, fmt.Errorf("节点 %s 不是中转", via.Name)
		}
		edgeMode, bound := t.Bindings[Edge{Up: prev, Down: viaID}]
		if !bound {
			return nil, fmt.Errorf("中转 %s 未绑定到所选上游", via.Name)
		}
		seg, viaComposite, err := t.segment(viaID)
		if err != nil {
			return nil, err
		}
		if !prevComposite {
			hops[len(hops)-1].Mode = edgeMode
		}
		hops = append(hops, seg...)
		prev, prevComposite = viaID, viaComposite
	}

	if err := t.CheckExit(hops); err != nil {
		return nil, err
	}
	req.Exit.Apply(hops)
	return hops, nil
}

// CheckExit 检查链 hops 的最后一个物理节点能不能做链尾。Derive 自己会调它；调用方
// 手里的链不是 Derive 推出来的时（管理员直接给出物理跳序列）也必须调它——不许
// 直接转发是硬约束，哪条建链路径都不能绕过。最后一跳的节点不在快照里时不在这里
// 报错，留给落库时的节点存在性检查。
func (t Topology) CheckExit(hops []Hop) error {
	if len(hops) == 0 {
		return nil
	}
	last, ok := t.Nodes[hops[len(hops)-1].NodeID]
	if !ok || last.Seats&SeatExit != 0 {
		return nil
	}
	return fmt.Errorf("节点 %s 禁止直接转发，必须在其后选择线路层", last.Name)
}

// InterNodeLegMode 校验并归一组合成员或绑定边上配置的转发模式，即节点到节点的那
// 些腿。没填按 userspace。它与出口那条腿的默认值（kernel，见 EnforceModes）相反，
// 所以两处不能共用一个“空值归一”：写组合成员与绑定边的入口都必须经这里，不能
// 把空值留给 EnforceModes。
func InterNodeLegMode(raw string) (string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case "":
		return ModeUserspace, nil
	case ModeKernel, ModeUserspace:
		return mode, nil
	default:
		return "", errors.New("转发模式必须为 kernel 或 userspace")
	}
}

// EnforceModes 把推导出来的各跳模式落成最终写进数据面的值，返回与 modes 等长的新
// 切片。modes[i] 是第 i 跳的模式，第 0 跳是入口。
//
// 依次是：
//
//  1. 没指定的（空，或不是两种模式之一）按 kernel。Derive 推出来的链里，到这里还
//     空着的只有出口那条腿：节点之间的腿取自组合成员与绑定边，两者入库时都已经
//     InterNodeLegMode 落值。管理员直接给出的物理跳序列里，任何一跳都可能空着。
//  2. udp 规则每一跳都是 kernel：用户态中继只转发 TCP。
//  3. entryOffersV6 为真时入口那一跳是 userspace。入口对外提供 IPv6 地址时，客户端
//     从 v6 进来、往后走 v4，内核 DNAT 不能跨地址族，只有用户态中继接得住。调用方
//     只在“入口节点有 IPv6 地址且规则是纯 tcp”时传真，所以它与第 2 条不会同时
//     生效。
func EnforceModes(proto string, entryOffersV6 bool, modes []string) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		if m != ModeUserspace {
			m = ModeKernel
		}
		if proto == "udp" {
			m = ModeKernel
		}
		out[i] = m
	}
	if entryOffersV6 && len(out) > 0 {
		out[0] = ModeUserspace
	}
	return out
}
