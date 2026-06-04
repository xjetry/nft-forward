# TUI 编辑/删除链式规则 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **派发 subagent 时必须传达：** 代码注释、KDoc、commit message 中**绝对禁止**出现执行过程信息（Task/Phase 编号、方案代号、审阅轮次、"按上一轮指示"等）。注释只解释 WHY 与 invariant。subagent 产出若违反，follow-up 清理。

**Goal:** 让 TUI 放开链式规则的受限编辑（本地端口、模式、备注）与删除整条链路，操作经 server 权威重算后下发给链路涉及的所有 agent；目标地址/端口/协议锁定只读。

**Architecture:** 链式操作走**命令式**路径（区别于非链式的快照式）：TUI → daemon socket → dialer 同步命令帧（带 `Envelope.ID` 配对回执）→ server hub。Hub 自己做 DB 操作（`RegenerateChain`/`DeleteChain`），经新增 `Redispatch` 回调把下发委托给 Server（与 `OnTrafficUpdate` 模式一致）。`(chain_id, 连接node)` 唯一定位一跳。`RegenerateChain` 扩展 `HopInput.DesiredPort`/`Comment` 让用户指定的端口/备注在重算后存活（`chain_hops` 加 `comment` 列）。

**Tech Stack:** Go，coder/websocket，SQLite（database/sql，外键开启），bubbletea/lipgloss TUI。

---

## File Structure

| 文件 | 职责 | 改动 |
|---|---|---|
| `internal/wsproto/messages.go` | WS 帧/payload | 加 `TypeChainHopEdit`/`TypeChainDelete`/`TypeChainCmdAck` 常量 + 三个 payload |
| `internal/db/migrations/0008_chain_hop_comment.sql` | schema | 新建：`chain_hops` 加 `comment` 列 |
| `internal/db/chains.go` | 链路编排 | `ChainHop` 加 `Comment`；`ListChainHops` 读 comment；`HopInput` 加 `DesiredPort`/`Comment`；`RegenerateChain` 采纳指定端口 + 保持自定义备注 |
| `internal/server/hub.go` | server 收帧 | 加 `Redispatch` 字段；readerLoop 加两分支；`applyChainHopEdit`/`applyChainDelete`/`sendChainAckErr` |
| `internal/server/server.go` | Server↔Hub 接线 | 加 `redispatchNodes`；`NewServer` wire `hub.Redispatch` |
| `internal/daemon/dialer.go` | node→server 同步命令 | 加 `cmdCh`/`pending`/`connected`/`idSeq`；`EditChainHop`/`DeleteChain`/`sendCommand`；serve loop 两处 |
| `internal/daemon/handlers.go` | daemon socket | 加 `/v1/chain/edit`、`/v1/chain/delete` 路由 + handler |
| `internal/daemonclient/client.go` | TUI→daemon 客户端 | 加 `ChainEdit`/`ChainDelete` |
| `internal/tui/tui.go` | TUI 渲染/编辑 | `daemonClient` 接口扩展；`rowAt` 放开链式；`editingChainID`；字段锁定矩阵；链式编辑/删除走命令 |

依赖顺序：Task 1（帧）→ Task 2（db）→ Task 3（hub+server）→ Task 4（dialer）→ Task 5（daemon socket + daemonclient）→ Task 6（TUI）。

---

## Task 1: wsproto 链式命令帧

**Files:**
- Modify: `internal/wsproto/messages.go:16-29`（常量块）、`:140` 之后（`PanelSegmentEdit` 旁）
- Test: `internal/wsproto/messages_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/wsproto/messages_test.go` 末尾追加：

```go
func TestChainCommandFramesRoundtrip(t *testing.T) {
	e := ChainHopEdit{ChainID: 7, ListenPort: 21000, Mode: nft.ModeUserspace, Comment: "edge hop"}
	b, _ := json.Marshal(e)
	var ge ChainHopEdit
	if err := json.Unmarshal(b, &ge); err != nil {
		t.Fatal(err)
	}
	if ge.ChainID != 7 || ge.ListenPort != 21000 || ge.Mode != nft.ModeUserspace || ge.Comment != "edge hop" {
		t.Fatalf("chain_hop_edit roundtrip mismatch: %+v", ge)
	}

	d := ChainDelete{ChainID: 9}
	b, _ = json.Marshal(d)
	var gd ChainDelete
	if err := json.Unmarshal(b, &gd); err != nil {
		t.Fatal(err)
	}
	if gd.ChainID != 9 {
		t.Fatalf("chain_delete roundtrip mismatch: %+v", gd)
	}

	a := ChainCmdAck{OK: false, Error: "端口被占用", Entry: ""}
	b, _ = json.Marshal(a)
	var ga ChainCmdAck
	if err := json.Unmarshal(b, &ga); err != nil {
		t.Fatal(err)
	}
	if ga.OK || ga.Error != "端口被占用" {
		t.Fatalf("chain_cmd_ack roundtrip mismatch: %+v", ga)
	}
}

func TestChainCommandTypeConstants(t *testing.T) {
	if TypeChainHopEdit != "chain_hop_edit" || TypeChainDelete != "chain_delete" || TypeChainCmdAck != "chain_cmd_ack" {
		t.Fatalf("unexpected chain type constants: %q %q %q", TypeChainHopEdit, TypeChainDelete, TypeChainCmdAck)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/wsproto/ -run 'TestChainCommand' -v`
Expected: 编译失败 —— `undefined: ChainHopEdit` 等

- [ ] **Step 3: 加常量**

`internal/wsproto/messages.go` 常量块（`TypePanelSegmentEdit` 之后）加三行：

```go
	TypePanelSegmentEdit  = "panel_segment_edit"
	TypeChainHopEdit      = "chain_hop_edit"
	TypeChainDelete       = "chain_delete"
	TypeChainCmdAck       = "chain_cmd_ack"
	TypePing              = "ping"
```

- [ ] **Step 4: 加 payload 类型**

在 `PanelSegmentEdit` 定义之后加：

```go
// ChainHopEdit carries a node's edit to its single hop in a relay chain.
// The hop is located server-side by (chain_id, connection node) — a chain
// can't repeat a node — so neither position nor target rides on the wire.
// Only listen_port/mode/comment are editable; the server recomputes targets
// and uses chain.proto, so the relay skeleton can't be rewritten from a node.
type ChainHopEdit struct {
	ChainID    int64  `json:"chain_id"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

// ChainDelete asks the server to delete an entire chain (all hops on all
// nodes), identified by ChainID. The requesting node must participate in it.
type ChainDelete struct {
	ChainID int64 `json:"chain_id"`
}

// ChainCmdAck is the server's reply to ChainHopEdit/ChainDelete, matched to
// the request via Envelope.ID. OK==true requires Error==""; on a successful
// edit Entry carries the chain's copyable entry endpoint. Mirrors ApplyAck's
// OK+Error contract: OK is the load-bearing signal, Error is human context.
type ChainCmdAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Entry string `json:"entry,omitempty"`
}
```

- [ ] **Step 5: gofmt + 测试 + 提交**

```bash
gofmt -w internal/wsproto/messages.go
go test ./internal/wsproto/
git add internal/wsproto/messages.go internal/wsproto/messages_test.go
git commit -m "feat(wsproto): add chain edit/delete command frames"
```

---

## Task 2: chain_hops.comment + RegenerateChain 采纳指定端口/保持备注

**Files:**
- Create: `internal/db/migrations/0008_chain_hop_comment.sql`
- Modify: `internal/db/chains.go:116-123`（`ChainHop`）、`:190-205`（`ListChainHops`）、`:277-281`（`HopInput`）、`:297-304`（`resolved`）、`:341`（构造 resolved）、`:344-388`（端口分配 + prev 读取）、`:390-410`（INSERT）
- Test: `internal/db/chains_test.go`

- [ ] **Step 1: 建 migration**

`internal/db/migrations/0008_chain_hop_comment.sql`：

```sql
-- 链路跳的自定义备注。空串表示无自定义,RegenerateChain 回退到默认生成的
-- "链路 X · 第N跳"。独立于 forwards.comment 存在,因为 RegenerateChain 每次
-- 重建 forwards 都会覆盖其 comment,只有存在 chain_hops 上的自定义值才能在
-- 重算后保留。
ALTER TABLE chain_hops ADD COLUMN comment TEXT NOT NULL DEFAULT '';
```

- [ ] **Step 2: 写失败测试**

在 `internal/db/chains_test.go` 末尾追加。`seedTwoHopChain` 是本任务用的 helper（若文件已有同名 helper 则复用既有的，不要重复定义）：

```go
// seedTwoHopChain creates an admin chain with hops on two fresh nodes and
// returns the chain plus both node IDs. Hop 0 targets hop 1; hop 1 targets
// the chain exit.
func seedTwoHopChain(t *testing.T, d *sql.DB) (*Chain, int64, int64) {
	t.Helper()
	n0, _ := CreateNode(d, "edge-0", "https://p0", "tok0")
	n1, _ := CreateNode(d, "edge-1", "https://p1", "tok1")
	// relay_host must be set or RegenerateChain rejects the hop.
	if _, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.10", n0.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.11", n1.ID); err != nil {
		t.Fatal(err)
	}
	c := &Chain{Name: "wire", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	id, err := CreateChain(d, c)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id
	tx, _ := d.Begin()
	if _, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: n0.ID}, {NodeID: n1.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	return c, n0.ID, n1.ID
}

func TestRegenerateChainHonorsDesiredPortAndSyncsUpstream(t *testing.T) {
	d := openMemDB(t)
	c, n0, n1 := seedTwoHopChain(t, d)

	// Edit hop 1's listen port to a specific value; hop 0 (upstream) must
	// retarget to it.
	hops, _ := ListChainHops(d, c.ID)
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
		if h.NodeID == n1 {
			inputs[i].DesiredPort = 21111
		}
	}
	tx, _ := d.Begin()
	if _, _, err := RegenerateChain(tx, c, inputs, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	fwds, _ := ListForwardsByChain(d, c.ID)
	byNode := map[int64]*Forward{}
	for _, f := range fwds {
		byNode[f.NodeID] = f
	}
	if byNode[n1].ListenPort != 21111 {
		t.Fatalf("hop 1 listen_port = %d, want 21111", byNode[n1].ListenPort)
	}
	if byNode[n0].TargetPort != 21111 {
		t.Fatalf("upstream hop 0 target_port = %d, want 21111 (must follow downstream)", byNode[n0].TargetPort)
	}
}

func TestRegenerateChainRejectsOutOfRangeDesiredPort(t *testing.T) {
	d := openMemDB(t)
	c, _, n1 := seedTwoHopChain(t, d)
	hops, _ := ListChainHops(d, c.ID)
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
		if h.NodeID == n1 {
			inputs[i].DesiredPort = 80 // below ChainPortMin
		}
	}
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, inputs, nil)
	tx.Rollback()
	if err == nil {
		t.Fatal("expected out-of-range desired port to be rejected")
	}
}

func TestRegenerateChainKeepsCustomCommentAcrossRegen(t *testing.T) {
	d := openMemDB(t)
	c, n0, n1 := seedTwoHopChain(t, d)

	// First edit: set a custom comment on hop n1.
	hops, _ := ListChainHops(d, c.ID)
	mk := func(custom map[int64]string) []HopInput {
		in := make([]HopInput, len(hops))
		for i, h := range hops {
			in[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
			if cm, ok := custom[h.NodeID]; ok {
				in[i].Comment = cm
			}
		}
		return in
	}
	tx, _ := d.Begin()
	RegenerateChain(tx, c, mk(map[int64]string{n1: "my custom"}), nil)
	tx.Commit()

	// Second regen with no comment input (mimics webui re-save): the custom
	// comment must survive, the other hop falls back to the default.
	tx, _ = d.Begin()
	RegenerateChain(tx, c, mk(nil), nil)
	tx.Commit()

	hops, _ = ListChainHops(d, c.ID)
	byNode := map[int64]*ChainHop{}
	for _, h := range hops {
		byNode[h.NodeID] = h
	}
	if byNode[n1].Comment != "my custom" {
		t.Fatalf("custom comment not preserved: %q", byNode[n1].Comment)
	}
	if byNode[n0].Comment != "" {
		t.Fatalf("non-custom hop should have empty chain_hops.comment, got %q", byNode[n0].Comment)
	}
	fwds, _ := ListForwardsByChain(d, c.ID)
	for _, f := range fwds {
		if f.NodeID == n0 && f.Comment == "" {
			t.Fatalf("forwards.comment for default hop should be generated, got empty")
		}
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/db/ -run 'TestRegenerateChain(Honors|Rejects|Keeps)' -v`
Expected: 编译失败 —— `unknown field DesiredPort` / `h.Comment undefined`

- [ ] **Step 4: `ChainHop` 加 Comment + `ListChainHops` 读 comment**

`internal/db/chains.go` 的 `ChainHop` struct（`:116-123`）加字段：

```go
type ChainHop struct {
	ChainID    int64
	Position   int
	NodeID     int64
	TunnelID   sql.NullInt64
	ListenPort int
	Mode       string
	Comment    string
}
```

`ListChainHops`（`:190-205`）的 query + scan 加 comment：

```go
	rows, err := d.Query(`SELECT chain_id,position,node_id,tunnel_id,listen_port,mode,comment FROM chain_hops WHERE chain_id=? ORDER BY position`, chainID)
	...
		if err := rows.Scan(&h.ChainID, &h.Position, &h.NodeID, &h.TunnelID, &h.ListenPort, &h.Mode, &h.Comment); err != nil {
```

- [ ] **Step 5: `HopInput` 加 DesiredPort/Comment**

`HopInput`（`:277-281`）：

```go
// HopInput is one ordered hop the caller wants the chain to have. TunnelID is
// set for tenant chains and invalid for admin chains. Mode is the requested
// data plane (udp coerces every hop to kernel). DesiredPort, when >0, pins
// this hop's listen_port to an explicit value (a node-side edit) instead of
// the keep-or-reallocate default; it must be in range and free or
// RegenerateChain fails. Comment, when non-empty, is a user override stored on
// the hop and preserved across future regenerations; empty keeps whatever the
// hop already had, falling back to a generated label.
type HopInput struct {
	NodeID      int64
	TunnelID    sql.NullInt64
	Mode        string
	DesiredPort int
	Comment     string
}
```

- [ ] **Step 6: `resolved` 携带 desiredPort/comment**

`RegenerateChain` 内 `resolved` struct（`:297-304`）加两字段：

```go
	type resolved struct {
		nodeID      int64
		relayHost   string
		tunnelID    sql.NullInt64
		mode        string
		rangeLo     int
		rangeHi     int
		desiredPort int
		comment     string
	}
```

构造 `rs[i]`（`:341`）补两字段：

```go
		rs[i] = resolved{nodeID: hop.NodeID, relayHost: relay, tunnelID: tunnelID, mode: mode, rangeLo: lo, rangeHi: hi, desiredPort: hop.DesiredPort, comment: hop.Comment}
```

- [ ] **Step 7: DELETE 前读 prev 备注**

在读 prev forwards 之后（`:346-355` 的 `prevPort` 块之后），加读现有 hop 备注（必须在 DELETE chain_hops 之前）：

```go
	prevHopComment := map[int64]string{}
	prevHops, err := ListChainHops(tx, c.ID)
	if err != nil {
		return "", nil, err
	}
	for _, h := range prevHops {
		prevHopComment[h.NodeID] = h.Comment
	}
```

- [ ] **Step 8: 端口分配采纳 desiredPort**

把端口分配循环（`:364-388`）的取端口逻辑替换为：

```go
	ports := make([]int, len(rs))
	for i, h := range rs {
		occ, err := OccupiedPortsOnNode(tx, h.nodeID, c.Proto, c.ID)
		if err != nil {
			return "", nil, err
		}
		if av, ok := avoid[h.nodeID]; ok {
			occ[av] = true
		}
		var p int
		if h.desiredPort > 0 {
			// Explicit port from a node-side edit: honor it, but a conflict
			// or out-of-range value is a user error to surface, not something
			// to silently reallocate around.
			if h.desiredPort < h.rangeLo || h.desiredPort > h.rangeHi {
				var name string
				_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
				return "", nil, fmt.Errorf("端口 %d 超出节点 %s 允许范围(%d-%d)", h.desiredPort, name, h.rangeLo, h.rangeHi)
			}
			if occ[h.desiredPort] {
				return "", nil, fmt.Errorf("端口 %d 在节点上已被占用", h.desiredPort)
			}
			p = h.desiredPort
		} else {
			p = prevPort[h.nodeID]
			if !(p >= h.rangeLo && p <= h.rangeHi && !occ[p]) {
				p = PickFreePort(h.rangeLo, h.rangeHi, occ)
				if p == 0 {
					var name string
					_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
					return "", nil, fmt.Errorf("节点 %s 端口段(%d-%d)无可用端口", name, h.rangeLo, h.rangeHi)
				}
			}
		}
		ports[i] = p
	}
```

- [ ] **Step 9: INSERT 写 comment（自定义优先、保持、回退默认）**

把生成 forwards/chain_hops 的循环（`:390-410`）替换为：

```go
	for i, h := range rs {
		var targetIP string
		var targetPort int
		if i < len(rs)-1 {
			targetIP = rs[i+1].relayHost
			targetPort = ports[i+1]
		} else {
			targetIP = c.ExitHost
			targetPort = c.ExitPort
		}
		// Custom comment precedence: explicit edit > preserved from the prior
		// hop row > none. chain_hops.comment stores only the custom value
		// (empty = none); forwards.comment shows the custom value or a
		// generated label carrying the live position.
		hopComment := h.comment
		if hopComment == "" {
			hopComment = prevHopComment[h.nodeID]
		}
		fwdComment := hopComment
		if fwdComment == "" {
			fwdComment = fmt.Sprintf("链路 %s · 第%d跳", c.Name, i+1)
		}
		if _, err := tx.Exec(`INSERT INTO chain_hops(chain_id,position,node_id,tunnel_id,listen_port,mode,comment) VALUES (?,?,?,?,?,?,?)`,
			c.ID, i, h.nodeID, h.tunnelID, ports[i], h.mode, hopComment); err != nil {
			return "", nil, err
		}
		if _, err := tx.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode,chain_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			h.nodeID, c.TenantID, h.tunnelID, c.Proto, ports[i], targetIP, targetPort, fwdComment, now(), h.mode, c.ID); err != nil {
			return "", nil, err
		}
		affected[h.nodeID] = true
	}
```

- [ ] **Step 10: 跑测试确认通过**

Run: `go test ./internal/db/ -run 'TestRegenerateChain' -v`
Expected: PASS（含既有 RegenerateChain 测试，新增三个）

- [ ] **Step 11: gofmt + 全包测试 + 提交**

```bash
gofmt -w internal/db/chains.go
go test ./internal/db/
git add internal/db/chains.go internal/db/migrations/0008_chain_hop_comment.sql internal/db/chains_test.go
git commit -m "feat(db): honor explicit hop port and preserve custom hop comment in chain regen"
```

---

## Task 3: hub 链式编辑/删除 + Server 重下发接线

**Files:**
- Modify: `internal/server/hub.go:29-41`（`Hub` struct）、`:243-249`（readerLoop panel 分支之后）、`:478` 之后（`applyPanelEdits` 旁）
- Modify: `internal/server/server.go:100-104`（hub 接线）、`dispatchAfterFanout` 之后（加 `redispatchNodes`）
- Test: `internal/server/hub_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/server/hub_test.go` 末尾追加。`seedTwoHopChainDB` 镜像 db 测试的种子逻辑（hub 测试包独立，需自带）：

```go
func seedTwoHopChainDB(t *testing.T, d *sql.DB) (*db.Chain, int64, int64) {
	t.Helper()
	n0, _ := db.CreateNode(d, "edge-0", "https://p0", "tok0")
	n1, _ := db.CreateNode(d, "edge-1", "https://p1", "tok1")
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.10", n0.ID)
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.11", n1.ID)
	c := &db.Chain{Name: "wire", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	id, err := db.CreateChain(d, c)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id
	tx, _ := d.Begin()
	if _, _, err := db.RegenerateChain(tx, c, []db.HopInput{{NodeID: n0.ID}, {NodeID: n1.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	return c, n0.ID, n1.ID
}

func TestHubApplyChainHopEditSyncsUpstreamAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, n0, n1 := seedTwoHopChainDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	entry, err := hub.applyChainHopEdit(n1, c.ID, 21222, "kernel", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if entry == "" {
		t.Fatal("expected entry endpoint returned")
	}
	fwds, _ := db.ListForwardsByChain(hub.DB, c.ID)
	byNode := map[int64]*db.Forward{}
	for _, f := range fwds {
		byNode[f.NodeID] = f
	}
	if byNode[n1].ListenPort != 21222 {
		t.Fatalf("hop n1 listen_port = %d, want 21222", byNode[n1].ListenPort)
	}
	if byNode[n0].TargetPort != 21222 {
		t.Fatalf("upstream n0 target_port = %d, want 21222", byNode[n0].TargetPort)
	}
	if len(got) == 0 {
		t.Fatal("Redispatch was not called")
	}
}

func TestHubApplyChainHopEditRejectsForeignNode(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, _, _ := seedTwoHopChainDB(t, hub.DB)
	other, _ := db.CreateNode(hub.DB, "outsider", "https://x", "tokx")
	if _, err := hub.applyChainHopEdit(other.ID, c.ID, 21000, "kernel", ""); err == nil {
		t.Fatal("node not on chain must be rejected")
	}
}

func TestHubApplyChainDeleteRemovesChainAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, n0, _ := seedTwoHopChainDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	if err := hub.applyChainDelete(n0, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetChain(hub.DB, c.ID); err == nil {
		t.Fatal("chain row should be gone")
	}
	fwds, _ := db.ListForwardsByChain(hub.DB, c.ID)
	if len(fwds) != 0 {
		t.Fatalf("chain forwards should be gone, got %d", len(fwds))
	}
	if len(got) == 0 {
		t.Fatal("Redispatch was not called")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestHubApplyChain' -v`
Expected: 编译失败 —— `hub.Redispatch undefined` / `hub.applyChainHopEdit undefined`

- [ ] **Step 3: `Hub` 加 Redispatch**

`internal/server/hub.go` 的 `Hub` struct，在 `OnTrafficUpdate` 之后加：

```go
	OnTrafficUpdate func(tenantID int64)

	// Redispatch re-pushes kernel state to a set of nodes after the hub
	// mutates chain state on their behalf. Like OnTrafficUpdate it keeps the
	// hub transport-only: the hub knows which nodes a chain edit touched but
	// delegates the actual dispatch to the owner that wires this, so the
	// dispatch path is never imported here.
	Redispatch func(nodeIDs []int64)
```

- [ ] **Step 4: readerLoop 加链式分支**

`readerLoop` 的 switch，在 `case wsproto.TypePanelSegmentEdit:` 块之后加：

```go
		case wsproto.TypeChainHopEdit:
			var e wsproto.ChainHopEdit
			if err := json.Unmarshal(env.Payload, &e); err != nil {
				sendChainAckErr(ac, env.ID, "malformed payload")
				continue
			}
			entry, cerr := h.applyChainHopEdit(ac.nodeID, e.ChainID, e.ListenPort, e.Mode, e.Comment)
			ack := wsproto.ChainCmdAck{OK: cerr == nil, Entry: entry}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeChainDelete:
			var dl wsproto.ChainDelete
			if err := json.Unmarshal(env.Payload, &dl); err != nil {
				sendChainAckErr(ac, env.ID, "malformed payload")
				continue
			}
			cerr := h.applyChainDelete(ac.nodeID, dl.ChainID)
			ack := wsproto.ChainCmdAck{OK: cerr == nil}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: env.ID, Payload: ackP})
```

- [ ] **Step 5: 实现 applyChainHopEdit/applyChainDelete/sendChainAckErr**

在 `applyPanelEdits` 之后加（`internal/server/hub.go`）：

```go
// applyChainHopEdit folds a node-reported edit to its hop in chainID back
// into the chain skeleton and re-dispatches every node the regeneration
// touched, returning the chain's copyable entry endpoint. The hop is located
// by (chainID, nodeID): a chain can't repeat a node, so that pair is unique.
// Only listen_port/mode/comment are editable — target/proto stay owned by
// chain orchestration, which is why RegenerateChain recomputes targets and
// uses chain.proto. A node may only edit a chain it actually participates in.
func (h *Hub) applyChainHopEdit(nodeID, chainID int64, listenPort int, mode, comment string) (string, error) {
	c, err := db.GetChain(h.DB, chainID)
	if err != nil {
		return "", fmt.Errorf("链路不存在")
	}
	hops, err := db.ListChainHops(h.DB, chainID)
	if err != nil {
		return "", err
	}
	found := false
	inputs := make([]db.HopInput, len(hops))
	for i, hp := range hops {
		in := db.HopInput{NodeID: hp.NodeID, TunnelID: hp.TunnelID, Mode: hp.Mode}
		if hp.NodeID == nodeID {
			found = true
			in.DesiredPort = listenPort
			in.Mode = db.NormalizeForwardMode(mode)
			in.Comment = comment
		}
		inputs[i] = in
	}
	if !found {
		return "", fmt.Errorf("节点不在该链路上")
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return "", err
	}
	entry, affected, err := db.RegenerateChain(tx, c, inputs, nil)
	if err != nil {
		tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if h.Redispatch != nil {
		h.Redispatch(affected)
	}
	return entry, nil
}

// applyChainDelete removes the whole chain (all hops on all nodes) on behalf
// of a node that participates in it, then re-dispatches every node that ran
// its forwards so the deleted rules leave the kernel.
func (h *Hub) applyChainDelete(nodeID, chainID int64) error {
	hops, err := db.ListChainHops(h.DB, chainID)
	if err != nil {
		return err
	}
	onChain := false
	for _, hp := range hops {
		if hp.NodeID == nodeID {
			onChain = true
			break
		}
	}
	if !onChain {
		return fmt.Errorf("节点不在该链路上")
	}
	nodes, err := db.DeleteChain(h.DB, chainID)
	if err != nil {
		return err
	}
	if h.Redispatch != nil {
		h.Redispatch(nodes)
	}
	return nil
}

func sendChainAckErr(ac *agentConn, id, msg string) {
	ackP, _ := json.Marshal(wsproto.ChainCmdAck{OK: false, Error: msg})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: id, Payload: ackP})
}
```

确认 `internal/server/hub.go` import 块含 `"fmt"`（若缺则加）。

- [ ] **Step 6: Server 接线 redispatchNodes**

`internal/server/server.go` 的 `NewServer`，在 `hub.OnTrafficUpdate = s.enforceTenantQuota` 之后加：

```go
	hub.OnTrafficUpdate = s.enforceTenantQuota
	hub.Redispatch = s.redispatchNodes
```

在 `dispatchAfterFanout` 之后加：

```go
// redispatchNodes re-pushes kernel state to every node a background (WS-driven)
// chain mutation touched. It's the no-ResponseWriter sibling of
// dispatchAfterFanout: per-node failures are logged and land in last_error,
// but there's no flash cookie to aggregate into.
func (s *Server) redispatchNodes(nodeIDs []int64) {
	for _, n := range nodeIDs {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("dispatch node %d (链式变更): %v", n, err)
		}
	}
}
```

- [ ] **Step 7: 跑测试 + 编译 + 提交**

```bash
go test ./internal/server/ -run 'TestHubApplyChain' -v
go build ./...
gofmt -w internal/server/hub.go internal/server/server.go
go test ./internal/server/
git add internal/server/hub.go internal/server/server.go internal/server/hub_test.go
git commit -m "feat(server): apply node-reported chain hop edits and chain deletes"
```

---

## Task 4: dialer 同步命令帧 + 回执配对

**Files:**
- Modify: `internal/daemon/dialer.go:50-62`（`Dialer` struct）、`:64-72`（`NewDialer`）、`:128` 之后（加方法）、`:178`（runOnce defer）、`:255`（serve loop 进入处）、`:263-282`（readCh switch）、`:298-312`（write case 旁）
- Test: `internal/daemon/dialer_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/daemon/dialer_test.go` 末尾追加：

```go
func TestDialerEditChainHopRoundtripsAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	// Echo a successful ChainCmdAck back, matched by Envelope.ID.
	fh.onAck(wsproto.TypeChainHopEdit, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.ChainCmdAck{OK: true, Entry: "10.0.0.10:21000"})
		return wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	// Wait for the connection to come up before sending the command.
	deadline := time.After(2 * time.Second)
	for !dl.connected.Load() {
		select {
		case <-deadline:
			t.Fatal("dialer never connected")
		case <-time.After(10 * time.Millisecond):
		}
	}
	ack, err := dl.EditChainHop(ctx, wsproto.ChainHopEdit{ChainID: 5, ListenPort: 21000})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Entry != "10.0.0.10:21000" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func TestDialerSendCommandFailsWhenDisconnected(t *testing.T) {
	dl := NewDialer(DialerConfig{URL: "ws://127.0.0.1:1/"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dl.DeleteChain(ctx, 9); err == nil {
		t.Fatal("expected error when not connected")
	}
}
```

> `fakeHub.onAck` 须支持对 `TypeChainHopEdit` 注册回包，且回包帧带回请求的 `Envelope.ID`。若现有 `onAck`/`handler` 只在 hello 阶段回 ack、进入读循环后不回包，则扩展 `fakeHub`：进入帧循环后，对收到的每个帧查 `onAck` 注册表，命中则写回其返回的 envelope。这是 fake 的能力补全，不引入过程信息注释。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run 'TestDialerEditChainHop|TestDialerSendCommandFails' -v`
Expected: 编译失败 —— `dl.connected undefined` / `dl.EditChainHop undefined`

- [ ] **Step 3: `Dialer` struct 加命令通道/配对状态**

`internal/daemon/dialer.go` 的 `Dialer` struct，在 `panelCh`/`pendingPanel` 之后加：

```go
	panelCh      chan []nft.Rule
	pendingPanel atomic.Pointer[[]nft.Rule]

	cmdCh     chan wsproto.Envelope
	pendMu    sync.Mutex
	pending   map[string]chan wsproto.ChainCmdAck
	idSeq     atomic.Uint64
	connected atomic.Bool
```

- [ ] **Step 4: `NewDialer` 初始化**

```go
	return &Dialer{
		cfg:     cfg,
		tuiCh:   make(chan []nft.Rule, 1),
		panelCh: make(chan []nft.Rule, 1),
		cmdCh:   make(chan wsproto.Envelope),
		pending: make(map[string]chan wsproto.ChainCmdAck),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
```

- [ ] **Step 5: 加 EditChainHop/DeleteChain/sendCommand**

在 `NotifyPanelEdited` 之后加：

```go
// EditChainHop sends a chain_hop_edit to the server and blocks for the ack.
// The chain hop's port/mode/comment edit is authoritative server-side; this
// returns the server's verdict (ack.OK / ack.Error) so the TUI can show a
// precise success or failure (e.g. "端口被占用") instead of failing silently.
func (d *Dialer) EditChainHop(ctx context.Context, e wsproto.ChainHopEdit) (wsproto.ChainCmdAck, error) {
	p, err := json.Marshal(e)
	if err != nil {
		return wsproto.ChainCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeChainHopEdit, p)
}

// DeleteChain sends a chain_delete to the server and blocks for the ack.
func (d *Dialer) DeleteChain(ctx context.Context, chainID int64) (wsproto.ChainCmdAck, error) {
	p, err := json.Marshal(wsproto.ChainDelete{ChainID: chainID})
	if err != nil {
		return wsproto.ChainCmdAck{}, err
	}
	return d.sendCommand(ctx, wsproto.TypeChainDelete, p)
}

// sendCommand writes a command frame tagged with a fresh request ID and waits
// for the matching ChainCmdAck (correlated by Envelope.ID) or ctx expiry. It
// fails fast when no session is up: with no serve loop draining cmdCh the send
// would otherwise block until the caller's timeout. A disconnect mid-wait is
// surfaced by runOnce, which drains pending with a connection-lost ack.
func (d *Dialer) sendCommand(ctx context.Context, frameType string, payload json.RawMessage) (wsproto.ChainCmdAck, error) {
	if !d.connected.Load() {
		return wsproto.ChainCmdAck{}, errors.New("daemon 未连接面板")
	}
	id := "cmd-" + strconv.FormatUint(d.idSeq.Add(1), 36)
	resCh := make(chan wsproto.ChainCmdAck, 1)
	d.pendMu.Lock()
	d.pending[id] = resCh
	d.pendMu.Unlock()
	defer func() {
		d.pendMu.Lock()
		delete(d.pending, id)
		d.pendMu.Unlock()
	}()

	select {
	case d.cmdCh <- wsproto.Envelope{Type: frameType, ID: id, Payload: payload}:
	case <-ctx.Done():
		return wsproto.ChainCmdAck{}, ctx.Err()
	case <-d.stop:
		return wsproto.ChainCmdAck{}, errors.New("daemon 停止中")
	}
	select {
	case ack := <-resCh:
		return ack, nil
	case <-ctx.Done():
		return wsproto.ChainCmdAck{}, ctx.Err()
	}
}
```

- [ ] **Step 6: serve loop 标记连接 + 断开清理**

在 `runOnce` 进入 serve loop 之前（`readCh := make(...)` 之前）加：

```go
	d.connected.Store(true)
	defer func() {
		d.connected.Store(false)
		// Wake any in-flight command waiters so they don't hang until their
		// own ctx times out; a fresher session can't deliver an ack tagged
		// with an ID minted on this dead connection.
		d.pendMu.Lock()
		for id, ch := range d.pending {
			select {
			case ch <- wsproto.ChainCmdAck{Error: "与面板的连接已断开"}:
			default:
			}
			delete(d.pending, id)
		}
		d.pendMu.Unlock()
	}()
```

- [ ] **Step 7: readCh 加 ChainCmdAck 分支**

`runOnce` serve loop 的 `case env := <-readCh:` switch，在 `case wsproto.TypeError:` 之后加：

```go
			case wsproto.TypeChainCmdAck:
				var ack wsproto.ChainCmdAck
				_ = json.Unmarshal(env.Payload, &ack)
				d.pendMu.Lock()
				if ch, ok := d.pending[env.ID]; ok {
					select {
					case ch <- ack:
					default:
					}
				}
				d.pendMu.Unlock()
```

- [ ] **Step 8: serve loop 加 cmdCh 发送 case**

在 `case rules := <-d.panelCh:` 块之后加：

```go
		case env := <-d.cmdCh:
			if err := writeOne(ctx, ws, env); err != nil {
				return helloAcked, err
			}
```

- [ ] **Step 9: 跑测试 + gofmt + 提交**

```bash
go test ./internal/daemon/ -run 'TestDialerEditChainHop|TestDialerSendCommandFails' -v
gofmt -w internal/daemon/dialer.go
go test ./internal/daemon/
git add internal/daemon/dialer.go internal/daemon/dialer_test.go
git commit -m "feat(daemon): send chain commands and await server ack over the dialer"
```

---

## Task 5: daemon socket 路由 + daemonclient 方法

**Files:**
- Modify: `internal/daemon/handlers.go:108-115`（`Handler` 路由）、`:171` 之后（加 handler）
- Modify: `internal/daemonclient/client.go:185` 之后（加方法）
- Test: `internal/daemonclient/client_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/daemonclient/client_test.go` 末尾追加。`newTestClient` 是该文件已用的 helper（起一个 httptest 服务并返回指向它的 Client）；若名称不同，沿用文件内既有的等价 helper：

```go
func TestChainEditPostsAndSurfacesServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chain/edit", func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			ChainID    int64  `json:"chain_id"`
			ListenPort int    `json:"listen_port"`
			Mode       string `json:"mode"`
			Comment    string `json:"comment"`
		}
		json.NewDecoder(r.Body).Decode(&got)
		if got.ChainID != 5 || got.ListenPort != 21000 {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		http.Error(w, "端口被占用", http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := New(srv.URL)

	err := c.ChainEdit(5, 21000, "kernel", "x")
	if err == nil || !strings.Contains(err.Error(), "端口被占用") {
		t.Fatalf("expected server error surfaced verbatim, got %v", err)
	}
}

func TestChainDeletePostsChainID(t *testing.T) {
	var gotID int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chain/delete", func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			ChainID int64 `json:"chain_id"`
		}
		json.NewDecoder(r.Body).Decode(&got)
		gotID = got.ChainID
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := New(srv.URL)

	if err := c.ChainDelete(9); err != nil {
		t.Fatal(err)
	}
	if gotID != 9 {
		t.Fatalf("server got chain_id %d, want 9", gotID)
	}
}
```

确认 `internal/daemonclient/client_test.go` import 含 `"net/http"`、`"net/http/httptest"`、`"strings"`、`"encoding/json"`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemonclient/ -run 'TestChain' -v`
Expected: 编译失败 —— `c.ChainEdit undefined` / `c.ChainDelete undefined`

- [ ] **Step 3: daemonclient 加方法**

在 `internal/daemonclient/client.go` 的 `PostRuleset` 之后加：

```go
// ChainEdit asks the daemon to apply an edit (listen_port/mode/comment) to
// this node's hop in a chain. The daemon relays it to the server and blocks
// for the verdict; a non-2xx body is the server's rejection reason (e.g.
// "端口被占用") surfaced verbatim so the TUI can show it.
func (c *Client) ChainEdit(chainID int64, listenPort int, mode, comment string) error {
	body, err := json.Marshal(struct {
		ChainID    int64  `json:"chain_id"`
		ListenPort int    `json:"listen_port"`
		Mode       string `json:"mode"`
		Comment    string `json:"comment"`
	}{chainID, listenPort, mode, comment})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/chain/edit", body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("%s", strings.TrimSpace(string(buf)))
	}
	return nil
}

// ChainDelete asks the daemon to delete the entire chain this node
// participates in. The daemon relays it to the server and blocks for the
// verdict.
func (c *Client) ChainDelete(chainID int64) error {
	body, err := json.Marshal(struct {
		ChainID int64 `json:"chain_id"`
	}{chainID})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/chain/delete", body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("%s", strings.TrimSpace(string(buf)))
	}
	return nil
}
```

- [ ] **Step 4: daemon 加路由 + handler**

`internal/daemon/handlers.go` 的 `Handler()`，在 `mux.HandleFunc("/v1/admin/demote-to-tui", ...)` 之后加：

```go
	mux.HandleFunc("/v1/chain/edit", d.handleChainEdit)
	mux.HandleFunc("/v1/chain/delete", d.handleChainDelete)
```

在 `handleRulesetOwner` 之后加：

```go
// handleChainEdit relays a TUI edit of a chain hop to the server through the
// dialer and blocks for the server's verdict. Chain edits are authoritative
// server-side (the relay skeleton spans nodes), so unlike owner-segment
// writes nothing is applied locally; the result returns synchronously so the
// TUI can show success or the server's rejection reason.
func (d *Daemon) handleChainEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ChainID    int64  `json:"chain_id"`
		ListenPort int    `json:"listen_port"`
		Mode       string `json:"mode"`
		Comment    string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dl := d.Dialer()
	if dl == nil {
		http.Error(w, "daemon 未连接面板，无法编辑链路", http.StatusServiceUnavailable)
		return
	}
	ack, err := dl.EditChainHop(r.Context(), wsproto.ChainHopEdit{
		ChainID: req.ChainID, ListenPort: req.ListenPort, Mode: req.Mode, Comment: req.Comment,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"entry": ack.Entry})
}

// handleChainDelete relays a TUI delete of an entire chain to the server.
func (d *Daemon) handleChainDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ChainID int64 `json:"chain_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dl := d.Dialer()
	if dl == nil {
		http.Error(w, "daemon 未连接面板，无法删除链路", http.StatusServiceUnavailable)
		return
	}
	ack, err := dl.DeleteChain(r.Context(), req.ChainID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
```

确认 `internal/daemon/handlers.go` import 含 `"nft-forward/internal/wsproto"`（若缺则加）。

- [ ] **Step 5: 跑测试 + 编译 + 提交**

```bash
go test ./internal/daemonclient/ -run 'TestChain' -v
go build ./...
gofmt -w internal/daemon/handlers.go internal/daemonclient/client.go
go test ./internal/daemon/ ./internal/daemonclient/
git add internal/daemon/handlers.go internal/daemonclient/client.go internal/daemonclient/client_test.go
git commit -m "feat(daemon): expose chain edit/delete over the unix socket"
```

---

## Task 6: TUI 链式行受限编辑 + 删除整链

> 链式行从"完全只读"放开为"受限可编辑"。字段锁定矩阵：tui 行无锁；panel 非链式锁 proto+本机端口；panel 链式锁 proto+目标，放开本机端口+模式+备注。链式编辑/删除走命令（`ChainEdit`/`ChainDelete`），不走快照、不乐观改本地。

**Files:**
- Modify: `internal/tui/tui.go:23-26`（接口）、`:74-86`（model）、`:144-155`（rowAt）、`:227-233`（updateList d）、`:256-293`（enterEditMode）、`:337-384`（updateEdit）、`:386-466`（submitEdit）、`:530-558`（updateConfirmDelete）、`:628-630`（View confirmDelete）、`:856-858`（viewForm 锁定提示）
- Test: `internal/tui/tui_test.go`

### 循环 A — daemonClient 接口 + fake + 字段锁定/editingChainID

- [ ] **Step 1: 写失败测试**

在 `internal/tui/tui_test.go` 末尾追加：

```go
func TestLockedFieldsByRowType(t *testing.T) {
	// tui row: nothing locked.
	m := model{editingOwner: "tui"}
	if len(m.lockedFields()) != 0 {
		t.Fatalf("tui edit should lock nothing, got %v", m.lockedFields())
	}
	// panel non-chain: proto + listen port locked.
	m = model{editingOwner: "panel", editingChainID: 0}
	lf := m.lockedFields()
	if !lf[fProto] || !lf[fSrcPort] || lf[fDestIP] {
		t.Fatalf("panel non-chain locks wrong fields: %v", lf)
	}
	// panel chain: proto + target locked, listen port free.
	m = model{editingOwner: "panel", editingChainID: 7}
	lf = m.lockedFields()
	if !lf[fProto] || !lf[fDestIP] || !lf[fDestPort] || lf[fSrcPort] {
		t.Fatalf("panel chain locks wrong fields: %v", lf)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestLockedFieldsByRowType' -v`
Expected: 编译失败 —— `m.editingChainID undefined` / `m.lockedFields undefined`

- [ ] **Step 3: 接口加方法**

`internal/tui/tui.go` 的 `daemonClient` 接口（`:23-26`）：

```go
type daemonClient interface {
	GetRuleset() (daemonclient.OwnerRuleset, error)
	PostRuleset(owner string, rules []nft.Rule) error
	ChainEdit(chainID int64, listenPort int, mode, comment string) error
	ChainDelete(chainID int64) error
}
```

- [ ] **Step 4: fakeDaemonClient 加方法**

在 `internal/tui/tui_test.go` 的 `fakeDaemonClient` 定义处加字段与方法（保持其他字段不动）：

```go
	chainEdits   []struct {
		ChainID    int64
		ListenPort int
		Mode       string
		Comment    string
	}
	chainDeletes []int64
	chainErr     error
```

```go
func (f *fakeDaemonClient) ChainEdit(chainID int64, listenPort int, mode, comment string) error {
	f.chainEdits = append(f.chainEdits, struct {
		ChainID    int64
		ListenPort int
		Mode       string
		Comment    string
	}{chainID, listenPort, mode, comment})
	return f.chainErr
}

func (f *fakeDaemonClient) ChainDelete(chainID int64) error {
	f.chainDeletes = append(f.chainDeletes, chainID)
	return f.chainErr
}
```

- [ ] **Step 5: model 加 editingChainID + lockedFields**

`model` struct 加字段（在 `editingOwner` 旁）：

```go
	editingOwner string
	// editingChainID is the chain a panel chain-hop edit targets (0 = the
	// row is not a chain hop). It routes submitEdit to the chain command path
	// and selects the field-lock set.
	editingChainID int64
```

在 `rowAt` 之后加：

```go
// lockedFields returns the form field indices that stay read-only for the
// row being edited. tui rows lock nothing. panel non-chain rows lock
// proto+listen_port (their server-side reconcile key). panel chain rows lock
// proto+target (the relay skeleton owned by the server) but free
// listen_port/mode/comment.
func (m model) lockedFields() map[int]bool {
	if m.editingOwner != "panel" {
		return nil
	}
	if m.editingChainID != 0 {
		return map[int]bool{fProto: true, fDestIP: true, fDestPort: true}
	}
	return map[int]bool{fProto: true, fSrcPort: true}
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestLockedFieldsByRowType' -v`
Expected: PASS

### 循环 B — rowAt 放开链式 + enterEditMode 设 editingChainID

- [ ] **Step 7: 写失败测试**

```go
func TestEnterEditModeChainRowEntersWithChainID(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	if m.mode != viewEdit {
		t.Fatal("chain hop should now be editable (enter edit mode)")
	}
	if m.editingOwner != "panel" || m.editingChainID != 7 {
		t.Fatalf("editingOwner=%q editingChainID=%d, want panel/7", m.editingOwner, m.editingChainID)
	}
	// listen port prefilled and editable.
	if m.inputs[fSrcPort].Value() != "21000" {
		t.Fatalf("listen port not prefilled: %q", m.inputs[fSrcPort].Value())
	}
}
```

- [ ] **Step 8: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestEnterEditModeChainRowEntersWithChainID' -v`
Expected: FAIL —— 旧 `rowAt` 对链式行返回 editable=false，`enterEditMode` 提前 return，mode 仍为 viewList

- [ ] **Step 9: rowAt 放开链式行**

`rowAt`（`:144-155`）替换为：

```go
// rowAt resolves a unified cursor index to its rule and owner. Indices
// [0,len(rules)) map to the tui segment; the remainder map to the panel
// segment. editable is now always true: chain hops are editable for their
// safe fields (listen_port/mode/comment) — which fields are locked is decided
// per row by lockedFields, not here.
func (m model) rowAt(i int) (r nft.Rule, owner string, editable bool) {
	if i < len(m.rules) {
		return m.rules[i], "tui", true
	}
	return m.panelRules[i-len(m.rules)], "panel", true
}
```

- [ ] **Step 10: enterEditMode 设 editingChainID、去掉只读拒绝**

`enterEditMode`（`:256-293`）开头替换（保留其后的预填逻辑不变）：

```go
func (m *model) enterEditMode() {
	r, owner, _ := m.rowAt(m.cursor)
	m.editingOwner = owner
	m.editingChainID = r.ChainID
	m.mode = viewEdit
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto
```

（其余 `m.protoIdx`…`m.inputs[fComment].SetValue(r.Comment)` 保持原样。）

并在 `enterAddMode`（`:245-254`）中补一行，保证新增行不残留上次的 chain 标记：

```go
	m.editingOwner = "tui" // new rules always belong to the tui segment
	m.editingChainID = 0
```

- [ ] **Step 11: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestEnterEditModeChainRowEntersWithChainID' -v`
Expected: PASS

### 循环 C — updateEdit/viewForm 用 lockedFields

- [ ] **Step 12: 写失败测试**

```go
func TestUpdateEditChainRowLocksTargetNotListenPort(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	// Focus the target IP (locked for chain rows): typing must be ignored.
	m.focusedInput = fDestIP
	before := m.inputs[fDestIP].Value()
	nm, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fDestIP].Value() != before {
		t.Fatal("target IP must be read-only for chain rows")
	}
	// Focus the listen port (free for chain rows): typing must be accepted.
	m.focusedInput = fSrcPort
	nm, _ = m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fSrcPort].Value() == "21000" {
		t.Fatal("listen port must be editable for chain rows")
	}
}
```

- [ ] **Step 13: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestUpdateEditChainRowLocksTargetNotListenPort' -v`
Expected: FAIL —— 旧逻辑只对 `editingOwner=="panel"` 锁 proto+srcPort，链式行的 srcPort 被错误锁住、target 未锁

- [ ] **Step 14: updateEdit 改用 lockedFields**

`updateEdit`（`:354-360`）把这段：

```go
	if m.editingOwner == "panel" && (m.focusedInput == fProto || m.focusedInput == fSrcPort) {
		return m, nil
	}
```

替换为：

```go
	// Locked fields swallow input. The lock set differs by row type: panel
	// non-chain pins proto+listen_port (its reconcile key); panel chain pins
	// proto+target (the relay skeleton).
	if m.lockedFields()[m.focusedInput] {
		return m, nil
	}
```

`viewForm`（`:856-858`）把：

```go
		if m.editingOwner == "panel" && (i == fProto || i == fSrcPort) {
			fieldView += helpStyle.Render("  (server 固定)")
		}
```

替换为：

```go
		if m.lockedFields()[i] {
			fieldView += helpStyle.Render("  (固定)")
		}
```

- [ ] **Step 15: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestUpdateEditChainRowLocksTargetNotListenPort' -v`
Expected: PASS

### 循环 D — submitEdit 链式走 ChainEdit

- [ ] **Step 16: 写失败测试**

```go
func TestSubmitEditChainRowSendsChainEdit(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless", Comment: "old"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fSrcPort].SetValue("21555")
	m.inputs[fComment].SetValue("new note")
	m.modeIdx = 1 // userspace
	nm, _ := m.submitEdit()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(fc.chainEdits) != 1 {
		t.Fatalf("expected one ChainEdit call, got %d", len(fc.chainEdits))
	}
	e := fc.chainEdits[0]
	if e.ChainID != 7 || e.ListenPort != 21555 || e.Mode != nft.ModeUserspace || e.Comment != "new note" {
		t.Fatalf("ChainEdit args wrong: %+v", e)
	}
	// Local panel row must NOT be optimistically mutated — server is authority.
	if mm.panelRules[0].SrcPort != 21000 {
		t.Fatalf("chain row must not be mutated locally, got SrcPort=%d", mm.panelRules[0].SrcPort)
	}
	if mm.mode != viewList {
		t.Fatal("should return to list view after submit")
	}
}

func TestSubmitEditChainRowSurfacesServerError(t *testing.T) {
	fc := &fakeDaemonClient{chainErr: fmt.Errorf("端口被占用")}
	m := initialModel(fc, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fSrcPort].SetValue("80")
	nm, _ := m.submitEdit()
	if !strings.Contains(nm.(model).err, "端口被占用") {
		t.Fatalf("server error not surfaced: %q", nm.(model).err)
	}
}
```

- [ ] **Step 17: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestSubmitEditChainRow' -v`
Expected: FAIL —— 链式行当前走非链式 panel 快照路径（`commitOwner("panel")`），不会调 `ChainEdit`

- [ ] **Step 18: submitEdit 加链式分支**

`submitEdit`（`:400` 处，`owner := m.editingOwner` 之后、`var seg []nft.Rule` 之前）插入链式命令分支：

```go
	owner := m.editingOwner

	// Chain hops are server-authoritative: only listen_port/mode/comment are
	// editable (proto/target are the locked relay skeleton). Send a command
	// and let the server re-dispatch — don't optimistically mutate the local
	// row, since the real result (including upstream changes on other nodes)
	// arrives via the next push.
	if owner == "panel" && m.editingChainID != 0 {
		if err := m.client.ChainEdit(m.editingChainID, srcPort, modeOptions[m.modeIdx], comment); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.mode = viewList
		m.status = fmt.Sprintf("已提交链路端口/模式变更（监听 %d），按 r 刷新查看", srcPort)
		m.err = ""
		return m, nil
	}

	var seg []nft.Rule
```

- [ ] **Step 19: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestSubmitEditChainRow' -v`
Expected: PASS

### 循环 E — 删除整链：updateList + updateConfirmDelete + View

- [ ] **Step 20: 写失败测试**

```go
func TestDeleteChainRowConfirmsThenSendsChainDelete(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	// d on a chain row enters confirm (unlike a plain server row).
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(model)
	if m.mode != viewConfirmDelete {
		t.Fatal("d on a chain row should enter confirm-delete")
	}
	// y confirms -> ChainDelete(7).
	nm, _ = m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	mm := nm.(model)
	if len(fc.chainDeletes) != 1 || fc.chainDeletes[0] != 7 {
		t.Fatalf("expected ChainDelete(7), got %v", fc.chainDeletes)
	}
	if mm.mode != viewList {
		t.Fatal("should return to list after delete")
	}
}

func TestDeleteNonChainServerRowStillRejected(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}, // server non-chain
	})
	m.cursor = 0
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	mm := nm.(model)
	if mm.mode == viewConfirmDelete {
		t.Fatal("non-chain server row must not be deletable here")
	}
	if !strings.Contains(mm.status, "托管") {
		t.Fatalf("expected rejection status, got %q", mm.status)
	}
}
```

- [ ] **Step 21: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestDelete(Chain|NonChain)' -v`
Expected: FAIL —— `d` 对所有 panel 行都拒绝；`updateConfirmDelete` 对 cursor≥len(rules) 直接回 list

- [ ] **Step 22: updateList d 分支放开链式行**

`updateList` 的 `case "d", "delete":`（`:227-233`）替换为：

```go
	case "d", "delete":
		if m.totalRows() == 0 {
			return m, nil
		}
		r, owner, _ := m.rowAt(m.cursor)
		if owner == "tui" || r.ChainID != 0 {
			// tui rows delete locally; chain rows delete the whole chain via
			// the server. Non-chain server rows aren't deletable from here.
			m.mode = viewConfirmDelete
			m.err = ""
		} else {
			m.status = "server 托管规则不能在此删除"
		}
```

- [ ] **Step 23: updateConfirmDelete y 分支区分链式**

`updateConfirmDelete` 的 `case "y", "Y":`（`:532-551`）替换为：

```go
	case "y", "Y":
		r, owner, _ := m.rowAt(m.cursor)
		if owner == "panel" && r.ChainID != 0 {
			if err := m.client.ChainDelete(r.ChainID); err != nil {
				m.err = err.Error()
				m.mode = viewList
				return m, nil
			}
			m.status = fmt.Sprintf("已提交删除链路「%s」，按 r 刷新查看", r.ChainName)
			m.mode = viewList
			return m, nil
		}
		if owner != "tui" {
			m.mode = viewList
			return m, nil
		}
		removed := m.rules[m.cursor]
		next := append([]nft.Rule{}, m.rules[:m.cursor]...)
		next = append(next, m.rules[m.cursor+1:]...)
		applied, err := commit(m.client, next)
		if err != nil {
			m.err = err.Error()
			m.mode = viewList
			return m, nil
		}
		m.rules = applied
		if m.cursor >= m.totalRows() && m.cursor > 0 {
			m.cursor--
		}
		m.status = fmt.Sprintf("已删除 %s/%d", removed.Proto, removed.SrcPort)
		m.mode = viewList
```

- [ ] **Step 24: View confirmDelete 文案区分链式**

`View()` 的 `case viewConfirmDelete:`（`:628-630`）替换为：

```go
	case viewConfirmDelete:
		if r, owner, _ := m.rowAt(m.cursor); owner == "panel" && r.ChainID != 0 {
			inner = m.viewConfirm(fmt.Sprintf(
				"确认删除整条链路「%s」？\n\n  这会删除该链路在所有节点上的全部转发，不可恢复。\n", r.ChainName))
		} else {
			inner = m.viewConfirm(
				fmt.Sprintf("确认删除该规则？\n\n  %s\n", m.rules[m.cursor].Display()))
		}
```

- [ ] **Step 25: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestDelete(Chain|NonChain)' -v`
Expected: PASS

- [ ] **Step 26: gofmt + 全 TUI 包测试 + 提交**

```bash
gofmt -w internal/tui/tui.go internal/tui/tui_test.go
go test ./internal/tui/
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "feat(tui): edit and delete chain rules with field locks and confirm"
```

---

## 收尾：全量验证

- [ ] **Step 1: 全仓库测试 + vet + 构建**

Run: `go test ./... && go vet ./... && go build ./...`
Expected: 全部 PASS；vet 无告警；构建成功

- [ ] **Step 2: 手动迁移自检**

Run: 用一个干净的临时 DB 启动 server（或现有测试覆盖 `migrations.ReadDir`），确认 `0008_chain_hop_comment.sql` 被应用、`chain_hops.comment` 存在。
Expected: `go test ./internal/db/` 已覆盖（`ListChainHops` scan comment 不报错即证明列存在）

完成后按 superpowers:finishing-a-development-branch 处理分支合并到 `main`。

---

## 自检（Self-Review）

**Spec 覆盖**（对照 `docs/superpowers/specs/2026-06-04-tui-chain-edit-delete-design.md`）：
- 三个命令帧（ChainHopEdit/ChainDelete/ChainCmdAck）→ Task 1 ✓
- `chain_hops` 加 comment 列 → Task 2 Step 1 ✓
- `RegenerateChain` DesiredPort 采纳 + 越界拒绝 + 上游联动 → Task 2 ✓
- Comment 显式/保持/回退三态 → Task 2 ✓
- hub 凭 (chain_id, node) 定位、RegenerateChain、Redispatch → Task 3 ✓
- 协议层锁 target/proto（命令帧不带）→ Task 1（帧不含 target/proto）+ Task 3（用重算结果/chain.proto）✓
- 同步回执（dialer pending 配对 + 连接断清理）→ Task 4 ✓
- 失败 fail-fast（未连接）→ Task 4 Step 5 ✓
- daemon socket endpoint + daemonclient → Task 5 ✓
- TUI 字段锁定矩阵（链式锁 proto/target、放开 port/mode/comment）→ Task 6 循环 A/C ✓
- 链式编辑走命令、不乐观改本地 → Task 6 循环 D ✓
- 删除整链 + 二次确认 + 文案 → Task 6 循环 E ✓
- 入口跳端口允许改（无特殊禁止；server 重算同步 entry_listen_port）→ Task 2 RegenerateChain `:413` 既有逻辑 + 无 position 限制 ✓

**类型一致性自检：**
- `HopInput{NodeID, TunnelID, Mode, DesiredPort, Comment}` —— Task 2 定义 / Task 3 `applyChainHopEdit` 构造一致 ✓
- `ChainHop.Comment` —— Task 2 加字段 / `ListChainHops` scan / `prevHopComment` 读取一致 ✓
- `wsproto.ChainHopEdit{ChainID, ListenPort, Mode, Comment}` —— Task 1 定义 / Task 4 `EditChainHop` 编码 / Task 3 hub 解码一致 ✓
- `wsproto.ChainCmdAck{OK, Error, Entry}` —— Task 1 定义 / Task 3 hub 写 / Task 4 dialer 读一致 ✓
- `Hub.Redispatch func([]int64)` —— Task 3 字段 / Server `redispatchNodes` wire 签名一致 ✓
- `Dialer.EditChainHop(ctx, ChainHopEdit)(ChainCmdAck,error)` / `DeleteChain(ctx,int64)` —— Task 4 定义 / Task 5 daemon handler 调用一致 ✓
- `Client.ChainEdit(chainID, listenPort, mode, comment)` / `ChainDelete(chainID)` —— Task 5 定义 / Task 6 接口 + fake 一致 ✓
- `model.editingChainID int64` —— Task 6 循环 A 加字段 / `enterEditMode` 写 / `lockedFields`/`submitEdit` 读一致 ✓

**非目标（不做）：** 非链式 panel 行删除；并发竞态；协议/目标编辑；跨编辑入口（TUI↔webui）一致性。
