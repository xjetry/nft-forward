package db

import (
	"database/sql"

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
	tx, err := d.Begin()
	if err != nil {
		return chain.Topology{}, err
	}
	defer tx.Rollback()
	nodes, err := ListNodes(tx)
	if err != nil {
		return chain.Topology{}, err
	}
	hops, err := ListAllNodeHops(tx)
	if err != nil {
		return chain.Topology{}, err
	}
	bindings, err := ListAllNodeBindings(tx)
	if err != nil {
		return chain.Topology{}, err
	}
	return topologyOf(nodes, hops, bindings), nil
}

// topologyOf 把节点行、成员行、绑定边换成 chain.Topology。节点行怎样变成 chain.Node
// （是不是组合、可以占哪些位置）只在这里定义，链路推导与节点目录共用。hops 要按
// (node_id, position) 排序（ListAllNodeHops 的次序），每个组合的成员按次序追加。
func topologyOf(nodes []*Node, hops []*NodeHop, bindings []*NodeBinding) chain.Topology {
	topo := chain.Topology{
		Nodes:    make(map[int64]chain.Node, len(nodes)),
		Members:  map[int64][]chain.Member{},
		Bindings: make(map[chain.Edge]string, len(bindings)),
	}
	for _, n := range nodes {
		topo.Nodes[n.ID] = chain.Node{
			ID: n.ID, Name: n.Name,
			Composite: n.NodeType == "composite",
			Seats:     NodeSeats(n.Roles, n.NoDirectExit),
		}
	}
	for _, h := range hops {
		topo.Members[h.NodeID] = append(topo.Members[h.NodeID], chain.Member{NodeID: h.HopNodeID, Mode: h.Mode})
	}
	for _, b := range bindings {
		topo.Bindings[chain.Edge{Up: b.UpstreamNodeID, Down: b.DownstreamNodeID}] = b.Mode
	}
	return topo
}
