package server

import (
	"net/http"

	"nft-forward/internal/chain"
	"nft-forward/internal/db"
)

// 规则的物理链怎么推导、谁能占链上的哪个位置、每条腿用什么转发模式，都在
// internal/chain 里。本文件只做面板这一侧的三件事：把库读成拓扑快照，把请求翻译成
// chain.Request，把推导结果换成 db.RegenerateRule 的输入。

// deriveChain 按 req 推导规则的物理链。推导拒绝（没有资格、没有绑定边、展不开、
// 链尾不许直接转发）返回 400，错误信息就是给请求方看的原因；读拓扑失败返回 500。
func (s *Server) deriveChain(req chain.Request) ([]db.HopInput, *ruleMutationError) {
	topo, err := db.LoadTopology(s.DB)
	if err != nil {
		return nil, &ruleMutationError{status: http.StatusInternalServerError, msg: err.Error()}
	}
	hops, err := topo.Derive(req)
	if err != nil {
		return nil, &ruleMutationError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return hopInputs(hops), nil
}

// rederiveChain 给“编辑已有规则 rl”用：这次编辑没有换入口时，把规则现在的出口模式
// 填进 req.Exit.Current 再推导，于是请求没有指定出口模式时沿用现状（取舍规则见
// chain.ExitChoice）。换了入口就不沿用，没有指定的出口模式回到默认值。
func (s *Server) rederiveChain(rl *db.Rule, req chain.Request) ([]db.HopInput, *ruleMutationError) {
	if req.Entry == rl.NodeID {
		current, err := db.ListRuleHops(s.DB, rl.ID)
		if err != nil {
			return nil, &ruleMutationError{status: http.StatusInternalServerError, msg: err.Error()}
		}
		if len(current) > 0 {
			req.Exit.Current = current[len(current)-1].Mode
		}
	}
	return s.deriveChain(req)
}

// explicitHop 是管理员直接给出的物理链里的一跳，建规则与改规则的请求体共用。
type explicitHop struct {
	NodeID int64  `json:"node_id"`
	Mode   string `json:"mode"`
	// ViaNodeID 标明这一跳属于哪个逻辑节点。按授权计流量时一个逻辑段只在段首那一跳
	// 记一次、记到这个逻辑节点的授权上（db.SegmentFirstHops），所以把一个组合展开
	// 写成显式链时，各跳都要带 via = 组合的 id。没给（0）时取这一跳自己的 node_id，
	// 即每跳自成一段。
	ViaNodeID int64 `json:"via_node_id"`
}

// explicitChain 把管理员直接给出的物理跳序列变成链。这是管理员搭任意链的通道，有意
// 不校验角色与绑定边；链尾能否直接转发仍然检查（chain.CheckExit），所以建规则与改
// 规则都必须经这里，不能各自把请求里的跳直接交给 db.RegenerateRule。
func (s *Server) explicitChain(given []explicitHop) ([]db.HopInput, *ruleMutationError) {
	hops := make([]chain.Hop, len(given))
	for i, h := range given {
		via := h.ViaNodeID
		if via == 0 {
			via = h.NodeID
		}
		hops[i] = chain.Hop{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: via}
	}
	topo, err := db.LoadTopology(s.DB)
	if err != nil {
		return nil, &ruleMutationError{status: http.StatusInternalServerError, msg: err.Error()}
	}
	if err := topo.CheckExit(hops); err != nil {
		return nil, &ruleMutationError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return hopInputs(hops), nil
}

// validateCompositeMembers 检查要写给组合 rootID 的成员列表（新建组合时 rootID 为
// 0）。成员不存在或会成环返回 400 与原因，读拓扑失败返回 500；通过时 err 为 nil。
func (s *Server) validateCompositeMembers(rootID int64, members []int64) (status int, err error) {
	topo, err := db.LoadTopology(s.DB)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if err := topo.ValidateMembers(rootID, members); err != nil {
		return http.StatusBadRequest, err
	}
	return 0, nil
}

// seatOverrides 返回 userID 的各条授权对节点入口/中转资格的覆盖，键是节点 id；只含
// 确实覆盖了的授权（见 db.GrantSeats）。推导规则链时，一个节点能不能做入口或中转
// 按规则属主的授权判定，属主没有覆盖的节点才看节点自身的资格。
//
// 读授权失败返回 500，不退回“没有覆盖”：覆盖可以收窄资格（节点自身是入口+中转、
// 授权只给中转），把读失败当成没有覆盖会放宽判定。
func (s *Server) seatOverrides(userID int64) (map[int64]chain.Seat, *ruleMutationError) {
	_, grants, err := db.ListNodesForUser(s.DB, userID)
	if err != nil {
		return nil, &ruleMutationError{status: http.StatusInternalServerError, msg: err.Error()}
	}
	overrides := map[int64]chain.Seat{}
	for _, g := range grants {
		if seats, ok := db.GrantSeats(g.Roles); ok {
			overrides[g.NodeID] = seats
		}
	}
	return overrides, nil
}

// hopInputs 把推导出的物理跳换成 db.RegenerateRule 的输入。端口与备注不属于链路
// 推导，由调用方在返回值上另填。
func hopInputs(hops []chain.Hop) []db.HopInput {
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID}
	}
	return inputs
}
