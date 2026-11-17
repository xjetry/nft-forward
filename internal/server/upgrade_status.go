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

// upgradeView is the outcome of the most recent upgrade push to a node, as
// shown on the node detail page.
//
//   - none: no push is on record.
//   - pending: the push was acked, the agent does not yet report the pushed
//     version, and the grace window is still open.
//   - ok: the push was acked and the agent reports the pushed version or a
//     newer one.
//   - stuck: the push was acked, the grace window has passed, and the agent
//     still reports an older version.
//   - error: the push itself failed.
//   - superseded: the push on record did not bring the agent to its version,
//     but the agent no longer trails the panel (it was upgraded some other
//     way); the record has nothing left to tell.
//
// A push that worked is always reported as ok. none and superseded both mean
// "nothing to show", and neither is a statement that an upgrade succeeded.
type upgradeView struct {
	At      int64  `json:"at,omitempty"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// deriveUpgradeStatus turns a node's stored last-upgrade columns plus its live
// agent_version into the outcome described on upgradeView. It surfaces the
// silent failure: an acked upgrade whose target version never took, past the
// grace window.
func deriveUpgradeStatus(n *db.Node, serverVersion string, now time.Time) upgradeView {
	if !n.LastUpgradeAt.Valid {
		return upgradeView{Status: "none"}
	}
	v := upgradeView{At: n.LastUpgradeAt.Int64, Version: n.LastUpgradeVersion, Error: n.LastUpgradeError}
	acked := n.LastUpgradeStatus != "error"
	reached := n.LastUpgradeVersion != "" && n.AgentVersion != "" && semverGE(n.AgentVersion, n.LastUpgradeVersion)
	switch {
	case acked && reached:
		v.Status = "ok"
	case n.AgentVersion != "" && semverGE(n.AgentVersion, serverVersion):
		return upgradeView{Status: "superseded"}
	case !acked:
		v.Status = "error"
	case now.Unix()-n.LastUpgradeAt.Int64 <= int64(upgradeGrace/time.Second):
		v.Status = "pending"
	default:
		v.Status = "stuck"
	}
	return v
}

// agentOutdated reports whether the node's agent has reported a version below
// the panel's, i.e. whether pushing the panel's agent to it would upgrade it.
// It is the one definition of "can be upgraded": the node list and detail
// responses carry its result for the UI's badges and upgrade button, and the
// bulk upgrade pushes to exactly the nodes it selects.
//
// A node that has never reported a version — no agent installed yet, a
// composite, the panel's own node — is not outdated: nothing is known about it
// that an upgrade would change.
func agentOutdated(n *db.Node, serverVersion string) bool {
	return n.AgentVersion != "" && !semverGE(n.AgentVersion, serverVersion)
}

// semverGE returns true when a >= b. Both are read as vMAJOR.MINOR.PATCH with
// an optional -prerelease and +build: the three numbers compare numerically, a
// prerelease sorts below the release it precedes (so a Go pseudo-version such
// as v0.29.10-0.20260626041818-5dd657947da4, which an agent built off a tag
// reports, is below v0.29.10), two prereleases of the same release compare as
// strings, and the build part is ignored. If either side is not of that form
// ("dev"), the two compare as plain strings.
func semverGE(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return a >= b
	}
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			return pa.nums[i] > pb.nums[i]
		}
	}
	switch {
	case pa.pre == "":
		return true
	case pb.pre == "":
		return false
	default:
		return pa.pre >= pb.pre
	}
}

type semver struct {
	nums [3]int
	pre  string
}

func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, v.pre = s[:i], s[i+1:]
		if v.pre == "" {
			return semver{}, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v.nums[i] = n
	}
	return v, true
}
