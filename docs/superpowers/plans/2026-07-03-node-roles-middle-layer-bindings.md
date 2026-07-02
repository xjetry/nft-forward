# 节点角色与中间层绑定（规则级联组链）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 spec `docs/superpowers/specs/2026-07-03-node-roles-middle-layer-bindings-design.md`：节点角色位掩码、中间层绑定边、规则级联组链、计费简化（倍率只看入口、专属配额按原始字节按逻辑段扣），并先落地 TUI 链式行编辑通道修复。

**Architecture:** 链在规则保存时由「入口段 ++ 各中间层段」拼接成 `[]db.HopInput` 再交给现有 `RegenerateRule` 物化，转发/下发层零改动。新增 `nodes.roles`、`node_bindings`、`rules.via_node_ids`、`rule_hops.via_node_id` 四处 schema；计费在 `hub.applyCounters` 内重写。

**Tech Stack:** Go + SQLite（手写迁移）+ chi；前端 React (vite) 位于 `web/`。

## Global Constraints

- **禁止过程污染**：代码注释与 commit message 只写 WHY 和不变量，绝不出现任务编号/方案代号/轮次等对话期信息（派发 subagent 时必须在 prompt 中传达本条）。
- nodes 表加列必须三处同步：`nodeCols`（`internal/db/queries.go:296`）、`scanNode`（同文件）、`ListNodesForUser` 的 inline scan（`internal/db/grants.go:98`）。漏一处会静默清空数据。
- 版本号严格 `vX.Y.Z` 三段；限速单位 MB/s（发版不在本计划范围内）。
- 出口段模式归规则（`exit_mode`→末跳）这一既有不变量在所有改动中保持。
- 测试命令统一 `go test ./internal/... -run <Name> -v`，收尾跑 `go test ./...`；前端 `cd web && npm run build` 验证可编译。
- Commit 风格沿用仓库惯例（`feat:`/`fix:`/`refactor:` + 简短英文描述）。

---

### Task 1: TUI 链式行编辑路由修复（daemon）

**Files:**
- Modify: `internal/daemon/handlers.go`（`handleUpdateRule` 数字 RuleID 分支，约 383-401 行）
- Test: `internal/daemon/handlers_test.go`

**Interfaces:**
- Consumes: `Dialer.EditRuleHop(ctx, wsproto.RuleHopEdit)`（`internal/daemon/dialer.go:174`，已存在）；`d.owners["panel"]` 中 `nft.Rule.RuleID/HopCount` 元数据。
- Produces: `func (d *Daemon) panelHopCount(ruleID int64) int`。

背景：hub 的 `handleRuleUpdate` 对多跳规则一律拒绝（`internal/server/hub.go:1045-1047`），TUI 链式行编辑经 `rule_update` 必然报错；专用 `rule_hop_edit` 通道两端俱全但无调用方。修复 = daemon 按跳数路由。

- [ ] **Step 1: 写失败测试（panelHopCount）**

在 `internal/daemon/handlers_test.go` 追加（该文件已在 package daemon，可直接访问未导出字段）：

```go
func TestPanelHopCount(t *testing.T) {
	d := &Daemon{owners: OwnerRuleset{
		"panel": {
			{ID: "a1", RuleID: 5, HopCount: 3},
			{ID: "a2", RuleID: 6, HopCount: 1},
		},
		"tui": {{ID: "b1"}},
	}}
	if got := d.panelHopCount(5); got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
	if got := d.panelHopCount(6); got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
	if got := d.panelHopCount(99); got != 0 {
		t.Fatalf("unknown rule want 0, got %d", got)
	}
}
```

注意：若 `Daemon` 字面量缺必填字段导致 panic，参考同文件 `newTestServer`（`handlers_test.go:45`）的构造方式改用它并通过 `d.mu`/`d.owners` 直接注入。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon -run TestPanelHopCount -v`
Expected: FAIL（`d.panelHopCount undefined`）

- [ ] **Step 3: 实现 panelHopCount + 路由分支**

`internal/daemon/handlers.go`，在 `handleUpdateRule` 附近加：

```go
// panelHopCount returns the HopCount metadata of the panel-segment rule with
// the given server RuleID, or 0 when the rule is not in local state. Chain
// rules (HopCount > 1) must be edited through the per-hop channel: the server
// rejects whole-rule updates on multi-hop rules.
func (d *Daemon) panelHopCount(ruleID int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.owners["panel"] {
		if r.RuleID == ruleID {
			return r.HopCount
		}
	}
	return 0
}
```

`handleUpdateRule` 数字分支（现约 384-401 行）改为：

```go
	// If the id is a numeric RuleID, route through the dialer to the server.
	if ruleID, ok := parseRuleID(id); ok {
		dl := d.Dialer()
		if dl == nil || !dl.IsConnected() {
			http.Error(w, "daemon not connected to server", http.StatusServiceUnavailable)
			return
		}
		var ack wsproto.RuleCmdAck
		var err error
		// Multi-hop chain rules only expose port/mode/comment to the node side;
		// the server owns the rest of the skeleton and rejects rule_update on
		// them, so those edits go through the per-hop channel instead.
		if d.panelHopCount(ruleID) > 1 {
			ack, err = dl.EditRuleHop(r.Context(), wsproto.RuleHopEdit{
				RuleID: ruleID, ListenPort: req.ListenPort, Mode: req.Mode, Comment: req.Comment,
			})
		} else {
			ack, err = dl.UpdateRule(r.Context(), updateToWSProto(ruleID, req))
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !ack.OK {
			http.Error(w, ack.Error, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"entry": ack.Entry})
		return
	}
```

- [ ] **Step 4: 写路由的线协议测试**

复用 `internal/daemon/dialer_test.go` 的 `newFakeHub`/`waitConnected` harness（`dialer_test.go:20-107`）。在 handlers_test.go 追加（handshake 响应写法参照 `TestDialerSendsHelloAndReceivesAck`，`dialer_test.go:109`）：

```go
func TestUpdateRuleRoutesChainRowsToRuleHopEdit(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	ok := func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	}
	fh.onAck(wsproto.TypeRuleHopEdit, ok)
	fh.onAck(wsproto.TypeRuleUpdate, ok)
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	d, _ := newTestServer(t, fakeDataplane())
	dl := NewDialer(DialerConfig{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/", Token: "tok", AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:  func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dl.runOnce(ctx)
	waitConnected(t, dl)
	d.mu.Lock()
	d.dialer = dl
	d.owners = OwnerRuleset{"panel": {
		{ID: "x1", RuleID: 5, HopCount: 3},
		{ID: "x2", RuleID: 6, HopCount: 1},
	}}
	d.mu.Unlock()

	put := func(id, body string) int {
		req := httptest.NewRequest(http.MethodPut, "/v1/rules/"+id, strings.NewReader(body))
		w := httptest.NewRecorder()
		d.handleRulesWithID(w, req)
		return w.Code
	}
	if code := put("5", `{"listen_port":12345,"mode":"userspace","comment":"c"}`); code != http.StatusOK {
		t.Fatalf("chain-row edit: want 200, got %d", code)
	}
	if code := put("6", `{"proto":"tcp","exit_host":"9.9.9.9","exit_port":443}`); code != http.StatusOK {
		t.Fatalf("single-hop edit: want 200, got %d", code)
	}
	var sawHopEdit, sawUpdate bool
	for _, f := range fh.Frames() {
		switch f.Type {
		case wsproto.TypeRuleHopEdit:
			sawHopEdit = true
			var e wsproto.RuleHopEdit
			_ = json.Unmarshal(f.Payload, &e)
			if e.RuleID != 5 || e.ListenPort != 12345 || e.Mode != "userspace" {
				t.Fatalf("rule_hop_edit payload wrong: %+v", e)
			}
		case wsproto.TypeRuleUpdate:
			sawUpdate = true
		}
	}
	if !sawHopEdit || !sawUpdate {
		t.Fatalf("want both channels used, hopEdit=%v update=%v", sawHopEdit, sawUpdate)
	}
}
```

注意：`d.dialer` 字段名与 `newTestServer`/`fakeDataplane` 的真实签名以 `daemon.go`/`handlers_test.go` 现状为准（同包测试可直接赋值未导出字段）；若 `Daemon` 持有 dialer 的字段名不同（grep `func (d *Daemon) Dialer()` 的返回体），按实际调整。

- [ ] **Step 5: 跑两个测试确认通过**

Run: `go test ./internal/daemon -run 'TestPanelHopCount|TestUpdateRuleRoutesChainRows' -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/daemon/handlers.go internal/daemon/handlers_test.go
git commit -m "fix(daemon): route chain-row rule edits through rule_hop_edit"
```

---

### Task 2: nodes.roles 位掩码（迁移 + 三处 scan）

**Files:**
- Create: `internal/db/migrations/0031_node_roles.sql`
- Modify: `internal/db/queries.go`（Node struct、`nodeCols`、`scanNode`）
- Modify: `internal/db/grants.go`（`ListNodesForUser` inline scan）
- Test: `internal/db/queries_test.go`

**Interfaces:**
- Produces: `db.NodeRoleEntry int64 = 1`、`db.NodeRoleVia int64 = 2`；`Node.Roles int64`（JSON `roles`）；`db.UpdateNodeRoles(d *sql.DB, id, roles int64) error`。

- [ ] **Step 1: 迁移文件**

`internal/db/migrations/0031_node_roles.sql`：

```sql
-- roles is a bitmask: bit0 = entry (rule-selectable entry node),
-- bit1 = via (middle-layer segment attachable behind an upstream node).
-- Every pre-existing node is an entry so current behavior is unchanged.
ALTER TABLE nodes ADD COLUMN roles INTEGER NOT NULL DEFAULT 1;
```

- [ ] **Step 2: 写失败测试**

`internal/db/queries_test.go` 追加：

```go
func TestNodeRolesRoundTrip(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "hk-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n.Roles != NodeRoleEntry {
		t.Fatalf("default roles want %d, got %d", NodeRoleEntry, n.Roles)
	}
	if err := UpdateNodeRoles(d, n.ID, NodeRoleEntry|NodeRoleVia); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.Roles != NodeRoleEntry|NodeRoleVia {
		t.Fatalf("roles want %d, got %d", NodeRoleEntry|NodeRoleVia, got.Roles)
	}
}
```

再给 `ListNodesForUser` 补 lockstep 回归（防静默清空）：

```go
func TestListNodesForUserCarriesRoles(t *testing.T) {
	d := openTestDB(t)
	n, _ := CreateNode(d, "hk-1", "", "")
	_ = UpdateNodeRoles(d, n.ID, NodeRoleVia)
	if err := GrantNode(d, 42, n.ID, 5, 0); err != nil {
		t.Fatal(err)
	}
	nodes, _, err := ListNodesForUser(d, 42)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("want 1 node, err=%v n=%d", err, len(nodes))
	}
	if nodes[0].Roles != NodeRoleVia {
		t.Fatalf("roles want %d, got %d", NodeRoleVia, nodes[0].Roles)
	}
}
```

Run: `go test ./internal/db -run 'TestNodeRoles|TestListNodesForUserCarriesRoles' -v` → FAIL（undefined）。

- [ ] **Step 3: 实现**

`internal/db/queries.go`：

1. Node struct（`Unidirectional` 之后、内存字段注释之前）加：

```go
	// Roles is a bitmask of what the node can be used as: NodeRoleEntry means
	// it can be picked as a rule's entry, NodeRoleVia means it can be attached
	// behind an upstream node as a middle-layer segment. A node may hold both.
	Roles int64 `json:"roles"`
```

2. 文件顶部（Node struct 前）加常量：

```go
const (
	NodeRoleEntry int64 = 1 << 0
	NodeRoleVia   int64 = 1 << 1
)
```

3. `nodeCols` 末尾追加 `,roles`；`scanNode` 的 Scan 末尾追加 `&n.Roles`（紧跟 `&relayHostDeclared, &relayHostV6Declared` 之后）。

4. 新增：

```go
func UpdateNodeRoles(d *sql.DB, id, roles int64) error {
	_, err := d.Exec(`UPDATE nodes SET roles=? WHERE id=?`, roles, id)
	return err
}
```

`internal/db/grants.go` `ListNodesForUser`：rows.Scan 里在 `&relayHostDeclared, &relayHostV6Declared,` 之后、`&g.MaxForwards` 之前插入 `&n.Roles,`（顺序必须与 nodeCols 完全一致）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/db -v` → 全部 PASS（顺带确认既有测试没被 scan 顺序打破）。

- [ ] **Step 5: 提交**

```bash
git add internal/db/migrations/0031_node_roles.sql internal/db/queries.go internal/db/grants.go internal/db/queries_test.go
git commit -m "feat(db): node roles bitmask (entry/via)"
```

---

### Task 3: node_bindings 表 + DB CRUD

**Files:**
- Create: `internal/db/migrations/0032_node_bindings.sql`
- Create: `internal/db/bindings.go`
- Test: `internal/db/bindings_test.go`

**Interfaces:**
- Produces:
  - `type NodeBinding struct { UpstreamNodeID int64 `json:"upstream_node_id"`; DownstreamNodeID int64 `json:"downstream_node_id"`; Mode string `json:"mode"` }`
  - `ListAllNodeBindings(d *sql.DB) ([]*NodeBinding, error)`
  - `ListBindingsForDownstream(d *sql.DB, downstreamID int64) ([]*NodeBinding, error)`
  - `ReplaceBindingsForDownstream(d *sql.DB, downstreamID int64, bindings []NodeBinding) error`（事务内整组替换）
  - `GetNodeBinding(d DBTX, upstreamID, downstreamID int64) (*NodeBinding, error)`（`sql.ErrNoRows` = 无边）

- [ ] **Step 1: 迁移**

`internal/db/migrations/0032_node_bindings.sql`：

```sql
-- A binding edge "downstream may be attached behind upstream" in a rule's
-- chain. mode is the junction segment's forwarding mode (upstream segment's
-- tail hop -> downstream segment's head) captured into rules at expand time.
CREATE TABLE node_bindings (
    upstream_node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    downstream_node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    mode TEXT NOT NULL DEFAULT 'userspace' CHECK(mode IN ('kernel','userspace')),
    PRIMARY KEY (upstream_node_id, downstream_node_id)
);
```

- [ ] **Step 2: 写失败测试**

`internal/db/bindings_test.go`：

```go
package db

import (
	"database/sql"
	"errors"
	"testing"
)

func TestNodeBindingsCRUD(t *testing.T) {
	d := openTestDB(t)
	up, _ := CreateNode(d, "entry-hk", "", "")
	mid, _ := CreateNode(d, "akari-hk", "", "")
	mid2, _ := CreateNode(d, "misaka", "", "")

	err := ReplaceBindingsForDownstream(d, mid.ID, []NodeBinding{
		{UpstreamNodeID: up.ID, DownstreamNodeID: mid.ID, Mode: "kernel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := GetNodeBinding(d, up.ID, mid.ID)
	if err != nil || b.Mode != "kernel" {
		t.Fatalf("want kernel edge, got %+v err=%v", b, err)
	}
	if _, err := GetNodeBinding(d, up.ID, mid2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing edge must be ErrNoRows, got %v", err)
	}

	// Replace is total for the downstream: dropping the old edge removes it.
	err = ReplaceBindingsForDownstream(d, mid.ID, []NodeBinding{
		{UpstreamNodeID: mid2.ID, DownstreamNodeID: mid.ID, Mode: "userspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetNodeBinding(d, up.ID, mid.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replaced-away edge must be gone, got %v", err)
	}
	all, _ := ListAllNodeBindings(d)
	if len(all) != 1 || all[0].UpstreamNodeID != mid2.ID {
		t.Fatalf("want 1 edge from mid2, got %+v", all)
	}
	ls, _ := ListBindingsForDownstream(d, mid.ID)
	if len(ls) != 1 {
		t.Fatalf("want 1 downstream edge, got %d", len(ls))
	}

	// Deleting a node cascades its edges away.
	if err := DeleteNode(d, mid2.ID); err != nil {
		t.Fatal(err)
	}
	all, _ = ListAllNodeBindings(d)
	if len(all) != 0 {
		t.Fatalf("cascade delete failed, edges left: %+v", all)
	}
}
```

Run: `go test ./internal/db -run TestNodeBindingsCRUD -v` → FAIL。

- [ ] **Step 3: 实现 `internal/db/bindings.go`**

```go
package db

import "database/sql"

// NodeBinding is an edge of the middle-layer graph: downstream may be attached
// behind upstream in a rule's chain. Mode is the junction segment's forwarding
// mode (upstream segment tail -> downstream segment head); it is captured into
// rule_hops when a rule expands, so edits affect only later (re)expansions.
type NodeBinding struct {
	UpstreamNodeID   int64  `json:"upstream_node_id"`
	DownstreamNodeID int64  `json:"downstream_node_id"`
	Mode             string `json:"mode"`
}

func scanNodeBinding(r rowScanner) (*NodeBinding, error) {
	b := &NodeBinding{}
	if err := r.Scan(&b.UpstreamNodeID, &b.DownstreamNodeID, &b.Mode); err != nil {
		return nil, err
	}
	return b, nil
}

const bindingCols = `upstream_node_id, downstream_node_id, mode`

func ListAllNodeBindings(d *sql.DB) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings ORDER BY downstream_node_id, upstream_node_id`, scanNodeBinding)
}

func ListBindingsForDownstream(d *sql.DB, downstreamID int64) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings WHERE downstream_node_id=? ORDER BY upstream_node_id`, scanNodeBinding, downstreamID)
}

func GetNodeBinding(d DBTX, upstreamID, downstreamID int64) (*NodeBinding, error) {
	return scanNodeBinding(d.QueryRow(`SELECT `+bindingCols+` FROM node_bindings WHERE upstream_node_id=? AND downstream_node_id=?`, upstreamID, downstreamID))
}

// ReplaceBindingsForDownstream swaps the downstream node's full upstream edge
// set in one transaction, mirroring how composite hops are replaced whole.
func ReplaceBindingsForDownstream(d *sql.DB, downstreamID int64, bindings []NodeBinding) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_bindings WHERE downstream_node_id=?`, downstreamID); err != nil {
		return err
	}
	for _, b := range bindings {
		mode := NormalizeForwardMode(b.Mode)
		if _, err := tx.Exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?,?)`,
			b.UpstreamNodeID, downstreamID, mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: 跑测试确认通过** → `go test ./internal/db -run TestNodeBindingsCRUD -v` PASS

- [ ] **Step 5: 提交**

```bash
git add internal/db/migrations/0032_node_bindings.sql internal/db/bindings.go internal/db/bindings_test.go
git commit -m "feat(db): node_bindings edge table for middle-layer graph"
```

---

### Task 4: rules.via_node_ids + rule_hops.via_node_id（溯源）

**Files:**
- Create: `internal/db/migrations/0033_rule_via.sql`
- Modify: `internal/db/queries.go`（Rule/RuleHop struct、`ruleCols`、`scanRule`、`ruleHopCols`、`scanRuleHop`）
- Modify: `internal/db/rules.go`（`HopInput`、`CreateRule`、`UpdateRuleHeader`、`RegenerateRule` 的 INSERT、via JSON 编解码）
- Modify: 所有「从现有 rule_hops 复制 HopInput」的调用点（下列 6 处）
- Test: `internal/db/queries_test.go`

**Interfaces:**
- Produces: `Rule.ViaNodeIDs []int64`（JSON `via_node_ids`）；`RuleHop.ViaNodeID int64`（JSON `via_node_id`）；`HopInput.ViaNodeID int64`。
- 约定：`rule_hops.via_node_id` = 该跳所属逻辑段的节点 ID（入口段 = `rules.node_id`；显式 hops 路径 = 该跳自身 NodeID）。

- [ ] **Step 1: 迁移**

`internal/db/migrations/0033_rule_via.sql`：

```sql
-- via_node_ids: ordered JSON array of the middle-layer node ids a rule's
-- chain runs through, persisted on the rule so every re-derivation path keeps
-- the layers instead of silently dropping them.
ALTER TABLE rules ADD COLUMN via_node_ids TEXT NOT NULL DEFAULT '[]';
-- via_node_id: which logical segment a physical hop was expanded from
-- (entry segment = rules.node_id). Quota suppression, per-grant accounting
-- and shaping group by it.
ALTER TABLE rule_hops ADD COLUMN via_node_id INTEGER NOT NULL DEFAULT 0;
UPDATE rule_hops SET via_node_id = (SELECT node_id FROM rules WHERE rules.id = rule_hops.rule_id);
```

- [ ] **Step 2: 写失败测试**

`internal/db/queries_test.go` 追加：

```go
func TestRuleViaRoundTripAndHopProvenance(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "entry", "", "")
	b, _ := CreateNode(d, "mid", "", "")
	_ = UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = UpdateNodeRelayHost(d, b.ID, "2.2.2.2")

	r := &Rule{NodeID: a.ID, Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443,
		ViaNodeIDs: []int64{b.ID}}
	tx, _ := d.Begin()
	id, err := CreateRule(tx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	hops := []HopInput{
		{NodeID: a.ID, Mode: "userspace", ViaNodeID: a.ID},
		{NodeID: b.ID, Mode: "kernel", ViaNodeID: b.ID},
	}
	if _, _, _, err := RegenerateRule(tx, r, hops, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, _ := GetRule(d, id)
	if len(got.ViaNodeIDs) != 1 || got.ViaNodeIDs[0] != b.ID {
		t.Fatalf("via round-trip failed: %+v", got.ViaNodeIDs)
	}
	rh, _ := ListRuleHops(d, id)
	if len(rh) != 2 || rh[0].ViaNodeID != a.ID || rh[1].ViaNodeID != b.ID {
		t.Fatalf("hop provenance wrong: %+v", rh)
	}
}
```

Run: `go test ./internal/db -run TestRuleViaRoundTrip -v` → FAIL。

- [ ] **Step 3: 实现 DB 层**

1. `internal/db/queries.go` Rule struct（`EntryFamily` 之后）加：

```go
	// ViaNodeIDs is the ordered middle-layer path the rule's chain runs
	// through (entry excluded). Persisted so node_id edits re-derive the same
	// chain; empty for plain single/composite rules.
	ViaNodeIDs []int64 `json:"via_node_ids"`
```

2. RuleHop struct（`Mode` 之后）加 `ViaNodeID int64 \`json:"via_node_id"\``。

3. `ruleCols` 追加 `,via_node_ids`；`scanRule` 里：

```go
	var viaJSON string
	// Scan 末尾追加 &viaJSON，然后：
	rl.ViaNodeIDs = decodeViaNodeIDs(viaJSON)
```

4. `ruleHopCols` 追加 `,via_node_id`；`scanRuleHop` Scan 在 `&h.Mode` 后、`&h.Comment` 前的位置按列序追加 `&h.ViaNodeID`（列序 = ruleHopCols 序：mode,comment,...,total_bytes,via_node_id → 实际把 `&h.ViaNodeID` 放到 Scan 参数最后）。

5. `internal/db/rules.go`：

```go
// encodeViaNodeIDs/decodeViaNodeIDs marshal the rule's middle-layer path for
// the TEXT column; a broken value decodes to an empty path rather than erroring
// (the chain snapshot in rule_hops still drives the data plane).
func encodeViaNodeIDs(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeViaNodeIDs(s string) []int64 {
	var ids []int64
	if s == "" || json.Unmarshal([]byte(s), &ids) != nil {
		return nil
	}
	return ids
}
```

（rules.go 需要 import `encoding/json`。）

6. `HopInput` 加字段 `ViaNodeID int64`（注释：所属逻辑段节点 ID，写入 rule_hops.via_node_id）。

7. `CreateRule` 的 INSERT 加列 `via_node_ids` 与值 `encodeViaNodeIDs(r.ViaNodeIDs)`；`UpdateRuleHeader` 的 UPDATE 同样加 `via_node_ids=?`。

8. `RegenerateRule` 的 INSERT（rules.go:675）加列 `via_node_id`、值 `h.viaNodeID`；`resolved` struct 加 `viaNodeID int64` 并在构造处 `rs[i]` 里带上 `viaNodeID: hop.ViaNodeID`。兜底：`if hop.ViaNodeID == 0 { rs[i].viaNodeID = r.NodeID }`（防旧调用点漏传时溯源列落 0）。

- [ ] **Step 4: 更新全部 HopInput 复制点（6 处）**

以下每处把 `db.HopInput{NodeID: h.NodeID, Mode: h.Mode}` 换成带溯源复制的版本 `db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID}`：

- `internal/server/api.go:1570`（header-only 编辑复制现链）
- `internal/server/api.go:1666`（apiReallocateRuleHop）
- `internal/server/shared.go:261`（regenerateRuleByID / rewire）
- `internal/server/hub.go:875`（applyRuleHopEdit，注意变量名是 `hp`）

显式 hops 与单跳 WS 路径（逻辑=物理，via=自身）：

- `internal/server/api.go:1321` 与 `api.go:1583`：`db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.NodeID}`
- `internal/server/hub.go:993`、`hub.go:1067`、`hub.go:1156`：`db.HopInput{NodeID: ac.nodeID, Mode: ..., ViaNodeID: ac.nodeID}`

（`api.go:1458` 的 hopsForNode 展开点在 Task 6 重写，此处不动。）

验证没有遗漏：`grep -rn "db.HopInput{NodeID" internal/ --include="*.go" | grep -v _test | grep -v ViaNodeID` 应为空。

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/... 2>&1 | tail -20`
Expected: `internal/db` 与 `internal/server` 全部 PASS（既有测试构造 HopInput 未传 via 时靠 RegenerateRule 的兜底落 rules.node_id，不应破坏任何断言）。

- [ ] **Step 6: 提交**

```bash
git add internal/db/migrations/0033_rule_via.sql internal/db/queries.go internal/db/rules.go internal/db/queries_test.go internal/server/api.go internal/server/shared.go internal/server/hub.go
git commit -m "feat(db): persist rule via path and hop segment provenance"
```

---

### Task 5: 组合倍率归自身列（迁移 + 停读聚合）

**Files:**
- Create: `internal/db/migrations/0034_composite_rate_multiplier.sql`
- Modify: `internal/db/queries.go`（删除 `ResolveCompositeRateMultiplier`，queries.go:395）
- Modify: `internal/server/api.go`（10 处调用点删除；组合创建/改链停止读写倍率语义）
- Modify: `web` 不动（本 task 只后端）
- Test: `internal/server/composite_hop_mult_test.go`（改写）

**Interfaces:**
- Produces: 组合节点的 `Node.RateMultiplier` 即其 nodes 行自身列（不再内存聚合）。`node_hops.traffic_multiplier` 保留为休眠列，代码不再消费其值（列仍写 0 以满足 NOT NULL/兼容）。

- [ ] **Step 1: 迁移**

`internal/db/migrations/0034_composite_rate_multiplier.sql`：

```sql
-- A composite's effective billing multiplier used to be the sum of its
-- per-hop multipliers, aggregated in memory. Billing now reads the entry
-- node's own rate_multiplier, so bake the sum into the composite's column to
-- keep existing pricing unchanged. node_hops.traffic_multiplier stays as a
-- dormant column (no reads or meaningful writes afterwards).
UPDATE nodes SET rate_multiplier = COALESCE(
    (SELECT SUM(traffic_multiplier) FROM node_hops WHERE node_hops.node_id = nodes.id),
    rate_multiplier)
WHERE node_type = 'composite';
```

- [ ] **Step 2: 改代码**

1. 删除 `internal/db/queries.go` 的 `ResolveCompositeRateMultiplier`（395-409 行附近）。
2. 删除 `internal/server/api.go` 的全部 10 处调用（`grep -n "ResolveCompositeRateMultiplier" internal/server/api.go`，行号约 306/1220/1406/1718/1720/2202/2230/2282/2891）。
3. `apiCreateNode` 组合分支（api.go:384-397）：hops 构造不再读 body 的 `traffic_multiplier` 也不再回读子节点倍率，统一 `TrafficMultiplier: 0`；body struct 里保留该字段但加注释「已废弃，服务端忽略」。新增：组合分支应用 `body.RateMultiplier`（与 remote 分支相同逻辑）：

```go
		if body.RateMultiplier > 0 && body.RateMultiplier != 1.0 {
			_ = db.UpdateNodeRateMultiplier(s.DB, n.ID, body.RateMultiplier)
		}
```

（放在 `CreateNodeHops` 成功之后、重新 GetNode 之前。）
4. `apiUpdateNodeHops`（api.go:574-647）：同样忽略每跳倍率入参，写 0。函数注释更新为「倍率不再是每跳属性，组合定价看节点自身 rate_multiplier」。

- [ ] **Step 3: 改测试**

`internal/server/composite_hop_mult_test.go` 现在断言「每跳倍率继承/求和」——按新语义重写为：组合节点显示倍率 = 自身 `rate_multiplier`；`POST /nodes` 组合分支接受 `rate_multiplier` 并落库。保留文件名，替换断言。核心断言示例：

```go
	// composite pricing lives on the composite's own column now
	comp, _ := db.GetNode(d, compID)
	if comp.RateMultiplier != 2.5 {
		t.Fatalf("composite rate_multiplier want 2.5, got %v", comp.RateMultiplier)
	}
```

- [ ] **Step 4: 跑测试** → `go test ./internal/server -run CompositeHopMult -v` PASS；`go test ./internal/...` 无回归（relay_stack 等其他 Resolve* 不受影响）。

- [ ] **Step 5: 提交**

```bash
git add internal/db/migrations/0034_composite_rate_multiplier.sql internal/db/queries.go internal/server/api.go internal/server/composite_hop_mult_test.go
git commit -m "refactor(billing): composite multiplier moves to the node's own column"
```

---

### Task 6: 链推导 hopsForChain + via 校验 + 四条规则 API 路径

**Files:**
- Modify: `internal/server/api.go`（`hopsForNode` 重构为 `expandSegment` + `hopsForChain`；apiCreateRule / apiUpdateRule / apiMyCreateRule / apiMyUpdateRule 接线）
- Test: `internal/server/chains_test.go` 或新建 `internal/server/via_chain_test.go`

**Interfaces:**
- Consumes: `db.GetNodeBinding`、`db.NodeRoleVia`、`Rule.ViaNodeIDs`、`HopInput.ViaNodeID`（Task 3/4 产物）。
- Produces:

```go
// expandSegment(nodeID) ([]db.HopInput, isComposite, error)
//   单点 → 1 跳（mode 留空待上层决定）；组合 → 按 node_hops 顺序展开，每跳
//   mode 取配置；所有跳 ViaNodeID = nodeID。
// hopsForChain(entryID int64, vias []int64, singleMode, exitMode string)
//   ([]db.HopInput, composite bool, error)
//   语义：entry 段 ++ via 段…；每个非末段的段尾跳 mode = 所经绑定边的 mode；
//   末跳 mode = exitMode（空 → 单点无 via 时退回 singleMode，即今天的行为）；
//   校验每个 via 具 NodeRoleVia 角色且 (prev→via) 绑定边存在。
//   composite = 入口是组合 || len(vias) > 0（供编辑端 explicit-mode 判定）。
```

- [ ] **Step 1: 写失败测试**

新建 `internal/server/via_chain_test.go`（harness 同包：`openDB`、`loginAsUser`、`makeComposite`、`New(d)`）：

```go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func bindVia(t *testing.T, d *sql.DB, up, down int64, mode string) {
	t.Helper()
	if err := db.ReplaceBindingsForDownstream(d, down, []db.NodeBinding{
		{UpstreamNodeID: up, DownstreamNodeID: down, Mode: mode},
	}); err != nil {
		t.Fatal(err)
	}
	n, _ := db.GetNode(d, down)
	if err := db.UpdateNodeRoles(d, down, n.Roles|db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}
}

func createMyRuleVia(t *testing.T, s *Server, cookie *http.Cookie, nodeID int64, vias []int64, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"node_id": nodeID, "via_node_ids": vias, "name": name, "proto": "tcp",
		"exit": "9.9.9.9:8443", "exit_mode": "userspace",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// 入口(单点) + 组合中间层：链 = entry ++ mid 的两个子节点；
// 衔接段模式取绑定边；末跳模式取规则 exit_mode。
func TestViaChainAssembly(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	m1, _ := db.CreateNode(d, "akari-1", "", "")
	m2, _ := db.CreateNode(d, "akari-2", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, m1.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, m2.ID, "3.3.3.3")
	mid := makeComposite(t, d, "akari", m1.ID, m2.ID)
	bindVia(t, d, entry.ID, mid.ID, "kernel")

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 3 {
		t.Fatalf("want 3 hops, got %d", len(hops))
	}
	// 段尾（entry，非末段）走绑定边 kernel；组合段内跳走配置 userspace；末跳走 exit_mode
	if hops[0].Mode != "kernel" || hops[1].Mode != "userspace" || hops[2].Mode != "userspace" {
		t.Fatalf("modes: %s/%s/%s", hops[0].Mode, hops[1].Mode, hops[2].Mode)
	}
	if hops[0].ViaNodeID != entry.ID || hops[1].ViaNodeID != mid.ID || hops[2].ViaNodeID != mid.ID {
		t.Fatalf("provenance: %d/%d/%d", hops[0].ViaNodeID, hops[1].ViaNodeID, hops[2].ViaNodeID)
	}
	if got := rules[0].ViaNodeIDs; len(got) != 1 || got[0] != mid.ID {
		t.Fatalf("rule via persisted wrong: %+v", got)
	}
}

// 服务端权威校验：无绑定边 / 无 via 角色 / 无授权 都必须拒绝。
func TestViaChainValidation(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "mid", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, mid.ID, "2.2.2.2")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	s, _ := New(d)

	// 有授权、有角色，但无绑定边 → 400
	n, _ := db.GetNode(d, mid.ID)
	_ = db.UpdateNodeRoles(d, mid.ID, n.Roles|db.NodeRoleVia)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-noedge"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no edge: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	// 有边但摘掉角色 → 400
	bindVia(t, d, entry.ID, mid.ID, "userspace")
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-norole"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no role: want 400, got %d", rec.Code)
	}
	// 有边有角色但撤销授权 → 403
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry|db.NodeRoleVia)
	_ = db.RevokeNode(d, uid, mid.ID)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-nogrant"); rec.Code != http.StatusForbidden {
		t.Fatalf("no grant: want 403, got %d", rec.Code)
	}
}

// 编辑防降级：不带 via_node_ids 的编辑保留原路径。
func TestEditWithoutViaFieldKeepsPath(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "mid", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, mid.ID, "2.2.2.2")
	bindVia(t, d, entry.ID, mid.ID, "userspace")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)

	body, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "name": "r1-renamed", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req := httptest.NewRequest("PUT", "/api/my/rules/"+strconv.FormatInt(rules[0].ID, 10), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 2 {
		t.Fatalf("via silently dropped on edit: %d hops", len(hops))
	}
	// 显式清空 via（送空数组）则回到单段
	body2, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "via_node_ids": []int64{}, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req2 := httptest.NewRequest("PUT", "/api/my/rules/"+strconv.FormatInt(rules[0].ID, 10), bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	s.Router().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("clear via: %d %s", w2.Code, w2.Body.String())
	}
	if hops, _ = db.ListRuleHops(d, rules[0].ID); len(hops) != 1 {
		t.Fatalf("explicit empty via must clear layers: %d hops", len(hops))
	}
}
```

（import 需含 `database/sql`、`strconv`。）
Run: `go test ./internal/server -run 'TestViaChain|TestEditWithoutVia' -v` → FAIL。

- [ ] **Step 2: 实现 expandSegment + hopsForChain**

替换 `internal/server/api.go` 的 `hopsForNode`（1446-1474）：

```go
// expandSegment expands one logical node into its physical hops: a single node
// is itself; a composite is its ordered children with the config's inter-node
// modes. Every hop carries the segment's logical node id for provenance. Mode
// of the segment's tail hop is left as stored (dormant for composites) — the
// chain assembler overwrites it with the junction edge mode or the rule's
// exit mode.
func (s *Server) expandSegment(nodeID int64) ([]db.HopInput, bool, error) {
	node, err := db.GetNode(s.DB, nodeID)
	if err != nil {
		return nil, false, fmt.Errorf("节点不存在")
	}
	if node.NodeType == "composite" {
		nh, _ := db.ListNodeHops(s.DB, nodeID)
		if len(nh) == 0 {
			return nil, true, fmt.Errorf("组合节点无子节点")
		}
		hops := make([]db.HopInput, len(nh))
		for i, h := range nh {
			hops[i] = db.HopInput{NodeID: h.HopNodeID, Mode: h.Mode, ViaNodeID: nodeID}
		}
		return hops, true, nil
	}
	return []db.HopInput{{NodeID: nodeID, Mode: "", ViaNodeID: nodeID}}, false, nil
}

// hopsForChain assembles a rule's physical chain: the entry segment followed
// by each middle-layer (via) segment. Non-final segment tails take the
// forwarding mode of the binding edge they cross; the final hop takes the
// rule's exitMode (empty falls back to singleMode for a bare single-node
// chain — the legacy alias — else the kernel default). Every via must carry
// the via role and be reachable from its predecessor through a binding edge;
// grants are the caller's policy. The composite flag tells edit handlers
// which fields count as an explicit exit-mode request: any chain beyond a
// bare single node never honors the legacy mode alias.
func (s *Server) hopsForChain(entryID int64, vias []int64, singleMode, exitMode string) ([]db.HopInput, bool, error) {
	hops, entryComposite, err := s.expandSegment(entryID)
	if err != nil {
		return nil, false, err
	}
	prev := entryID
	for _, viaID := range vias {
		viaNode, err := db.GetNode(s.DB, viaID)
		if err != nil {
			return nil, true, fmt.Errorf("中间层节点不存在")
		}
		if viaNode.Roles&db.NodeRoleVia == 0 {
			return nil, true, fmt.Errorf("节点 %s 不是中间层", viaNode.Name)
		}
		edge, err := db.GetNodeBinding(s.DB, prev, viaID)
		if err != nil {
			return nil, true, fmt.Errorf("中间层 %s 未绑定到所选上游", viaNode.Name)
		}
		seg, _, err := s.expandSegment(viaID)
		if err != nil {
			return nil, true, err
		}
		// The junction (previous segment's tail -> this segment's head) is an
		// inter-node leg owned by the binding edge, not by the rule.
		hops[len(hops)-1].Mode = edge.Mode
		hops = append(hops, seg...)
		prev = viaID
	}
	composite := entryComposite || len(vias) > 0
	mode := exitMode
	if mode == "" && !composite {
		mode = singleMode
	}
	hops[len(hops)-1].Mode = mode
	return hops, composite, nil
}
```

- [ ] **Step 3: 接线四条 API 路径**

四个 body struct（apiCreateRule 约 1286、apiUpdateRule 约 1502、apiMyCreateRule、apiMyUpdateRule）都加字段：

```go
		// ViaNodeIDs is the ordered middle-layer path. A pointer tells "not
		// sent" (edits keep the stored path — old clients must not silently
		// strip layers) apart from an explicit empty list (clear the layers).
		ViaNodeIDs *[]int64 `json:"via_node_ids"`
```

**apiCreateRule**（1323-1329 的 `body.NodeID > 0` 分支）：

```go
	} else if body.NodeID > 0 {
		var vias []int64
		if body.ViaNodeIDs != nil {
			vias = *body.ViaNodeIDs
		}
		derived, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode)
		if derr != nil {
			jsonErr(w, http.StatusBadRequest, derr.Error())
			return
		}
		hops = derived
	}
```

并在 `rl := &db.Rule{...}` 里加 `ViaNodeIDs: viasOf(body.ViaNodeIDs)`，其中：

```go
func viasOf(p *[]int64) []int64 {
	if p == nil {
		return nil
	}
	return *p
}
```

（放 shared.go。显式 hops 路径 via 留空。）

**apiUpdateRule**：switch 首个 case 改为 `case body.NodeID > 0 || body.ViaNodeIDs != nil:`，within：

```go
		entryID := body.NodeID
		if entryID == 0 {
			entryID = rl.NodeID
		}
		vias := rl.ViaNodeIDs
		if body.ViaNodeIDs != nil {
			vias = *body.ViaNodeIDs
		}
		derived, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode)
		...
		explicit := body.ExitMode != "" || (!composite && body.Mode != "")
		if !explicit && entryID == rl.NodeID {
			if existing, _ := db.ListRuleHops(s.DB, id); len(existing) > 0 {
				hops[len(hops)-1].Mode = existing[len(existing)-1].Mode
			}
		}
		rl.NodeID = entryID
		rl.ViaNodeIDs = vias
```

（沿用原有的 keep-exit-mode 逻辑，仅把判定从 `body.NodeID == rl.NodeID` 换成 `entryID == rl.NodeID`。header-only 与显式 hops 分支不动——header-only 复制现链已带 via 溯源。）

**apiMyCreateRule**（2363-2399）：在 `hopsForChain` 之前对 vias 逐个 `db.GetNodeGrant(s.DB, u.ID, viaID)`，失败 → 403 `"无权使用中间层节点"`；调用改 `s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode)`；`rl.ViaNodeIDs = vias`。

**apiMyUpdateRule**：与 apiUpdateRule 同构（entryID/vias 解析 + via grant 检查 + hopsForChain + 防降级保留），先读函数现状再改，保持其原有的 grant/quota 检查顺序。

- [ ] **Step 4: 跑测试**

Run: `go test ./internal/server -run 'TestViaChain|TestEditWithoutVia' -v` → PASS
Run: `go test ./internal/server -v 2>&1 | tail -5` → 全部 PASS（`composite_exit_mode_test`、`entry_family_test`、`chains_test`、`my_composite_test`、`proto_compat_test` 是关键回归面：hopsForChain 在 vias 为空时的行为必须与旧 hopsForNode 完全一致）。

- [ ] **Step 5: 提交**

```bash
git add internal/server/api.go internal/server/shared.go internal/server/via_chain_test.go
git commit -m "feat(server): cascade chain assembly with via segments and binding-edge junction modes"
```

---

### Task 7: 计费重构（入口单次计费 + 逻辑段专属配额 + 压制/限速跟随）

**Files:**
- Modify: `internal/server/hub.go`（`applyCounters`，588-854）
- Modify: `internal/db/traffic.go`（删 `HopMultipliers`，加 `NodeRateMultipliers`、`SegmentFirstHops`、改 `RulesAffectedByNode`）
- Modify: `internal/db/queries.go`（`ActiveRuleHopsForPush`）
- Modify: `internal/server/server.go`（`buildRules` 的 shaping 键）
- Test: `internal/server/traffic_accounting_test.go`（追加）、`internal/db/traffic_test.go`（追加）

**Interfaces:**
- Produces:
  - `db.NodeRateMultipliers(d *sql.DB) (map[int64]float64, error)` — `SELECT id, rate_multiplier FROM nodes`。
  - `db.SegmentFirstHops(d *sql.DB, ruleIDs []int64) (map[int64]map[int]int64, error)` — 每条规则「段首跳 position → 段逻辑节点 ID」，SQL：`SELECT rule_id, MIN(position), via_node_id FROM rule_hops WHERE rule_id IN (...) GROUP BY rule_id, via_node_id`。
- 计费新口径（spec 表）：全局用量只在 position 0 计 `billedDelta × 入口倍率 × billingRate`；专属配额在每段首跳向该段逻辑节点的 grant 累加 `billedDelta`（原始）；hop totals 入口跳存计费值、其余跳存 `billedDelta`；出口账本不变。

- [ ] **Step 1: 写失败测试（DB 函数）**

`internal/db/traffic_test.go` 追加：

```go
func TestSegmentFirstHops(t *testing.T) {
	d := openTestDB(t)
	a, _ := CreateNode(d, "e", "", "")
	b, _ := CreateNode(d, "m1", "", "")
	c, _ := CreateNode(d, "m2", "", "")
	_ = UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = UpdateNodeRelayHost(d, c.ID, "3.3.3.3")
	r := &Rule{NodeID: a.ID, Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	tx, _ := d.Begin()
	id, _ := CreateRule(tx, r)
	r.ID = id
	// 入口段 a；via 段（组合层 L）含 b、c —— 溯源统一记 L=b.ID 模拟组合层
	_, _, _, err := RegenerateRule(tx, r, []HopInput{
		{NodeID: a.ID, Mode: "userspace", ViaNodeID: a.ID},
		{NodeID: b.ID, Mode: "userspace", ViaNodeID: b.ID},
		{NodeID: c.ID, Mode: "userspace", ViaNodeID: b.ID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	m, err := SegmentFirstHops(d, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{0: a.ID, 1: b.ID}
	if len(m[id]) != 2 || m[id][0] != want[0] || m[id][1] != want[1] {
		t.Fatalf("segment firsts want %v, got %v", want, m[id])
	}
}
```

Run: `go test ./internal/db -run TestSegmentFirstHops -v` → FAIL。

- [ ] **Step 2: 实现 DB 函数**

`internal/db/traffic.go`：删除 `HopMultipliers`（66-88），新增：

```go
// NodeRateMultipliers returns every node's rate_multiplier keyed by id. The
// entry node's value is the whole rule's billing multiplier — middle-layer
// and composite-child hops don't stack their own factors.
func NodeRateMultipliers(d *sql.DB) (map[int64]float64, error) {
	rows, err := d.Query(`SELECT id, rate_multiplier FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]float64{}
	for rows.Next() {
		var id int64
		var mult float64
		if err := rows.Scan(&id, &mult); err != nil {
			return nil, err
		}
		m[id] = mult
	}
	return m, rows.Err()
}

// SegmentFirstHops maps each rule's segment-first hop positions to the
// segment's logical node id. Per-grant byte accounting charges a segment's
// grant exactly once per counter batch — at its first hop — since every hop
// of a segment carries the same bytes.
func SegmentFirstHops(d *sql.DB, ruleIDs []int64) (map[int64]map[int]int64, error) {
	if len(ruleIDs) == 0 {
		return map[int64]map[int]int64{}, nil
	}
	args := make([]any, len(ruleIDs))
	ph := make([]string, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
		ph[i] = "?"
	}
	rows, err := d.Query(`SELECT rule_id, MIN(position), via_node_id FROM rule_hops
		WHERE rule_id IN (`+strings.Join(ph, ",")+`) GROUP BY rule_id, via_node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]map[int]int64{}
	for rows.Next() {
		var ruleID, via int64
		var pos int
		if err := rows.Scan(&ruleID, &pos, &via); err != nil {
			return nil, err
		}
		if m[ruleID] == nil {
			m[ruleID] = map[int]int64{}
		}
		m[ruleID][pos] = via
	}
	return m, rows.Err()
}
```

（traffic.go 需 import `strings`。）

`RulesAffectedByNode`（traffic.go:147-157）改为按溯源匹配：

```go
// RulesAffectedByNode returns the distinct hop-node IDs of all rules owned by
// userID whose chain runs a segment of the given logical node (entry or via —
// grants, and thus quotas, live on logical nodes).
func RulesAffectedByNode(d *sql.DB, userID, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		WHERE r.owner_id = ?
		  AND rh.rule_id IN (
		      SELECT rh2.rule_id FROM rule_hops rh2 WHERE rh2.via_node_id = ?)`,
		userID, nodeID, nodeID)
}
```

（占位符数量随 SQL 调整：这里是 2 个参数。）

`internal/db/queries.go` `ActiveRuleHopsForPush`：第三个 NOT EXISTS 的 `un.node_id = rh2.node_id` 改为 `un.node_id = rh2.via_node_id`；删除第四个 NOT EXISTS（r3/un2 子句）——backfill 后入口段 via = rules.node_id，逻辑段匹配已覆盖它。

- [ ] **Step 3: 写失败测试（applyCounters 新口径）**

`internal/server/traffic_accounting_test.go` 追加（构造 `h := &Hub{DB: d}`，直接调 `h.applyCounters`；样本类型 `wsproto.CounterSample{Proto, ListenPort, BytesUp, BytesDown}`，参照该文件既有用法）：

```go
// 入口倍率 2.0、层节点自身倍率 3.0：全局用量只按入口跳 ×2 计一次；
// 入口/层两个 grant 各按原始字节累加一次；层内第二跳不再入任何 grant。
func TestBillingEntryOnlyAndRawGrantBytes(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "e", "", "")
	m1, _ := db.CreateNode(d, "m1", "", "")
	m2, _ := db.CreateNode(d, "m2", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, m1.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, m2.ID, "3.3.3.3")
	_ = db.UpdateNodeRateMultiplier(d, entry.ID, 2.0)
	_ = db.UpdateNodeRateMultiplier(d, m1.ID, 3.0)
	mid := makeComposite(t, d, "layer", m1.ID, m2.ID)

	uid, _ := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)

	r := &db.Rule{NodeID: entry.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true},
		Name: "x", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, ViaNodeIDs: []int64{mid.ID}}
	tx, _ := d.Begin()
	id, _ := db.CreateRule(tx, r)
	r.ID = id
	_, _, _, err := db.RegenerateRule(tx, r, []db.HopInput{
		{NodeID: entry.ID, Mode: "userspace", ViaNodeID: entry.ID},
		{NodeID: m1.ID, Mode: "userspace", ViaNodeID: mid.ID},
		{NodeID: m2.ID, Mode: "userspace", ViaNodeID: mid.ID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	hops, _ := db.ListRuleHops(d, id)

	h := &Hub{DB: d}
	// 每跳各上报 1000 字节（同一份流量流经三跳）
	for _, hp := range hops {
		h.applyCounters(hp.NodeID, []wsproto.CounterSample{
			{Proto: "tcp", ListenPort: hp.ListenPort, BytesUp: 600, BytesDown: 400},
		})
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 2000 { // 1000 × 入口倍率 2.0，只计一次
		t.Fatalf("global used want 2000, got %d", u.TrafficUsedBytes)
	}
	ge, _ := db.GetNodeGrant(d, uid, entry.ID)
	gm, _ := db.GetNodeGrant(d, uid, mid.ID)
	if ge.TrafficUsedBytes != 1000 || gm.TrafficUsedBytes != 1000 {
		t.Fatalf("grant raw bytes want 1000/1000, got %d/%d", ge.TrafficUsedBytes, gm.TrafficUsedBytes)
	}
	// hop totals：入口跳计费值 2000，其余跳原始 1000
	hops, _ = db.ListRuleHops(d, id)
	if hops[0].TotalBytes != 2000 || hops[1].TotalBytes != 1000 || hops[2].TotalBytes != 1000 {
		t.Fatalf("hop totals: %d/%d/%d", hops[0].TotalBytes, hops[1].TotalBytes, hops[2].TotalBytes)
	}
}
```

Run: FAIL（旧逻辑全局会计 3 跳之和）。

- [ ] **Step 4: 重写 applyCounters 计费段**

`internal/server/hub.go` 588-766 的改动要点（完整替换相关段落）：

1. 开头替换加载：

```go
	multipliers, err := db.NodeRateMultipliers(h.DB)
	if err != nil {
		log.Printf("hub: load node rate multipliers: %v", err)
		multipliers = map[int64]float64{}
	}
	segFirst, err := db.SegmentFirstHops(h.DB, ruleIDs)
	if err != nil {
		log.Printf("hub: node %d load segment first hops: %v", nodeID, err)
		segFirst = map[int64]map[int]int64{}
	}
```

（`ruleIDs` 已在函数里收集；`hopMultipliers` 相关行删除。注意 `segFirst` 的加载要放在 `ruleIDs` 构建之后。）

2. 循环内 695-766 替换为：

```go
		// Global quota: the same bytes flow through every hop, so the user is
		// billed exactly once — at the entry hop — with the entry node's
		// multiplier (a composite entry carries its own rate_multiplier).
		weighted := billedDelta
		var userID int64
		hasOwner := r != nil && r.OwnerID.Valid && billedDelta > 0
		if hasOwner {
			userID = r.OwnerID.Int64
			u, cached := userCache[userID]
			if !cached {
				u, _ = db.GetUserByID(h.DB, userID)
				userCache[userID] = u
				if u != nil {
					if reset, _ := db.CheckAndResetTrafficCycle(h.DB, u); reset {
						if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
							_ = db.SetUserDisabled(h.DB, userID, false, "")
						}
						if nodes, err := db.DistinctUserNodes(h.DB, userID); err == nil && h.Redispatch != nil {
							go h.Redispatch(nodes)
						}
					}
				}
			}
			mult := multipliers[r.NodeID]
			if mult <= 0 {
				mult = 1.0
			}
			billingRate := 1.0
			if u != nil && u.BillingRate > 0 {
				billingRate = u.BillingRate
			}
			weighted = int64(math.Round(float64(billedDelta) * mult * billingRate))
		}

		// rule_hops: last_bytes stay raw for speed display. total_bytes keeps
		// the billed amount on the entry hop (what rule traffic displays and
		// quotas consume) and raw bytes elsewhere.
		w := hopWrites[rh.ID]
		if w == nil {
			w = &hopWrite{}
			hopWrites[rh.ID] = w
		}
		w.lastBytes = totalDelta
		w.lastUp = s.BytesUp
		w.lastDown = s.BytesDown
		if rh.Position == 0 {
			w.addWeighted += weighted
		} else {
			w.addWeighted += billedDelta
		}

		if !hasOwner || billedDelta <= 0 {
			continue
		}

		// Per-grant quota: raw bytes, charged once per segment at its first
		// hop, onto the segment's logical node grant (entry grant included —
		// the entry segment's via is rules.node_id).
		if via, ok := segFirst[rh.RuleID][rh.Position]; ok {
			userNodeAdds[userNode{userID, via}] += billedDelta
			touched[userNode{userID, via}] = true
		}
		if rh.Position == 0 && weighted > 0 {
			userAdds[userID] += weighted
		}
```

（原 `touched[userNode{userID, nodeID}] = true` 一行删除——touched 现按逻辑节点标记，配额执行回调 `OnTrafficUpdate(userID, 逻辑节点)` 与 `RulesAffectedByNode` 的逻辑段匹配是同一口径。出口账本段的 `touched[userNode{key.UserID, nodeID}]` 保持不动。）

3. `internal/server/server.go` `buildRules`（378 行）shaping 键从 `r.NodeID` 改为 `rh.ViaNodeID`：

```go
				if gs, ok := shapes[[2]int64{r.OwnerID.Int64, rh.ViaNodeID}]; ok {
```

注释同步为「shaping 按该跳所属逻辑段的授权取值：入口段跟入口 grant，中间层段跟层 grant」。

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/server -run TestBillingEntryOnly -v && go test ./internal/db -run 'TestSegmentFirstHops' -v` → PASS
Run: `go test ./internal/... 2>&1 | tail -20` → 检查既有计费/压制测试（`traffic_accounting_test`、`pernode_quota_*`、`grant_rate_buildrules_test`、`cycle_reset_redispatch_test`）。**预期部分旧断言按旧口径写成（加权、逐跳累计），逐个按新口径改断言**——改动要在 commit message 外的测试注释里写明新口径依据（入口单次计费/原始字节），不留旧值魔数。

- [ ] **Step 6: 提交**

```bash
git add internal/server/hub.go internal/server/server.go internal/db/traffic.go internal/db/queries.go internal/server/traffic_accounting_test.go internal/db/traffic_test.go
git commit -m "refactor(billing): entry-only global multiplier, raw per-segment grant bytes"
```

（若 Step 5 改了其他测试文件，一并 add。）

---

### Task 8: 管理端 API：roles 与 bindings 端点

**Files:**
- Modify: `internal/server/server.go`（admin 路由组，466 行后）
- Modify: `internal/server/api.go`（三个 handler）
- Test: `internal/server/handlers_admin_test.go`（追加）

**Interfaces:**
- Produces（均 admin-only）:
  - `POST /api/nodes/{id}/roles` body `{"roles": 3}` → handler `apiUpdateNodeRolesMask`
  - `GET /api/nodes/{id}/bindings` → `{"bindings": [NodeBinding...]}`（downstream = id）→ handler `apiListNodeBindings`
  - `POST /api/nodes/{id}/bindings` body `{"bindings": [{"upstream_node_id":1,"mode":"userspace"}]}`（整组替换）→ handler `apiUpdateNodeBindings`
  - `GET /api/node-bindings` → 全量边 → handler `apiListAllNodeBindings`
- 命名注意：`/api/node-roles`（落地代理角色，存 settings）已存在且语义不同，勿混用。

- [ ] **Step 1: 失败测试**

`internal/server/handlers_admin_test.go` 追加（admin 登录 helper 参照该文件既有测试，通常为 `loginAsAdmin(t, d)` 一类；先 grep 确认名字再写）：

```go
func TestNodeRolesAndBindingsEndpoints(t *testing.T) {
	d := openDB(t)
	up, _ := db.CreateNode(d, "entry-hk", "", "")
	mid, _ := db.CreateNode(d, "akari", "", "")
	_, cookie := loginAsAdmin(t, d)
	s, _ := New(d)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		return w
	}

	if w := do("POST", fmt.Sprintf("/api/nodes/%d/roles", mid.ID), `{"roles":3}`); w.Code != 200 {
		t.Fatalf("set roles: %d %s", w.Code, w.Body.String())
	}
	n, _ := db.GetNode(d, mid.ID)
	if n.Roles != 3 {
		t.Fatalf("roles want 3, got %d", n.Roles)
	}
	// downstream 必须有 via 角色才能被绑定
	body := fmt.Sprintf(`{"bindings":[{"upstream_node_id":%d,"mode":"kernel"}]}`, up.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), body); w.Code != 200 {
		t.Fatalf("set bindings: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), ""); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"mode":"kernel"`) {
		t.Fatalf("list bindings: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/api/node-bindings", ""); w.Code != 200 ||
		!strings.Contains(w.Body.String(), fmt.Sprintf(`"upstream_node_id":%d`, up.ID)) {
		t.Fatalf("list all: %d %s", w.Code, w.Body.String())
	}
	// 摘掉 via 角色后再绑 → 400
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/bindings", mid.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("bind non-via: want 400, got %d", w.Code)
	}
}
```

- [ ] **Step 2: 实现 handler + 路由**

`internal/server/api.go`（放在 apiUpdateNodeHops 附近）：

```go
func (s *Server) apiUpdateNodeRolesMask(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Roles int64 `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Roles&^(db.NodeRoleEntry|db.NodeRoleVia) != 0 || body.Roles == 0 {
		jsonErr(w, http.StatusBadRequest, "roles 非法")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if err := db.UpdateNodeRoles(s.DB, id, body.Roles); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.roles", strconv.FormatInt(id, 10), strconv.FormatInt(body.Roles, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiListNodeBindings(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	bs, err := db.ListBindingsForDownstream(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

func (s *Server) apiListAllNodeBindings(w http.ResponseWriter, r *http.Request) {
	bs, err := db.ListAllNodeBindings(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

func (s *Server) apiUpdateNodeBindings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if node.Roles&db.NodeRoleVia == 0 {
		jsonErr(w, http.StatusBadRequest, "该节点不是中间层，先为其开启中间层角色")
		return
	}
	var body struct {
		Bindings []struct {
			UpstreamNodeID int64  `json:"upstream_node_id"`
			Mode           string `json:"mode"`
		} `json:"bindings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	bs := make([]db.NodeBinding, len(body.Bindings))
	for i, b := range body.Bindings {
		if b.UpstreamNodeID == id {
			jsonErr(w, http.StatusBadRequest, "不能绑定到自身")
			return
		}
		if _, err := db.GetNode(s.DB, b.UpstreamNodeID); err != nil {
			jsonErr(w, http.StatusBadRequest, "上游节点不存在")
			return
		}
		bs[i] = db.NodeBinding{UpstreamNodeID: b.UpstreamNodeID, DownstreamNodeID: id, Mode: b.Mode}
	}
	if err := db.ReplaceBindingsForDownstream(s.DB, id, bs); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.bindings", strconv.FormatInt(id, 10), fmt.Sprintf("%d edges", len(bs)))
	jsonOK(w, map[string]any{"ok": true})
}
```

`internal/server/server.go` admin 组（466 行 `POST /nodes/{id}/hops` 之后）：

```go
			r.Post("/nodes/{id}/roles", s.apiUpdateNodeRolesMask)
			r.Get("/nodes/{id}/bindings", s.apiListNodeBindings)
			r.Post("/nodes/{id}/bindings", s.apiUpdateNodeBindings)
			r.Get("/node-bindings", s.apiListAllNodeBindings)
```

- [ ] **Step 3: 跑测试** → `go test ./internal/server -run TestNodeRolesAndBindings -v` PASS

- [ ] **Step 4: 提交**

```bash
git add internal/server/server.go internal/server/api.go internal/server/handlers_admin_test.go
git commit -m "feat(api): node role mask and middle-layer binding endpoints"
```

---

### Task 9: my API 级联数据（授权交集的绑定边）

**Files:**
- Modify: `internal/server/api.go`（`apiMyListRules`，约 2220-2300）
- Test: `internal/server/my_composite_test.go`（追加）

**Interfaces:**
- Produces: `GET /api/my/rules` 响应新增 `"bindings": [NodeBinding...]` — 仅包含两端都在用户授权集合内的边。`nodes` 里每个节点自带 `roles`（Task 2 的 JSON 字段）；规则项自带 `via_node_ids`（Rule 内嵌）。

- [ ] **Step 1: 失败测试**

```go
func TestMyRulesCarriesGrantedBindings(t *testing.T) {
	d := openDB(t)
	up, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "akari", "", "")
	other, _ := db.CreateNode(d, "misaka", "", "")
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleVia)
	_ = db.UpdateNodeRoles(d, other.ID, db.NodeRoleVia)
	_ = db.ReplaceBindingsForDownstream(d, mid.ID, []db.NodeBinding{{UpstreamNodeID: up.ID, DownstreamNodeID: mid.ID, Mode: "userspace"}})
	_ = db.ReplaceBindingsForDownstream(d, other.ID, []db.NodeBinding{{UpstreamNodeID: up.ID, DownstreamNodeID: other.ID, Mode: "userspace"}})

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, up.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0) // other 未授权 → 其边不下发

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my/rules", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	var resp struct {
		Bindings []db.NodeBinding `json:"bindings"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Bindings) != 1 || resp.Bindings[0].DownstreamNodeID != mid.ID {
		t.Fatalf("want only granted edge, got %+v", resp.Bindings)
	}
}
```

- [ ] **Step 2: 实现**

`apiMyListRules` 里（grantedNodes 拿到之后）：

```go
	grantedSet := map[int64]bool{}
	for _, n := range grantedNodes {
		grantedSet[n.ID] = true
	}
	allEdges, _ := db.ListAllNodeBindings(s.DB)
	edges := make([]*db.NodeBinding, 0, len(allEdges))
	for _, e := range allEdges {
		// Only edges the user can actually traverse: both endpoints granted.
		if grantedSet[e.UpstreamNodeID] && grantedSet[e.DownstreamNodeID] {
			edges = append(edges, e)
		}
	}
```

响应 map 增加 `"bindings": edges`。

- [ ] **Step 3: 跑测试** → PASS，提交：

```bash
git add internal/server/api.go internal/server/my_composite_test.go
git commit -m "feat(api): expose granted middle-layer bindings to the user rule form"
```

---

### Task 10: Web 规则表单级联（RuleFormModal）

**Files:**
- Modify: `web/src/components/RuleFormModal.jsx`
- Modify: `web/src/pages/my/Rules.jsx`、`web/src/pages/my/RuleDetail.jsx`（编辑入口传参）、`web/src/pages/rules/List.jsx`、`web/src/pages/rules/Detail.jsx`（admin 侧传参）

**Interfaces:**
- Consumes: `/my/rules` 的 `bindings`（Task 9）；admin 侧 `GET /node-bindings`（Task 8）；节点 `roles` 字段。
- Produces: `RuleFormModal` 新 prop `bindings = []`；`form.via_node_ids: number[]`；`ruleFormToPayload` 恒发送 `via_node_ids`（数组，空数组=清空——表单是新 bundle，语义明确）。

- [ ] **Step 1: RuleFormModal 改造**

1. `EMPTY` 加 `via_node_ids: []`；组件签名加 `bindings = []`。
2. 入口选择器只列入口角色（roles 缺省按 1 处理，兼容尚未打角色的响应）：

```jsx
  const entryNodes = nodes.filter(n => ((n.roles ?? 1) & 1) !== 0)
  const groups = [
    { label: '单点', options: entryNodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
    { label: '组合', options: entryNodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
  ]
```

3. 级联候选与渲染（放在「入口类型」块之后、「名称」之前）：

```jsx
  const nodeById = Object.fromEntries(nodes.map(n => [n.id, n]))
  // 级联层级：chain[i] 的候选 = 绑定图中 upstream=chain[i-1] 的下级 ∩ 拿到的
  // 节点集合（my 端即授权集合）∩ 中间层角色 ∩ 未在链上。过滤后为空则不再
  // 渲染下一级——有框就有得选。
  const viaChain = (form.via_node_ids || []).map(Number).filter(id => nodeById[id])
  const viaCandidates = (upstreamId, chainSoFar) =>
    bindings
      .filter(b => b.upstream_node_id === upstreamId)
      .map(b => nodeById[b.downstream_node_id])
      .filter(n => n && ((n.roles ?? 0) & 2) !== 0)
      .filter(n => n.id !== Number(form.node_id) && !chainSoFar.includes(n.id))
  const pickVia = (level, v) => setForm(f => {
    const next = (f.via_node_ids || []).slice(0, level)
    if (v) next.push(Number(v))
    return { ...f, via_node_ids: next }
  })
  const viaLevels = []
  if (form.node_id) {
    let upstream = Number(form.node_id)
    const soFar = []
    for (let level = 0; ; level++) {
      const chosen = viaChain[level]
      const cands = viaCandidates(upstream, soFar)
      if (!cands.length && !chosen) break
      viaLevels.push({ level, cands, chosen })
      if (!chosen) break
      soFar.push(chosen)
      upstream = chosen
    }
  }
```

JSX（网格内）：

```jsx
          {viaLevels.map(({ level, cands, chosen }) => (
            <Fragment key={level}>
              <label className="fl">{level === 0 ? '线路层' : `线路层 ${level + 1}`}</label>
              <Select value={chosen ?? ''} onChange={v => pickVia(level, v)} placeholder="直接转发"
                options={[{ value: '', label: '直接转发' },
                  ...cands.map(n => ({ value: n.id, label: fmtRate(n) }))]} />
            </Fragment>
          ))}
          {viaChain.length > 0 && (
            <>
              <label className="fl"></label>
              <div className="text-xs text-ink-mut">
                <span className="font-mono">
                  {[nodeById[Number(form.node_id)]?.name, ...viaChain.map(id => nodeById[id]?.name), '目标']
                    .filter(Boolean).join(' → ')}
                </span>
                <span className="ml-2">链路更长的规则占用更多全局转发名额</span>
              </div>
            </>
          )}
```

（import `Fragment` from 'react'。）
4. 入口切换时清空级联：在既有 `useEffect([form.node_id])` 的族校验旁加 `setForm(f => f.via_node_ids?.length ? { ...f, via_node_ids: [] } : f)`——注意放在 node_id 变化的 effect 中，避免每次渲染重置。
5. 栈标签/出口 v6 提示随链尾：`nodeStack` 的出口能力取链尾节点（无 via 时即入口节点）：

```jsx
  const tailNode = viaChain.length ? nodeById[viaChain[viaChain.length - 1]] : nodeById[Number(form.node_id)]
```

「出口段模式」区块的 `composite` 判定改为 `selNode.node_type === 'composite' || viaChain.length > 0`。
6. `ruleToForm` 加 `via_node_ids: rule.via_node_ids || []`（copyInitial 自动继承）；`ruleFormToPayload` 加 `via_node_ids: (form.via_node_ids || []).map(Number)`。

- [ ] **Step 2: 页面接线**

- `web/src/pages/my/Rules.jsx`：`const { rules = [], nodes = [], node_by_id = {}, show_rate, bindings = [] } = data || {}`；两处 `<RuleFormModal ...>`（创建与编辑，若编辑在 RuleDetail.jsx 则同样）传 `bindings={bindings}`。
- `web/src/pages/rules/List.jsx` 与 `rules/Detail.jsx`（admin）：加载时并行 `api.get('/node-bindings')`，把 `bindings` 传入表单。

- [ ] **Step 3: 构建验证**

Run: `cd web && npm run build`
Expected: 构建通过。再手动冒烟：管理端建两个节点、打角色、绑边、建规则走级联（无自动化 UI 测试，此项目前端无测试基建）。

- [ ] **Step 4: 提交**

```bash
git add web/src/components/RuleFormModal.jsx web/src/pages/my/Rules.jsx web/src/pages/my/RuleDetail.jsx web/src/pages/rules/List.jsx web/src/pages/rules/Detail.jsx
git commit -m "feat(web): cascaded middle-layer picker with chain preview in rule form"
```

---

### Task 11: Web 管理端节点页（角色/绑定管理 + 移除每跳倍率）

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx`（新增 RolesCard、BindingsCard；`CompositeHopsCard` 移除倍率列；组合节点补 rate_multiplier 编辑）
- Modify: `web/src/pages/nodes/List.jsx`（`CompositeNodeModal` 移除倍率输入；列表加角色徽章）
- Modify: `web/src/components/ui.jsx`（可选：角色徽章小组件）

**Interfaces:**
- Consumes: Task 8 的四个端点。

- [ ] **Step 1: RolesCard（Detail.jsx，节点详情通用卡片）**

```jsx
function RolesCard({ node, onDone }) {
  const [roles, setRoles] = useState(node.roles ?? 1)
  const [saving, setSaving] = useState(false)
  const toast = useToast()
  const toggle = (bit) => setRoles(r => r ^ bit)
  const save = async () => {
    if (!roles) { toast('至少保留一个角色', 'error'); return }
    setSaving(true)
    try { await api.post(`/nodes/${node.id}/roles`, { roles }); toast('已保存'); onDone() }
    catch (err) { toast(err.message, 'error') } finally { setSaving(false) }
  }
  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <h2 className="m-0 text-[15px] font-bold mb-1.5">节点角色</h2>
      <p className="text-[12.5px] text-ink-mut mb-2.5">
        入口：可被规则选为入口。中间层：可绑定到上游节点之后，供规则级联选用。可同时勾选。
      </p>
      <div className="flex items-center gap-5">
        {[[1, '入口'], [2, '中间层']].map(([bit, label]) => (
          <label key={bit} className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={(roles & bit) !== 0} onChange={() => toggle(bit)} />{label}
          </label>
        ))}
        <button onClick={save} disabled={saving || roles === (node.roles ?? 1)} className="btn-primary ml-auto">保存</button>
      </div>
    </section>
  )
}
```

- [ ] **Step 2: BindingsCard（仅 roles 含中间层时渲染）**

```jsx
function BindingsCard({ node, allNodes, onDone }) {
  const [rows, setRows] = useState(null) // [{upstream_node_id, mode}]
  const [saving, setSaving] = useState(false)
  const toast = useToast()
  useEffect(() => {
    api.get(`/nodes/${node.id}/bindings`)
      .then(d => setRows((d.bindings || []).map(b => ({ upstream_node_id: b.upstream_node_id, mode: b.mode }))))
      .catch(() => setRows([]))
  }, [node.id])
  if (!rows) return null
  const candidates = allNodes.filter(n => n.id !== node.id)
  const addRow = () => setRows(rs => [...rs, { upstream_node_id: '', mode: 'userspace' }])
  const setRow = (i, k, v) => setRows(rs => rs.map((r, j) => j === i ? { ...r, [k]: v } : r))
  const removeRow = (i) => setRows(rs => rs.filter((_, j) => j !== i))
  const addAll = () => setRows(candidates.map(n => ({ upstream_node_id: n.id, mode: 'userspace' })))
  const save = async () => {
    if (rows.some(r => !r.upstream_node_id)) { toast('请为每一行选择上游节点，或删除空行', 'error'); return }
    setSaving(true)
    try {
      await api.post(`/nodes/${node.id}/bindings`, {
        bindings: rows.map(r => ({ upstream_node_id: Number(r.upstream_node_id), mode: r.mode })),
      })
      toast('已保存'); onDone()
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false) }
  }
  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <div className="flex items-baseline gap-2.5 mb-1.5">
        <h2 className="m-0 text-[15px] font-bold">上游绑定</h2>
        <span className="text-[12.5px] text-ink-mut">{rows.length} 条</span>
      </div>
      <p className="text-[12.5px] text-ink-mut mb-2.5">
        绑定后，选中这些上游（入口或中间层）的规则可以级联接入本节点。模式作用于衔接段
        （上游段尾跳 → 本层首跳）；修改对此后新建的规则生效。
      </p>
      <div className="space-y-2">
        {rows.map((r, i) => (
          <div key={i} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
            <Select className="flex-1" placeholder="-- 选择上游节点 --" searchable value={r.upstream_node_id}
              onChange={v => setRow(i, 'upstream_node_id', v)}
              options={candidates.filter(n => n.id === Number(r.upstream_node_id) || !rows.some((rr, j) => j !== i && Number(rr.upstream_node_id) === n.id)).map(n => ({ value: n.id, label: n.name }))} />
            <Select value={r.mode} onChange={v => setRow(i, 'mode', v)} style={{ width: 120 }}
              options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
            <button type="button" onClick={() => removeRow(i)} className="btn-danger-sm text-xs px-1.5">×</button>
          </div>
        ))}
      </div>
      <div className="flex items-center gap-3 pt-3">
        <button type="button" onClick={addRow} className="btn-secondary text-xs">+ 添加上游</button>
        <button type="button" onClick={addAll} className="btn-secondary text-xs">全选</button>
        <button onClick={save} disabled={saving} className="btn-primary">保存</button>
      </div>
    </section>
  )
}
```

挂载：Detail.jsx 中 `CompositeHopsCard` 挂载点（375-379）附近，对所有节点渲染 `<RolesCard node={node} onDone={reload} />`，对 `(node.roles & 2) !== 0` 渲染 `<BindingsCard node={node} allNodes={allNodesForPicker} onDone={reload} />`（`allNodesForPicker` 用详情接口已有的节点列表或补拉 `/nodes`）。

- [ ] **Step 3: 移除每跳倍率 UI + 组合倍率编辑**

- `List.jsx` `CompositeNodeModal`：删除 mult 输入框（528-530）、`pickNode` 的 mult 播种改为普通 `setHop(i, 'node_id', v)`、payload 里删 `traffic_multiplier`；新增「倍率」单字段（发送 `rate_multiplier`，apiCreateNode 组合分支已支持）。
- `Detail.jsx` `CompositeHopsCard`：删除 mult 输入框（627-629）与 dirty 比较里的 mult 项、payload 里删 `traffic_multiplier`。
- `Detail.jsx` 组合节点的「节点配置」卡（213-223，现仅改名）：加 rate_multiplier 编辑（复用单点节点已有的倍率编辑控件写法，提交 `POST /nodes/{id}/rate-multiplier`）。
- `List.jsx` 列表行名称旁加角色徽章：`{(n.roles & 2) !== 0 && <span className="badge">中间层</span>}`（样式参照 NodeTypeBadge 的写法）。

- [ ] **Step 4: 构建 + 提交**

Run: `cd web && npm run build` → 通过。

```bash
git add web/src/pages/nodes/Detail.jsx web/src/pages/nodes/List.jsx web/src/components/ui.jsx
git commit -m "feat(web): node role editing and upstream binding management"
```

---

### Task 12: 规则链路展示（my + admin）

**Files:**
- Modify: `web/src/pages/my/RuleDetail.jsx`（链路行）
- Modify: `web/src/components/RulesTable.jsx`（节点列显示 via 链）
- Modify: `web/src/pages/rules/Detail.jsx`（admin 各跳表加所属段列，可选）

- [ ] **Step 1: my 侧渲染**

`RuleDetail.jsx` 的「节点」行（123-124）扩为链路：

```jsx
  const viaNames = (rule.via_node_ids || []).map(id => nodeById[id]?.name).filter(Boolean)
  // 展示: 入口名 → via1 → via2（via 均为用户被授权节点，名字可见）
```

值渲染 `viaNames.length ? [entryName, ...viaNames].join(' → ') : entryName`。

`RulesTable.jsx`（84-93 节点名渲染处）：`variant="my"` 时若 `r.via_node_ids?.length`，在节点名后追加 ` +${r.via_node_ids.length}层` 的浅色小字（完整链路在详情页看）。

- [ ] **Step 2: admin 规则详情**

`rules/Detail.jsx` 各跳状态表（119-150）加一列「所属段」：值 `hop.via_node_id`，用响应里的 `node_by_id`（或 nodes map）解析名字；入口段与层段颜色区分可后续再做。

- [ ] **Step 3: 构建 + 提交**

Run: `cd web && npm run build` → 通过。

```bash
git add web/src/pages/my/RuleDetail.jsx web/src/components/RulesTable.jsx web/src/pages/rules/Detail.jsx
git commit -m "feat(web): render via chain on rule list and detail views"
```

---

### Task 13: 收尾验证

- [ ] **Step 1: 全量测试** — `go test ./... 2>&1 | tail -20` → 全 PASS。
- [ ] **Step 2: 前端构建** — `cd web && npm run build` → 通过。
- [ ] **Step 3: 端到端冒烟（本地起面板）** — 建 2 个单点入口 + 1 个组合入口 + 1 个单点中间层 + 1 个组合中间层；打角色、绑边（一条 kernel 一条 userspace）；授权一个用户入口+其一中间层；用户侧验证：级联只出现授权且绑定的层、链路预览正确、无下级时不出框、创建后规则详情链路正确；管理端验证 hops 表溯源列。改绑定边模式后确认旧规则不变、新规则取新模式。
- [ ] **Step 4: spec 对照** — 逐节核对 `docs/superpowers/specs/2026-07-03-node-roles-middle-layer-bindings-design.md`，确认每条要求有落点；发现缺口回补任务。
- [ ] **Step 5: 提交遗漏文件并汇报** — 汇总改动与口径变化（计费一次性切换点）给用户。

---

## Self-Review 记录

- Spec 覆盖：TUI 修复(T1)、roles(T2)、bindings(T3)、via 持久化+溯源(T4)、倍率归入口(T5+T7)、链推导与校验(T6)、计费/压制/限速(T7)、管理端 API(T8)、my 级联数据(T9)、表单级联+预览+动态栈(T10)、节点管理 UI(T11)、链路展示(T12)。防降级（不带字段保留）在 T6；`max_forwards` 只计入口 grant：`apiMyCreateRule` 的 `CountRulesForUserNode` 本就只查入口，无需改动，T6 测试隐含覆盖。
- 类型一致性：`NodeRoleEntry/NodeRoleVia`、`NodeBinding`、`ViaNodeIDs []int64`、`ViaNodeID int64`、`hopsForChain/expandSegment`、`SegmentFirstHops map[int64]map[int]int64` 全计划统一。
- 已知留白（有意）：`apiMyUpdateRule` 与 admin 同构改造需先读函数现状（T6 Step 3 已注明）；daemon 测试中 `d.dialer` 字段名以实码为准（T1 已注明）。这两处是「按实码适配」而非缺设计。
