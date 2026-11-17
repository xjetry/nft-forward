package db

import (
	"database/sql"

	"nft-forward/internal/chain"
)

// NodeCatalog 是某一时刻的全部节点行、组合的成员关系、各节点的原始流量。节点的派生
// 属性只从它算：组合没有自己的 agent，也没有自己的地址，它的在线状态、出入口地址、
// 成员链、原始流量都要从展开后的物理成员推出，而成员通常不在调用方手里那一小批节点
// 里（用户侧只拿得到被授权的组合，拿不到它的成员）。目录总是全量加载、没有过滤参数，
// “按全量节点算”这个前提由它的构造方式保证，不靠调用方记得传全量。
type NodeCatalog struct {
	nodes []*Node
	byID  map[int64]*Node
	// chains 是能展开的组合 → 物理成员链。展不开的组合（没有成员、成员不存在、成环、
	// 超过 chain.MaxFlattenedHops）不在其中：它承载不了规则，也就没有派生属性。
	chains map[int64][]chain.Hop
	// raw 是物理节点自己上报的原始字节累计，键只有上报过的节点。
	raw map[int64]int64
}

// LoadNodeCatalog 在一个事务里读出节点行、成员关系与原始流量，三者是同一时刻的库。
// 与 LoadTopology 一样：Open 把连接数限制为 1，调用方不能在自己还开着事务时调用。
func LoadNodeCatalog(d *sql.DB) (*NodeCatalog, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	nodes, err := ListNodes(tx)
	if err != nil {
		return nil, err
	}
	hops, err := ListAllNodeHops(tx)
	if err != nil {
		return nil, err
	}
	raw, err := NodeRawTraffic(tx)
	if err != nil {
		return nil, err
	}
	return NewNodeCatalog(nodes, hops, raw), nil
}

// NewNodeCatalog 是 LoadNodeCatalog 不读库的那一半。nodes 必须是全量节点。
func NewNodeCatalog(nodes []*Node, hops []*NodeHop, raw map[int64]int64) *NodeCatalog {
	c := &NodeCatalog{
		nodes:  nodes,
		byID:   make(map[int64]*Node, len(nodes)),
		chains: map[int64][]chain.Hop{},
		raw:    raw,
	}
	if c.raw == nil {
		c.raw = map[int64]int64{}
	}
	for _, n := range nodes {
		c.byID[n.ID] = n
	}
	topo := topologyOf(nodes, hops, nil)
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		if leaves, err := topo.Flatten(n.ID); err == nil {
			c.chains[n.ID] = leaves
		}
	}
	return c
}

// Nodes 是全部节点行，按 sort_order、id 排序。
func (c *NodeCatalog) Nodes() []*Node { return c.nodes }

// Node 按 id 取节点行，不存在时为 nil。
func (c *NodeCatalog) Node(id int64) *Node { return c.byID[id] }

// NodeDerived 是节点不在自己那一行上、要从别的行推出的属性。物理节点只有 Online
// （就是它的 online 列）；其余字段只对能展开的组合有值。
type NodeDerived struct {
	// Online：物理节点取 online 列；组合要求展开后的每个物理成员都在线且未停用，
	// 展不开的组合离线。
	Online bool
	// EntryRelayHost/EntryRelayHostV6 取第一个物理成员的中继地址，ExitRelayHostV6
	// 取最后一个物理成员的 IPv6 中继地址：入口由链首那台接流量，出口由链尾那台拨目标。
	EntryRelayHost   string
	EntryRelayHostV6 string
	ExitRelayHostV6  string
	// Members 是展开后的物理成员，按次序。
	Members []NodeMember
}

// Derive 算出节点 n 的派生属性。n 可以来自别的查询（比如带授权列的
// ListNodesForUser），组合的成员一律在目录里的全量节点中找。
//
// chains 里的每个成员都在 byID 里：展开用的拓扑就是由同一批节点建的，成员不在其中
// 的组合 Flatten 会失败、不进 chains。
func (c *NodeCatalog) Derive(n *Node) NodeDerived {
	if n.NodeType != "composite" {
		return NodeDerived{Online: n.Online == 1}
	}
	leaves := c.chains[n.ID]
	if len(leaves) == 0 {
		return NodeDerived{}
	}
	d := NodeDerived{Online: true, Members: make([]NodeMember, len(leaves))}
	for i, leaf := range leaves {
		m := c.byID[leaf.NodeID]
		d.Members[i] = NodeMember{NodeID: m.ID, Name: m.Name, NodeType: m.NodeType, Mode: leaf.Mode}
		if m.Online != 1 || m.Disabled {
			d.Online = false
		}
	}
	first, last := c.byID[leaves[0].NodeID], c.byID[leaves[len(leaves)-1].NodeID]
	d.EntryRelayHost, d.EntryRelayHostV6 = first.RelayHost, first.RelayHostV6
	d.ExitRelayHostV6 = last.RelayHostV6
	return d
}

// OwnRawBytes 是节点自己上报的原始字节累计。组合不上报计数，恒为 0。
func (c *NodeCatalog) OwnRawBytes(id int64) int64 { return c.raw[id] }

// RawTrafficForDisplay 是给页面展示的“每个节点转发了多少原始字节”：物理节点取自己
// 上报的累计（从未上报过的不在表里），能展开的组合取它第一个物理成员的累计。组合自己
// 不转发，链首那台的累计里还包含它承载的其他规则，所以组合这一项是展示用的近似值；
// 计费与用量汇总要用 OwnRawBytes，把各项相加会把组合与它的链首重复计入。
func (c *NodeCatalog) RawTrafficForDisplay() map[int64]int64 {
	out := make(map[int64]int64, len(c.raw)+len(c.chains))
	for id, b := range c.raw {
		out[id] = b
	}
	for id, leaves := range c.chains {
		out[id] = c.raw[leaves[0].NodeID]
	}
	return out
}
