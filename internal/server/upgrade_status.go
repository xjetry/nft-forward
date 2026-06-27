package server

import (
	"strconv"
	"strings"
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
func deriveUpgradeStatus(n *db.Node, serverVersion string, now time.Time) upgradeView {
	if !n.LastUpgradeAt.Valid {
		return upgradeView{Status: "none"}
	}
	// A node at or above the panel's current version has nothing to report;
	// any stored failure or attempt is stale (e.g. it was upgraded out of band).
	if n.AgentVersion != "" && semverGE(n.AgentVersion, serverVersion) {
		return upgradeView{Status: "none"}
	}
	v := upgradeView{At: n.LastUpgradeAt.Int64, Version: n.LastUpgradeVersion, Error: n.LastUpgradeError}
	switch {
	case n.LastUpgradeStatus == "error":
		v.Status = "error"
	case n.LastUpgradeVersion != "" && semverGE(n.AgentVersion, n.LastUpgradeVersion):
		v.Status = "ok"
	case now.Unix()-n.LastUpgradeAt.Int64 <= int64(upgradeGrace/time.Second):
		v.Status = "pending"
	default:
		v.Status = "stuck"
	}
	return v
}

// semverGE returns true when a >= b using numeric comparison on the three
// version segments. Non-semver strings compare lexicographically as fallback.
func semverGE(a, b string) bool {
	pa := parseSemver(a)
	pb := parseSemver(b)
	if pa == nil || pb == nil {
		return a >= b
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return true
}

func parseSemver(s string) []int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}
