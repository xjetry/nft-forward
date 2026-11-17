package server

import (
	"encoding/json"

	"nft-forward/internal/nft"
	"nft-forward/internal/rulechange"
	"nft-forward/internal/wsproto"
)

// handleRuleCommand 处理节点发来的一条规则命令：解码，交给 rulechange 执行，把规则
// 集变了的节点交给 Redispatch，应答。
//
// 命令以节点属主的名义执行（rulechange.Changes.NodeActor）；谁能改什么、怎样算合法，
// 都由 rulechange 判定，与面板上的建规则、改规则是同一套。这里只把线上的帧翻译成它
// 的参数：
//
//   - rule_create：在这台节点上建一条规则，入口就是它自己；
//   - rule_update：改住在这台节点上的一条单跳规则；
//   - migrate_rules：把节点的本地规则交给面板接管；
//   - rule_hop_edit：改这台节点在某条规则里的那一跳；
//   - rule_delete：删掉一条经过这台节点的规则。
func (h *Hub) handleRuleCommand(ac *agentConn, env wsproto.Envelope) {
	run, ok := decodeRuleCommand(h.Rules, ac.nodeID, env)
	if !ok {
		sendRuleAck(ac, env.ID, wsproto.RuleCmdAck{Error: "malformed payload"})
		return
	}
	actor, cerr := h.Rules.NodeActor(ac.nodeID)
	var res rulechange.Result
	if cerr == nil {
		res, cerr = run(actor)
	}
	if cerr != nil {
		sendRuleAck(ac, env.ID, wsproto.RuleCmdAck{Error: cerr.Msg})
		return
	}
	if h.Redispatch != nil && len(res.Affected) > 0 {
		h.Redispatch(res.Affected)
	}
	sendRuleAck(ac, env.ID, wsproto.RuleCmdAck{OK: true, Entry: res.Entry})
}

// decodeRuleCommand 把一帧规则命令解成“给定发起方就能执行”的函数。帧解不出来时 ok
// 为假。
func decodeRuleCommand(rules *rulechange.Changes, nodeID int64, env wsproto.Envelope) (run func(rulechange.Actor) (rulechange.Result, *rulechange.Error), ok bool) {
	switch env.Type {
	case wsproto.TypeRuleCreate:
		var req wsproto.RuleCreate
		if json.Unmarshal(env.Payload, &req) != nil {
			return nil, false
		}
		return func(actor rulechange.Actor) (rulechange.Result, *rulechange.Error) {
			return rules.Create(actor,
				rulechange.Fields{Name: req.Name, Proto: req.Proto, Exit: rulechange.JoinExit(req.ExitHost, req.ExitPort), Comment: req.Comment},
				rulechange.Path{Entry: nodeID, ExitMode: req.Mode, EntryPort: req.ListenPort}, false)
		}, true
	case wsproto.TypeRuleUpdate:
		var req wsproto.RuleUpdate
		if json.Unmarshal(env.Payload, &req) != nil {
			return nil, false
		}
		return func(actor rulechange.Actor) (rulechange.Result, *rulechange.Error) {
			return rules.Update(actor, req.RuleID,
				rulechange.Fields{Proto: req.Proto, Exit: rulechange.JoinExit(req.ExitHost, req.ExitPort), Comment: req.Comment},
				rulechange.Path{ExitMode: req.Mode, EntryPort: req.ListenPort})
		}, true
	case wsproto.TypeMigrateRules:
		var req wsproto.MigrateRules
		if json.Unmarshal(env.Payload, &req) != nil {
			return nil, false
		}
		return func(actor rulechange.Actor) (rulechange.Result, *rulechange.Error) {
			return rules.Adopt(actor, adoptedLocals(req.Rules))
		}, true
	case wsproto.TypeRuleHopEdit:
		var req wsproto.RuleHopEdit
		if json.Unmarshal(env.Payload, &req) != nil {
			return nil, false
		}
		return func(actor rulechange.Actor) (rulechange.Result, *rulechange.Error) {
			return rules.EditHop(actor, req.RuleID, rulechange.HopEdit{ListenPort: req.ListenPort, Mode: req.Mode, Comment: req.Comment})
		}, true
	case wsproto.TypeRuleDelete:
		var req wsproto.RuleDelete
		if json.Unmarshal(env.Payload, &req) != nil {
			return nil, false
		}
		return func(actor rulechange.Actor) (rulechange.Result, *rulechange.Error) {
			return rules.Delete(actor, req.RuleID)
		}, true
	}
	return nil, false
}

// adoptedLocals 把节点上报的本地规则翻译成接管的输入。本地规则的目标写在 DestHost
// （域名）或 DestIP 里，两者都有时取域名。
func adoptedLocals(local []nft.Rule) []rulechange.Local {
	out := make([]rulechange.Local, len(local))
	for i, r := range local {
		host := r.DestIP
		if r.DestHost != "" {
			host = r.DestHost
		}
		out[i] = rulechange.Local{
			Fields:     rulechange.Fields{Proto: r.Proto, Exit: rulechange.JoinExit(host, r.DestPort), Comment: r.Comment},
			ListenPort: r.SrcPort,
			Mode:       r.Mode,
		}
	}
	return out
}

func sendRuleAck(ac *agentConn, id string, ack wsproto.RuleCmdAck) {
	p, _ := json.Marshal(ack)
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: id, Payload: p})
}
