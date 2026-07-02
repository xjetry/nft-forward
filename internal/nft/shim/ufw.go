package shim

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"nft-forward/internal/nft"
)

const (
	ufwForwardChain  = "ufw-user-forward"
	ufwInputChain    = "ufw-user-input"
	ufw6ForwardChain = "ufw6-user-forward"
	ufw6InputChain   = "ufw6-user-input"
)

// UfwShim integrates with ufw's user-extension chains. It manages
// ufw-user-forward (FORWARD accepts for DNAT targets) and ufw-user-input
// (INPUT accepts for userspace TCP listen ports so the embedded relay is
// reachable when INPUT defaults to drop), plus their ufw6-user-* IPv6
// counterparts via ip6tables — ufw tracks the two protocol families in
// separate tables, so a rule added to one never covers the other.
//
// runIpt6 is nil-safe: callers that only wire runIpt (as every pre-dual-stack
// test here does) get the original IPv4-only behavior, since every v6 call
// site is guarded.
//
// All mutations go through iptables/ip6tables (not raw nft) to preserve
// iptables-nft generation tracking; raw nft writes to iptables-managed
// chains break ufw status detection.
type UfwShim struct {
	runIpt  iptRunner
	runIpt6 iptRunner
}

// iptRunner runs `iptables <args>` (or `ip6tables <args>`) and returns
// stdout. Tests substitute a fake.
type iptRunner func(args ...string) (string, error)

func NewUfwShim() *UfwShim {
	return &UfwShim{runIpt: defaultIptRunner, runIpt6: defaultIpt6Runner}
}

func (s *UfwShim) Name() string { return "ufw" }

func (s *UfwShim) Detect() bool {
	_, err := s.runIpt("-L", ufwForwardChain, "-n")
	return err == nil
}

func (s *UfwShim) Sync(state FirewallState) error {
	if err := s.syncForward(state.ForwardRules); err != nil {
		return err
	}
	return s.syncInput(state.ListenPorts)
}

func (s *UfwShim) syncForward(rules []nft.Rule) error {
	if err := syncForwardFamily(s.runIpt, ufwForwardChain, rules, false); err != nil {
		return err
	}
	if s.runIpt6 == nil {
		return nil
	}
	return syncForwardFamily(s.runIpt6, ufw6ForwardChain, rules, true)
}

// syncForwardFamily rebuilds one chain's owned rules for the given address
// family. v6 selects which of rules' dest addresses this pass owns — the
// same rule list feeds both the v4 and v6 chains, each keeping only its
// matching addresses.
func syncForwardFamily(run iptRunner, chain string, rules []nft.Rule, v6 bool) error {
	if _, err := run("-L", chain, "-n"); err != nil {
		return nil
	}
	if err := deleteOwned(run, chain); err != nil {
		return err
	}
	if _, err := run("-A", chain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-m", "comment", "--comment", OwnerComment,
		"-j", "ACCEPT"); err != nil {
		return err
	}
	for _, r := range rules {
		if r.DestIP == "" || nft.IsIPv6(r.DestIP) != v6 {
			continue
		}
		for _, proto := range iptExpandProto(r.Proto) {
			if _, err := run("-A", chain,
				"-d", r.DestIP,
				"-p", proto, "--dport", strconv.Itoa(r.DestPort),
				"-m", "comment", "--comment", OwnerComment,
				"-j", "ACCEPT"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *UfwShim) syncInput(ports []ListenPort) error {
	if err := syncInputFamily(s.runIpt, ufwInputChain, ports); err != nil {
		return err
	}
	if s.runIpt6 == nil {
		return nil
	}
	return syncInputFamily(s.runIpt6, ufw6InputChain, ports)
}

// syncInputFamily rebuilds one chain's owned listen-port accepts. Unlike
// syncForwardFamily, ports carry no address — a userspace TCP listener binds
// dual-stack (net.Listen("tcp", ...) with no host), so the same port list
// applies to both the v4 and v6 chains.
func syncInputFamily(run iptRunner, chain string, ports []ListenPort) error {
	if _, err := run("-L", chain, "-n"); err != nil {
		return nil
	}
	if err := deleteOwned(run, chain); err != nil {
		return err
	}
	for _, p := range ports {
		if _, err := run("-A", chain,
			"-p", p.Proto, "--dport", strconv.Itoa(p.Port),
			"-m", "comment", "--comment", OwnerComment,
			"-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}

// deleteOwned parses `<runner> -S <chain>` to find owner-tagged rules,
// then deletes them by rule number in reverse order.
func deleteOwned(run iptRunner, chain string) error {
	out, err := run("-S", chain)
	if err != nil {
		return nil
	}
	var ruleNums []int
	num := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		num++
		if strings.Contains(line, OwnerComment) {
			ruleNums = append(ruleNums, num)
		}
	}
	for i := len(ruleNums) - 1; i >= 0; i-- {
		if _, err := run("-D", chain, strconv.Itoa(ruleNums[i])); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup attempts every chain even when one fails — an early return on a
// v4 chain would strand owner-tagged accepts in the ufw6 chains with no
// process left to ever remove them. The first error is reported after all
// chains have been tried.
func (s *UfwShim) Cleanup() error {
	var firstErr error
	cleanup := func(run iptRunner, chains ...string) {
		for _, chain := range chains {
			if _, err := run("-L", chain, "-n"); err != nil {
				continue
			}
			if err := deleteOwned(run, chain); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	cleanup(s.runIpt, ufwForwardChain, ufwInputChain)
	if s.runIpt6 != nil {
		cleanup(s.runIpt6, ufw6ForwardChain, ufw6InputChain)
	}
	return firstErr
}

// iptExpandProto splits "tcp+udp" into separate iptables protocol entries.
func iptExpandProto(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}

func defaultIptRunner(args ...string) (string, error) {
	return runIptablesBin("iptables", args...)
}

func defaultIpt6Runner(args ...string) (string, error) {
	return runIptablesBin("ip6tables", args...)
}

func runIptablesBin(bin string, args ...string) (string, error) {
	all := append([]string{"-w", "5"}, args...)
	cmd := exec.Command(bin, all...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %v: %s",
			bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
