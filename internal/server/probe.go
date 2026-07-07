package server

import (
	"encoding/json"
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
	target := r.URL.Query().Get("target")
	if target == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(probeResult{Error: "missing target"})
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// probeProto: "tcp+udp" rules confirm via TCP (a real handshake beats
	// UDP's indeterminate silence); anything but explicit udp probes tcp.
	proto := probeProto(r.URL.Query().Get("proto"))

	actor := userFromCtx(r.Context())

	nodeStr := r.URL.Query().Get("node")
	if nodeStr != "" {
		nodeID, err := strconv.ParseInt(nodeStr, 10, 64)
		if err != nil {
			json.NewEncoder(w).Encode(probeResult{Error: "invalid node id"})
			return
		}
		n, err := db.GetNode(s.DB, nodeID)
		if err != nil {
			json.NewEncoder(w).Encode(probeResult{Error: "node not found"})
			return
		}
		// A non-admin may probe only through nodes they've been granted, so the
		// node can't be used as a scanning proxy against arbitrary targets.
		if actor != nil && actor.Role != "admin" {
			if _, err := db.CheckNodeAccess(s.DB, actor.ID, nodeID); err != nil {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(probeResult{Error: "无权操作该节点"})
				return
			}
		}
		if n.NodeType == "composite" {
			s.probeCompositeToTarget(w, nodeID, target, proto)
			return
		}
		ack, err := s.Hub.SendProbe(nodeID, target, proto)
		if err != nil {
			json.NewEncoder(w).Encode(probeResult{Error: err.Error()})
			return
		}
		json.NewEncoder(w).Encode(probeResult{OK: ack.OK, Latency: ack.Latency, Error: ack.Error})
		return
	}

	// The node-less branch makes the panel process itself dial the target — an
	// SSRF primitive into the panel's own network. The UI never uses it (it
	// always passes a node), so restrict it to admins.
	if actor == nil || actor.Role != "admin" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(probeResult{Error: "无权操作"})
		return
	}

	if proto == "udp" {
		// The panel host can't do the agent's connected-socket ICMP dance
		// meaningfully for remote targets behind it; keep the admin-only
		// node-less branch TCP-equivalent by refusing rather than lying.
		json.NewEncoder(w).Encode(probeResult{Error: "udp probe requires a node"})
		return
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, probeTimeout)
	elapsed := time.Since(start)
	if err != nil {
		json.NewEncoder(w).Encode(probeResult{Error: err.Error()})
		return
	}
	conn.Close()
	json.NewEncoder(w).Encode(probeResult{OK: true, Latency: int(elapsed.Milliseconds())})
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

func (s *Server) probeCompositeToTarget(w http.ResponseWriter, compositeID int64, target, proto string) {
	hops, err := db.ListNodeHops(s.DB, compositeID)
	if err != nil || len(hops) == 0 {
		json.NewEncoder(w).Encode(probeResult{Error: "no hops"})
		return
	}
	// Only the last child dials the real target, so it's the only leg with a
	// concrete data-plane endpoint to probe outside a rule — inter-child legs
	// have no listen port until the composite is instantiated in a rule (use the
	// rule-level chain probe for full per-hop latency). Still, report every
	// child: a dead middle child means the composite can't work, so its liveness
	// must fold into the overall result rather than being ignored while the exit
	// leg alone reports OK.
	results := make([]hopProbe, len(hops))
	allOK := true
	total := 0
	for i, h := range hops {
		n, gerr := db.GetNode(s.DB, h.HopNodeID)
		name := fmt.Sprintf("#%d", h.HopNodeID)
		if gerr == nil {
			name = n.Name
		}
		hp := hopProbe{Node: name}
		if i == len(hops)-1 {
			hp.Target = target
			ack, perr := s.Hub.SendProbe(h.HopNodeID, target, proto)
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
		} else if gerr != nil || n.Online != 1 || n.Disabled {
			hp.Error = "节点离线"
		}
		if hp.Error != "" {
			allOK = false
		} else {
			total += hp.Latency
		}
		results[i] = hp
	}
	json.NewEncoder(w).Encode(probeResult{OK: allOK, Latency: total, Hops: results})
}

func (s *Server) probeChainEndpoint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// rule_id is the canonical param; rule/chain are older aliases kept working.
	ruleIDStr := r.URL.Query().Get("rule_id")
	if ruleIDStr == "" {
		ruleIDStr = r.URL.Query().Get("rule")
	}
	if ruleIDStr == "" {
		ruleIDStr = r.URL.Query().Get("chain")
	}
	ruleID, err := strconv.ParseInt(ruleIDStr, 10, 64)
	if err != nil {
		json.NewEncoder(w).Encode(probeResult{Error: "invalid rule id"})
		return
	}
	rule, err := db.GetRule(s.DB, ruleID)
	if err != nil {
		json.NewEncoder(w).Encode(probeResult{Error: "rule not found"})
		return
	}
	// A non-admin may probe only their own rule, so the per-hop node names and
	// targets of other users' rules don't leak through the chain probe.
	if u := userFromCtx(r.Context()); u != nil && u.Role != "admin" {
		if !rule.OwnerID.Valid || rule.OwnerID.Int64 != u.ID {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(probeResult{Error: "无权操作该规则"})
			return
		}
	}
	hops, err := db.ListRuleHops(s.DB, ruleID)
	if err != nil || len(hops) == 0 {
		json.NewEncoder(w).Encode(probeResult{Error: "no hops"})
		return
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
		nodeName := fmt.Sprintf("#%d", h.NodeID)
		if n, err := db.GetNode(s.DB, h.NodeID); err == nil {
			nodeName = n.Name
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
	json.NewEncoder(w).Encode(probeResult{OK: allOK, Latency: total, Hops: results})
}
