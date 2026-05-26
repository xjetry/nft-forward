package daemon

import "time"

// AgentMeta holds the dialer's persisted runtime state — bookkeeping the
// daemon needs across restarts to honor the "ACK before clear" invariant
// for the tui→panel segment migration, and to short-circuit redundant
// apply_ruleset pushes after reconnect.
type AgentMeta struct {
	// MigratedAt is the timestamp at which the daemon last received a
	// register_local_ack from the panel. Zero means the local tui
	// segment has never been handed off — the dialer will try again on
	// every successful (re)connect.
	MigratedAt time.Time `json:"migrated_at,omitempty"`

	// LastAppliedRev is the panel-segment version identifier the daemon
	// has most recently acknowledged. Reported in hello so the server
	// can skip pushing an apply_ruleset whose contents the daemon already
	// has on disk.
	LastAppliedRev string `json:"last_applied_rev,omitempty"`

	// PanelURL is purely diagnostic. The authoritative connect target
	// is the --connect flag in the systemd unit; this field is written
	// for ops visibility when reading state.json by hand.
	PanelURL string `json:"panel_url,omitempty"`
}
