# TUI 新增「用户」列 + 列间距修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **派发 subagent 时必须传达:** 代码注释、commit message 中绝对禁止出现执行过程信息(Task/Phase/Step 编号、方案代号、审阅轮次等)。注释只解释 WHY 与不变量。

**Goal:** TUI 统一列表新增「用户」列展示 server 下发规则所属租户,并统一加宽列间距,根治长目标与相邻列粘连。

**Architecture:** tenant 名作为 `nft.Rule` 的展示元信息(与 ChainID/ChainName 同等),由 server `buildRules` 填充、`computeRev` 排除;TUI 引入统一的列尾间距 helper 并新增「用户」列。

**Tech Stack:** Go,bubbletea/lipgloss TUI,SQLite。

依赖顺序:Task 1(nft 字段)→ Task 2(server 填充 + rev 排除)→ Task 3(TUI 列 + 间距)。

---

## Task 1: nft.Rule 增加 TenantName 展示元信息

**Files:**
- Modify: `internal/nft/nft.go:41-42`(`ChainName` 之后)
- Test: `internal/nft/nft_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/nft/nft_test.go` 末尾追加:

```go
func TestRule_TenantNameRoundTripAndOmitempty(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, TenantName: "qqpw"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tenant_name":"qqpw"`) {
		t.Fatalf("tenant_name not marshaled: %s", b)
	}
	var got Rule
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.TenantName != "qqpw" {
		t.Fatalf("tenant_name round-trip mismatch: %q", got.TenantName)
	}

	// Empty tenant must be omitted from the wire (display-only metadata).
	bare, _ := json.Marshal(Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80})
	if strings.Contains(string(bare), "tenant_name") {
		t.Fatalf("empty tenant_name should be omitted, got: %s", bare)
	}
}
```

确认 `nft_test.go` 的 import 含 `encoding/json` 和 `strings`(若缺则补上)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/ -run 'TestRule_TenantNameRoundTripAndOmitempty' -v`
Expected: 编译失败 —— `unknown field TenantName`

- [ ] **Step 3: 加字段**

`internal/nft/nft.go` 的 `Rule` struct,把 ChainID/ChainName 那段注释与字段改为:

```go
	// ChainID/ChainName/TenantName are panel-side metadata: when a rule
	// belongs to a relay chain they identify it so the TUI can show the
	// owning chain and gate which fields are locally editable; TenantName
	// names the owning tenant for display. The data plane (DNAT / userspace /
	// MergedRuleset / DNS) never reads them.
	ChainID    int64  `json:"chain_id,omitempty"`
	ChainName  string `json:"chain_name,omitempty"`
	TenantName string `json:"tenant_name,omitempty"`
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/nft/ -run 'TestRule_TenantNameRoundTripAndOmitempty' -v`
Expected: PASS

- [ ] **Step 5: 全 nft 包回归 + gofmt + 提交**

Run: `go test ./internal/nft/`
Expected: 全 PASS(加 omitempty 字段不影响既有 apply/merge/validate)

```bash
gofmt -w internal/nft/nft.go
git add internal/nft/nft.go internal/nft/nft_test.go
git commit -m "feat(nft): add TenantName display metadata to Rule"
```

---

## Task 2: server 填充 TenantName + computeRev 排除

**Files:**
- Modify: `internal/server/server.go:218-265`(`buildRules`)、`:275-281`(`computeRev`)
- Test: `internal/server/buildrules_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/server/buildrules_test.go` 末尾追加:

```go
func TestBuildRules_FillsTenantName(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	tid, err := db.CreateTenant(d, &db.Tenant{Name: "qqpw"})
	if err != nil {
		t.Fatal(err)
	}
	withTenant := &db.Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 17171, TargetIP: "72.234.229.145", TargetPort: 17171, TenantID: sql.NullInt64{Int64: tid, Valid: true}}
	noTenant := &db.Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 18000, TargetIP: "10.0.0.1", TargetPort: 18000}

	rules := buildRules(d, []*db.Forward{withTenant, noTenant})
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	if rules[0].TenantName != "qqpw" {
		t.Fatalf("tenant forward should carry tenant name, got %q", rules[0].TenantName)
	}
	if rules[1].TenantName != "" {
		t.Fatalf("tenant-less forward should leave TenantName empty, got %q", rules[1].TenantName)
	}
}

func TestComputeRev_ExcludesTenantName(t *testing.T) {
	base := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, TenantName: "alpha"}}
	renamed := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, TenantName: "beta"}}
	if computeRev(base) != computeRev(renamed) {
		t.Fatal("tenant rename must not change rev (display-only metadata)")
	}
}
```

确认 `buildrules_test.go` 的 import 含 `database/sql`、`nft-forward/internal/db`、`nft-forward/internal/nft`(按缺失补上)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestBuildRules_FillsTenantName|TestComputeRev_ExcludesTenantName' -v`
Expected: FAIL —— `TenantName` 未被填充(为空);computeRev 因 TenantName 不同而 rev 不同

- [ ] **Step 3: buildRules 填充 TenantName**

`internal/server/server.go` 的 `buildRules`,在 `chains := map[int64]*db.Chain{}` 旁加 tenants 缓存,并在填完 chain 元信息后填 tenant 名。具体:

在 `chains := map[int64]*db.Chain{}` 之后加一行:

```go
	chains := map[int64]*db.Chain{}
	tenants := map[int64]*db.Tenant{}
```

在 `if f.ChainID.Valid { ... }` 块之后(`if resolver.IsHostname(...)` 之前)插入:

```go
		if f.TenantID.Valid {
			tn, ok := tenants[f.TenantID.Int64]
			if !ok {
				tn, _ = db.GetTenant(d, f.TenantID.Int64)
				if tn != nil {
					tenants[f.TenantID.Int64] = tn
				}
			}
			if tn != nil {
				rule.TenantName = tn.Name
			}
		}
```

- [ ] **Step 4: computeRev 排除 TenantName**

`internal/server/server.go` 的 `computeRev`,在归零 chain 元信息处补一行:

```go
	for i, r := range rules {
		r.ChainID = 0
		r.ChainName = ""
		r.TenantName = ""
		bare[i] = r
	}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -run 'TestBuildRules_FillsTenantName|TestComputeRev_ExcludesTenantName' -v`
Expected: PASS

- [ ] **Step 6: 全 server 包回归 + gofmt + 提交**

Run: `go test ./internal/server/`
Expected: 全 PASS

```bash
gofmt -w internal/server/server.go
git add internal/server/server.go internal/server/buildrules_test.go
git commit -m "feat(server): stamp tenant name onto pushed rules for display"
```

---

## Task 3: TUI 新增「用户」列 + 统一列尾间距

**Files:**
- Modify: `internal/tui/tui.go:644-657`(列宽常量)、`:688-698`(renderTableRow)、`:700-746`(viewList)
- Test: `internal/tui/tui_test.go`

### 循环 A — padCol helper + 列宽 + renderTableRow 间距

- [ ] **Step 1: 写失败测试**

在 `internal/tui/tui_test.go` 末尾追加:

```go
func TestRenderTableRow_ColumnsHaveTrailingGap(t *testing.T) {
	// A destination that fills its column must still leave a visible gap before
	// the remote-port column (root cause of "seednet.xjetry.fun8443").
	longDest := "seednet.xjetry.fun"
	line := stripANSI(renderTableRow("tcp", "42421", longDest, "8443", "x"))
	// The dest cell occupies colDest cells; its content must be followed by at
	// least colGap spaces before the dstPort digits.
	idx := strings.Index(line, longDest)
	if idx < 0 {
		t.Fatalf("dest not rendered intact: %q", line)
	}
	after := line[idx+len(longDest):]
	if !strings.HasPrefix(after, strings.Repeat(" ", colGap)) {
		t.Fatalf("dest must be followed by >=%d spaces, got %q", colGap, after)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestRenderTableRow_ColumnsHaveTrailingGap' -v`
Expected: 编译失败 —— `undefined: colGap`(且 colDest=18 容不下 18-cell 域名 + gap)

- [ ] **Step 3: 更新列宽常量 + 加 colGap/colTenant**

把 `internal/tui/tui.go` 的列宽常量块替换为:

```go
// Column widths in terminal cells (CJK double-width characters count as 2 cells).
// These constants must match between the header and every data row. Each fixed
// column reserves colGap trailing cells (content is truncated to width-colGap)
// so adjacent columns never visually merge, even when content fills the width.
const (
	colGap     = 2
	colOwner   = 16 // 本地 / server / 链路 X（链路名过长则截断）
	colTenant  = 12 // 租户名 / —
	colProto   = 10 // tcp+udp / tcp (U)
	colSrcPort = 12 // 65535
	colDest    = 24 // IPv4(15) 或常见域名 + gap
	colDstPort = 12 // 65535
	// colComment is flexible — it consumes the remainder of the line.

	// colMargin is the horizontal margin (in cells) on each side of the viewport.
	colMargin = 2
)

// padCol renders s into a fixed colW-cell column, truncating content to
// colW-colGap so at least colGap trailing cells separate it from the next
// column.
func padCol(s string, colW int) string {
	return cellStyle(colW).Render(truncateCell(s, colW-colGap))
}
```

- [ ] **Step 4: renderTableRow 改用 padCol**

把 `renderTableRow` 替换为:

```go
// renderTableRow assembles a fixed-width table row from five cell strings:
// proto, srcPort, dest, dstPort (each a gap-padded fixed column) and comment
// (flexible, already styled by the caller). The assembled line carries no
// styling of its own — callers apply row styles after.
func renderTableRow(proto, srcPort, dest, dstPort, comment string) string {
	return padCol(proto, colProto) +
		padCol(srcPort, colSrcPort) +
		padCol(dest, colDest) +
		padCol(dstPort, colDstPort) +
		comment
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestRenderTableRow_ColumnsHaveTrailingGap' -v`
Expected: PASS

### 循环 B — viewList 新增「用户」列 + commentWidth 保护

- [ ] **Step 6: 改/写测试**

把 `internal/tui/tui_test.go` 中的 `TestViewListRendersUnifiedSegments` 替换为(新增用户列断言):

```go
func TestViewListRendersUnifiedSegments(t *testing.T) {
	m := model{
		mode:  viewList,
		width: 140,
		rules: []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		panelRules: []nft.Rule{
			{Proto: "tcp", SrcPort: 17171, DestIP: "72.234.229.145", DestPort: 17171, TenantName: "qqpw", Comment: "ss"},
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
		},
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "来源") || !strings.Contains(out, "用户") {
		t.Fatalf("header must include 来源 and 用户 columns, got:\n%s", out)
	}
	if !strings.Contains(out, "本地") {
		t.Fatalf("tui row should be tagged 本地, got:\n%s", out)
	}
	if !strings.Contains(out, "server") {
		t.Fatalf("tenant-less panel row should be tagged server, got:\n%s", out)
	}
	if !strings.Contains(out, "qqpw") {
		t.Fatalf("tenant panel row should show its tenant in the 用户 column, got:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("rows without a tenant should show — in the 用户 column, got:\n%s", out)
	}
}

func TestViewListClampsCommentWidthOnNarrowTerminal(t *testing.T) {
	// Wide fixed columns must not drive commentWidth negative on a narrow term.
	m := model{
		mode:       viewList,
		width:      40,
		panelRules: []nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}},
	}
	_ = m.View() // must not panic
}
```

- [ ] **Step 7: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestViewListRendersUnifiedSegments|TestViewListClampsCommentWidthOnNarrowTerminal' -v`
Expected: FAIL —— 无「用户」列/「—」；窄终端 commentWidth 为负(cellStyle 负宽异常)

- [ ] **Step 8: viewList 加用户列 + commentWidth 下限**

把 `viewList` 中 `else {` 分支内、从 header 到行循环结束的部分替换为:

```go
	} else {
		header := padCol("来源", colOwner) +
			padCol("用户", colTenant) +
			renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
		b.WriteString(headerStyle.Render(header) + "\n")

		fixedWidth := colOwner + colTenant + colProto + colSrcPort + colDest + colDstPort
		const minComment = 10
		innerWidth := m.width - 2*colMargin
		if innerWidth < fixedWidth+minComment {
			// Narrow terminal: keep a minimum comment column so commentWidth
			// never goes negative; the row may exceed the viewport and the
			// terminal soft-wraps, which is preferable to a broken render.
			innerWidth = fixedWidth + minComment
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
			tenantTag := "—"
			if r.TenantName != "" {
				tenantTag = r.TenantName
			}
			line := padCol(ownerTag, colOwner) +
				padCol(tenantTag, colTenant) +
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
```

- [ ] **Step 9: 跑测试确认通过**

Run: `go test ./internal/tui/ -run 'TestViewListRendersUnifiedSegments|TestViewListClampsCommentWidthOnNarrowTerminal' -v`
Expected: PASS

### 循环 C — 修复因列宽变化而失败的既有测试

- [ ] **Step 10: 跑全 tui 包,定位受列宽影响的既有测试**

Run: `go test ./internal/tui/ 2>&1 | tail -30`
Expected: 可能有既有测试失败 —— 它们硬编码了旧的 `colProto+colSrcPort+colDest+colDstPort = 46` 或断言内容占满列宽。具体涉及 `TestRenderTableRow_ColumnAlignment`、`TestRenderTableRow_FiveColumns`、`TestViewList_ColumnConsistency`、`TestColProtoFitsLongestOption`、`TestRenderTableRow_TruncationEllipsis`。

- [ ] **Step 11: 更新这些测试的期望值**

逐个按新常量修正:
- 凡用字面量 `46` 或注释「58 cells」表达固定列总宽处,改为用 `colProto + colSrcPort + colDest + colDstPort` 表达式(避免再硬编码数字)。
- `TestColProtoFitsLongestOption`:它断言 `colProto >= width("TCP+UDP")`(7),新 colProto=10 仍满足,无需改;若它顺带断言了内容占满,放宽到「内容区 = colProto-colGap >= 7」。
- 涉及「目标 cell 内容紧贴下一列」的断言改为「内容后有 colGap 空格」(参照循环 A 的 `TestRenderTableRow_ColumnsHaveTrailingGap` 写法)。
- `TestRenderTableRow_TruncationEllipsis`:截断阈值从 colDest 变为 colDest-colGap,更新触发截断的输入长度与期望。

逐处只调整期望值/阈值,不弱化「header 与每行固定列宽一致」这一核心断言。

- [ ] **Step 12: 跑全 tui 包确认全绿**

Run: `go test ./internal/tui/`
Expected: 全 PASS

- [ ] **Step 13: gofmt + 提交**

```bash
gofmt -w internal/tui/tui.go internal/tui/tui_test.go
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "feat(tui): add tenant column and uniform column spacing"
```

---

## 收尾验证

- [ ] Run: `go test ./... && go vet ./... && go build ./...`，全绿。

完成后按 superpowers:finishing-a-development-branch 处理分支。

---

## 自检(Self-Review)

**Spec 覆盖**(对照 `docs/superpowers/specs/2026-06-04-tui-tenant-column-design.md`):
- nft.Rule TenantName 元信息 → Task 1 ✓
- buildRules 填充(仿 chains 缓存)→ Task 2 ✓
- computeRev 排除 → Task 2 ✓
- TUI 用户列(空显 —)→ Task 3 循环 B ✓
- 统一列尾间距(colGap + padCol)+ 加宽 colDest → Task 3 循环 A ✓
- 列顺序 来源|用户|协议|本机端口|目标|远程端口|备注 → Task 3 循环 B ✓

**类型一致性:**
- `nft.Rule.TenantName string` —— Task 1 定义 / Task 2 填充 / Task 3 读取一致 ✓
- `padCol(s string, colW int) string` —— Task 3 循环 A 定义,renderTableRow + viewList 调用一致 ✓
- `db.GetTenant(d, id) (*db.Tenant, error)` + `Tenant.Name` —— Task 2 用法与现有签名一致 ✓

**非目标**:不动「来源」取值;不处理链式 hop 的 chain_id 为空现象;不展示租户其他属性。
