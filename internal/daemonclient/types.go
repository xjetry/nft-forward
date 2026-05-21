package daemonclient

import "nft-forward/internal/nft"

// OwnerRuleset mirrors the daemon's internal type but lives here so client
// callers (TUI today, server/agent later) do not need to import
// internal/daemon. Both sides serialize to the same JSON shape, so a
// type-level mismatch is impossible by construction.
type OwnerRuleset map[string][]nft.Rule

// segmentPayload is the body of POST /v1/ruleset/{owner}.
type segmentPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// fullPayload is the body of GET /v1/ruleset.
type fullPayload struct {
	Owners OwnerRuleset `json:"owners"`
}
