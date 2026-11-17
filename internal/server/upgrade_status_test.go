package server

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
)

func TestDeriveUpgradeStatus(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	mk := func(at int64, ver, status, errText, agent string) *db.Node {
		n := &db.Node{AgentVersion: agent, LastUpgradeVersion: ver, LastUpgradeStatus: status, LastUpgradeError: errText}
		if at > 0 {
			n.LastUpgradeAt = sql.NullInt64{Int64: at, Valid: true}
		}
		return n
	}
	cases := []struct {
		name   string
		node   *db.Node
		server string
		now    time.Time
		want   string
	}{
		{"never pushed", mk(0, "", "", "", "v1.0.0"), "v1.0.0", base, "none"},

		// A push that worked is ok — also when the pushed version is the
		// panel's own, which is the case right after a successful upgrade.
		{"acked, agent at the pushed version which is the panel's", mk(base.Unix(), "v0.32.4", "acked", "", "v0.32.4"), "v0.32.4", base, "ok"},
		{"acked, agent at the pushed version, panel has moved on", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.2"), "v0.32.4", base, "ok"},
		{"acked, agent beyond the pushed version", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.4"), "v0.32.4", base, "ok"},
		{"acked, agent newer than the panel", mk(base.Unix(), "v0.32.2", "acked", "", "v0.33.0"), "v0.32.4", base, "ok"},

		{"acked, not there yet, within grace", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.1"), "v0.32.4", base.Add(30 * time.Second), "pending"},
		{"acked, agent version unknown, within grace", mk(base.Unix(), "v0.32.2", "acked", "", ""), "v0.32.4", base.Add(30 * time.Second), "pending"},
		{"acked, not there, past grace", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.1"), "v0.32.4", base.Add(5 * time.Minute), "stuck"},
		// A pseudo-version of a release is a build from before that release.
		{"acked, agent on a prerelease of the pushed version, past grace", mk(base.Unix(), "v0.32.4", "acked", "", "v0.32.4-0.20260626041818-5dd657947da4"), "v0.32.4", base.Add(5 * time.Minute), "stuck"},

		{"failed, agent below the pushed version", mk(base.Unix(), "v0.32.2", "error", "节点未连接", "v0.32.1"), "v0.32.4", base, "error"},
		// A failed push is never reported as a success, even if the agent got
		// to that version some other way.
		{"failed, agent at the pushed version but below the panel", mk(base.Unix(), "v0.32.2", "error", "节点未连接", "v0.32.2"), "v0.32.4", base, "error"},
		{"failed, agent no longer trails the panel", mk(base.Unix(), "v0.32.2", "error", "节点未连接", "v0.32.4"), "v0.32.4", base, "superseded"},
		{"acked for a version above the panel's, agent at the panel's", mk(base.Unix(), "v0.33.0", "acked", "", "v0.32.4"), "v0.32.4", base.Add(5 * time.Minute), "superseded"},
	}
	for _, tc := range cases {
		got := deriveUpgradeStatus(tc.node, tc.server, tc.now)
		if got.Status != tc.want {
			t.Errorf("%s: status=%q want %q", tc.name, got.Status, tc.want)
		}
		// "Nothing to show" carries nothing; every other status carries the
		// push it is about.
		blank := got.At == 0 && got.Version == "" && got.Error == ""
		if nothing := tc.want == "none" || tc.want == "superseded"; nothing != blank {
			t.Errorf("%s: status %q with at=%d version=%q error=%q", tc.name, got.Status, got.At, got.Version, got.Error)
		}
	}
}

func TestSemverGE(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.32.4", "v0.32.4", true},
		{"v0.32.5", "v0.32.4", true},
		{"v0.32.3", "v0.32.4", false},
		// Numeric, not lexical: 10 is above 9.
		{"v0.10.0", "v0.9.0", true},
		{"v0.9.0", "v0.10.0", false},
		{"0.32.4", "v0.32.4", true},
		// A prerelease is below its release and above the release before it.
		{"v0.32.4-0.20260626041818-5dd657947da4", "v0.32.4", false},
		{"v0.32.4", "v0.32.4-0.20260626041818-5dd657947da4", true},
		{"v0.32.4-0.20260626041818-5dd657947da4", "v0.32.3", true},
		{"v0.32.4-0.20260701000000-aaaaaaaaaaaa", "v0.32.4-0.20260626041818-5dd657947da4", true},
		{"v0.32.4-0.20260626041818-5dd657947da4", "v0.32.4-0.20260701000000-aaaaaaaaaaaa", false},
		// Build metadata does not order versions.
		{"v0.32.4+dirty", "v0.32.4", true},
		{"v0.32.4", "v0.32.4+dirty", true},
		// Not a version on either side: plain string comparison.
		{"dev", "dev", true},
		{"dev", "v0.32.4", false},
		{"v0.32.4", "dev", true},
	} {
		if got := semverGE(tc.a, tc.b); got != tc.want {
			t.Errorf("semverGE(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestAgentOutdated(t *testing.T) {
	for _, tc := range []struct {
		agent, panel string
		want         bool
	}{
		{"v0.32.3", "v0.32.4", true},
		{"v0.32.4", "v0.32.4", false},
		{"v0.33.0", "v0.32.4", false},
		{"v0.32.4-0.20260626041818-5dd657947da4", "v0.32.4", true},
		// Never reported a version: nothing known to upgrade.
		{"", "v0.32.4", false},
		// A dev agent trails any release; against a dev panel nothing does.
		{"dev", "v0.32.4", true},
		{"v0.32.4", "dev", false},
		{"dev", "dev", false},
	} {
		if got := agentOutdated(&db.Node{AgentVersion: tc.agent}, tc.panel); got != tc.want {
			t.Errorf("agent %q against panel %q: outdated=%v, want %v", tc.agent, tc.panel, got, tc.want)
		}
	}
}
