package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// recorder is a test runner that captures calls so we can assert on the
// nft commands the shim issued.
type recorder struct {
	listOut   string
	listErr   error
	scripts   []string
	scriptErr error
	listArgs  [][]string
}

func (r *recorder) run(args ...string) (string, error) {
	r.listArgs = append(r.listArgs, args)
	return r.listOut, r.listErr
}

func (r *recorder) runScript(script string) error {
	r.scripts = append(r.scripts, script)
	return r.scriptErr
}

func newDockerUserShimWith(r *recorder) *DockerUserShim {
	return &DockerUserShim{runNft: r.run, runNftScript: r.runScript}
}

func TestDockerUserShimName(t *testing.T) {
	s := NewDockerUserShim()
	if s.Name() != "docker-user" {
		t.Fatalf("got %q", s.Name())
	}
}

func TestDockerUserShimDetectTrue(t *testing.T) {
	r := &recorder{listOut: `chain DOCKER-USER { ... }`}
	s := newDockerUserShimWith(r)
	if !s.Detect() {
		t.Fatal("expected Detect to return true on successful list")
	}
}

func TestDockerUserShimDetectFalseOnError(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newDockerUserShimWith(r)
	if s.Detect() {
		t.Fatal("expected Detect to return false when chain missing")
	}
}

func TestDockerUserShimSyncSkipsWhenAbsent(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newDockerUserShimWith(r)
	if err := s.Sync(FirewallState{}); err != nil {
		t.Fatalf("Sync should swallow missing chain: %v", err)
	}
	if len(r.scripts) != 0 {
		t.Fatalf("no script should have been run, got %v", r.scripts)
	}
}

func TestDockerUserShimSyncInjectsRule(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
	}
}`,
	}
	s := newDockerUserShimWith(r)
	rules := []nft.Rule{{Proto: "tcp", DestIP: "10.20.1.20", DestPort: 8443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	// The fake's chain listing succeeds for any family, so Sync's ip and ip6
	// passes both produce a script (see the fake's listOut in newDockerUserShimWith).
	if len(r.scripts) != 2 {
		t.Fatalf("expected 2 scripts (ip + ip6), got %d", len(r.scripts))
	}
	if !strings.Contains(r.scripts[0], "ip daddr 10.20.1.20 tcp dport 8443 counter accept") {
		t.Fatalf("rule missing from ip script:\n%s", r.scripts[0])
	}
	if strings.Contains(r.scripts[1], "ip daddr") {
		t.Fatalf("ip6 script must not carry the v4 rule as an ip6 daddr match:\n%s", r.scripts[1])
	}
}

// TestDockerUserShimSyncMirrorsIPv6Rule verifies a v6 DNAT target lands only
// in the ip6 family's script, as "ip6 daddr" — not "ip daddr" ip6tables
// would reject at nft -f time.
func TestDockerUserShimSyncMirrorsIPv6Rule(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
	}
}`,
	}
	s := newDockerUserShimWith(r)
	rules := []nft.Rule{{Proto: "tcp", DestIP: "2001:db8::1", DestPort: 8443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	if len(r.scripts) != 2 {
		t.Fatalf("expected 2 scripts (ip + ip6), got %d", len(r.scripts))
	}
	if strings.Contains(r.scripts[0], "2001:db8::1") {
		t.Fatalf("v6 dest must not land in the ip script:\n%s", r.scripts[0])
	}
	if !strings.Contains(r.scripts[1], "ip6 daddr 2001:db8::1 tcp dport 8443 counter accept") {
		t.Fatalf("v6 rule missing from ip6 script:\n%s", r.scripts[1])
	}
}

func TestDockerUserShimSyncDeletesStaleThenAdds(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
		ct state established,related counter accept comment "nft-forward managed" # handle 7
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 8
	}
}`,
	}
	s := newDockerUserShimWith(r)
	if err := s.Sync(FirewallState{}); err != nil {
		t.Fatal(err)
	}
	script := r.scripts[0]
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 7") {
		t.Fatalf("stale handle 7 not deleted:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 8") {
		t.Fatalf("stale handle 8 not deleted:\n%s", script)
	}
}

func TestDockerUserShimCleanupRemovesAll(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
		ct state established,related counter accept comment "nft-forward managed" # handle 7
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 8
	}
}`,
	}
	s := newDockerUserShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	// The fake's chain listing succeeds for any family, so Cleanup's ip and
	// ip6 passes both emit a script.
	if len(r.scripts) != 2 {
		t.Fatalf("expected 2 cleanup scripts (ip + ip6), got %d", len(r.scripts))
	}
	script := r.scripts[0]
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 7") {
		t.Fatalf("handle 7 should be deleted:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 8") {
		t.Fatalf("handle 8 should be deleted:\n%s", script)
	}
	if strings.Contains(script, "add rule") {
		t.Fatalf("cleanup must not re-add rules:\n%s", script)
	}
}

func TestDockerUserShimCleanupAbsentNoOp(t *testing.T) {
	r := &recorder{listErr: errors.New("no chain")}
	s := newDockerUserShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatalf("Cleanup should swallow missing chain: %v", err)
	}
	if len(r.scripts) != 0 {
		t.Fatalf("no script should run when chain absent, got %v", r.scripts)
	}
}
