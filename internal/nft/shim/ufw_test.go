package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// ufwFake is a test double for iptRunner. It tracks every call and returns
// preset responses: -L and -S succeed only for chains in chainContent.
type ufwFake struct {
	calls        []string
	chainContent map[string]string // chain name → iptables -S output
}

func newUfwFake(chains map[string]string) *ufwFake {
	if chains == nil {
		chains = make(map[string]string)
	}
	return &ufwFake{chainContent: chains}
}

func (f *ufwFake) run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) >= 2 && (args[0] == "-L" || args[0] == "-S") {
		chain := args[1]
		content, ok := f.chainContent[chain]
		if !ok {
			return "", errors.New("iptables: No chain/target/match by that name")
		}
		if args[0] == "-S" {
			return content, nil
		}
		return "", nil
	}
	return "", nil
}

func TestUfwShimName(t *testing.T) {
	s := NewUfwShim()
	if s.Name() != "ufw" {
		t.Fatalf("got %q", s.Name())
	}
}

func TestUfwShimDetectTrue(t *testing.T) {
	f := newUfwFake(map[string]string{ufwForwardChain: ""})
	s := &UfwShim{runIpt: f.run}
	if !s.Detect() {
		t.Fatal("expected Detect to return true when chain exists")
	}
}

func TestUfwShimDetectFalseOnError(t *testing.T) {
	f := newUfwFake(nil)
	s := &UfwShim{runIpt: f.run}
	if s.Detect() {
		t.Fatal("expected Detect to return false when chain missing")
	}
}

func TestUfwShimSyncTargetsUfwUserForward(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	rules := []nft.Rule{{Proto: "tcp", DestIP: "192.168.1.5", DestPort: 443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "-A ufw-user-forward") {
		t.Fatalf("must target ufw-user-forward, got:\n%s", joined)
	}
	if strings.Contains(joined, "DOCKER-USER") {
		t.Fatalf("must not mention DOCKER-USER:\n%s", joined)
	}
	if !strings.Contains(joined, "-d 192.168.1.5 -p tcp --dport 443") {
		t.Fatalf("rule missing:\n%s", joined)
	}
}

func TestUfwSync_InputChainGetsListenPorts(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	err := s.Sync(FirewallState{
		ForwardRules: []nft.Rule{{Proto: "tcp", DestIP: "10.0.0.1", DestPort: 443}},
		ListenPorts:  []ListenPort{{Proto: "tcp", Port: 8443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "-A ufw-user-forward") || !strings.Contains(joined, "-d 10.0.0.1") {
		t.Errorf("forward chain not synced:\n%s", joined)
	}
	if !strings.Contains(joined, "-A ufw-user-input") || !strings.Contains(joined, "--dport 8443") {
		t.Errorf("input chain not synced:\n%s", joined)
	}
}

func TestUfwSync_CtStateRuleAdded(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	if err := s.Sync(FirewallState{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "--ctstate ESTABLISHED,RELATED") {
		t.Fatalf("ct state rule missing:\n%s", joined)
	}
}

func TestUfwSync_DeletesExistingOwnedRules(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: strings.Join([]string{
			"-N ufw-user-forward",
			`-A ufw-user-forward -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "nft-forward managed" -j ACCEPT`,
			`-A ufw-user-forward -d 10.0.0.1/32 -p tcp -m tcp --dport 80 -m comment --comment "nft-forward managed" -j ACCEPT`,
			"-A ufw-user-forward -j RETURN",
		}, "\n"),
		ufwInputChain: "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	if err := s.Sync(FirewallState{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	// Should delete rule 2 first (reverse order), then rule 1
	if !strings.Contains(joined, "-D ufw-user-forward 2") {
		t.Fatalf("expected delete of rule 2:\n%s", joined)
	}
	if !strings.Contains(joined, "-D ufw-user-forward 1") {
		t.Fatalf("expected delete of rule 1:\n%s", joined)
	}
	del2 := strings.Index(joined, "-D ufw-user-forward 2")
	del1 := strings.Index(joined, "-D ufw-user-forward 1")
	if del2 > del1 {
		t.Fatalf("must delete higher-numbered rule first:\n%s", joined)
	}
}

func TestUfwSync_TCPPlusUDP_ExpandsToTwoRules(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	rules := []nft.Rule{{Proto: "tcp+udp", DestIP: "10.0.0.1", DestPort: 443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "-p tcp --dport 443") {
		t.Fatalf("tcp rule missing:\n%s", joined)
	}
	if !strings.Contains(joined, "-p udp --dport 443") {
		t.Fatalf("udp rule missing:\n%s", joined)
	}
}

func TestUfwSync_SkipsIPv6(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	rules := []nft.Rule{{Proto: "tcp", DestIP: "2001:db8::1", DestPort: 443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if strings.Contains(joined, "2001:db8::1") {
		t.Fatalf("IPv6 address should be skipped:\n%s", joined)
	}
}

func TestUfwShimCleanupRemovesOwned(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: strings.Join([]string{
			"-N ufw-user-forward",
			`-A ufw-user-forward -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "nft-forward managed" -j ACCEPT`,
			"-A ufw-user-forward -j RETURN",
		}, "\n"),
		ufwInputChain: "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "-D ufw-user-forward 1") {
		t.Fatalf("should delete owned rule:\n%s", joined)
	}
	if strings.Contains(joined, "-A ") {
		t.Fatalf("cleanup must not add rules:\n%s", joined)
	}
}

func TestUfwShimCleanupAbsentNoOp(t *testing.T) {
	f := newUfwFake(nil)
	s := &UfwShim{runIpt: f.run}
	if err := s.Cleanup(); err != nil {
		t.Fatalf("Cleanup should be no-op when chains absent: %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "-D ") || strings.HasPrefix(c, "-A ") {
			t.Fatalf("no mutations expected when chains absent, got: %s", c)
		}
	}
}

func TestUfwSync_IPv6MirroredToUfw6Chains(t *testing.T) {
	f4 := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	f6 := newUfwFake(map[string]string{
		ufw6ForwardChain: "-N ufw6-user-forward\n",
		ufw6InputChain:   "-N ufw6-user-input\n",
	})
	s := &UfwShim{runIpt: f4.run, runIpt6: f6.run}
	rules := []nft.Rule{
		{Proto: "tcp", DestIP: "10.0.0.1", DestPort: 443},
		{Proto: "tcp", DestIP: "2001:db8::1", DestPort: 443},
	}
	err := s.Sync(FirewallState{
		ForwardRules: rules,
		ListenPorts:  []ListenPort{{Proto: "tcp", Port: 8443}},
	})
	if err != nil {
		t.Fatal(err)
	}

	joined4 := strings.Join(f4.calls, "\n")
	if strings.Contains(joined4, "2001:db8::1") {
		t.Fatalf("v6 dest must not land in the v4 chain:\n%s", joined4)
	}
	if !strings.Contains(joined4, "-d 10.0.0.1") {
		t.Fatalf("v4 dest missing from v4 chain:\n%s", joined4)
	}
	if !strings.Contains(joined4, "-A ufw-user-input") || !strings.Contains(joined4, "--dport 8443") {
		t.Fatalf("v4 input chain not synced:\n%s", joined4)
	}

	joined6 := strings.Join(f6.calls, "\n")
	if !strings.Contains(joined6, "-A ufw6-user-forward") || !strings.Contains(joined6, "-d 2001:db8::1") {
		t.Fatalf("v6 forward chain missing the v6 dest:\n%s", joined6)
	}
	if strings.Contains(joined6, "10.0.0.1") {
		t.Fatalf("v4 dest must not land in the v6 chain:\n%s", joined6)
	}
	if !strings.Contains(joined6, "--ctstate ESTABLISHED,RELATED") {
		t.Fatalf("v6 forward chain missing the ct-state accept:\n%s", joined6)
	}
	// Listen ports are family-agnostic (a userspace listener binds dual-stack),
	// so the same port must reach the v6 input chain too.
	if !strings.Contains(joined6, "-A ufw6-user-input") || !strings.Contains(joined6, "--dport 8443") {
		t.Fatalf("v6 input chain not synced:\n%s", joined6)
	}
}

func TestUfwSync_NilIpt6RunnerSkipsV6Chains(t *testing.T) {
	f := newUfwFake(map[string]string{
		ufwForwardChain: "-N ufw-user-forward\n",
		ufwInputChain:   "-N ufw-user-input\n",
	})
	s := &UfwShim{runIpt: f.run} // runIpt6 left nil, as production callers never do
	rules := []nft.Rule{{Proto: "tcp", DestIP: "2001:db8::1", DestPort: 443}}
	if err := s.Sync(FirewallState{ForwardRules: rules}); err != nil {
		t.Fatalf("nil runIpt6 must not panic or error: %v", err)
	}
}

func TestUfwShimCleanup_MirrorsV6Chains(t *testing.T) {
	f4 := newUfwFake(map[string]string{
		ufwForwardChain: strings.Join([]string{
			"-N ufw-user-forward",
			`-A ufw-user-forward -m comment --comment "nft-forward managed" -j ACCEPT`,
		}, "\n"),
		ufwInputChain: "-N ufw-user-input\n",
	})
	f6 := newUfwFake(map[string]string{
		ufw6ForwardChain: strings.Join([]string{
			"-N ufw6-user-forward",
			`-A ufw6-user-forward -m comment --comment "nft-forward managed" -j ACCEPT`,
		}, "\n"),
		ufw6InputChain: "-N ufw6-user-input\n",
	})
	s := &UfwShim{runIpt: f4.run, runIpt6: f6.run}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f6.calls, "\n"), "-D ufw6-user-forward 1") {
		t.Fatalf("cleanup must delete owned rule in the v6 chain too:\n%s", strings.Join(f6.calls, "\n"))
	}
}
