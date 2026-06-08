package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"
)

const probeTimeout = 5 * time.Second

type probeResult struct {
	OK      bool   `json:"ok"`
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
