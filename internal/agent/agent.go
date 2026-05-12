package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"nft-forward/internal/nft"
	"nft-forward/internal/tc"
)

type Config struct {
	Listen    string
	Token     string
	StatePath string
	Iface     string
}

type Agent struct {
	cfg   Config
	mu    sync.Mutex
	rules []nft.Rule
}

type ApplyRequest struct {
	Rules []nft.Rule `json:"rules"`
}

type StatusResponse struct {
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"started_at"`
	LastApply  time.Time `json:"last_apply,omitempty"`
	RuleCount  int       `json:"rule_count"`
	NftAvail   bool      `json:"nft_available"`
	IPForward  bool      `json:"ip_forward"`
}

var startedAt = time.Now()

func New(cfg Config) *Agent {
	return &Agent{cfg: cfg}
}

// Bootstrap loads previously-saved rules from disk and pushes them into the
// kernel so a fresh boot of the node restores prior state without waiting for
// the panel to call /v1/apply.
func (a *Agent) Bootstrap() error {
	rules, err := loadState(a.cfg.StatePath)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.rules = rules
	a.mu.Unlock()
	if err := nft.Apply(rules); err != nil {
		return err
	}
	if err := tc.Apply(rules, a.cfg.Iface); err != nil {
		return err
	}
	return nil
}

func (a *Agent) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", a.requireAuth(a.handleStatus))
	mux.HandleFunc("/v1/apply", a.requireAuth(a.handleApply))
	mux.HandleFunc("/v1/counters", a.requireAuth(a.handleCounters))
	mux.HandleFunc("/healthz", a.handleHealth)

	log.Printf("nft-agent listening on %s", a.cfg.Listen)
	return http.ListenAndServe(a.cfg.Listen, mux)
}

func (a *Agent) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h2 := r.Header.Get("Authorization")
		if len(h2) <= len(prefix) || h2[:len(prefix)] != prefix {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		got := []byte(h2[len(prefix):])
		want := []byte(a.cfg.Token)
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	count := len(a.rules)
	a.mu.Unlock()
	resp := StatusResponse{
		Version:   "0.1.0",
		StartedAt: startedAt,
		RuleCount: count,
		NftAvail:  nft.Available(),
		IPForward: nft.IPForwardEnabled(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleCounters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counters, err := a.GetCounters()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counters": counters})
}

func (a *Agent) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.ApplyRules(req.Rules); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": len(req.Rules)})
}

// ApplyRules is the in-process equivalent of POST /v1/apply. The embedded
// agent (panel-side) calls this directly to avoid an HTTP loop back to
// itself.
func (a *Agent) ApplyRules(rules []nft.Rule) error {
	for _, rl := range rules {
		if err := nft.Validate(rl); err != nil {
			return fmt.Errorf("invalid rule %s/%d: %w", rl.Proto, rl.SrcPort, err)
		}
	}
	if err := nft.Apply(rules); err != nil {
		return err
	}
	if err := tc.Apply(rules, a.cfg.Iface); err != nil {
		return fmt.Errorf("tc: %w", err)
	}
	if err := saveState(a.cfg.StatePath, rules); err != nil {
		log.Printf("warn: saveState: %v", err)
	}
	a.mu.Lock()
	a.rules = rules
	a.mu.Unlock()
	return nil
}

func (a *Agent) GetCounters() ([]nft.Counter, error) {
	return nft.Counters()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func loadState(path string) ([]nft.Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []nft.Rule{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return []nft.Rule{}, nil
	}
	var rules []nft.Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func saveState(path string, rules []nft.Rule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
