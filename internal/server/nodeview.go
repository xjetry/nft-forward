package server

import (
	"database/sql"

	"nft-forward/internal/chain"
	"nft-forward/internal/db"
)

// 节点对外的形状按受众分三种：管理员面（/api 下的管理员路由）、用户面（/api/my/*）、
// /api/v1（apiv1.go 的 v1Node）。三种视图都只由本文件的构造函数从 db.NodeCatalog
// 装配：派生属性（在线、出入口地址、成员链）由 NodeCatalog.Derive 一次给全，目录
// 总是全量加载，所以“组合要拿全量节点来算”不需要调用方记得。db.Node 自己拒绝序列化
// （db.Node.MarshalJSON），绕开这里直接把行写进响应会在第一次请求时失败。

// compositeView 是组合才有的派生字段；物理节点上全为空，按 omitempty 不出现。
// 前端（components/ui.jsx 的 nodeStack）对组合读 entry_*/exit_*，对物理节点读它自己的
// relay_host/relay_host_v6。
type compositeView struct {
	EntryRelayHost   string           `json:"entry_relay_host,omitempty"`
	EntryRelayHostV6 string           `json:"entry_relay_host_v6,omitempty"`
	ExitRelayHostV6  string           `json:"exit_relay_host_v6,omitempty"`
	Hops             []nodeMemberView `json:"hops,omitempty"`
}

// nodeMemberView 是组合展开后的一个物理成员。
type nodeMemberView struct {
	NodeID   int64  `json:"node_id"`
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
	Mode     string `json:"mode"`
}

func compositeViewOf(d db.NodeDerived) compositeView {
	v := compositeView{EntryRelayHost: d.EntryRelayHost, EntryRelayHostV6: d.EntryRelayHostV6, ExitRelayHostV6: d.ExitRelayHostV6}
	if len(d.Members) > 0 {
		v.Hops = make([]nodeMemberView, len(d.Members))
		for i, m := range d.Members {
			v.Hops[i] = nodeMemberView{NodeID: m.NodeID, Name: m.Name, NodeType: m.NodeType, Mode: m.Mode}
		}
	}
	return v
}

func onlineFlag(online bool) int {
	if online {
		return 1
	}
	return 0
}

// adminNodeView 是管理员面上的节点：行上的每一列（凭据除外，只在节点详情里给，见
// adminNodeDetailView），加上派生属性。roles 键给的是节点自己可占的位置。
type adminNodeView struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	NodeType            string         `json:"node_type"`
	OwnerID             *int64         `json:"owner_id,omitempty"`
	Address             string         `json:"address"`
	RelayHost           string         `json:"relay_host"`
	RelayHostV6         string         `json:"relay_host_v6"`
	RelayHostDeclared   bool           `json:"relay_host_declared"`
	RelayHostV6Declared bool           `json:"relay_host_v6_declared"`
	Online              int            `json:"online"`
	AgentVersion        string         `json:"agent_version"`
	AgentSHA            string         `json:"agent_sha"`
	LastSeen            *int64         `json:"last_seen,omitempty"`
	LastApplyAt         sql.NullInt64  `json:"last_apply_at"`
	LastError           sql.NullString `json:"last_error"`
	LastWarning         string         `json:"last_warning"`
	Disabled            bool           `json:"disabled"`
	PortRange           string         `json:"port_range"`
	SortOrder           int64          `json:"sort_order"`
	CreatedAt           int64          `json:"created_at"`
	LastUpgradeAt       sql.NullInt64  `json:"last_upgrade_at"`
	LastUpgradeVersion  string         `json:"last_upgrade_version,omitempty"`
	LastUpgradeStatus   string         `json:"last_upgrade_status,omitempty"`
	LastUpgradeError    string         `json:"last_upgrade_error,omitempty"`
	RateMultiplier      float64        `json:"rate_multiplier"`
	Unidirectional      bool           `json:"unidirectional"`
	NoDirectExit        bool           `json:"no_direct_exit"`
	Seats               chain.Seat     `json:"roles"`
	compositeView
}

func adminNodeOf(c *db.NodeCatalog, n *db.Node) adminNodeView {
	d := c.Derive(n)
	return adminNodeView{
		ID: n.ID, Name: n.Name, NodeType: n.NodeType, OwnerID: n.OwnerID, Address: n.Address,
		RelayHost: n.RelayHost, RelayHostV6: n.RelayHostV6,
		RelayHostDeclared: n.RelayHostDeclared, RelayHostV6Declared: n.RelayHostV6Declared,
		Online: onlineFlag(d.Online), AgentVersion: n.AgentVersion, AgentSHA: n.AgentSHA,
		LastSeen: n.LastSeen, LastApplyAt: n.LastApplyAt, LastError: n.LastError, LastWarning: n.LastWarning,
		Disabled: n.Disabled, PortRange: n.PortRange, SortOrder: n.SortOrder, CreatedAt: n.CreatedAt,
		LastUpgradeAt: n.LastUpgradeAt, LastUpgradeVersion: n.LastUpgradeVersion,
		LastUpgradeStatus: n.LastUpgradeStatus, LastUpgradeError: n.LastUpgradeError,
		RateMultiplier: n.RateMultiplier, Unidirectional: d.Unidirectional, NoDirectExit: d.NoDirectExit,
		Seats:         n.Seats,
		compositeView: compositeViewOf(d),
	}
}

func adminNodesOf(c *db.NodeCatalog, nodes []*db.Node) []adminNodeView {
	out := make([]adminNodeView, len(nodes))
	for i, n := range nodes {
		out[i] = adminNodeOf(c, n)
	}
	return out
}

func adminNodesByID(views []adminNodeView) map[int64]adminNodeView {
	m := make(map[int64]adminNodeView, len(views))
	for _, v := range views {
		m[v.ID] = v
	}
	return m
}

// adminNodeDetailView 是管理员节点详情里的节点：多带接入凭据，供页面生成安装命令。
type adminNodeDetailView struct {
	adminNodeView
	Secret string `json:"secret"`
}

// myNodeView 是用户面上一个被授权的节点。只带用户页面读得到的字段：名字与类型、
// 这条授权下生效的入口/中转资格、倍率与计费方向、能否作链尾、停用与在线、中继地址（规则表单
// 据此标出 v4/v6 能力），以及组合的派生字段。机器的接入与运行状态（连接地址、
// agent 版本、同步错误、升级记录）、属主、端口范围都不给。
type myNodeView struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	NodeType       string     `json:"node_type"`
	Seats          chain.Seat `json:"roles"`
	RateMultiplier float64    `json:"rate_multiplier"`
	Unidirectional bool       `json:"unidirectional"`
	NoDirectExit   bool       `json:"no_direct_exit"`
	Disabled       bool       `json:"disabled"`
	Online         int        `json:"online"`
	RelayHost      string     `json:"relay_host"`
	RelayHostV6    string     `json:"relay_host_v6"`
	compositeView
}

// myNodeOf 是被授权节点 n 在授权 g 下的用户视图。roles 键给的是授权下生效的位置
// （db.EffectiveSeats）：用户能把这个节点当什么用，由授权决定。
func myNodeOf(c *db.NodeCatalog, n *db.Node, g *db.UserNode) myNodeView {
	d := c.Derive(n)
	return myNodeView{
		ID: n.ID, Name: n.Name, NodeType: n.NodeType,
		Seats:          db.EffectiveSeats(n.Seats, g.Seats),
		RateMultiplier: n.RateMultiplier, Unidirectional: d.Unidirectional, NoDirectExit: d.NoDirectExit,
		Disabled: n.Disabled, Online: onlineFlag(d.Online),
		RelayHost: n.RelayHost, RelayHostV6: n.RelayHostV6,
		compositeView: compositeViewOf(d),
	}
}

// grantedNodes 是用户 u 被授权的节点：用户视图（与授权按下标对齐，即
// db.ListNodesForUser 的次序），以及装配它们时用的节点目录——同一次请求里的规则
// 视图要用同一份目录解析成员与链路。
func (s *Server) grantedNodes(u *db.User) ([]myNodeView, []*db.UserNode, *db.NodeCatalog, error) {
	cat, err := db.LoadNodeCatalog(s.DB)
	if err != nil {
		return nil, nil, nil, err
	}
	rows, grants, err := db.ListNodesForUser(s.DB, u.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	views := make([]myNodeView, len(rows))
	for i, n := range rows {
		views[i] = myNodeOf(cat, n, grants[i])
	}
	return views, grants, cat, nil
}

func myNodesByID(views []myNodeView) map[int64]myNodeView {
	m := make(map[int64]myNodeView, len(views))
	for _, v := range views {
		m[v.ID] = v
	}
	return m
}
