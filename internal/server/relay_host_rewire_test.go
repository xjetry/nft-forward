package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/wsproto"
)

// 反方向：地址没变的 hello 不触发重接。节点每次重连都会 hello，重接要重新生成并
// 下发所有引用它的规则，不能在地址未变时白做一遍。
func TestHelloWithUnchangedRelayHostDoesNotRewire(t *testing.T) {
	srv, hub, _ := newHubTestServer(t)
	changed := make(chan int64, 4)
	hub.OnRelayHostChange = func(nodeID int64) { changed <- nodeID }

	hello := func() {
		c := dialWS(t, srv)
		hp, _ := json.Marshal(wsproto.Hello{
			NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
			DeclaredRelayHost: "10.9.9.9",
		})
		sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
		_ = recvEnvelope(t, c)
		// 帧按序处理：pong 回来说明 hello 的后续处理已经走完。
		syncByPing(t, c)
		c.Close(websocket.StatusNormalClosure, "")
	}

	hello() // 第一次：relay_host 从空变成声明值
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("前置条件：第一次声明地址应触发一次重接")
	}

	hello() // 第二次：声明值与库里相同
	select {
	case id := <-changed:
		t.Fatalf("地址未变的 hello 触发了重接（node %d）", id)
	case <-time.After(300 * time.Millisecond):
	}
}

// 上游跳的目标地址是建规则时从下游节点的 relay_host 抄下来的快照。节点用
// --relay-host 声明的地址在每次 hello 时都会覆盖库里的 relay_host，所以声明换了
// 地址之后，其他节点上指向它的跳必须跟着重新生成，否则上游继续把流量送往旧地址。
func TestHelloDeclaredRelayHostRewiresUpstreamHops(t *testing.T) {
	e := newUpgradeTestEnv(t)
	// rule-edge-0（入口，relay 10.0.0.10）→ rule-edge-1（出口，relay 10.0.0.11，
	// token tok-rule-1）。
	rl, n0, _ := seedTwoHopRuleDB(t, e.d)
	if got := hopOnNode(t, e.d, rl.ID, n0).TargetHost; got != "10.0.0.11" {
		t.Fatalf("前置条件：入口跳应指向下游的旧地址，实际 %q", got)
	}

	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-rule-1", AgentVersion: "v0.0.1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "10.9.9.9",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	var ack wsproto.HelloAck
	if env := recvEnvelope(t, c); json.Unmarshal(env.Payload, &ack) != nil || ack.Error != "" {
		t.Fatalf("hello 被拒绝: %+v", ack)
	}
	// 之后面板会向这个连接下发规则集：在后台应答，免得下发卡在等待 ack 上。
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if json.Unmarshal(b, &env) != nil {
				return
			}
			if env.Type == wsproto.TypeApplyRuleset {
				var a wsproto.ApplyRuleset
				_ = json.Unmarshal(env.Payload, &a)
				p, _ := json.Marshal(wsproto.ApplyAck{Rev: a.Rev, OK: true})
				out, _ := json.Marshal(wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: p})
				_ = c.Write(context.Background(), websocket.MessageText, out)
			}
		}
	}()

	// 重接在 hello 之后异步进行：等到入口跳的目标换成新地址。
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := hopOnNode(t, e.d, rl.ID, n0).TargetHost
		if got == "10.9.9.9" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("节点声明新的数据面地址后，上游跳仍指向 %q，应为 10.9.9.9", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
