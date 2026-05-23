package shim

import (
	"nft-forward/internal/nft"
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

func (s *DockerUserShim) Sync(rules []nft.Rule) error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil // chain absent; nothing to do
	}
	stale := parseShimHandles(out)
	script := renderShimScript(dockerUserFamily, dockerUserTable, dockerUserChain, rules, stale)
	return s.runNftScript(script)
}

func (s *DockerUserShim) Cleanup() error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	if len(stale) == 0 {
		return nil
	}
	// Cleanup emits only deletes, no adds. Build a delete-only script
	// inline to avoid re-emitting the ct state add line that
	// renderShimScript always includes.
	var script string
	for _, h := range stale {
		script += formatDelete(dockerUserFamily, dockerUserTable, dockerUserChain, h)
	}
	return s.runNftScript(script)
}

// formatDelete produces a single `delete rule family table chain handle N`
// line, terminated by newline. Shared with other shims via the package.
func formatDelete(family, table, chain string, handle int) string {
	return "delete rule " + family + " " + table + " " + chain + " handle " + itoa(handle) + "\n"
}

// itoa is a tiny local integer formatter to avoid pulling in strconv
// just for one use. fmt.Sprintf would work too but introduces a wider
// dependency surface for what should be trivial.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
