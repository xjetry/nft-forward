package daemon

// AgentMeta holds the dialer's persisted runtime state — bookkeeping
// the daemon needs across restarts to short-circuit redundant
// apply_ruleset pushes after reconnect.
type AgentMeta struct {
	// LastAppliedRev is the panel-segment version identifier the daemon
	// has most recently acknowledged. Reported in hello so the server
	// can skip pushing an apply_ruleset whose contents the daemon already
	// has on disk.
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
}
