package server

import (
	"encoding/json"
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
	// ExitMode 是出口那一跳的转发模式（见 chain.ExitChoice）。改规则时不给就保持现状。
	ExitMode string        `json:"exit_mode"`
	Hops     []explicitHop `json:"hops"`
	// ViaNodeIDs 是入口之后的中转层。用指针区分“没提到”（改规则时沿用已存的）与
	// 明确给出的空列表（去掉中转层）。
	ViaNodeIDs *[]int64 `json:"via_node_ids"`
	removedRuleFields
}

// removedRuleFields 接住规则请求体里已经删除的顶层字段。字段从结构体里拿掉之后，
// json 解码会把请求里的同名键当未知键丢掉、不报错：仍在发 mode 的客户端以为自己
// 指定了出口模式，实际没有。所以留一个只用来发现它的位置，由 decodeRuleBody 拒绝。
// 建规则、改规则的请求体都嵌入它：这里的 ruleWriteBody，以及 /api/v1 的两个。
type removedRuleFields struct {
	// Mode 是出口模式的旧字段，已由 exit_mode 取代。用 RawMessage 只为看它出现没有，
	// 不看值：空串与 null 也算带了。hops 里每一跳的 mode 是另一个字段，不在这一层。
	Mode json.RawMessage `json:"mode"`
}

func (f *removedRuleFields) removedFields() *removedRuleFields { return f }

// ruleBody 是 decodeRuleBody 能解的请求体：嵌入了 removedRuleFields 的结构体的指针。
type ruleBody interface {
	removedFields() *removedRuleFields
}

// decodeRuleBody 解出建规则、改规则的请求体。返回的字符串是拒绝理由（一律按 400
// 回），空串表示解出来了。写规则的六个入口都经它解码，“带了已删除的字段就拒绝”
// 因此不靠各入口自己记得去查。
func decodeRuleBody(r *http.Request, body ruleBody) string {
	if err := decodeJSON(r, body); err != nil {
		return "请求格式错误"
	}
	if body.removedFields().Mode != nil {
		return "字段 mode 已移除：出口的转发模式请用 exit_mode"
	}
	return ""
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
		ExitMode: b.ExitMode, EntryPort: b.EntryPort,
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

func (s *Server) apiCreateRule(w http.ResponseWriter, r *http.Request) {
	var body ruleWriteBody
	if msg := decodeRuleBody(r, &body); msg != "" {
		jsonErr(w, http.StatusBadRequest, msg)
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
	if msg := decodeRuleBody(r, &body); msg != "" {
		jsonErr(w, http.StatusBadRequest, msg)
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
	// 这个接口要的是“这条规则不在了”。规则已经不在时这个结果已经成立，按成功回答：
	// 重试，或另一位管理员晚到的同一个请求，不是错误。这时 res 是空的，没有节点要
	// 重推。用户侧的删除（apiMyDeleteRule）不这样回答：发起方只能删自己名下的规则，
	// 读不到规则就无从判断它是不是他的。
	if cerr != nil && cerr.Kind != rulechange.NotFound {
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
	if msg := decodeRuleBody(r, &body); msg != "" {
		jsonErr(w, http.StatusBadRequest, msg)
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
	if msg := decodeRuleBody(r, &body); msg != "" {
		jsonErr(w, http.StatusBadRequest, msg)
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
