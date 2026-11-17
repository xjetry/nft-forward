package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// pushLog 记录一个模拟节点收到的每一次规则下发。节点是先记录、再回 apply_ack，
// 而面板的下发要等到 ack 才返回，所以一次同步下发返回时它必然已经记在这里。
type pushLog struct {
	mu     sync.Mutex
	pushes [][]nft.Rule
}

func (p *pushLog) record(rules []nft.Rule) {
	p.mu.Lock()
	p.pushes = append(p.pushes, rules)
	p.mu.Unlock()
}

// count 是到目前为止收到的下发次数。
func (p *pushLog) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pushes)
}

// forwards 报告最近一次下发里是否有监听 port 的规则；ok=false 表示还没有收到过下发。
func (p *pushLog) forwards(port int) (has, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pushes) == 0 {
		return false, false
	}
	for _, r := range p.pushes[len(p.pushes)-1] {
		if r.SrcPort == port {
			return true, true
		}
	}
	return false, true
}

// waitForwarding 等到最近一次下发里“是否转发 port”等于 want。下发可能由计数入账或
// 节点上线触发，是异步的，所以轮询；超时即失败。
func (p *pushLog) waitForwarding(t *testing.T, port int, want bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		has, ok := p.forwards(port)
		if ok && has == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：期望端口 %d 转发=%v，最近一次下发里转发=%v（收到过下发=%v）", what, port, want, has, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeAgent 是一条走完真实握手的节点连接。握手成功后它在后台应答面板发来的帧：
// 规则下发记进 pushes 并回 apply_ack（带 applyWarning），探测回 probe（默认不通）。
type fakeAgent struct {
	conn         *websocket.Conn
	ack          wsproto.HelloAck
	pushes       *pushLog
	applyWarning string
	probe        wsproto.ProbeAck
	// cmdAcks 收后台读循环读到的 rule_cmd_ack，由 ruleCmd 按帧 id 认领。
	cmdAcks chan wsproto.Envelope
}

// ruleCmd 以节点的身份向面板发一条规则命令（rule_create / migrate_rules 等）并等它的
// 应答。期间面板发来的下发照常由后台读循环应答。
func (a *fakeAgent) ruleCmd(t *testing.T, typ string, payload any) wsproto.RuleCmdAck {
	t.Helper()
	id := "cmd-" + typ
	p, _ := json.Marshal(payload)
	sendJSON(t, a.conn, wsproto.Envelope{Type: typ, ID: id, Payload: p})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env := <-a.cmdAcks:
			if env.ID != id {
				continue
			}
			var ack wsproto.RuleCmdAck
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				t.Fatalf("rule_cmd_ack 解码: %v", err)
			}
			return ack
		case <-deadline:
			t.Fatalf("面板没有应答 %s", typ)
		}
	}
}

// wsURL 把 httptest 服务的地址换成 agent 端点的 ws 地址。
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/agents"
}

// helloOver 在 url 上发一帧 hello 并读回 hello_ack，不做别的；连接随测试结束关闭。
// 握手被拒时 ack.Error 非空，由调用方断言。
func helloOver(t *testing.T, url string, hello wsproto.Hello) (*websocket.Conn, wsproto.HelloAck) {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	hp, _ := json.Marshal(hello)
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	var ack wsproto.HelloAck
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeHelloAck {
		t.Fatalf("握手的第一帧应答应为 hello_ack，实际 %s", env.Type)
	}
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatalf("hello_ack 解码: %v", err)
	}
	return c, ack
}

// connectFakeAgent 握手成功后启动后台应答，并等面板把这次握手带来的状态落完库才
// 返回。configure 在后台应答开始前调用，用来设置 applyWarning / probe，避免与读循环
// 竞争。
func connectFakeAgent(t *testing.T, url string, hello wsproto.Hello, configure func(*fakeAgent)) *fakeAgent {
	t.Helper()
	c, ack := helloOver(t, url, hello)
	if ack.Error != "" {
		t.Fatalf("hello 被拒绝: %s", ack.Error)
	}
	a := &fakeAgent{conn: c, ack: ack, pushes: &pushLog{}, cmdAcks: make(chan wsproto.Envelope, 8)}
	if configure != nil {
		configure(a)
	}
	reply := func(typ, id string, v any) {
		p, _ := json.Marshal(v)
		b, _ := json.Marshal(wsproto.Envelope{Type: typ, ID: id, Payload: p})
		_ = c.Write(context.Background(), websocket.MessageText, b)
	}
	const settlePing = "handshake-settled"
	settled := make(chan struct{})
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
			switch env.Type {
			case wsproto.TypeApplyRuleset:
				var ar wsproto.ApplyRuleset
				_ = json.Unmarshal(env.Payload, &ar)
				a.pushes.record(ar.Rules)
				reply(wsproto.TypeApplyAck, env.ID, wsproto.ApplyAck{Rev: ar.Rev, OK: true, Warning: a.applyWarning})
			case wsproto.TypeProbe:
				reply(wsproto.TypeProbeAck, env.ID, a.probe)
			case wsproto.TypeRuleCmdAck:
				a.cmdAcks <- env
			case wsproto.TypePong:
				if env.ID == settlePing {
					close(settled)
				}
			}
		}
	}()

	// hello_ack 之后面板还要把这次握手带来的状态落库（在线标记、数据面地址、端口范围，
	// 见 Hub.serveAgent），然后才在同一个协程里进读循环。pong 由读循环应答
	// （Hub.readerLoop），所以收到它就说明那些写入都已完成：调用方拿到返回值后可以
	// 直接读库断言，不必各自等待。
	reply(wsproto.TypePing, settlePing, wsproto.Ping{})
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("握手后面板没有应答 ping：读循环没有起来")
	}
	return a
}

// reportTraffic 让节点上报监听端口 port 的一批计数。
func (a *fakeAgent) reportTraffic(t *testing.T, port int, up, down int64) {
	t.Helper()
	p, _ := json.Marshal(wsproto.Counters{Samples: []wsproto.CounterSample{{ListenPort: port, Proto: "tcp", BytesUp: up, BytesDown: down}}})
	sendJSON(t, a.conn, wsproto.Envelope{Type: wsproto.TypeCounters, ID: "c", Payload: p})
}
