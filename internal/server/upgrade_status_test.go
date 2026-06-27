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
		{"never", mk(0, "", "", "", "v1.0.0"), "v1.0.0", base, "none"},
		{"ok exact", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.2"), "v0.32.4", base, "ok"},
		{"at server version hides push", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.4"), "v0.32.4", base, "none"},
		{"error", mk(base.Unix(), "v0.32.2", "error", "节点未连接", "v0.32.1"), "v0.32.4", base, "error"},
		{"pending within grace", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.1"), "v0.32.4", base.Add(30 * time.Second), "pending"},
		{"stuck past grace", mk(base.Unix(), "v0.32.2", "acked", "", "v0.32.1"), "v0.32.4", base.Add(5 * time.Minute), "stuck"},
		{"current hides stale error", mk(base.Unix(), "v0.32.2", "error", "节点未连接", "v0.32.4"), "v0.32.4", base, "none"},
		{"newer than server hides stale", mk(base.Unix(), "v0.32.2", "acked", "", "v0.33.0"), "v0.32.4", base, "none"},
	}
	for _, tc := range cases {
		got := deriveUpgradeStatus(tc.node, tc.server, tc.now)
		if got.Status != tc.want {
			t.Errorf("%s: status=%q want %q", tc.name, got.Status, tc.want)
		}
	}
}
