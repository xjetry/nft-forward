# TUI 可见 panel 段(阶段一) 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 TUI 只读显示 server 下发的 panel 段转发(含所属链路名),解决操作员在节点上"看不到链式/下发规则"的问题。

**Architecture:** 给 `nft.Rule` 增加两个 `omitempty` 的 chain 元信息字段(纯元信息,数据平面不读取);server 的 `buildRules` 在 `forward→nft.Rule` 转换时填充;TUI 读取 daemon 的 `panel` 段并在主列表下方渲染一个只读区块。本阶段**不引入任何编辑能力**(留待后续阶段)。

**Tech Stack:** Go;bubbletea/lipgloss(TUI);SQLite;标准 `testing`。

---

## File Structure

- `internal/nft/nft.go` — `Rule` 结构增加 `ChainID`/`ChainName` 字段(元信息,inert)。
- `internal/nft/nft_test.go` — 验证新字段不影响数据平面、空值 `omitempty`。
- `internal/server/server.go` — `buildRules` 填充 chain 元信息(沿用 tunnel 缓存模式新增 chain 缓存)。
- `internal/server/buildrules_test.go`(新) — 验证链式 forward 带 chain 元信息、独立 forward 不带。
- `internal/tui/tui.go` — `model` 增 `panelRules`;`loadInitialRules` 返回两段;`Run`/`initialModel`/`refresh` 同步;`viewList` 渲染只读 panel 区块。
- `internal/tui/tui_test.go` — 验证 panel 段加载与只读区块渲染。

每个 Task 自成一体、可独立提交。

---

### Task 1: nft.Rule 携带 inert 的 chain 元信息

**Files:**
- Modify: `internal/nft/nft.go:25-37`
- Test: `internal/nft/nft_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/nft/nft_test.go` 顶部 import 补 `"encoding/json"` 和 `"strings"`(当前仅有 `context`/`errors`/`testing`/`resolver`),并新增:

```go
func TestRuleChainMetaIsInert(t *testing.T) {
	// Chain metadata is panel-side only; it must never change data-plane behavior.
	r := Rule{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 200,
		ChainID: 7, ChainName: "seednet-vless"}
	if r.EffectiveMode() != ModeKernel {
		t.Fatalf("chain meta must not affect EffectiveMode, got %q", r.EffectiveMode())
	}
	// A rule without chain metadata must not serialize the fields.
	b, err := json.Marshal(Rule{Proto: "tcp", SrcPort: 100, DestPort: 200})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "chain_id") || strings.Contains(string(b), "chain_name") {
		t.Fatalf("empty chain meta must be omitted, got %s", b)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/nft/ -run TestRuleChainMetaIsInert`
Expected: 编译失败 — `unknown field 'ChainID' in struct literal`。

- [ ] **Step 3: 加字段**

在 `internal/nft/nft.go` 的 `Rule` 结构里,`Mode` 字段之后、结构右括号之前加:

```go
	// ChainID/ChainName are panel-side metadata: when a rule belongs to a relay
	// chain, they identify it so the TUI can show the owning chain and gate
	// which fields are locally editable. The data plane (DNAT / userspace /
	// MergedRuleset / DNS) never reads them.
	ChainID   int64  `json:"chain_id,omitempty"`
	ChainName string `json:"chain_name,omitempty"`
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/nft/ -count=1`
Expected: PASS(包括既有测试,确认加字段无回归)。

- [ ] **Step 5: 提交**

```bash
git add internal/nft/nft.go internal/nft/nft_test.go
git commit -m 'feat(nft): add inert chain metadata fields to Rule'
```

---

### Task 2: buildRules 填充 chain 元信息

**Files:**
- Modify: `internal/server/server.go` — `buildRules`(:218-251) 填充元信息;`computeRev`(:257-262) 排除元信息(否则链路改名等会污染 rev 哈希,触发重连节点无谓的全量重应用)
- Test: `internal/server/buildrules_test.go`(新建)

- [ ] **Step 1: 写失败测试**

新建 `internal/server/buildrules_test.go`:

```go
package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func TestBuildRulesStampsChainMeta(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "hop1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	chainID, err := db.CreateChain(d, &db.Chain{
		Name: "seednet-vless", Proto: "tcp", ExitHost: "exit.example", ExitPort: 8443,
	})
	if err != nil {
		t.Fatal(err)
	}
	// One chain-owned forward and one standalone forward on the same node.
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "10.0.0.2", TargetPort: 20001,
		ChainID: sql.NullInt64{Int64: chainID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.9", TargetPort: 443,
	}); err != nil {
		t.Fatal(err)
	}

	forwards, err := db.ActiveForwardsForPush(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	rules := buildRules(d, forwards)

	var chained, standalone *nft.Rule
	for i := range rules {
		switch rules[i].SrcPort {
		case 20000:
			chained = &rules[i]
		case 30000:
			standalone = &rules[i]
		}
	}
	if chained == nil || standalone == nil {
		t.Fatalf("expected both forwards in rules, got %+v", rules)
	}
	if chained.ChainID != chainID || chained.ChainName != "seednet-vless" {
		t.Fatalf("chain forward should carry meta, got ChainID=%d ChainName=%q",
			chained.ChainID, chained.ChainName)
	}
	if standalone.ChainID != 0 || standalone.ChainName != "" {
		t.Fatalf("standalone forward must have no chain meta, got ChainID=%d ChainName=%q",
			standalone.ChainID, standalone.ChainName)
	}
}

func TestComputeRevIgnoresChainMeta(t *testing.T) {
	// Chain metadata must not change the revision hash: a chain rename must not
	// trigger a redundant re-apply when the data plane is unchanged.
	base := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001}}
	withMeta := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001,
		ChainID: 5, ChainName: "seednet-vless"}}
	if computeRev(base) != computeRev(withMeta) {
		t.Fatalf("chain metadata must not affect rev: %q vs %q", computeRev(base), computeRev(withMeta))
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/server/ -run 'TestBuildRulesStampsChainMeta|TestComputeRevIgnoresChainMeta'`
Expected: FAIL — buildRules 未填充(ChainName 为空),且 computeRev 含元信息(两 rev 不相等)。

- [ ] **Step 3: 在 buildRules 填充 chain 元信息**

在 `internal/server/server.go` 的 `buildRules` 中:在 `tunnels := map[int64]*db.Tunnel{}` 下一行加 chain 缓存:

```go
	chains := map[int64]*db.Chain{}
```

并在构造 `rule` 之后、`if resolver.IsHostname(f.TargetIP)` 之前插入:

```go
		if f.ChainID.Valid {
			rule.ChainID = f.ChainID.Int64
			c, ok := chains[f.ChainID.Int64]
			if !ok {
				c, _ = db.GetChain(d, f.ChainID.Int64)
				if c != nil {
					chains[f.ChainID.Int64] = c
				}
			}
			if c != nil {
				rule.ChainName = c.Name
			}
		}
```

并修改 `computeRev`(`internal/server/server.go:257-262`),在 `json.Marshal` 前排除 chain 元信息,使 rev 只反映数据平面:

```go
func computeRev(rules []nft.Rule) string {
	// Chain metadata is panel-side display info, not part of the data plane;
	// exclude it so a chain rename does not force a redundant re-apply on
	// reconnecting nodes.
	bare := make([]nft.Rule, len(rules))
	for i, r := range rules {
		r.ChainID = 0
		r.ChainName = ""
		bare[i] = r
	}
	h := sha256.New()
	b, _ := json.Marshal(bare)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/server/ -count=1`
Expected: PASS(含既有测试,确认下发路径无回归)。

- [ ] **Step 5: 提交**

```bash
git add internal/server/server.go internal/server/buildrules_test.go
git commit -m 'feat(server): stamp chain id/name onto pushed rules'
```

---

### Task 3: TUI 加载 panel 段(数据层)

**Files:**
- Modify: `internal/tui/tui.go`(`model` 结构、`loadInitialRules`、`Run`、`initialModel`、`refresh`)
- Test: `internal/tui/tui_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/tui/tui_test.go` 新增(该文件已 import `daemonclient`;如缺 `nft` 也补上):

```go
func TestLoadInitialRulesSplitsTuiAndPanel(t *testing.T) {
	fc := &fakeDaemonClient{owners: daemonclient.OwnerRuleset{
		"tui":   {{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		"panel": {{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200,
			ChainID: 5, ChainName: "seednet-vless"}},
	}}
	tuiRules, panelRules, err := loadInitialRules(fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(tuiRules) != 1 || tuiRules[0].SrcPort != 100 {
		t.Fatalf("tui segment wrong: %+v", tuiRules)
	}
	if len(panelRules) != 1 || panelRules[0].ChainName != "seednet-vless" {
		t.Fatalf("panel segment wrong: %+v", panelRules)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/tui/ -run TestLoadInitialRulesSplitsTuiAndPanel`
Expected: 编译失败 — `loadInitialRules` 当前只返回 2 个值(`[]nft.Rule, error`)。

- [ ] **Step 3: 改数据层**

(a) `model` 结构(`internal/tui/tui.go:74-91`)在 `rules []nft.Rule` 下一行加:

```go
	panelRules []nft.Rule // server-pushed segment, shown read-only
```

(b) `loadInitialRules`(:117-127)整体替换为:

```go
// loadInitialRules fetches the local (tui) and server-pushed (panel) segments
// from the daemon. nil segments become empty slices so the rest of the TUI
// does not have to nil-check.
func loadInitialRules(client daemonClient) (tui []nft.Rule, panel []nft.Rule, err error) {
	owners, err := client.GetRuleset()
	if err != nil {
		return nil, nil, fmt.Errorf("加载规则失败: %w", err)
	}
	tui = owners["tui"]
	if tui == nil {
		tui = []nft.Rule{}
	}
	panel = owners["panel"]
	if panel == nil {
		panel = []nft.Rule{}
	}
	return tui, panel, nil
}
```

(c) `Run`(:95-103)替换为:

```go
func Run(client daemonClient) error {
	rules, panelRules, err := loadInitialRules(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, rules, panelRules), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
```

(d) `initialModel`(:105-112)替换为:

```go
func initialModel(client daemonClient, rules, panelRules []nft.Rule) model {
	return model{
		mode:       viewList,
		rules:      rules,
		panelRules: panelRules,
		inputs:     buildInputs(),
		client:     client,
	}
}
```

(e) `refresh`(:517-529)替换为:

```go
func (m *model) refresh() {
	owners, err := m.client.GetRuleset()
	if err != nil {
		m.err = err.Error()
		return
	}
	tui := owners["tui"]
	if tui == nil {
		tui = []nft.Rule{}
	}
	m.rules = tui
	panel := owners["panel"]
	if panel == nil {
		panel = []nft.Rule{}
	}
	m.panelRules = panel
	m.status = "已从 daemon 重新加载"
}
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/tui/ -count=1`
Expected: PASS。若编译报某处仍按旧签名调用 `loadInitialRules`/`initialModel`,按上面新签名更新该调用点后重跑。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m 'feat(tui): load the panel segment alongside the tui segment'
```

---

### Task 4: TUI 渲染只读 panel 区块(显示层)

**Files:**
- Modify: `internal/tui/tui.go`(`viewList`,:617-675)
- Test: `internal/tui/tui_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/tui/tui_test.go` 新增(确保已 import `"strings"`):

```go
func TestViewListRendersReadOnlyPanelSection(t *testing.T) {
	m := model{
		mode:  viewList,
		width: 100,
		rules: []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		panelRules: []nft.Rule{
			{Proto: "tcp", SrcPort: 44751, DestIP: "104.251.236.89", DestPort: 42421,
				ChainName: "seednet-vless"},
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
		},
	}
	out := m.View()
	if !strings.Contains(out, "seednet-vless") {
		t.Fatalf("panel section should show the chain name, got:\n%s", out)
	}
	if !strings.Contains(out, "server 托管") {
		t.Fatalf("panel section should label server-managed rules, got:\n%s", out)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/tui/ -run TestViewListRendersReadOnlyPanelSection`
Expected: FAIL — 输出不含 "seednet-vless"/"server 托管"(尚未渲染 panel 区)。

- [ ] **Step 3: 渲染 panel 只读区块**

在 `internal/tui/tui.go` 的 `viewList` 中,定位主列表 `if len(m.rules) == 0 { ... } else { ... }` 块结束的 `}`(:664)之后、`b.WriteString("\n")`(:666,状态区)之前,插入:

```go
	// Read-only view of server-pushed (panel) forwards: managed by the panel,
	// not editable here. Chain hops show their owning chain; standalone
	// panel forwards are tagged generically.
	if len(m.panelRules) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("server 托管转发（只读）") + "\n")
		for _, r := range m.panelRules {
			target := r.DestIP
			if r.DestHost != "" {
				target = r.DestHost
			}
			tag := "server 托管"
			if r.ChainName != "" {
				tag = "链路 " + r.ChainName
			}
			proto := strings.ToLower(r.Proto)
			if r.EffectiveMode() == nft.ModeUserspace {
				proto += " (U)"
			}
			line := fmt.Sprintf("  %s  %d → %s:%d  [%s]", proto, r.SrcPort, target, r.DestPort, tag)
			b.WriteString(helpStyle.Render(line) + "\n")
		}
	}
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/tui/ -count=1`
Expected: PASS。

- [ ] **Step 5: 全量回归 + 提交**

```bash
go build ./... && go test ./... -count=1
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m 'feat(tui): render server-managed forwards as a read-only section'
```

Expected: 全部 PASS。

---

## Self-Review

**Spec 覆盖:** 阶段一在 spec「分阶段实现 · 阶段一」的三项改动——`nft.Rule` 元信息(Task 1)、server 填充(Task 2)、TUI 合并显示标注(Task 3+4)——均有对应 Task。编辑/同步(阶段二、三)明确不在本计划。

**占位扫描:** 无 TBD/TODO;每个代码步骤均给出完整代码与精确插入位置。

**类型一致性:** `loadInitialRules` 新签名 `(tui, panel []nft.Rule, err error)` 在 `Run`(Task 3c)同步;`model.panelRules`(Task 3a)在 `refresh`(Task 3e)与 `viewList`(Task 4)一致使用;`nft.Rule.ChainID int64`/`ChainName string`(Task 1)在 buildRules(Task 2)与 TUI(Task 3/4)一致。

**边界:** panel 区块渲染独立于 `len(m.rules)==0` 分支,故 tui 段为空、仅有 panel 段时也会显示;操作键(`a/e/d/c`)只作用于 `m.rules`,天然不触及 `panelRules`,无需额外加锁防护。
