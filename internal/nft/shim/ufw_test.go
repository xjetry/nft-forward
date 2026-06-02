package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func newUfwShimWith(r *recorder) *UfwShim {
	return &UfwShim{runNft: r.run, runNftScript: r.runScript}
}

func TestUfwShimName(t *testing.T) {
	s := NewUfwShim()
	if s.Name() != "ufw" {
		t.Fatalf("got %q", s.Name())
	}
}

func TestUfwShimDetectTrue(t *testing.T) {
	r := &recorder{listOut: `chain ufw-user-forward { ... }`}
	s := newUfwShimWith(r)
	if !s.Detect() {
		t.Fatal("expected Detect to return true on successful list")
	}
}

func TestUfwShimDetectFalseOnError(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newUfwShimWith(r)
	if s.Detect() {
		t.Fatal("expected Detect to return false when chain missing")
	}
}

func TestUfwShimSyncTargetsUfwUserForward(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain ufw-user-forward {
	}
}`,
	}
	s := newUfwShimWith(r)
	rules := []nft.Rule{{Proto: "tcp", DestIP: "192.168.1.5", DestPort: 443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	script := r.scripts[0]
	if !strings.Contains(script, "ufw-user-forward") {
		t.Fatalf("script must target ufw-user-forward, got:\n%s", script)
	}
	if strings.Contains(script, "DOCKER-USER") {
		t.Fatalf("script must not mention DOCKER-USER:\n%s", script)
	}
	if !strings.Contains(script, "ip daddr 192.168.1.5 tcp dport 443") {
		t.Fatalf("rule missing:\n%s", script)
	}
}

func TestUfwSync_InputChainGetsListenPorts(t *testing.T) {
	var scripts []string
	s := &UfwShim{
		runNft:       func(args ...string) (string, error) { return "", nil },
		runNftScript: func(script string) error { scripts = append(scripts, script); return nil },
	}
	err := s.Sync(FirewallState{
		ForwardRules: []nft.Rule{{Proto: "tcp", DestIP: "10.0.0.1", DestPort: 443}},
		ListenPorts:  []ListenPort{{Proto: "tcp", Port: 8443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(scripts, "\n")
	if !strings.Contains(joined, "ufw-user-forward") || !strings.Contains(joined, "ip daddr 10.0.0.1") {
		t.Errorf("forward chain not synced:\n%s", joined)
	}
	if !strings.Contains(joined, "ufw-user-input") || !strings.Contains(joined, "tcp dport 8443") {
		t.Errorf("input chain not synced:\n%s", joined)
	}
}

func TestUfwShimCleanupRemovesAll(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain ufw-user-forward {
		ct state established,related counter accept comment "nft-forward managed" # handle 22
	}
}`,
	}
	s := newUfwShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.scripts[0], "delete rule ip filter ufw-user-forward handle 22") {
		t.Fatalf("handle 22 should be deleted:\n%s", r.scripts[0])
	}
}
