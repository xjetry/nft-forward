package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// serveFakePanelOnSocket 在 path 的 unix socket 上起一个假的面板本机入口：握手时回
// hello_ack，其余帧只记录。返回的 fakeHub 用来查看收到的帧、向 daemon 发帧。
func serveFakePanelOnSocket(t *testing.T, path string) *fakeHub {
	t.Helper()
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 1, Name: "self"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fh.handler(t)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return fh
}

// waitFrame 等到假面板收到一帧 typ 类型的帧并返回它。
func waitFrame(t *testing.T, fh *fakeHub, typ string) wsproto.Envelope {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, f := range fh.Frames() {
			if f.Type == typ {
				return f
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("假面板没有收到 %s 帧，收到的帧：%+v", typ, fh.Frames())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runDaemon 在后台跑 d.Run，测试结束时停掉并等它退出。
func runDaemon(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon 没有在取消后退出")
		}
	})
}

// 面板机本机形态的 daemon（LocalPanel）经面板的本机入口 socket 拨号，走的是与远程
// 节点相同的 agent 协议：握手不带 token，之后面板经这条连接下发的规则集落到面板名下。
func TestServerLocalDaemonDialsThePanelSocket(t *testing.T) {
	dir := shortSockDir(t)
	panelSock := filepath.Join(dir, "panel.sock")
	fh := serveFakePanelOnSocket(t, panelSock)

	d, err := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
		SocketPath: filepath.Join(dir, "d.sock"),
		Dataplane:  &fakeDataplane{},
		Mode:       LocalPanel(panelSock),
	})
	if err != nil {
		t.Fatal(err)
	}
	runDaemon(t, d)

	var hello wsproto.Hello
	if err := json.Unmarshal(waitFrame(t, fh, wsproto.TypeHello).Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.NodeToken != "" {
		t.Fatalf("本机入口按入口认定节点，握手不应带 token，实际 %q", hello.NodeToken)
	}

	// 面板经这条连接下发规则集。
	rules := []nft.Rule{{ID: "p1", RuleID: 7, Proto: "tcp", SrcPort: 12003, DestIP: "10.0.0.9", DestPort: 80}}
	payload, _ := json.Marshal(wsproto.ApplyRuleset{Rev: "rev-1", Rules: rules})
	frame, _ := json.Marshal(wsproto.Envelope{Type: wsproto.TypeApplyRuleset, ID: "a1", Payload: payload})
	fh.mu.Lock()
	conn := fh.conn
	fh.mu.Unlock()
	if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}

	var ack wsproto.ApplyAck
	if err := json.Unmarshal(waitFrame(t, fh, wsproto.TypeApplyAck).Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Rev != "rev-1" {
		t.Fatalf("daemon 应确认应用了下发的规则集，实际 %+v", ack)
	}
	owners, meta := d.SnapshotForDialer()
	if len(owners["panel"]) != 1 || owners["panel"][0].SrcPort != 12003 {
		t.Fatalf("下发的规则应落到面板名下，实际 %+v", owners["panel"])
	}
	if meta.LastAppliedRev != "rev-1" {
		t.Fatalf("应记下已应用的 rev，实际 %q", meta.LastAppliedRev)
	}
}

// 反方向：单机形态的 daemon 不拨任何面板——拨号器只为托管形态创建，单机形态下
// 根本没有拨号器。
func TestStandaloneDaemonHasNoDialer(t *testing.T) {
	daemonSock := filepath.Join(shortSockDir(t), "d.sock")

	d, err := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
		SocketPath: daemonSock,
		Dataplane:  &fakeDataplane{},
		Mode:       Standalone(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runDaemon(t, d)

	// 等 daemon 自己的 socket 起来：拨号器若存在，是在这之后的同一段启动代码里建的。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, err := net.Dial("unix", daemonSock); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon 的 socket 没有起来")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if d.dialer.Load() != nil {
		t.Fatal("单机 daemon 不应有拨号器")
	}
}

// daemon 接入面板时把本地规则交给面板接管（migrate_rules）。面板在应答它之前可能先
// 发来一次规则下发——握手后面板总会核对一次规则集，两件事在面板那边并行。下发必须
// 照常被应用并应答，迁移的应答到达后本地名下才清掉：两帧谁先到都不能丢。
func TestLocalRulesAreAdoptedEvenWhenAPushArrivesFirst(t *testing.T) {
	dir := shortSockDir(t)
	panelSock := filepath.Join(dir, "panel.sock")
	statePath := filepath.Join(t.TempDir(), "state.json")
	local := nft.Rule{ID: "f1a92408", Proto: "tcp", SrcPort: 12021, DestIP: "198.51.100.9", DestPort: 80}
	if err := SaveState(statePath, OwnerRuleset{"tui": []nft.Rule{local}}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}

	// 假面板：hello 之后紧跟着发一次下发（此时面板还不知道那条本地规则）；收到
	// migrate_rules 后应答成功。
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 1, Name: "self"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	migrated := make(chan struct{})
	fh.onAck(wsproto.TypeMigrateRules, func(env wsproto.Envelope) wsproto.Envelope {
		// 先把一次下发塞到应答前面，再应答。
		push, _ := json.Marshal(wsproto.ApplyRuleset{Rev: "rev-before-adopt", Rules: nil})
		frame, _ := json.Marshal(wsproto.Envelope{Type: wsproto.TypeApplyRuleset, ID: "p0", Payload: push})
		fh.mu.Lock()
		conn := fh.conn
		fh.mu.Unlock()
		_ = conn.Write(context.Background(), websocket.MessageText, frame)
		close(migrated)
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	l, err := net.Listen("unix", panelSock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fh.handler(t)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d, err := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(dir, "d.sock"),
		Dataplane:  &fakeDataplane{},
		Mode:       LocalPanel(panelSock),
	})
	if err != nil {
		t.Fatal(err)
	}
	runDaemon(t, d)

	select {
	case <-migrated:
	case <-time.After(3 * time.Second):
		t.Fatalf("daemon 没有发 migrate_rules，收到的帧：%+v", fh.Frames())
	}
	var ack wsproto.ApplyAck
	if err := json.Unmarshal(waitFrame(t, fh, wsproto.TypeApplyAck).Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Rev != "rev-before-adopt" {
		t.Fatalf("先到的那次下发应被应用并应答，实际 %+v", ack)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		owners, _ := d.SnapshotForDialer()
		if len(owners["tui"]) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("面板应答接管成功后本地名下应清空，实际仍有 %+v", owners["tui"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 接管的另一种到达顺序：面板为本地规则建好面板规则后，带着它的下发先于接管的应答
// 到达。这时本地名下还留着同一端口的那条规则；下发必须被接受（本地那条让位），而且
// 从头到尾数据面里都有这个端口的规则——接管不能造成转发中断。
func TestAdoptionConvergesWhenTheAdoptedRuleIsPushedBeforeTheAck(t *testing.T) {
	dir := shortSockDir(t)
	panelSock := filepath.Join(dir, "panel.sock")
	statePath := filepath.Join(t.TempDir(), "state.json")
	const port = 12021
	local := nft.Rule{ID: "f1a92408", Proto: "tcp", SrcPort: port, DestIP: "198.51.100.9", DestPort: 80}
	other := nft.Rule{ID: "0b0b0b0b", Proto: "tcp", SrcPort: port + 1, DestIP: "198.51.100.9", DestPort: 81}
	if err := SaveState(statePath, OwnerRuleset{"tui": []nft.Rule{local, other}}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}

	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 1, Name: "self"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	// 假面板只接管了第一条（第二条没有出现在下发里），并把下发放在应答前面。
	fh.onAck(wsproto.TypeMigrateRules, func(env wsproto.Envelope) wsproto.Envelope {
		adopted := nft.Rule{RuleID: 7, Proto: "tcp", SrcPort: port, DestIP: "198.51.100.9", DestPort: 80}
		push, _ := json.Marshal(wsproto.ApplyRuleset{Rev: "rev-adopted", Rules: []nft.Rule{adopted}})
		frame, _ := json.Marshal(wsproto.Envelope{Type: wsproto.TypeApplyRuleset, ID: "p1", Payload: push})
		fh.mu.Lock()
		conn := fh.conn
		fh.mu.Unlock()
		_ = conn.Write(context.Background(), websocket.MessageText, frame)
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	l, err := net.Listen("unix", panelSock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fh.handler(t)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	dp := &fakeDataplane{}
	d, err := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(dir, "d.sock"),
		Dataplane:  dp,
		Mode:       LocalPanel(panelSock),
	})
	if err != nil {
		t.Fatal(err)
	}
	runDaemon(t, d)

	var ack wsproto.ApplyAck
	if err := json.Unmarshal(waitFrame(t, fh, wsproto.TypeApplyAck).Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("带着已接管规则的下发应被接受，实际被拒: %s", ack.Error)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		owners, _ := d.SnapshotForDialer()
		if len(owners["tui"]) == 0 && len(owners["panel"]) == 1 {
			if owners["panel"][0].RuleID != 7 || owners["panel"][0].SrcPort != port {
				t.Fatalf("面板名下应是被接管后的那条规则，实际 %+v", owners["panel"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("应收敛到“面板名下 1 条、本地名下为空”，实际 panel=%+v tui=%+v", owners["panel"], owners["tui"])
		}
		time.Sleep(10 * time.Millisecond)
	}

	dp.mu.Lock()
	calls := append([][]nft.Rule(nil), dp.nftCalls...)
	dp.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("数据面一次都没有被写入")
	}
	for i, rules := range calls {
		n := 0
		for _, r := range rules {
			if r.SrcPort == port {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("第 %d 次写数据面时端口 %d 上应恰有一条规则（不中断、不重复），实际 %d 条：%+v", i, port, n, rules)
		}
	}
}

// 规则集的写入是“取当前内容 → 改 → 解析 → 下数据面 → 提交”，中间的解析可以很慢。
// 两次写入若交叠，后提交的不能把先提交的改动冲掉：这里让面板下发卡在解析上，其间
// 删掉一条本地规则，两个改动最后都必须在。
func TestConcurrentOwnerWritesDoNotLoseEachOthersChanges(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{"tui": {{ID: "t1", Proto: "tcp", SrcPort: 12031, DestIP: "198.51.100.9", DestPort: 80}}}

	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	var mu sync.Mutex
	d.resolveFn = func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
		mu.Lock()
		block := first
		first = false
		mu.Unlock()
		if block {
			close(entered)
			<-release
		}
		return rules, true, nil
	}

	pushDone := make(chan error, 1)
	go func() {
		_, err := d.SetPanelRuleset(context.Background(), "rev-1", []nft.Rule{
			{RuleID: 7, Proto: "tcp", SrcPort: 12032, DestIP: "203.0.113.5", DestPort: 443},
		})
		pushDone <- err
	}()
	<-entered // 面板下发已取走当前内容，正卡在解析上

	deleteDone := make(chan struct{})
	go func() {
		d.clearLocalRules()
		close(deleteDone)
	}()
	// 给第二个写入一点时间：没有串行化时它会在这段时间里跑完并提交。
	select {
	case <-deleteDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-pushDone; err != nil {
		t.Fatalf("面板下发: %v", err)
	}
	<-deleteDone

	owners, _ := d.SnapshotForDialer()
	if len(owners["panel"]) != 1 || owners["panel"][0].RuleID != 7 {
		t.Fatalf("面板下发的规则应在，实际 %+v", owners["panel"])
	}
	if len(owners["tui"]) != 0 {
		t.Fatalf("其间删掉的本地规则不应被后提交的写入带回来，实际 %+v", owners["tui"])
	}
}

// 面板下发占用某个端口时，本地名下占同一端口的规则让位；其余本地规则不受影响。
func TestPanelPushSupersedesOnlyTheLocalRulesOnItsPorts(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{"tui": {
		{ID: "t1", Proto: "tcp", SrcPort: 12021, DestIP: "198.51.100.9", DestPort: 80},
		{ID: "t2", Proto: "udp", SrcPort: 12021, DestIP: "198.51.100.9", DestPort: 80},
		{ID: "t3", Proto: "tcp", SrcPort: 12022, DestIP: "198.51.100.9", DestPort: 81},
	}}
	if _, err := d.SetPanelRuleset(context.Background(), "rev-1", []nft.Rule{
		{RuleID: 7, Proto: "tcp", SrcPort: 12021, DestIP: "203.0.113.5", DestPort: 443},
	}); err != nil {
		t.Fatalf("面板下发应被接受: %v", err)
	}
	owners, _ := d.SnapshotForDialer()
	if len(owners["panel"]) != 1 || owners["panel"][0].RuleID != 7 {
		t.Fatalf("面板名下应是下发的那一条，实际 %+v", owners["panel"])
	}
	var left []string
	for _, r := range owners["tui"] {
		left = append(left, r.ID)
	}
	if len(left) != 2 || left[0] != "t2" || left[1] != "t3" {
		t.Fatalf("只有同协议同端口的 t1 该让位，本地名下应剩 t2、t3，实际 %v", left)
	}
}

// DELETE /v1/rules/panel 清空面板名下的规则——卸载面板角色时由安装脚本调用，把面板下发过
// 的规则撤出数据面。本地规则不受影响。
func TestClearPanelRulesLeavesLocalRulesAlone(t *testing.T) {
	dp := &fakeDataplane{}
	d, srv := newTestServer(t, dp)
	defer srv.Close()
	ctx := context.Background()
	if _, err := d.SetPanelRuleset(ctx, "rev-1", []nft.Rule{{ID: "p1", RuleID: 7, Proto: "tcp", SrcPort: 12003, DestIP: "10.0.0.9", DestPort: 80}}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.owners["tui"] = []nft.Rule{{ID: "t1", Proto: "tcp", SrcPort: 12004, DestIP: "10.0.0.8", DestPort: 80}}
	d.mu.Unlock()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/rules/panel", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清空面板名下的规则应成功，实际 HTTP %d", resp.StatusCode)
	}

	owners, meta := d.SnapshotForDialer()
	if len(owners["panel"]) != 0 {
		t.Fatalf("面板名下应为空，实际 %+v", owners["panel"])
	}
	if len(owners["tui"]) != 1 || owners["tui"][0].ID != "t1" {
		t.Fatalf("本地规则不应受影响，实际 %+v", owners["tui"])
	}
	if meta.LastAppliedRev != "" {
		t.Fatalf("清空后已应用的 rev 应作废（面板下次连上要重新下发），实际 %q", meta.LastAppliedRev)
	}
	dp.mu.Lock()
	last := dp.nftCalls[len(dp.nftCalls)-1]
	dp.mu.Unlock()
	if len(last) != 1 || last[0].SrcPort != 12004 {
		t.Fatalf("数据面应只剩本地规则，实际 %+v", last)
	}
}

// 面板名下没有别的本地写入口：整体替换只能由面板经 agent 连接下发。
func TestNoLocalEndpointReplacesThePanelRules(t *testing.T) {
	d, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	body := `{"rules":[{"proto":"tcp","src_port":12005,"dest_ip":"10.0.0.7","dest_port":80,"rule_id":9}]}`
	for _, path := range []string{"/v1/apply", "/v1/rules/panel"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			t.Fatalf("POST %s 不应被接受（HTTP %d）", path, resp.StatusCode)
		}
	}
	if owners, _ := d.SnapshotForDialer(); len(owners["panel"]) != 0 {
		t.Fatalf("面板名下不应被本地请求写入，实际 %+v", owners["panel"])
	}
}
