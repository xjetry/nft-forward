package agent

import (
	"context"
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
	"nft-forward/internal/resolver"
	"nft-forward/internal/tc"
)

type Config struct {
	Listen    string
	Token     string
	StatePath string
	Iface     string
}

type Agent struct {
	cfg      Config
	mu       sync.Mutex
	rules    []nft.Rule
	resolver *resolver.Resolver
	stopDNS  chan struct{}
}

type ApplyRequest struct {
	Rules []nft.Rule `json:"rules"`
}

type StatusResponse struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	LastApply time.Time `json:"last_apply,omitempty"`
	RuleCount int       `json:"rule_count"`
	NftAvail  bool      `json:"nft_available"`
	IPForward bool      `json:"ip_forward"`
}

var startedAt = time.Now()

func New(cfg Config) *Agent {
	return &Agent{
		cfg:      cfg,
		resolver: resolver.New(),
		stopDNS:  make(chan struct{}),
	}
}

// Bootstrap loads previously-saved rules from disk and pushes them into the
// kernel so a fresh boot of the node restores prior state without waiting for
// the panel to call /v1/apply.
func (a *Agent) Bootstrap() error {
	rules, err := loadState(a.cfg.StatePath)
	if err != nil {
		return err
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, a.resolver)
	if dnsErr != nil {
		log.Printf("warn: dns at bootstrap: %v", dnsErr)
	}
	a.mu.Lock()
	a.rules = resolved
	a.mu.Unlock()
	if err := nft.Apply(resolved); err != nil {
		return err
	}
	if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
		return err
	}
	return nil
}

func (a *Agent) Serve() error {
	go a.dnsLoop(dnsInterval())

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
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, a.resolver)
	if dnsErr != nil {
		log.Printf("warn: dns: %v", dnsErr)
	}
	// Reject only when a host-only rule has no usable IP after resolution; an
	// IP-only rule that never needed DNS must still apply cleanly.
	for _, rl := range resolved {
		if rl.DestIP == "" {
			return fmt.Errorf("rule %s/%d: 无法解析目标域名 %s", rl.Proto, rl.SrcPort, rl.DestHost)
		}
	}
	if err := nft.Apply(resolved); err != nil {
		return err
	}
	if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
		return fmt.Errorf("tc: %w", err)
	}
	if err := saveState(a.cfg.StatePath, resolved); err != nil {
		log.Printf("warn: saveState: %v", err)
	}
	a.mu.Lock()
	a.rules = resolved
	a.mu.Unlock()
	return nil
}

// dnsLoop periodically re-resolves any DestHost-bearing rules. When a target
// IP moves (typical DDNS event), we rebuild the nftables ruleset in place so
// new flows hit the new backend without operator intervention.
func (a *Agent) dnsLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.stopDNS:
			return
		case <-t.C:
			a.mu.Lock()
			snap := append([]nft.Rule(nil), a.rules...)
			a.mu.Unlock()
			if !hasHost(snap) {
				continue
			}
			resolved, changed, err := nft.ResolveHosts(context.Background(), snap, a.resolver)
			if err != nil {
				log.Printf("dns refresh: %v", err)
			}
			if !changed {
				continue
			}
			if err := nft.Apply(resolved); err != nil {
				log.Printf("dns refresh apply: %v", err)
				continue
			}
			if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
				log.Printf("dns refresh tc: %v", err)
			}
			a.mu.Lock()
			a.rules = resolved
			a.mu.Unlock()
			_ = saveState(a.cfg.StatePath, resolved)
			log.Printf("dns refresh: %d rule(s) re-applied", len(resolved))
		}
	}
}

func hasHost(rules []nft.Rule) bool {
	for _, r := range rules {
		if r.DestHost != "" {
			return true
		}
	}
	return false
}

func dnsInterval() time.Duration {
	if s := os.Getenv("NFT_FORWARD_DNS_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
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
