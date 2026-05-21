package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"nft-forward/internal/nft"
)

// Default* paths for the three legacy state files that may exist on hosts
// that previously ran nft-forward (TUI), nft-agent, or nft-server (with its
// embedded agent). The daemon imports these into owner segments on first
// boot so users do not lose their existing rules.
const (
	DefaultLegacyTUIPath           = "/etc/nft-forward/rules.json"
	DefaultLegacyAgentPath         = "/var/lib/nft-forward/agent-state.json"
	DefaultLegacyEmbeddedAgentPath = "/var/lib/nft-forward/embedded-agent-state.json"
)

// LegacyMigrationPaths bundles the three legacy state file locations the
// daemon checks at boot. Each is a string so callers (notably tests) can
// override individual fields.
type LegacyMigrationPaths struct {
	TUI           string
	Agent         string
	EmbeddedAgent string
}

// DefaultLegacyPaths returns the production legacy path set.
func DefaultLegacyPaths() LegacyMigrationPaths {
	return LegacyMigrationPaths{
		TUI:           DefaultLegacyTUIPath,
		Agent:         DefaultLegacyAgentPath,
		EmbeddedAgent: DefaultLegacyEmbeddedAgentPath,
	}
}

// migrateLegacyState reads any of the three legacy state files that exist
// at the given paths and returns them as a partially-populated OwnerRuleset.
// Each processed file is renamed to "<path>.migrated" so the daemon can be
// re-run idempotently and an operator has a clear breadcrumb of what was
// imported. Rules from agent-state.json and embedded-agent-state.json both
// land in the "panel" segment; if both are non-empty, embedded wins because
// it represents the controller's authoritative view.
//
// Empty files (no rules) are still renamed (they have been consumed), but
// do not produce an owner key — we don't want GET /v1/ruleset to expose
// always-empty owners after migration.
func migrateLegacyState(p LegacyMigrationPaths) (OwnerRuleset, error) {
	out := OwnerRuleset{}

	tuiRules, err := readLegacyRules(p.TUI)
	if err != nil {
		return nil, fmt.Errorf("read legacy tui state %s: %w", p.TUI, err)
	}
	if tuiRules != nil {
		if len(tuiRules) > 0 {
			out["tui"] = tuiRules
		}
		if err := os.Rename(p.TUI, p.TUI+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy tui state: %w", err)
		}
	}

	agentRules, err := readLegacyRules(p.Agent)
	if err != nil {
		return nil, fmt.Errorf("read legacy agent state %s: %w", p.Agent, err)
	}
	if agentRules != nil {
		if len(agentRules) > 0 {
			out["panel"] = agentRules
		}
		if err := os.Rename(p.Agent, p.Agent+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy agent state: %w", err)
		}
	}

	embeddedRules, err := readLegacyRules(p.EmbeddedAgent)
	if err != nil {
		return nil, fmt.Errorf("read legacy embedded agent state %s: %w", p.EmbeddedAgent, err)
	}
	if embeddedRules != nil {
		// embedded-agent-state.json wins over agent-state.json — server is
		// authoritative for the panel segment when both exist.
		if len(embeddedRules) > 0 {
			out["panel"] = embeddedRules
		}
		if err := os.Rename(p.EmbeddedAgent, p.EmbeddedAgent+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy embedded state: %w", err)
		}
	}

	return out, nil
}

// readLegacyRules reads a legacy `[]nft.Rule` JSON array file. Returns
// (nil, nil) when the file does not exist so the caller can distinguish
// "no migration needed" from "migration produced empty result".
func readLegacyRules(path string) ([]nft.Rule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return []nft.Rule{}, nil
	}
	var rules []nft.Rule
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return rules, nil
}
