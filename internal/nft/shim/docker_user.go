package shim

import (
	"strconv"
)

const (
	dockerUserFamily = "ip"
	dockerUserTable  = "filter"
	dockerUserChain  = "DOCKER-USER"
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

// Sync updates DOCKER-USER with FORWARD accepts for the current DNAT targets.
// Docker manages host INPUT filtering separately, so listen ports are ignored.
func (s *DockerUserShim) Sync(state FirewallState) error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil // chain absent; nothing to do
	}
	stale := parseShimHandles(out)
	script := renderShimScript(dockerUserFamily, dockerUserTable, dockerUserChain, state.ForwardRules, stale)
	return s.runNftScript(script)
}

func (s *DockerUserShim) Cleanup() error {
	return cleanupChain(s.runNft, s.runNftScript, dockerUserFamily, dockerUserTable, dockerUserChain)
}

// formatDelete produces a single `delete rule family table chain handle N`
// line, terminated by newline. Shared with other shims via the package.
func formatDelete(family, table, chain string, handle int) string {
	return "delete rule " + family + " " + table + " " + chain + " handle " + strconv.Itoa(handle) + "\n"
}
