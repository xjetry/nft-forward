package server

import (
	"encoding/json"
	"net"
	"net/http"
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
