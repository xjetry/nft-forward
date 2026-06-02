package shim

const (
	ufwFamily     = "ip"
	ufwTable      = "filter"
	ufwChain      = "ufw-user-forward"
	ufwInputChain = "ufw-user-input"
)

// UfwShim integrates with ufw's user-extension chains. It manages
// ufw-user-forward (FORWARD accepts for DNAT targets) and ufw-user-input
// (INPUT accepts for userspace TCP listen ports so the embedded relay is
// reachable when INPUT defaults to drop).
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

func (s *UfwShim) Sync(state FirewallState) error {
	if err := s.syncChain(ufwChain, func(stale []int) string {
		return renderShimScript(ufwFamily, ufwTable, ufwChain, state.ForwardRules, stale)
	}); err != nil {
		return err
	}
	return s.syncChain(ufwInputChain, func(stale []int) string {
		return renderInputShimScript(ufwFamily, ufwTable, ufwInputChain, state.ListenPorts, stale)
	})
}

// syncChain lists one chain (skipping when absent), parses owner-tagged stale
// handles, and runs the built script in one atomic nft -f transaction.
func (s *UfwShim) syncChain(chain string, build func(stale []int) string) error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, chain)
	if err != nil {
		return nil // chain absent
	}
	stale := parseShimHandles(out)
	return s.runNftScript(build(stale))
}

func (s *UfwShim) Cleanup() error {
	for _, chain := range []string{ufwChain, ufwInputChain} {
		out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, chain)
		if err != nil {
			continue
		}
		stale := parseShimHandles(out)
		if len(stale) == 0 {
			continue
		}
		var script string
		for _, h := range stale {
			script += formatDelete(ufwFamily, ufwTable, chain, h)
		}
		if err := s.runNftScript(script); err != nil {
			return err
		}
	}
	return nil
}
