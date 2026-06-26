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
		if n.NodeType == "composite" {
			s.probeCompositeToTarget(w, nodeID, target)
			return
		}
		ack, err := s.Hub.SendProbe(nodeID, target)
		if err != nil {
			json.NewEncoder(w).Encode(probeResult{Error: err.Error()})
			return
		}
		json.NewEncoder(w).Encode(probeResult{OK: ack.OK, Latency: ack.Latency, Error: ack.Error})
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

func (s *Server) probeCompositeToTarget(w http.ResponseWriter, compositeID int64, target string) {
	hops, err := db.ListNodeHops(s.DB, compositeID)
	if err != nil || len(hops) == 0 {
		json.NewEncoder(w).Encode(probeResult{Error: "no hops"})
		return
	}
	lastHop := hops[len(hops)-1]
	nodeName := fmt.Sprintf("#%d", lastHop.HopNodeID)
	if n, err := db.GetNode(s.DB, lastHop.HopNodeID); err == nil {
		nodeName = n.Name
	}
	ack, err := s.Hub.SendProbe(lastHop.HopNodeID, target)
	if err != nil {
		json.NewEncoder(w).Encode(probeResult{Error: err.Error(), Hops: []hopProbe{{Node: nodeName, Target: target, Error: err.Error()}}})
		return
	}
	hp := hopProbe{Node: nodeName, Target: target, Latency: ack.Latency, Error: ack.Error}
	json.NewEncoder(w).Encode(probeResult{OK: ack.OK, Latency: ack.Latency, Hops: []hopProbe{hp}, Error: ack.Error})
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
	}
	var tasks []probeTask
	for i, h := range hops {
		target := net.JoinHostPort(h.TargetHost, strconv.Itoa(h.TargetPort))
		nodeName := fmt.Sprintf("#%d", h.NodeID)
		if n, err := db.GetNode(s.DB, h.NodeID); err == nil {
			nodeName = n.Name
		}
		tasks = append(tasks, probeTask{idx: i, nodeID: h.NodeID, name: nodeName, target: target})
	}

	results := make([]hopProbe, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t probeTask) {
			defer wg.Done()
			hp := hopProbe{Node: t.name, Target: t.target}
			ack, err := s.Hub.SendProbe(t.nodeID, t.target)
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
