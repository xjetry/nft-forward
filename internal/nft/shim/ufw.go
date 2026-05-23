package shim

import (
	"nft-forward/internal/nft"
)

const (
	ufwFamily = "ip"
	ufwTable  = "filter"
	ufwChain  = "ufw-user-forward"
)

// UfwShim integrates with ufw's ufw-user-forward chain. Same general
// pattern as DOCKER-USER: ufw provides this chain as the documented
// extension point for forward-direction rules added by external tools.
type UfwShim struct {
	runNft       nftRunner
	runNftScript nftScriptRunner
}

func NewUfwShim() *UfwShim {
	return &UfwShim{
		runNft:       defaultNftRunner,
		runNftScript: defaultNftScriptRunner,
	}
}

func (s *UfwShim) Name() string { return "ufw" }

func (s *UfwShim) Detect() bool {
	_, err := s.runNft("list", "chain", ufwFamily, ufwTable, ufwChain)
	return err == nil
}

func (s *UfwShim) Sync(rules []nft.Rule) error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, ufwChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	script := renderShimScript(ufwFamily, ufwTable, ufwChain, rules, stale)
	return s.runNftScript(script)
}

func (s *UfwShim) Cleanup() error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, ufwChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	if len(stale) == 0 {
		return nil
	}
	var script string
	for _, h := range stale {
		script += formatDelete(ufwFamily, ufwTable, ufwChain, h)
	}
	return s.runNftScript(script)
}
