# TUI 非链式 panel 编辑同步回 server — 实现 Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **派发 subagent 时必须传达:** 代码注释、KDoc、commit message 中**绝对禁止**出现执行过程信息(Task/Phase 编号、方案代号、审阅轮次、"按上一轮指示"等)。注释只解释 WHY 与 invariant。subagent 产出若违反,follow-up 清理。

**Goal:** 让 TUI 把对 server 托管(panel 段)**非链式**转发的编辑同步回 server 并落库,使 server 成为编辑后的权威来源;链式行端口/目标保持只读。

**Architecture:** 复用既有的 tui 段上报链路范式(`setOwnerRuleset` → hook → dialer 通道 → WS 帧 → hub → DB),对称地新增一条 panel 段上报链路:新增 `panel_segment_edit` 帧、`panelHook`/`NotifyPanelEdited`、`hub.applyPanelEdits`、`db.UpdateForward`。TUI 把 tui 段与 panel 段合并成一个带来源标记列的统一列表,编辑按行的 owner 路由提交。server 端对链式行兜底拒绝(`UpdateForward` 的 `chain_id IS NULL` + `applyPanelEdits` 的 `ChainID.Valid` 双重保险)。

**Tech Stack:** Go,coder/websocket,SQLite(database/sql,启用外键),bubbletea/lipgloss TUI。

---

## File Structure

| 文件 | 职责 | 改动 |
|---|---|---|
| `internal/wsproto/messages.go` | WS 帧/payload 定义 | 加 `TypePanelSegmentEdit` 常量 + `PanelSegmentEdit` payload |
| `internal/db/queries.go` | forwards 表读写 | 加 `UpdateForward`(只改非链式可编辑字段) |
| `internal/daemon/dialer.go` | node→server 出站帧 | 加 `panelCh`/`pendingPanel`/`NotifyPanelEdited` + write-loop case + `OnPanelNotice` |
| `internal/daemon/handlers.go` | owner 写路径 | 加 `panelHook` 字段;`setOwnerRuleset` 末尾按 owner 触发对应 hook |
| `internal/daemon/daemon.go` | dialer 接线 | 接线 `OnPanelNotice` 占位 + `d.panelHook` |
| `internal/server/hub.go` | server 收帧 | readerLoop 加 `TypePanelSegmentEdit` 分支 + `applyPanelEdits` |
| `internal/tui/tui.go` | TUI 渲染/编辑 | 统一列表(来源标记列)+ 跨段导航 + 按 owner 编辑/提交 |

依赖顺序:Task 1(帧)→ Task 2(db)→ Task 3(dialer)→ Task 4(daemon)→ Task 5(hub)→ Task 6(TUI)。

---

## Task 1: wsproto 新增 panel_segment_edit 帧

**Files:**
- Modify: `internal/wsproto/messages.go:16-28`(常量块)、`:127-129`(`TuiSegmentChanged` 旁)
- Test: `internal/wsproto/messages_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/wsproto/messages_test.go` 末尾追加:

```go
func TestPanelSegmentEditRoundtrip(t *testing.T) {
	p := PanelSegmentEdit{Forwards: []Forward{
		{Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.9", TargetPort: 443, Comment: "edge", Mode: nft.ModeKernel},
	}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got PanelSegmentEdit
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].ListenPort != 30000 || got.Forwards[0].TargetIP != "10.0.0.9" {
		t.Fatalf("panel_segment_edit roundtrip mismatch: %+v", got)
	}
}

func TestPanelSegmentEditTypeConstant(t *testing.T) {
	if TypePanelSegmentEdit != "panel_segment_edit" {
		t.Fatalf("unexpected type constant %q", TypePanelSegmentEdit)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/wsproto/ -run 'TestPanelSegmentEdit' -v`
Expected: 编译失败 —— `undefined: TypePanelSegmentEdit` / `undefined: PanelSegmentEdit`

- [ ] **Step 3: 加常量**

在 `internal/wsproto/messages.go` 常量块(`TypeTuiSegmentChanged` 之后)加一行:

```go
	TypeTuiSegmentChanged   = "tui_segment_changed"
	TypePanelSegmentEdit    = "panel_segment_edit"
	TypePing                = "ping"
```

(其余常量保持对齐;`gofmt` 会重排对齐列。)

- [ ] **Step 4: 加 payload 类型**

在 `TuiSegmentChanged` 定义之后加:

```go
// PanelSegmentEdit carries a node's edits to its panel-segment forwards
// back to the server. It mirrors TuiSegmentChanged: a full snapshot of the
// segment, not a delta. The server locates each forward by
// (node_id, proto, listen_port), reads chain_id from the DB to decide
// whether the row is editable, and persists non-chain edits into the
// forwards table — so chain_id never needs to ride on the wire.
type PanelSegmentEdit struct {
	Forwards []Forward `json:"forwards"`
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/wsproto/ -run 'TestPanelSegmentEdit' -v`
Expected: PASS(两个测试)

- [ ] **Step 6: gofmt + 全包测试 + 提交**

```bash
gofmt -w internal/wsproto/messages.go
go test ./internal/wsproto/
git add internal/wsproto/messages.go internal/wsproto/messages_test.go
git commit -m "feat(wsproto): add panel_segment_edit frame for node-reported panel edits"
```

---

## Task 2: db.UpdateForward(只改非链式可编辑字段)

**Files:**
- Modify: `internal/db/queries.go`(`DeleteForward` 之后,约 :368)
- Test: `internal/db/queries_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/db/queries_test.go` 末尾追加:

```go
func TestUpdateForward_NonChainRowUpdatesEditableFields(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.1", TargetPort: 30000, Comment: "old", Mode: "kernel"})
	if err != nil {
		t.Fatal(err)
	}

	affected, err := UpdateForward(d, n.ID, "tcp", 30000, "10.9.9.9", 8443, "new", "userspace")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("expected 1 row affected, got %d", affected)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "10.9.9.9" || got.TargetPort != 8443 || got.Comment != "new" || got.Mode != "userspace" {
		t.Fatalf("editable fields not updated: %+v", got)
	}
}

func TestUpdateForward_ChainRowIsNotTouched(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	// Seed a chains row directly so the chain-tagged forward's chain_id FK resolves.
	res, err := d.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20001, TargetIP: "5.6.7.8", TargetPort: 20002, ChainID: sql.NullInt64{Int64: cid, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}

	affected, err := UpdateForward(d, n.ID, "tcp", 20001, "1.1.1.1", 1, "hijack", "userspace")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 0 {
		t.Fatalf("chain row must not be updated, got %d rows affected", affected)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "5.6.7.8" || got.TargetPort != 20002 {
		t.Fatalf("chain row port/target must stay intact: %+v", got)
	}
}

func TestUpdateForward_EmptyModeNormalizesToKernel(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	id, _ := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 30001, TargetIP: "10.0.0.1", TargetPort: 30001, Mode: "userspace"})

	if _, err := UpdateForward(d, n.ID, "tcp", 30001, "10.0.0.1", 30001, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := GetForward(d, id)
	if got.Mode != "kernel" {
		t.Fatalf("empty mode should normalize to kernel, got %q", got.Mode)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/db/ -run 'TestUpdateForward' -v`
Expected: 编译失败 —— `undefined: UpdateForward`

- [ ] **Step 3: 实现 UpdateForward**

在 `internal/db/queries.go` 的 `DeleteForward` 之后加:

```go
// UpdateForward updates only the editable fields of a non-chain forward,
// located by (node_id, proto, listen_port). chain_id IS NULL is a
// server-side backstop: a chained hop's listen_port/target are a relay
// skeleton owned by chain orchestration (RegenerateChain wires neighbor
// hops together), so even a reported edit that names a chain row leaves it
// untouched. Returns rows affected so the caller distinguishes a hit from a
// miss (chained row, or an unknown port).
func UpdateForward(d *sql.DB, nodeID int64, proto string, listenPort int, targetIP string, targetPort int, comment, mode string) (int64, error) {
	res, err := d.Exec(`UPDATE forwards SET target_ip=?, target_port=?, comment=?, mode=? WHERE node_id=? AND proto=? AND listen_port=? AND chain_id IS NULL`,
		targetIP, targetPort, comment, NormalizeForwardMode(mode), nodeID, proto, listenPort)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/db/ -run 'TestUpdateForward' -v`
Expected: PASS(三个测试)

- [ ] **Step 5: gofmt + 全包测试 + 提交**

```bash
gofmt -w internal/db/queries.go
go test ./internal/db/
git add internal/db/queries.go internal/db/queries_test.go
git commit -m "feat(db): add UpdateForward to persist non-chain forward edits"
```

---

## Task 3: dialer 发送 panel_segment_edit 帧

**Files:**
- Modify: `internal/daemon/dialer.go:35-47`(`DialerConfig`)、`:49-58`(`Dialer` struct)、`:60-67`(`NewDialer`)、`:81-103` 之后(加 `NotifyPanelEdited`)、`:273-279`(write-loop tui case 之后)
- Test: `internal/daemon/dialer_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/daemon/dialer_test.go` 末尾追加:

```go
func TestDialerSendsPanelSegmentEditOnNotify(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{}, AgentMeta{}
		},
		OnApply:       func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
		OnPanelNotice: func(_ []wsproto.Forward) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	dl.NotifyPanelEdited([]nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}})

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("never received panel_segment_edit frame; frames=%+v", fh.Frames())
		case <-tick.C:
			for _, f := range fh.Frames() {
				if f.Type != wsproto.TypePanelSegmentEdit {
					continue
				}
				var pse wsproto.PanelSegmentEdit
				if err := json.Unmarshal(f.Payload, &pse); err != nil {
					t.Fatalf("unmarshal panel_segment_edit: %v", err)
				}
				if len(pse.Forwards) == 1 && pse.Forwards[0].ListenPort == 30000 && pse.Forwards[0].TargetIP == "10.0.0.9" {
					return
				}
				t.Fatalf("unexpected panel_segment_edit payload: %+v", pse)
			}
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run 'TestDialerSendsPanelSegmentEditOnNotify' -v`
Expected: 编译失败 —— `unknown field OnPanelNotice` / `dl.NotifyPanelEdited undefined`

- [ ] **Step 3: `DialerConfig` 加 `OnPanelNotice`**

`internal/daemon/dialer.go` 的 `DialerConfig` 中,在 `OnTuiNotice` 之后加:

```go
	OnTuiNotice   func(forwards []wsproto.Forward) // optional; nil = skip notice
	OnPanelNotice func(forwards []wsproto.Forward) // optional; nil = skip notice
```

- [ ] **Step 4: `Dialer` struct 加 panel 通道**

在 `tuiCh`/`pendingTui` 之后加:

```go
	tuiCh      chan []nft.Rule
	pendingTui atomic.Pointer[[]nft.Rule]

	panelCh      chan []nft.Rule
	pendingPanel atomic.Pointer[[]nft.Rule]
```

- [ ] **Step 5: `NewDialer` 初始化 panelCh**

```go
	return &Dialer{
		cfg:     cfg,
		tuiCh:   make(chan []nft.Rule, 1),
		panelCh: make(chan []nft.Rule, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
```

- [ ] **Step 6: 加 NotifyPanelEdited**

在 `NotifyTuiChanged` 之后加(逐字镜像,tuiCh/pendingTui → panelCh/pendingPanel):

```go
// NotifyPanelEdited accepts a new panel-segment snapshot from the
// unix-socket handler after a TUI edit to a server-managed forward.
// Last-write-wins, mirroring NotifyTuiChanged: a queued snapshot is
// superseded by a newer one so only the latest state reaches the panel.
func (d *Dialer) NotifyPanelEdited(rules []nft.Rule) {
	cp := append([]nft.Rule(nil), rules...)
	select {
	case d.panelCh <- cp:
	default:
		d.pendingPanel.Store(&cp)
		if p := d.pendingPanel.Swap(nil); p != nil {
			select {
			case d.panelCh <- *p:
			default:
				// channel still full; a fresher snapshot will come through
			}
		}
	}
}
```

- [ ] **Step 7: write-loop 加 panelCh case**

在 `case rules := <-d.tuiCh:` 块之后加:

```go
		case rules := <-d.panelCh:
			if d.cfg.OnPanelNotice == nil {
				continue
			}
			fwds := rulesToForwards(rules)
			pp, _ := json.Marshal(wsproto.PanelSegmentEdit{Forwards: fwds})
			_ = writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypePanelSegmentEdit, Payload: pp})
```

- [ ] **Step 8: 跑测试确认通过**

Run: `go test ./internal/daemon/ -run 'TestDialerSendsPanelSegmentEditOnNotify' -v`
Expected: PASS

- [ ] **Step 9: gofmt + 全包测试 + 提交**

```bash
gofmt -w internal/daemon/dialer.go
go test ./internal/daemon/
git add internal/daemon/dialer.go internal/daemon/dialer_test.go
git commit -m "feat(daemon): emit panel_segment_edit frame on panel-segment notify"
```

---

## Task 4: daemon setOwnerRuleset 按 owner 触发 panelHook + 接线

**Files:**
- Modify: `internal/daemon/handlers.go:58-64`(struct 字段)、`:239-246`(setOwnerRuleset 末尾 hook 触发)
- Modify: `internal/daemon/daemon.go:170-196`(dialer 接线)
- Test: `internal/daemon/handlers_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/daemon/handlers_test.go` 的 `TestPostRulesetTUIInvokesDialerHook` 之后追加:

```go
func TestPostRulesetPanelInvokesPanelHook(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Dataplane:  &fakeDataplane{},
	})
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan []nft.Rule, 1)
	d.panelHook = func(r []nft.Rule) { called <- r }
	d.tuiHook = func(r []nft.Rule) { t.Fatalf("tuiHook fired on panel owner write") }

	if err := d.setOwnerRuleset(context.Background(), "panel",
		[]nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}}, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if len(got) != 1 || got[0].SrcPort != 30000 {
			t.Fatalf("unexpected panelHook rules: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("panelHook never called")
	}
}

func TestPostRulesetTUIDoesNotInvokePanelHook(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Dataplane:  &fakeDataplane{},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.panelHook = func(r []nft.Rule) { t.Fatalf("panelHook fired on tui owner write") }
	if err := d.setOwnerRuleset(context.Background(), "tui",
		[]nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, ""); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run 'TestPostRulesetPanelInvokesPanelHook|TestPostRulesetTUIDoesNotInvokePanelHook' -v`
Expected: 编译失败 —— `d.panelHook undefined`

- [ ] **Step 3: 加 panelHook 字段**

`internal/daemon/handlers.go` 中,在 `tuiHook` 字段定义之后加:

```go
	tuiHook func(rules []nft.Rule)

	// panelHook mirrors tuiHook for owner=="panel" writes. Production wires
	// it to dialer.NotifyPanelEdited so a TUI edit to a server-managed
	// forward is reported back to the panel for persistence. Invoked outside
	// d.mu for the same reason as tuiHook.
	panelHook func(rules []nft.Rule)
```

- [ ] **Step 4: setOwnerRuleset 末尾按 owner 触发**

把 `setOwnerRuleset` 末尾的 hook 捕获 + 触发(原 `hook := d.tuiHook` … `if owner == "tui" && hook != nil { hook(hookRules) }`)替换为:

```go
	tuiHook := d.tuiHook
	panelHook := d.panelHook
	hookRules := append([]nft.Rule(nil), candidate[owner]...)
	d.mu.Unlock()

	switch owner {
	case "tui":
		if tuiHook != nil {
			tuiHook(hookRules)
		}
	case "panel":
		if panelHook != nil {
			panelHook(hookRules)
		}
	}
	return nil
}
```

- [ ] **Step 5: 跑测试确认通过(handlers 层)**

Run: `go test ./internal/daemon/ -run 'TestPostRuleset' -v`
Expected: PASS(含既有 `TestPostRulesetTUIInvokesDialerHook`、`TestPostRulesetPanelDoesNotInvokeDialerHook` 与新增两个)

- [ ] **Step 6: daemon.go 接线 panelHook + OnPanelNotice 占位**

`internal/daemon/daemon.go` 的 `NewDialer(DialerConfig{...})` 中,在 `OnTuiNotice` 之后加占位回调:

```go
			OnTuiNotice: func(_ []wsproto.Forward) {},
			// Non-nil marker so the dialer emits panel_segment_edit frames;
			// payloads are built inside the dialer from its panelCh, so this
			// callback itself is a no-op.
			OnPanelNotice: func(_ []wsproto.Forward) {},
			CountersFn:    d.counterSamples,
```

并在 `d.tuiHook = func(...) {...}` 赋值块之后(同一 `d.mu.Lock()`/`Unlock()` 区间内)加:

```go
		d.panelHook = func(rules []nft.Rule) {
			if dl := d.Dialer(); dl != nil {
				dl.NotifyPanelEdited(rules)
			}
		}
```

- [ ] **Step 7: 编译 + 全包测试确认无回归**

Run: `go build ./... && go test ./internal/daemon/`
Expected: 构建成功;全部测试 PASS

- [ ] **Step 8: gofmt + 提交**

```bash
gofmt -w internal/daemon/handlers.go internal/daemon/daemon.go
git add internal/daemon/handlers.go internal/daemon/daemon.go internal/daemon/handlers_test.go
git commit -m "feat(daemon): report panel-segment edits to the panel via panelHook"
```

---

## Task 5: server hub 接收并落库 panel 编辑

**Files:**
- Modify: `internal/server/hub.go:233-242`(readerLoop tui 分支之后)、`:430` 之后(`applyCounters` 旁加 `applyPanelEdits`)
- Test: `internal/server/hub_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/server/hub_test.go` 末尾追加:

```go
func TestHubPanelSegmentEditUpdatesNonChainForward(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.1", TargetPort: 30000, Comment: "old", Mode: "kernel",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	pse, _ := json.Marshal(wsproto.PanelSegmentEdit{Forwards: []wsproto.Forward{
		{Proto: "tcp", ListenPort: 30000, TargetIP: "10.9.9.9", TargetPort: 8443, Comment: "new", Mode: "userspace"},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypePanelSegmentEdit, Payload: pse})
	syncByPing(t, c)

	got, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "10.9.9.9" || got.TargetPort != 8443 || got.Comment != "new" || got.Mode != "userspace" {
		t.Fatalf("panel edit not persisted: %+v", got)
	}
}

func TestHubPanelSegmentEditIgnoresChainForward(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	res, err := hub.DB.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 20001, TargetIP: "5.6.7.8", TargetPort: 20002,
		ChainID: sql.NullInt64{Int64: cid, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A chained hop reported with a tampered target must be rejected.
	hub.applyPanelEdits(n.ID, []wsproto.Forward{
		{Proto: "tcp", ListenPort: 20001, TargetIP: "1.1.1.1", TargetPort: 1, Comment: "hijack"},
	})

	got, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "5.6.7.8" || got.TargetPort != 20002 {
		t.Fatalf("chain hop port/target must stay intact: %+v", got)
	}
}

func TestHubPanelSegmentEditSkipsUnknownForward(t *testing.T) {
	_, hub, n := newHubTestServer(t)
	// No matching forward row exists; applyPanelEdits must not error/panic.
	hub.applyPanelEdits(n.ID, []wsproto.Forward{
		{Proto: "tcp", ListenPort: 65000, TargetIP: "10.0.0.1", TargetPort: 65000},
	})
}
```

确认 `internal/server/hub_test.go` import 块含 `"database/sql"`(已存在,用于 `sql.NullInt64`)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestHubPanelSegmentEdit' -v`
Expected: 编译失败 —— `hub.applyPanelEdits undefined`

- [ ] **Step 3: readerLoop 加 panel 分支**

`internal/server/hub.go` 的 readerLoop switch,在 `case wsproto.TypeTuiSegmentChanged:` 块之后加:

```go
		case wsproto.TypePanelSegmentEdit:
			var pse wsproto.PanelSegmentEdit
			if err := json.Unmarshal(env.Payload, &pse); err != nil {
				log.Printf("hub: node %d malformed panel_segment_edit: %v", ac.nodeID, err)
				continue
			}
			h.applyPanelEdits(ac.nodeID, pse.Forwards)
```

- [ ] **Step 4: 实现 applyPanelEdits**

在 `applyCounters` 之后加:

```go
// applyPanelEdits folds a node's edits to its panel-segment forwards back
// into the forwards table so the server becomes their authority. Each
// forward is located by (node_id, proto, listen_port). Only non-chain rows
// are updated: a chained hop's listen_port/target form a relay skeleton
// owned by chain orchestration (RegenerateChain wires neighbor hops
// together), so a node-side edit must never rewrite it — UpdateForward's
// chain_id IS NULL guard is the second backstop behind this ChainID.Valid
// skip.
//
// Per-edit failures (DB error, or a lookup miss meaning the forward was
// deleted on the panel side between the node's snapshot and the frame's
// arrival) are logged and the loop continues, mirroring applyCounters: one
// bad row shouldn't abandon the rest of the batch.
func (h *Hub) applyPanelEdits(nodeID int64, forwards []wsproto.Forward) {
	for _, f := range forwards {
		existing, err := db.GetForwardByNodeProtoPort(h.DB, nodeID, f.Proto, f.ListenPort)
		if err != nil {
			log.Printf("hub: node %d panel edit for %s/%d matched no forward row (rule may have been deleted)", nodeID, f.Proto, f.ListenPort)
			continue
		}
		if existing.ChainID.Valid {
			continue
		}
		if _, err := db.UpdateForward(h.DB, nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, f.Mode); err != nil {
			log.Printf("hub: node %d panel edit update for %s/%d: %v", nodeID, f.Proto, f.ListenPort, err)
			continue
		}
	}
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -run 'TestHubPanelSegmentEdit' -v`
Expected: PASS(三个测试)

- [ ] **Step 6: gofmt + 全包测试 + 提交**

```bash
gofmt -w internal/server/hub.go
go test ./internal/server/
git add internal/server/hub.go internal/server/hub_test.go
git commit -m "feat(server): persist node-reported non-chain panel edits"
```

---

## Task 6: TUI 统一列表 + 跨段导航 + 按 owner 编辑/提交

> 本 Task 内 `enterEditMode`/`submitEdit` 必须与导航一并改为 owner-aware:导航放开到 panel 段后,旧的按 `m.rules[m.cursor]` 取值会越界,故合并在同一 Task,按多个 TDD 循环推进。

**Files:**
- Modify: `internal/tui/tui.go:74-92`(model)、`:183-218`(updateList)、`:231-268`(enterEditMode)、`:354-408`(submitEdit)、`:472-500`(updateConfirmDelete)、`:523-540`(refresh)、`:542-550`(commit)、`:575-585`(const 列宽)、`:628-710`(viewList)
- Test: `internal/tui/tui_test.go`

### 循环 A — helper:totalRows / rowAt

- [ ] **Step 1: 写失败测试**

在 `internal/tui/tui_test.go` 末尾追加:

```go
func TestRowAtResolvesOwnerAndEditable(t *testing.T) {
	m := model{
		rules: []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		panelRules: []nft.Rule{
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},                                   // standalone (editable)
			{Proto: "tcp", SrcPort: 44751, DestIP: "1.2.3.4", DestPort: 42421, ChainID: 7, ChainName: "vless"},   // chain hop (read-only)
		},
	}
	if m.totalRows() != 3 {
		t.Fatalf("totalRows = %d, want 3", m.totalRows())
	}
	if r, owner, editable := m.rowAt(0); owner != "tui" || !editable || r.SrcPort != 100 {
		t.Fatalf("row 0 = (%+v, %q, %v)", r, owner, editable)
	}
	if r, owner, editable := m.rowAt(1); owner != "panel" || !editable || r.SrcPort != 30000 {
		t.Fatalf("row 1 = (%+v, %q, %v)", r, owner, editable)
	}
	if _, owner, editable := m.rowAt(2); owner != "panel" || editable {
		t.Fatalf("row 2 should be panel read-only, got owner=%q editable=%v", owner, editable)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestRowAtResolvesOwnerAndEditable' -v`
Expected: 编译失败 —— `m.totalRows undefined` / `m.rowAt undefined`

- [ ] **Step 3: 实现 helper**

在 `internal/tui/tui.go` 的 `loadInitialRules` 之后(或 model 定义附近)加:

```go
// totalRows is the count of selectable rows across both segments: the
// editable tui segment followed by the server-managed panel segment.
func (m model) totalRows() int {
	return len(m.rules) + len(m.panelRules)
}

// rowAt resolves a unified cursor index to its rule, owner, and whether it
// is editable. Indices [0,len(rules)) map to the tui segment; the remainder
// map to the panel segment. A panel rule is editable only when it is not a
// chain hop (ChainID==0): chain hops carry a relay skeleton whose
// port/target must not be edited from the TUI.
func (m model) rowAt(i int) (r nft.Rule, owner string, editable bool) {
	if i < len(m.rules) {
		return m.rules[i], "tui", true
	}
	p := m.panelRules[i-len(m.rules)]
	return p, "panel", p.ChainID == 0
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestRowAtResolvesOwnerAndEditable' -v`
Expected: PASS

### 循环 B — viewList 统一渲染(替换 phase 1 只读区)

- [ ] **Step 5: 改/写测试**

删除 `internal/tui/tui_test.go` 中的 `TestViewListRendersReadOnlyPanelSection`,替换为:

```go
func TestViewListRendersUnifiedSegments(t *testing.T) {
	m := model{
		mode:  viewList,
		width: 120,
		rules: []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		panelRules: []nft.Rule{
			{Proto: "tcp", SrcPort: 44751, DestIP: "104.251.236.89", DestPort: 42421, ChainID: 7, ChainName: "seednet-vless"},
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
		},
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "本地") {
		t.Fatalf("expected tui row tagged 本地, got:\n%s", out)
	}
	if !strings.Contains(out, "seednet-vless") {
		t.Fatalf("expected chain row to show chain name, got:\n%s", out)
	}
	if !strings.Contains(out, "server") {
		t.Fatalf("expected standalone panel row tagged server, got:\n%s", out)
	}
	for _, port := range []string{"100", "44751", "30000"} {
		if !strings.Contains(out, port) {
			t.Fatalf("expected listen port %s rendered, got:\n%s", port, out)
		}
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestViewListRendersUnifiedSegments' -v`
Expected: FAIL —— 旧 viewList 把 panel 行渲染为 "server 托管" 独立区,断言 "server"/"本地" 与统一列表布局不满足(或 `本地` 标记缺失)

- [ ] **Step 7: 加列宽常量**

`internal/tui/tui.go` 的列宽常量块(`colProto` 等所在)加:

```go
	colOwner   = 18 // "链路 seednet-vless" / "本地" / "server"
	colProto   = 8  // "tcp+udp " (longest option 7 chars + 1 pad)
```

- [ ] **Step 8: 重写 viewList**

把 `viewList`(从 `if len(m.rules) == 0 {` 起到 phase 1 panel 只读区结束,即 status 行之前)替换为统一列表渲染:

```go
func (m model) viewList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("nft-forward — IPv4 端口转发") + "\n\n")

	if m.totalRows() == 0 {
		b.WriteString(helpStyle.Render("  （暂无规则 — 按 a 新增）") + "\n")
	} else {
		header := cellStyle(colOwner).Render("来源") +
			renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
		b.WriteString(headerStyle.Render(header) + "\n")

		fixedWidth := colOwner + colProto + colSrcPort + colDest + colDstPort
		innerWidth := m.width - 2*colMargin
		if innerWidth < fixedWidth+1 {
			innerWidth = 80 - 2*colMargin
		}
		commentWidth := innerWidth - fixedWidth

		for i := 0; i < m.totalRows(); i++ {
			r, owner, _ := m.rowAt(i)
			destHost := r.DestIP
			if r.DestHost != "" {
				destHost = r.DestHost
			}
			protoCell := strings.ToLower(r.Proto)
			if r.EffectiveMode() == nft.ModeUserspace {
				protoCell += " (U)"
			}
			ownerTag := "本地"
			if owner == "panel" {
				if r.ChainID != 0 {
					ownerTag = "链路 " + r.ChainName
				} else {
					ownerTag = "server"
				}
			}
			line := cellStyle(colOwner).Render(truncateCell(ownerTag, colOwner)) +
				renderTableRow(
					protoCell,
					strconv.Itoa(r.SrcPort),
					destHost,
					strconv.Itoa(r.DestPort),
					cellStyle(commentWidth).Render(r.Comment),
				)
			if i == m.cursor {
				b.WriteString(selectedStyle.Render(line) + "\n")
			} else {
				b.WriteString(line + "\n")
			}
		}
	}

	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errStyle.Render("错误: "+m.err) + "\n")
	} else if m.status != "" {
		b.WriteString(okStyle.Render(m.status) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ 选择 • a 新增 • e 编辑 • d 删除 • c 清空 • r 重载 • q 退出"))
	return b.String()
}
```

- [ ] **Step 9: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestViewListRendersUnifiedSegments' -v`
Expected: PASS

### 循环 C — 导航跨段 + cursor 钳制

- [ ] **Step 10: 写失败测试**

```go
func TestUpdateListNavigatesAcrossSegments(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
	}, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
	})
	// down from tui row 0 must reach panel row (cursor 1).
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyDown})
	m = nm.(model)
	if m.cursor != 1 {
		t.Fatalf("cursor after down = %d, want 1 (panel segment)", m.cursor)
	}
	// down again must clamp at totalRows-1.
	nm, _ = m.updateList(tea.KeyMsg{Type: tea.KeyDown})
	if nm.(model).cursor != 1 {
		t.Fatalf("cursor must clamp at last row, got %d", nm.(model).cursor)
	}
}
```

- [ ] **Step 11: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestUpdateListNavigatesAcrossSegments' -v`
Expected: FAIL —— down 仍按 `len(m.rules)-1` 钳制,cursor 停在 0

- [ ] **Step 12: 改 updateList 导航边界**

`updateList` 的 `down`/`j` 分支改为:

```go
	case "down", "j":
		if m.cursor < m.totalRows()-1 {
			m.cursor++
		}
```

- [ ] **Step 13: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestUpdateListNavigatesAcrossSegments' -v`
Expected: PASS

### 循环 D — 编辑路由(enterEditMode 拒链式 + submitEdit 按 owner + commitOwner)

- [ ] **Step 14: 写失败测试**

```go
func TestEnterEditModeRejectsChainRow(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 44751, DestIP: "1.2.3.4", DestPort: 42421, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0 // panel chain hop (no tui rows)
	m.enterEditMode()
	if m.mode == viewEdit {
		t.Fatal("chain hop must not enter edit mode")
	}
	if !strings.Contains(m.status, "只读") {
		t.Fatalf("expected read-only status, got %q", m.status)
	}
}

func TestSubmitEditPanelRowPostsPanelOwner(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
	}, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443, Comment: "old"},
	})
	m.cursor = 1 // panel standalone row
	m.enterEditMode()
	if m.mode != viewEdit || m.editingOwner != "panel" {
		t.Fatalf("expected panel edit mode, got mode=%v owner=%q", m.mode, m.editingOwner)
	}
	m.inputs[fDestIP].SetValue("10.9.9.9")
	m.inputs[fDestPort].SetValue("8443")
	m.inputs[fComment].SetValue("new")
	nm, _ := m.submitEdit()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if fc.postedOwner != "panel" {
		t.Fatalf("expected post to panel owner, got %q", fc.postedOwner)
	}
	if len(mm.panelRules) != 1 || mm.panelRules[0].DestIP != "10.9.9.9" || mm.panelRules[0].DestPort != 8443 {
		t.Fatalf("panel rule not updated locally: %+v", mm.panelRules)
	}
	// tui segment must be untouched.
	if fc.postedOwner == "tui" || len(mm.rules) != 1 || mm.rules[0].SrcPort != 100 {
		t.Fatalf("tui segment must be untouched: posted=%q rules=%+v", fc.postedOwner, mm.rules)
	}
}

func TestCommitOwnerPostsGivenOwner(t *testing.T) {
	fc := &fakeDaemonClient{}
	applied, err := commitOwner(fc, "panel", []nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}})
	if err != nil {
		t.Fatal(err)
	}
	if fc.postedOwner != "panel" || len(applied) != 1 {
		t.Fatalf("commitOwner posted owner=%q applied=%+v", fc.postedOwner, applied)
	}
}
```

- [ ] **Step 15: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestEnterEditModeRejectsChainRow|TestSubmitEditPanelRowPostsPanelOwner|TestCommitOwnerPostsGivenOwner' -v`
Expected: 编译失败 —— `m.editingOwner undefined` / `commitOwner undefined`

- [ ] **Step 16: model 加 editingOwner**

`model` struct 加字段:

```go
	rules      []nft.Rule
	panelRules []nft.Rule // server-pushed segment
	cursor     int
	// editingOwner records which segment the in-progress edit targets so
	// submitEdit posts back to the right owner ("tui" or "panel").
	editingOwner string
```

- [ ] **Step 17: 重写 enterEditMode 用 rowAt**

把 `enterEditMode` 替换为:

```go
func (m *model) enterEditMode() {
	r, owner, editable := m.rowAt(m.cursor)
	if !editable {
		m.status = "链式规则端口/目标只读，请在面板修改"
		return
	}
	m.editingOwner = owner
	m.mode = viewEdit
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto

	m.protoIdx = 0
	for i, p := range protoOptions {
		if p == r.Proto {
			m.protoIdx = i
			break
		}
	}
	m.modeIdx = 0
	for i, md := range modeOptions {
		if md == r.EffectiveMode() {
			m.modeIdx = i
			break
		}
	}
	m.inputs[fSrcPort].SetValue(strconv.Itoa(r.SrcPort))
	destValue := r.DestIP
	if r.DestHost != "" {
		destValue = r.DestHost
	}
	m.inputs[fDestIP].SetValue(destValue)
	m.inputs[fDestPort].SetValue(strconv.Itoa(r.DestPort))
	m.inputs[fComment].SetValue(r.Comment)
}
```

- [ ] **Step 18: 重写 submitEdit 按 owner 路由**

把 `submitEdit` 替换为:

```go
func (m model) submitEdit() (tea.Model, tea.Cmd) {
	proto := protoOptions[m.protoIdx]
	srcPortStr := strings.TrimSpace(m.inputs[fSrcPort].Value())
	destInput := strings.TrimSpace(m.inputs[fDestIP].Value())
	destPortStr := strings.TrimSpace(m.inputs[fDestPort].Value())
	comment := strings.TrimSpace(m.inputs[fComment].Value())

	srcPort, err1 := strconv.Atoi(srcPortStr)
	destPort, err2 := strconv.Atoi(destPortStr)
	if err1 != nil || err2 != nil {
		m.err = "端口必须为数字"
		return m, nil
	}

	owner := m.editingOwner
	var seg []nft.Rule
	var idx int
	if owner == "panel" {
		seg = m.panelRules
		idx = m.cursor - len(m.rules)
	} else {
		seg = m.rules
		idx = m.cursor
	}

	r := nft.Rule{
		ID:        seg[idx].ID,
		Proto:     proto,
		Mode:      modeOptions[m.modeIdx],
		SrcPort:   srcPort,
		DestPort:  destPort,
		Comment:   comment,
		ChainID:   seg[idx].ChainID,   // preserved; for editable rows this is 0
		ChainName: seg[idx].ChainName,
	}
	if resolver.IsHostname(destInput) {
		r.DestHost = destInput
	} else {
		r.DestIP = destInput
	}
	if err := nft.Validate(r); err != nil {
		m.err = err.Error()
		return m, nil
	}
	for i, existing := range seg {
		if i != idx && existing.Proto == r.Proto && existing.SrcPort == r.SrcPort {
			m.err = fmt.Sprintf("%s/%d 已被转发占用", r.Proto, r.SrcPort)
			return m, nil
		}
	}

	next := append([]nft.Rule{}, seg...)
	next[idx] = r
	applied, err := commitOwner(m.client, owner, next)
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	if owner == "panel" {
		m.panelRules = applied
	} else {
		m.rules = applied
	}
	m.mode = viewList
	statusTarget := r.DestIP
	if r.DestHost != "" {
		statusTarget = r.DestHost
	}
	m.status = fmt.Sprintf("已更新 %s/%d → %s:%d", r.Proto, r.SrcPort, statusTarget, r.DestPort)
	m.err = ""
	return m, nil
}
```

- [ ] **Step 19: 加 commitOwner,commit 改为封装**

把 `commit` 替换为:

```go
// commitOwner posts a full segment snapshot for owner to the daemon. Raw
// rules go on the wire — the daemon resolves hostnames at apply time — so
// DestHost/DestIP are sent as the user typed them.
func commitOwner(client daemonClient, owner string, rules []nft.Rule) ([]nft.Rule, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	if err := client.PostRuleset(owner, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func commit(client daemonClient, rules []nft.Rule) ([]nft.Rule, error) {
	return commitOwner(client, "tui", rules)
}
```

- [ ] **Step 20: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestEnterEditModeRejectsChainRow|TestSubmitEditPanelRowPostsPanelOwner|TestCommitOwnerPostsGivenOwner' -v`
Expected: PASS(三个测试);`TestCommitPostsRawRules`(commit→tui)与 `TestProtoSelectorEditPreFill` 仍 PASS

### 循环 E — "e"/"d" 分支与删除/刷新的跨段处理

- [ ] **Step 21: 写失败测试**

```go
func TestUpdateListDeleteRejectsPanelRow(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
	})
	m.cursor = 0 // panel row (no tui rows)
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	mm := nm.(model)
	if mm.mode == viewConfirmDelete {
		t.Fatal("delete must not target a panel-segment row")
	}
	if !strings.Contains(mm.status, "托管") {
		t.Fatalf("expected server-managed delete rejection, got %q", mm.status)
	}
}

func TestRefreshClampsCursor(t *testing.T) {
	fc := &fakeDaemonClient{owners: daemonclient.OwnerRuleset{
		"tui": {{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
	}}
	m := initialModel(fc, nil, nil)
	m.cursor = 5 // stale cursor beyond any row
	m.refresh()
	if m.cursor != 0 {
		t.Fatalf("refresh must clamp cursor to a valid row, got %d", m.cursor)
	}
}
```

- [ ] **Step 22: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestUpdateListDeleteRejectsPanelRow|TestRefreshClampsCursor' -v`
Expected: FAIL —— `d` 在 panel 行误进 confirmDelete;refresh 不钳制 cursor

- [ ] **Step 23: 改 updateList 的 "e"/"d" 分支**

`updateList` 的 `e` 与 `d` 分支替换为:

```go
	case "a", "n", "+":
		m.enterAddMode()
		return m, textinput.Blink
	case "e":
		if m.totalRows() == 0 {
			m.status = "no rule to edit"
			return m, nil
		}
		m.enterEditMode()
		return m, textinput.Blink
	case "d", "delete":
		if m.cursor < len(m.rules) && len(m.rules) > 0 {
			m.mode = viewConfirmDelete
			m.err = ""
		} else if m.totalRows() > 0 && m.cursor >= len(m.rules) {
			m.status = "server 托管规则不能在此删除"
		}
```

- [ ] **Step 24: updateConfirmDelete 的 cursor 调整改为基于 totalRows**

`updateConfirmDelete` 中删除成功后的钳制改为:

```go
		m.rules = applied
		if m.cursor >= m.totalRows() && m.cursor > 0 {
			m.cursor--
		}
```

- [ ] **Step 25: refresh 末尾加 cursor 钳制**

`refresh` 在设置 `m.panelRules = panel` 之后、`m.status = ...` 之前加:

```go
	m.panelRules = panel
	if m.cursor >= m.totalRows() {
		m.cursor = m.totalRows() - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.status = "已从 daemon 重新加载"
```

- [ ] **Step 26: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestUpdateListDeleteRejectsPanelRow|TestRefreshClampsCursor' -v`
Expected: PASS

- [ ] **Step 27: gofmt + 全 TUI 包测试 + 提交**

```bash
gofmt -w internal/tui/tui.go
go test ./internal/tui/
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "feat(tui): merge tui and panel segments into one editable list"
```

---

## 收尾:全量验证

- [ ] **Step 1: 全仓库测试 + vet**

Run: `go test ./... && go vet ./...`
Expected: 全部 PASS;vet 无告警

- [ ] **Step 2: 构建**

Run: `go build ./...`
Expected: 成功

完成后按 superpowers:finishing-a-development-branch 处理分支合并到 `main`。

---

## 自检(Self-Review)

**Spec 覆盖**(对照 `docs/superpowers/specs/2026-06-03-tui-panel-visibility-design.md` 阶段二):
- 帧 `TypePanelSegmentEdit` + payload → Task 1 ✓
- `setOwnerRuleset` panel 触发 `panelHook`(锁外、对称)→ Task 4 ✓
- daemon 接线 `panelHook` → `NotifyPanelEdited` → Task 3 + Task 4 ✓
- dialer `NotifyPanelEdited` + 发帧 → Task 3 ✓
- TUI 非链式 panel 编辑 → `PostRuleset("panel", ...)` → Task 6 ✓
- hub `readerLoop` 加分支 + 逐条按键 reconcile,只接受非链式 → Task 5 ✓
- `db.UpdateForward`(按 node_id+proto+listen_port,`chain_id IS NULL` 兜底)→ Task 2 ✓
- 链式行只读(TUI 拒绝 + server 兜底)→ Task 6(`rowAt` editable) + Task 5(`ChainID.Valid` skip) + Task 2(`chain_id IS NULL`)三重 ✓

**类型一致性自检:**
- `UpdateForward(d, nodeID, proto, listenPort, targetIP, targetPort, comment, mode)` —— Task 2 定义 / Task 5 `applyPanelEdits` 调用一致 ✓
- `commitOwner(client, owner, rules)` —— Task 6 定义并被 `submitEdit` 调用;`commit` 仅 tui 封装 ✓
- `rowAt` 返回 `(nft.Rule, string, bool)`;`enterEditMode`/`viewList` 用法一致 ✓
- `model.editingOwner` 在 `enterEditMode` 写、`submitEdit` 读 ✓
- `wsproto.PanelSegmentEdit{Forwards []Forward}` —— Task 1 定义 / Task 3 编码 / Task 5 解码一致 ✓

**非目标(不做):** 链式安全字段编辑(留待后续阶段);并发竞态;node 重连 rev 短路补发。
