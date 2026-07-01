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
	ufwForwardChain = "ufw-user-forward"
	ufwInputChain   = "ufw-user-input"
)

// UfwShim integrates with ufw's user-extension chains. It manages
// ufw-user-forward (FORWARD accepts for DNAT targets) and ufw-user-input
// (INPUT accepts for userspace TCP listen ports so the embedded relay is
// reachable when INPUT defaults to drop).
//
// All mutations go through iptables (not raw nft) to preserve iptables-nft
// generation tracking; raw nft writes to iptables-managed chains break
// ufw status detection.
type UfwShim struct {
	runIpt iptRunner
}

// iptRunner runs `iptables <args>` and returns stdout. Tests substitute a fake.
type iptRunner func(args ...string) (string, error)

func NewUfwShim() *UfwShim {
	return &UfwShim{runIpt: defaultIptRunner}
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
	if _, err := s.runIpt("-L", ufwForwardChain, "-n"); err != nil {
		return nil
	}
	if err := s.deleteOwned(ufwForwardChain); err != nil {
		return err
	}
	if _, err := s.runIpt("-A", ufwForwardChain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-m", "comment", "--comment", OwnerComment,
		"-j", "ACCEPT"); err != nil {
		return err
	}
	for _, r := range rules {
		if r.DestIP == "" || nft.IsIPv6(r.DestIP) {
			continue
		}
		for _, proto := range iptExpandProto(r.Proto) {
			if _, err := s.runIpt("-A", ufwForwardChain,
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
	if _, err := s.runIpt("-L", ufwInputChain, "-n"); err != nil {
		return nil
	}
	if err := s.deleteOwned(ufwInputChain); err != nil {
		return err
	}
	for _, p := range ports {
		if _, err := s.runIpt("-A", ufwInputChain,
			"-p", p.Proto, "--dport", strconv.Itoa(p.Port),
			"-m", "comment", "--comment", OwnerComment,
			"-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}

// deleteOwned parses `iptables -S <chain>` to find owner-tagged rules,
// then deletes them by rule number in reverse order.
func (s *UfwShim) deleteOwned(chain string) error {
	out, err := s.runIpt("-S", chain)
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
		if _, err := s.runIpt("-D", chain, strconv.Itoa(ruleNums[i])); err != nil {
			return err
		}
	}
	return nil
}

func (s *UfwShim) Cleanup() error {
	for _, chain := range []string{ufwForwardChain, ufwInputChain} {
		if _, err := s.runIpt("-L", chain, "-n"); err != nil {
			continue
		}
		if err := s.deleteOwned(chain); err != nil {
			return err
		}
	}
	return nil
}

// iptExpandProto splits "tcp+udp" into separate iptables protocol entries.
func iptExpandProto(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}

func defaultIptRunner(args ...string) (string, error) {
	all := append([]string{"-w", "5"}, args...)
	cmd := exec.Command("iptables", all...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("iptables %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
