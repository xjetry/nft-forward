package db

import (
	"database/sql"
	"log"

	"nft-forward/internal/chain"
)

// NodeSeats 把节点行上的两处设置换算成它在链里可以占的位置：角色掩码 roles 给出
// 入口与中转，no_direct_exit 取反给出链尾。这是“库里怎么存”到 chain.Seat 的唯一
// 换算，LoadTopology 用它填每个节点的 Seats。
func NodeSeats(roles int64, noDirectExit bool) chain.Seat {
	var seats chain.Seat
	if roles&NodeRoleEntry != 0 {
		seats |= chain.SeatEntry
	}
	if roles&NodeRoleVia != 0 {
		seats |= chain.SeatVia
	}
	if !noDirectExit {
		seats |= chain.SeatExit
	}
	return seats
}

// GrantSeats 把一条授权上的角色掩码换算成对节点入口/中转资格的覆盖。roles 为 0
// 表示这条授权不覆盖、沿用节点自身的资格，此时 overrides 为 false。覆盖值整体取代
// 节点自身的入口/中转资格（可以多出、也可以少掉某一位），不涉及链尾资格。
func GrantSeats(roles int64) (seats chain.Seat, overrides bool) {
	if roles == 0 {
		return 0, false
	}
	if roles&NodeRoleEntry != 0 {
		seats |= chain.SeatEntry
	}
	if roles&NodeRoleVia != 0 {
		seats |= chain.SeatVia
	}
	return seats, true
}

// LoadTopology 读出链路推导（internal/chain）要用的拓扑快照：全部节点、组合的成员
// 关系、绑定边。
//
// 三次读放在同一个事务里。Open 把连接数限制为 1，事务从 Begin 到结束独占这条连接，
// 所以三次读之间插不进别的写入，读到的是同一时刻的库。同样因为只有一条连接，
// 调用方不能在自己还开着事务时调用本函数——Begin 会一直等那条连接。
func LoadTopology(d *sql.DB) (chain.Topology, error) {
	topo := chain.Topology{
		Nodes:    map[int64]chain.Node{},
		Members:  map[int64][]chain.Member{},
		Bindings: map[chain.Edge]string{},
	}
	tx, err := d.Begin()
	if err != nil {
		return topo, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, name, node_type, roles, no_direct_exit FROM nodes`)
	if err != nil {
		return topo, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			n            chain.Node
			nodeType     string
			roles        int64
			noDirectExit int
		)
		if err := rows.Scan(&n.ID, &n.Name, &nodeType, &roles, &noDirectExit); err != nil {
			return topo, err
		}
		n.Composite = nodeType == "composite"
		n.Seats = NodeSeats(roles, noDirectExit == 1)
		topo.Nodes[n.ID] = n
	}
	if err := rows.Err(); err != nil {
		return topo, err
	}
	rows.Close()

	// ListAllNodeHops 按 (node_id, position) 排序，所以每个组合的成员是按次序追加的。
	hops, err := ListAllNodeHops(tx)
	if err != nil {
		return topo, err
	}
	for _, h := range hops {
		topo.Members[h.NodeID] = append(topo.Members[h.NodeID], chain.Member{NodeID: h.HopNodeID, Mode: h.Mode})
	}

	bindings, err := ListAllNodeBindings(tx)
	if err != nil {
		return topo, err
	}
	for _, b := range bindings {
		topo.Bindings[chain.Edge{Up: b.UpstreamNodeID, Down: b.DownstreamNodeID}] = b.Mode
	}
	return topo, nil
}

// compositeChains 把 nodes 里的每个组合展开成它的物理成员链，返回 组合 id → 链。
// 拓扑取自库里的全量节点与成员关系，与 nodes 是不是全量无关。展不开的组合（没有
// 成员、成员不存在、成环、超过 chain.MaxFlattenedHops）不在返回值里——它承载不了
// 规则，调用方把它当作没有成员链。读库失败时记日志并返回空表，效果相同。
func compositeChains(d *sql.DB, nodes []*Node) map[int64][]chain.Hop {
	hasComposite := false
	for _, n := range nodes {
		if n.NodeType == "composite" {
			hasComposite = true
			break
		}
	}
	if !hasComposite {
		return nil
	}
	topo, err := LoadTopology(d)
	if err != nil {
		log.Printf("load chain topology: %v", err)
		return nil
	}
	return flattenComposites(topo, nodes)
}

// flattenComposites 是 compositeChains 不读库的那一半。
func flattenComposites(topo chain.Topology, nodes []*Node) map[int64][]chain.Hop {
	chains := map[int64][]chain.Hop{}
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		if hops, err := topo.Flatten(n.ID); err == nil {
			chains[n.ID] = hops
		}
	}
	return chains
}

// 下面四个函数填组合节点的派生属性。组合没有自己的 agent，也没有自己的地址：它的
// 在线状态、出入口地址、成员链、原始流量都从展开后的物理成员推出，库里没有对应的
// 列。四个函数用的是同一份展开（compositeChains）。

// ResolveCompositeOnline 填 nodes 里每个组合的 Online：展开后的每个物理成员都在线
// 且未停用，组合才在线；展不开的组合离线。成员的在线状态取自 nodes，所以 nodes
// 必须是全量节点——不在其中的成员按离线算。
func ResolveCompositeOnline(d *sql.DB, nodes []*Node) {
	resolveCompositeOnline(nodes, compositeChains(d, nodes))
}

func resolveCompositeOnline(nodes []*Node, chains map[int64][]chain.Hop) {
	// 先按库里的值记下每个节点是否可达，再改组合的 Online：判定只看物理成员，
	// 不受本函数对别的组合的改写影响。
	reachable := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		reachable[n.ID] = n.Online == 1 && !n.Disabled
	}
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		leaves := chains[n.ID]
		online := len(leaves) > 0
		for _, leaf := range leaves {
			if !reachable[leaf.NodeID] {
				online = false
				break
			}
		}
		n.Online = 0
		if online {
			n.Online = 1
		}
	}
}

// ResolveCompositeRelayStack 填 nodes 里每个组合的入口地址（EntryRelayHost、
// EntryRelayHostV6，取自第一个物理成员）与出口的 IPv6 地址（ExitRelayHostV6，取自
// 最后一个物理成员）。成员的地址取自 nodes，所以 nodes 必须是全量节点；展不开的
// 组合、以及首尾成员不在 nodes 里的，相应字段保持为空。
func ResolveCompositeRelayStack(d *sql.DB, nodes []*Node) {
	resolveCompositeRelayStack(nodes, compositeChains(d, nodes))
}

func resolveCompositeRelayStack(nodes []*Node, chains map[int64][]chain.Hop) {
	byID := make(map[int64]*Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, n := range nodes {
		leaves := chains[n.ID]
		if n.NodeType != "composite" || len(leaves) == 0 {
			continue
		}
		if first := byID[leaves[0].NodeID]; first != nil {
			n.EntryRelayHost = first.RelayHost
			n.EntryRelayHostV6 = first.RelayHostV6
		}
		if last := byID[leaves[len(leaves)-1].NodeID]; last != nil {
			n.ExitRelayHostV6 = last.RelayHostV6
		}
	}
}

// ResolveCompositeHops 填 nodes 里每个组合的 Hops：展开后的物理成员，按次序，各带
// 组合里给它配的转发模式。成员的名字与类型取自 all，all 必须是全量节点；nodes 可以
// 只是其中一部分——用户侧只拿得到被授权的节点，而组合的成员通常不在授权之列。
// 展不开的组合 Hops 为空。
func ResolveCompositeHops(d *sql.DB, nodes, all []*Node) {
	resolveCompositeHops(nodes, compositeChains(d, nodes), all)
}

func resolveCompositeHops(nodes []*Node, chains map[int64][]chain.Hop, all []*Node) {
	byID := make(map[int64]*Node, len(all))
	for _, n := range all {
		byID[n.ID] = n
	}
	for _, n := range nodes {
		leaves := chains[n.ID]
		if n.NodeType != "composite" || len(leaves) == 0 {
			continue
		}
		n.Hops = make([]NodeChildHop, len(leaves))
		for i, leaf := range leaves {
			n.Hops[i] = NodeChildHop{NodeID: leaf.NodeID, Mode: leaf.Mode}
			if member := byID[leaf.NodeID]; member != nil {
				n.Hops[i].Name = member.Name
				n.Hops[i].NodeType = member.NodeType
			}
		}
	}
}

// FillCompositeRawTraffic 把 nodes 里每个组合在 raw 中的值设成它第一个物理成员的
// 原始字节数，调用方于是可以直接读 raw[组合 id]。组合自己不转发，这里拿链首那台
// 的原始流量代表整条链的吞吐；链首那台的原始流量里也包含它承载的其他规则，所以
// 这是展示用的近似值。展不开的组合不写 raw；链首从未转发过时读到的是 0，与物理
// 节点没有记录时一致。
func FillCompositeRawTraffic(d *sql.DB, nodes []*Node, raw map[int64]int64) {
	fillCompositeRawTraffic(nodes, compositeChains(d, nodes), raw)
}

func fillCompositeRawTraffic(nodes []*Node, chains map[int64][]chain.Hop, raw map[int64]int64) {
	for _, n := range nodes {
		leaves := chains[n.ID]
		if n.NodeType != "composite" || len(leaves) == 0 {
			continue
		}
		raw[n.ID] = raw[leaves[0].NodeID]
	}
}
