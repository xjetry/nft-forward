package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// nodeRuleUpdate 以节点身份发一帧 rule_update 并等到与之配对的 rule_cmd_ack。
// payload 用原始 JSON：断言针对的是“线上实际到达的字段”，不随 wsproto 结构体的
// 字段增删而变。
func nodeRuleUpdate(t *testing.T, c *websocket.Conn, id, payload string) wsproto.RuleCmdAck {
	t.Helper()
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRuleUpdate, ID: id, Payload: json.RawMessage(payload)})
	for {
		env := recvEnvelope(t, c)
		if env.Type != wsproto.TypeRuleCmdAck || env.ID != id {
			continue
		}
		var ack wsproto.RuleCmdAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}
}

// 规则名只在面板上编辑：节点 TUI 的规则表单没有“名称”这一栏，节点发来的
// rule_update 因此不携带、也无权决定规则名。面板处理这一帧时必须保留原名，
// 无论帧里没有 name（当前节点的实际形态），还是带着一个 name（旧版节点会带空串）。
func TestNodeRuleUpdateLeavesRuleNameAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"帧里没有 name", `{"rule_id":%d,"proto":"tcp","exit_host":"10.0.0.20","exit_port":9001,"mode":"kernel","comment":"edited"}`},
		{"帧里 name 为空串", `{"rule_id":%d,"proto":"tcp","exit_host":"10.0.0.20","exit_port":9001,"mode":"kernel","comment":"edited","name":""}`},
		{"帧里 name 为别的值", `{"rule_id":%d,"proto":"tcp","exit_host":"10.0.0.20","exit_port":9001,"mode":"kernel","comment":"edited","name":"renamed-by-node"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hub, n := newHubTestServer(t)
			hash, _ := HashPassword("pw")
			uid, err := db.CreateUser(hub.DB, "owner1", hash, "user")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hub.DB.Exec(`UPDATE nodes SET owner_id=? WHERE id=?`, uid, n.ID); err != nil {
				t.Fatal(err)
			}
			// createStandaloneRuleHop 建出的规则名为 "test"。
			ruleID, _ := createStandaloneRuleHop(t, hub.DB, n.ID, "tcp", 0, "10.0.0.10", 9000, sql.NullInt64{Int64: uid, Valid: true})

			c := dialWS(t, srv)
			hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
			sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
			_ = recvEnvelope(t, c)

			ack := nodeRuleUpdate(t, c, "upd-1", fmt.Sprintf(tc.payload, ruleID))
			if !ack.OK {
				t.Fatalf("rule_update 被拒绝: %+v", ack)
			}

			rl, err := db.GetRule(hub.DB, ruleID)
			if err != nil {
				t.Fatal(err)
			}
			// 先确认这一帧确实生效了，否则“名字没变”可能只是什么都没改。
			if rl.ExitHost != "10.0.0.20" || rl.ExitPort != 9001 || rl.Comment != "edited" {
				t.Fatalf("更新未生效: exit=%s:%d comment=%q", rl.ExitHost, rl.ExitPort, rl.Comment)
			}
			if rl.Name != "test" {
				t.Fatalf("规则名被节点侧更新改写: got %q, want %q", rl.Name, "test")
			}
		})
	}
}
