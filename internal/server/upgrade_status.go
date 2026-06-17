package server

import (
	"time"

	"nft-forward/internal/db"
)

// upgradeGrace is how long after an acked upgrade we still treat an unchanged
// agent_version as "in progress" (the daemon restarts and reconnects). Past it,
// an unchanged version means the restart almost certainly failed.
const upgradeGrace = 90 * time.Second

// upgradeView is the derived upgrade state shown on the node detail page.
type upgradeView struct {
	At      int64  `json:"at,omitempty"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"` // none | ok | error | pending | stuck
	Error   string `json:"error,omitempty"`
}

// deriveUpgradeStatus turns a node's stored last-upgrade columns plus its live
// agent_version into a display status. It surfaces the silent failure: an acked
// upgrade whose target version never took, past the grace window.
func deriveUpgradeStatus(n *db.Node, now time.Time) upgradeView {
	if !n.LastUpgradeAt.Valid {
		return upgradeView{Status: "none"}
	}
	v := upgradeView{At: n.LastUpgradeAt.Int64, Version: n.LastUpgradeVersion, Error: n.LastUpgradeError}
	switch {
	case n.LastUpgradeStatus == "error":
		v.Status = "error"
	case n.LastUpgradeVersion != "" && n.AgentVersion == n.LastUpgradeVersion:
		v.Status = "ok"
	case now.Unix()-n.LastUpgradeAt.Int64 <= int64(upgradeGrace/time.Second):
		v.Status = "pending"
	default:
		v.Status = "stuck"
	}
	return v
}
