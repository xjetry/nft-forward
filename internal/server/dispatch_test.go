package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// 节点应用成功但带回告警（例如某条规则的目标解析不了）时，下发算成功：告警记在
// last_warning 上，last_error 保持为空。
func TestDispatchToNode_StoresWarningNotError(t *testing.T) {
	e := newLocalAgentEnv(t)
	const warning = "1 条规则的目标无法解析：端口 8080 → bad.invalid"
	connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64"}, func(a *fakeAgent) {
		a.applyWarning = warning
	})

	if err := e.s.dispatchToNode(e.self.ID); err != nil {
		t.Fatalf("dispatch should succeed with warning: %v", err)
	}
	got := e.selfNode(t)
	if got.LastWarning != warning {
		t.Fatalf("expected last_warning %q, got %q", warning, got.LastWarning)
	}
	if got.LastError.Valid {
		t.Fatalf("last_error should stay clear, got %q", got.LastError.String)
	}
}

// 反方向：节点没有连接时下发失败，错误记在 last_error 上。self 节点也不例外——
// 它没有另一条绕开连接的下发路径。
func TestDispatchToNode_WithoutConnectionRecordsError(t *testing.T) {
	e := newLocalAgentEnv(t)
	remote, err := db.CreateNode(e.d, "edge", "", "tok-edge")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*db.Node{e.self, remote} {
		createStandaloneRuleHop(t, e.d, n.ID, "tcp", 0, "10.0.0.30", 9200, sql.NullInt64{})
		if err := e.s.dispatchToNode(n.ID); err == nil {
			t.Fatalf("节点 %s 没有连接，下发应失败", n.Name)
		}
		got, _ := db.GetNode(e.d, n.ID)
		if !got.LastError.Valid || got.LastError.String == "" {
			t.Fatalf("节点 %s 的下发失败应记在 last_error 上", n.Name)
		}
	}
}
