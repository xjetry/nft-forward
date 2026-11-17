package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"nft-forward/internal/db"
)

const probeTimeout = 5 * time.Second

type probeResult struct {
	OK      bool       `json:"ok"`
	Latency int        `json:"latency_ms"`
	Error   string     `json:"error,omitempty"`
	Hops    []hopProbe `json:"hops,omitempty"`
}

type hopProbe struct {
	Node    string `json:"node"`
	Target  string `json:"target"`
	Latency int    `json:"latency_ms"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) probeEndpoint(w http.ResponseWriter, r *http.Request) {
	res, status := s.runProbe(r.Context(), r.URL.Query().Get("target"), r.URL.Query().Get("node"), r.URL.Query().Get("proto"))
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	json.NewEncoder(w).Encode(res)
}

// probeFailure is the answer when a statement failed while preparing a probe:
// nothing was probed, so it is an internal error and never a probe result. As
// a result it would read "this target is unreachable" or "no such node".
func probeFailure(err error) (probeResult, int) {
	return probeResult{Error: err.Error()}, http.StatusInternalServerError
}

// probeLookupErr answers a failed lookup of the node or rule a probe names;
// err is not nil. A row that does not exist is a completed probe with nothing
// to reach (notFound, 200); any other error is a probeFailure.
func probeLookupErr(err error, notFound string) (probeResult, int) {
	if errors.Is(err, sql.ErrNoRows) {
		return probeResult{Error: notFound}, http.StatusOK
	}
	return probeFailure(err)
}

// runProbe performs a single target probe and returns the result plus the HTTP
// status a REST caller should surface. A status of 200 covers both a reachable
// and an unreachable target (the result's OK/Error carry that); non-200 is a
// transport-level refusal (bad request, forbidden) or a probeFailure, which
// the caller maps to an error envelope. The permission collapsing — non-admins may only probe through nodes
// they're granted, and the SSRF-prone node-less branch is admin-only — lives here
// so every entry point (session UI and token API) enforces it identically.
func (s *Server) runProbe(ctx context.Context, target, nodeStr, protoRaw string) (probeResult, int) {
	if target == "" {
		return probeResult{Error: "missing target"}, http.StatusBadRequest
	}
	// probeProto: "tcp+udp" rules confirm via TCP (a real handshake beats
	// UDP's indeterminate silence); anything but explicit udp probes tcp.
	proto := probeProto(protoRaw)
	actor := userFromCtx(ctx)

	if nodeStr != "" {
		nodeID, err := strconv.ParseInt(nodeStr, 10, 64)
		if err != nil {
			return probeResult{Error: "invalid node id"}, http.StatusOK
		}
		n, err := db.GetNode(s.DB, nodeID)
		if err != nil {
			return probeLookupErr(err, "node not found")
		}
		// 经一个节点向外探测，与把它用进规则是同一个问题（db.NodeAccess）：否则任何
		// 登录用户都能拿别人的节点当扫描代理。读授权失败是面板的失败，不是“无权”。
		if actor != nil {
			_, ok, err := db.NodeAccess(s.DB, actor, nodeID)
			if err != nil {
				return probeFailure(err)
			}
			if !ok {
				return probeResult{Error: "无权操作该节点"}, http.StatusForbidden
			}
		}
		if n.NodeType == "composite" {
			return s.probeCompositeToTarget(nodeID, target, proto)
		}
		ack, err := s.Hub.SendProbe(nodeID, target, proto)
		if err != nil {
			return probeResult{Error: err.Error()}, http.StatusOK
		}
		return probeResult{OK: ack.OK, Latency: ack.Latency, Error: ack.Error}, http.StatusOK
	}

	// The node-less branch makes the panel process itself dial the target — an
	// SSRF primitive into the panel's own network. The UI never uses it (it
	// always passes a node), so restrict it to admins.
	if actor == nil || actor.Role != "admin" {
		return probeResult{Error: "无权操作"}, http.StatusForbidden
	}

	if proto == "udp" {
		// The panel host can't do the agent's connected-socket ICMP dance
		// meaningfully for remote targets behind it; keep the admin-only
		// node-less branch TCP-equivalent by refusing rather than lying.
		return probeResult{Error: "udp probe requires a node"}, http.StatusOK
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, probeTimeout)
	elapsed := time.Since(start)
	if err != nil {
		return probeResult{Error: err.Error()}, http.StatusOK
	}
	conn.Close()
	return probeResult{OK: true, Latency: int(elapsed.Milliseconds())}, http.StatusOK
}

// probeProto maps a rule/query proto value onto what the probe path actually
// dials: only explicit "udp" selects the UDP probe; "tcp+udp" and everything
// else (including empty) use TCP, whose handshake gives a definite answer.
func probeProto(p string) string {
	if p == "udp" {
		return "udp"
	}
	return "tcp"
}

func (s *Server) probeCompositeToTarget(compositeID int64, target, proto string) (probeResult, int) {
	// 探测面对的是展平后的物理节点，与建规则时用的是同一份展平（chain.Topology
	// 的 Flatten）。组合的成员可以还是组合，而探测帧只能发给有 agent 连接的节点
	// （Hub.SendProbe 按节点 id 找连接）；只遍历 node_hops 的第一层，会把嵌套的
	// 组合当成一台机器去探测，得到的永远是“未连接”。
	//
	// 拓扑读不出来是面板的失败；读出来了但展不平（成员不存在、成环）是这个组合的
	// 配置问题，作为探测结果里的错误如实给出。
	topo, err := db.LoadTopology(s.DB)
	if err != nil {
		return probeFailure(err)
	}
	hops, err := topo.Flatten(compositeID)
	if err != nil {
		return probeResult{Error: err.Error()}, http.StatusOK
	}
	// Only the last physical node dials the real target, so it's the only leg
	// with a concrete data-plane endpoint to probe outside a rule — the legs
	// between members have no listen port until the composite is instantiated
	// in a rule (use the rule-level chain probe for full per-hop latency).
	// Still, report every node: a dead middle node means the composite can't
	// work, so its liveness must fold into the overall result rather than being
	// ignored while the exit leg alone reports OK.
	results := make([]hopProbe, len(hops))
	allOK := true
	total := 0
	for i, h := range hops {
		// 节点行在这里另读一次：中间成员是否在线要看行上的 online 与 disabled，
		// 这两项不在拓扑快照里。拓扑读出之后被删掉的成员按它的 id 报告，算作一个
		// 不在的成员；别的读失败是面板的失败。
		n, err := db.GetNode(s.DB, h.NodeID)
		gone := errors.Is(err, sql.ErrNoRows)
		if err != nil && !gone {
			return probeFailure(err)
		}
		name := fmt.Sprintf("#%d", h.NodeID)
		if !gone {
			name = n.Name
		}
		hp := hopProbe{Node: name}
		if i == len(hops)-1 {
			hp.Target = target
			ack, perr := s.Hub.SendProbe(h.NodeID, target, proto)
			switch {
			case perr != nil:
				hp.Error = perr.Error()
			case !ack.OK:
				if hp.Error = ack.Error; hp.Error == "" {
					hp.Error = "不通"
				}
			default:
				hp.Latency = ack.Latency
			}
		} else if gone || n.Online != 1 || n.Disabled {
			hp.Error = "节点离线"
		}
		if hp.Error != "" {
			allOK = false
		} else {
			total += hp.Latency
		}
		results[i] = hp
	}
	return probeResult{OK: allOK, Latency: total, Hops: results}, http.StatusOK
}

func (s *Server) probeChainEndpoint(w http.ResponseWriter, r *http.Request) {
	res, status := s.runProbeChain(r.Context(), chainRuleIDParam(r))
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	json.NewEncoder(w).Encode(res)
}

// chainRuleIDParam reads the rule id a chain probe targets. rule_id is canonical;
// rule/chain are older aliases kept working.
func chainRuleIDParam(r *http.Request) string {
	if v := r.URL.Query().Get("rule_id"); v != "" {
		return v
	}
	if v := r.URL.Query().Get("rule"); v != "" {
		return v
	}
	return r.URL.Query().Get("chain")
}

// runProbeChain probes every hop of a rule's deployed chain in parallel and
// returns the aggregate result plus the HTTP status a REST caller should surface
// (non-200 for a forbidden cross-owner probe, or a probeFailure). Ownership is enforced here so
// a non-admin can't read another user's per-hop node names/targets.
func (s *Server) runProbeChain(ctx context.Context, ruleIDStr string) (probeResult, int) {
	ruleID, err := strconv.ParseInt(ruleIDStr, 10, 64)
	if err != nil {
		return probeResult{Error: "invalid rule id"}, http.StatusOK
	}
	rule, err := db.GetRule(s.DB, ruleID)
	if err != nil {
		return probeLookupErr(err, "rule not found")
	}
	// A non-admin may probe only their own rule, so the per-hop node names and
	// targets of other users' rules don't leak through the chain probe.
	if u := userFromCtx(ctx); u != nil && u.Role != "admin" {
		if !rule.OwnerID.Valid || rule.OwnerID.Int64 != u.ID {
			return probeResult{Error: "无权操作该规则"}, http.StatusForbidden
		}
	}
	hops, err := db.ListRuleHops(s.DB, ruleID)
	if err != nil {
		return probeFailure(err)
	}
	if len(hops) == 0 {
		return probeResult{Error: "no hops"}, http.StatusOK
	}

	type probeTask struct {
		idx    int
		nodeID int64
		name   string
		target string
		proto  string
	}
	var tasks []probeTask
	for i, h := range hops {
		target := net.JoinHostPort(h.TargetHost, strconv.Itoa(h.TargetPort))
		// A node deleted since the hops were read is probed under its id;
		// the probe then reports it as not connected.
		nodeName := fmt.Sprintf("#%d", h.NodeID)
		switch n, err := db.GetNode(s.DB, h.NodeID); {
		case err == nil:
			nodeName = n.Name
		case !errors.Is(err, sql.ErrNoRows):
			return probeFailure(err)
		}
		tasks = append(tasks, probeTask{idx: i, nodeID: h.NodeID, name: nodeName, target: target, proto: probeProto(h.Proto)})
	}

	results := make([]hopProbe, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t probeTask) {
			defer wg.Done()
			hp := hopProbe{Node: t.name, Target: t.target}
			ack, err := s.Hub.SendProbe(t.nodeID, t.target, t.proto)
			if err != nil {
				hp.Error = err.Error()
			} else if !ack.OK {
				hp.Error = ack.Error
			} else {
				hp.Latency = ack.Latency
			}
			results[i] = hp
		}(i, t)
	}
	wg.Wait()

	total := 0
	allOK := true
	for i := range results {
		if results[i].Error != "" {
			allOK = false
		} else {
			total += results[i].Latency
		}
	}
	return probeResult{OK: allOK, Latency: total, Hops: results}, http.StatusOK
}
