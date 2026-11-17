package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/chain"
	"nft-forward/internal/rulechange"
)

// 规则写入在会话接口这一面的入口。谁能改什么、怎样算合法、上限、链路推导、事务与审
// 计都在 internal/rulechange；这里的处理函数只做三件事：把请求体解成它的参数，把它
// 返回的要重推的节点交给下发，把结果或拒绝写成这一面的响应。/api/v1 那一面的入口在
// apiv1.go，agent 通道的入口在 hub_rules.go，三面调的是同一个 s.Rules。

// ruleWriteBody 是建规则、改规则的请求体，管理员接口与用户接口共用。Hops 原样交给
// rulechange，由它拒绝不是管理员的发起方。
type ruleWriteBody struct {
	NodeID    int64  `json:"node_id"`
	Name      string `json:"name"`
	Proto     string `json:"proto"`
	Exit      string `json:"exit"`
	EntryPort int    `json:"entry_port"`
	Comment   string `json:"comment"`
	// ExitMode 是出口那一跳的转发模式；Mode 是它的旧字段，只对裸单点生效（见
	// chain.ExitChoice）。改规则时两者都不给就保持现状。
	Mode     string        `json:"mode"`
	ExitMode string        `json:"exit_mode"`
	Hops     []explicitHop `json:"hops"`
	// ViaNodeIDs 是入口之后的中转层。用指针区分“没提到”（改规则时沿用已存的）与
	// 明确给出的空列表（去掉中转层）。
	ViaNodeIDs *[]int64 `json:"via_node_ids"`
}

// explicitHop 是管理员直接给出的物理链里的一跳。ViaNodeID 标明这一跳属于哪个逻辑节
// 点，省略时取这一跳自己的 node_id（见 rulechange 对显式链的说明）。
type explicitHop struct {
	NodeID    int64  `json:"node_id"`
	Mode      string `json:"mode"`
	ViaNodeID int64  `json:"via_node_id"`
}

func (b ruleWriteBody) fields() rulechange.Fields {
	return rulechange.Fields{Name: b.Name, Proto: b.Proto, Exit: b.Exit, Comment: b.Comment}
}

func (b ruleWriteBody) path() rulechange.Path {
	p := rulechange.Path{
		Entry: b.NodeID, Vias: b.ViaNodeIDs,
		ExitMode: b.ExitMode, LegacyMode: b.Mode, EntryPort: b.EntryPort,
	}
	for _, h := range b.Hops {
		p.Hops = append(p.Hops, chain.Hop{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID})
	}
	return p
}

// ruleChangeStatus 把 rulechange 的拒绝类别换成 HTTP 状态码。两面的入口共用这张表，
// 各自再套自己的响应外壳。
func ruleChangeStatus(e *rulechange.Error) int {
	switch e.Kind {
	case rulechange.Forbidden:
		return http.StatusForbidden
	case rulechange.NotFound:
		return http.StatusNotFound
	case rulechange.Conflict:
		return http.StatusConflict
	case rulechange.Internal:
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

func writeRuleChangeErr(w http.ResponseWriter, e *rulechange.Error) {
	jsonErr(w, ruleChangeStatus(e), e.Msg)
}

// ruleChanged 下发一次规则变更波及的节点，等它们应答后返回。
func (s *Server) ruleChanged(res rulechange.Result) {
	s.dispatchNodes("规则变更", res.Affected)
}

// apiRewireRules 按落库的链重新生成这些规则并下发（见 rulechange.Changes.Rewire）。
func (s *Server) apiRewireRules(ruleIDs []int64) {
	s.dispatchNodes("规则变更", s.Rules.Rewire(ruleIDs))
}

func (s *Server) apiCreateRule(w http.ResponseWriter, r *http.Request) {
	var body ruleWriteBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, cerr := s.Rules.Create(rulechange.Actor{User: userFromCtx(r.Context())}, body.fields(), body.path(), false)
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"rule": res.Rule, "entry": res.Entry, "entry_v6": res.EntryV6})
}

func (s *Server) apiUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body ruleWriteBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, cerr := s.Rules.Update(rulechange.Actor{User: userFromCtx(r.Context())}, id, body.fields(), body.path())
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true, "entry": res.Entry, "entry_v6": res.EntryV6})
}

func (s *Server) apiDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	res, cerr := s.Rules.Delete(rulechange.Actor{User: userFromCtx(r.Context())}, id)
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiReallocateRuleHop(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	pos, err := strconv.Atoi(chi.URLParam(r, "pos"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "跳序号非法")
		return
	}
	// 请求体可以为空：不给端口表示让面板另挑一个。
	var body struct {
		Port int `json:"port"`
	}
	_ = decodeJSON(r, &body)
	res, cerr := s.Rules.ReallocateHop(rulechange.Actor{User: userFromCtx(r.Context())}, id, pos, body.Port)
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiMyCreateRule(w http.ResponseWriter, r *http.Request) {
	var body ruleWriteBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, cerr := s.Rules.Create(rulechange.Actor{User: userFromCtx(r.Context())}, body.fields(), body.path(), false)
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true, "rule_id": res.Rule.ID, "entry": res.Entry, "entry_v6": res.EntryV6})
}

func (s *Server) apiMyUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body ruleWriteBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, cerr := s.Rules.Update(rulechange.Actor{User: userFromCtx(r.Context())}, id, body.fields(), body.path())
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true, "entry": res.Entry, "entry_v6": res.EntryV6})
}

func (s *Server) apiMyDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	res, cerr := s.Rules.Delete(rulechange.Actor{User: userFromCtx(r.Context())}, id)
	if cerr != nil {
		writeRuleChangeErr(w, cerr)
		return
	}
	s.ruleChanged(res)
	jsonOK(w, map[string]any{"ok": true})
}
