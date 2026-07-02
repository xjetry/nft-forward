package shim

import (
	"strconv"

	"nft-forward/internal/nft"
)

const (
	dockerUserFamily  = "ip"
	dockerUserFamily6 = "ip6"
	dockerUserTable   = "filter"
	dockerUserChain   = "DOCKER-USER"
)

// DockerUserShim integrates with Docker's DOCKER-USER chain. Docker
// places this chain at the head of the FORWARD chain explicitly so
// upstream applications can append accept rules without conflicting
// with Docker's own rule generation.
type DockerUserShim struct {
	runNft       nftRunner
	runNftScript nftScriptRunner
}

func NewDockerUserShim() *DockerUserShim {
	return &DockerUserShim{
		runNft:       defaultNftRunner,
		runNftScript: defaultNftScriptRunner,
	}
}

func (s *DockerUserShim) Name() string { return "docker-user" }

func (s *DockerUserShim) Detect() bool {
	_, err := s.runNft("list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	return err == nil
}

// Sync updates DOCKER-USER with FORWARD accepts for the current DNAT
// targets, in both the ip and ip6 tables — Docker creates DOCKER-USER
// separately in each family, and a rule added to one never covers the
// other. Docker manages host INPUT filtering separately, so listen ports
// are ignored.
func (s *DockerUserShim) Sync(state FirewallState) error {
	if err := s.syncFamily(dockerUserFamily, state.ForwardRules); err != nil {
		return err
	}
	return s.syncFamily(dockerUserFamily6, state.ForwardRules)
}

func (s *DockerUserShim) syncFamily(family string, rules []nft.Rule) error {
	out, err := s.runNft("-a", "list", "chain", family, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil // chain absent for this family; nothing to do
	}
	stale := parseShimHandles(out)
	script := renderShimScript(family, dockerUserTable, dockerUserChain, rules, stale)
	return s.runNftScript(script)
}

func (s *DockerUserShim) Cleanup() error {
	if err := cleanupChain(s.runNft, s.runNftScript, dockerUserFamily, dockerUserTable, dockerUserChain); err != nil {
		return err
	}
	return cleanupChain(s.runNft, s.runNftScript, dockerUserFamily6, dockerUserTable, dockerUserChain)
}

// formatDelete produces a single `delete rule family table chain handle N`
// line, terminated by newline. Shared with other shims via the package.
func formatDelete(family, table, chain string, handle int) string {
	return "delete rule " + family + " " + table + " " + chain + " handle " + strconv.Itoa(handle) + "\n"
}
