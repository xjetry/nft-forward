# 规则错误与节点状态解耦 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让单条规则的目标无法解析不再把节点状态染成"错误"；节点保持正常并以琥珀警告呈现坏规则，同时在创建/编辑入口拦截语法即错的目标地址。

**Architecture:** agent 的面板下发路径（`SetPanelRuleset` → `reconcileOwners`）从"任一规则解析失败即整份拒绝"改为"尽力而为、只下发能解析的、把无法解析的作为返回值上交"（与 bootstrap 既有行为对齐）。被跳过的规则汇成警告字符串，沿 `ApplyAck.Warning` → `Dispatch` → `MarkNodeApplied` 回传，落到新增的 `nodes.last_warning`，前端显示琥珀（非红）。语法校验（`PlausibleHostname`）在 `parseExit`（面板）与本地建/改规则处拦截 `4212` 类目标。

**Tech Stack:** Go 1.26（`go test ./internal/...`）、SQLite（`internal/db/migrations/*.sql` 顺序迁移）、React + Vite（`web/`）。

**Design spec:** `docs/superpowers/specs/2026-07-02-rule-error-node-status-decoupling-design.md`

## Global Constraints

- 模块路径 `nft-forward`；测试用 `go test ./internal/...`，构建用 `go build ./...`。
- **禁止**在代码注释 / commit message 中出现任务编号、方案代号、审阅轮次等过程信息；注释只解释 WHY 与不变量。
- 版本号规则与本改动无关，不触发发版。
- 给 `nodes` 表加列必须**三处对齐**：`nodeCols` 常量（被两个 SELECT 共用）+ `scanNode` 的 `Scan` + `grants.go` 内联 `Scan`；漏掉任一处会静默错位/清空列表。
- `last_error`（红）优先于 `last_warning`（琥珀）；干净成功清两者，成功但有跳过则清 error、置 warning，下发硬失败清 warning、置 error。

---

### Task 1: resolver.PlausibleHostname

**Files:**
- Modify: `internal/resolver/resolver.go`
- Test: `internal/resolver/resolver_test.go`

**Interfaces:**
- Produces: `func PlausibleHostname(s string) bool` — 当 `s` 可能作为 DNS 名解析时为 true。IP 返回 false（IP 由调用方另行判定），空串 / 含非法字符 / 去尾点后末标签全数字（如 `4212`、`1.2.3.999`）返回 false。

- [ ] **Step 1: 写失败测试**

在 `internal/resolver/resolver_test.go` 末尾追加：

```go
func TestPlausibleHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"example.com", true},
		{"sub.example.com.", true}, // 尾点 FQDN
		{"x-1.example.org", true},
		{"localhost", true},
		{"", false},
		{"4212", false},      // 纯数字端口误填为 host
		{"1.2.3.999", false}, // 末标签全数字（打错的 IP）
		{"1.2.3.4", false},   // 合法 IP 不是 hostname
		{"bad_host!", false}, // 非法字符
	}
	for _, c := range cases {
		if got := PlausibleHostname(c.in); got != c.want {
			t.Errorf("PlausibleHostname(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/resolver/ -run TestPlausibleHostname -v`
Expected: FAIL，`undefined: PlausibleHostname`

- [ ] **Step 3: 实现**

在 `internal/resolver/resolver.go` 的 import 块加入 `"strings"`，并在 `IsHostname` 之后追加：

```go
// PlausibleHostname reports whether s could ever resolve as a DNS name. It
// builds on IsHostname (which already rejects IPs and illegal characters) and
// additionally rejects a name whose rightmost label is all-numeric: a numeric
// TLD can never exist, so such a string is a user error (a bare port like
// "4212", or a mistyped address like "1.2.3.999") rather than a resolvable host.
func PlausibleHostname(s string) bool {
	if !IsHostname(s) {
		return false
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return false
	}
	labels := strings.Split(s, ".")
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/resolver/ -v`
Expected: PASS（含既有 `TestIsHostname`）

- [ ] **Step 5: 提交**

```bash
git add internal/resolver/resolver.go internal/resolver/resolver_test.go
git commit -m "feat(resolver): add PlausibleHostname to reject numeric-label hosts"
```

---

### Task 2: parseExit 出口地址语法校验

**Files:**
- Modify: `internal/server/shared.go:119-134` (`parseExit`)
- Test: `internal/server/shared_test.go`（若不存在则新建）

**Interfaces:**
- Consumes: `resolver.PlausibleHostname` (Task 1)
- Produces: `parseExit(raw string) (string, int, error)` 签名不变；对既非合法 IP、又非 plausible 域名的 host 返回错误。

- [ ] **Step 1: 写失败测试**

若 `internal/server/shared_test.go` 不存在则新建，文件头：

```go
package server

import "testing"

func TestParseExit(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"1.2.3.4:80", false},
		{"example.com:443", false},
		{"[2001:db8::1]:80", false},
		{"4212:80", true},  // 纯数字 host —— 被误填的端口
		{"host:0", true},   // 端口非法
		{":80", true},      // host 空
		{"nohostport", true},
	}
	for _, c := range cases {
		_, _, err := parseExit(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseExit(%q) err = %v, wantErr = %v", c.raw, err, c.wantErr)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestParseExit -v`
Expected: FAIL，`parseExit("4212:80")` 未报错（当前实现接受任意非空 host）

- [ ] **Step 3: 实现**

确认 `internal/server/shared.go` import 块含 `"nft-forward/internal/resolver"`（若无则加）。在 `parseExit` 中 `if host == ""` 判空之后、`return host, port, nil` 之前插入：

```go
	if net.ParseIP(host) == nil && !resolver.PlausibleHostname(host) {
		return "", 0, fmt.Errorf("出口地址非法：%q 不是合法 IP 或域名", host)
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/server/ -run TestParseExit -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/server/shared.go internal/server/shared_test.go
git commit -m "feat(server): reject non-IP, non-hostname exit targets at rule creation"
```

---

### Task 3: reconcileOwners 统一容错（核心）

**Files:**
- Modify: `internal/daemon/dns.go`（加 `partitionResolved`，删 `requireResolvedHosts`）
- Modify: `internal/daemon/handlers.go:93-147` (`reconcileOwners`)，`:333`/`:384`/`:452`（本地建/改/删 tui 分支 + 建/改的语法校验）
- Modify: `internal/daemon/daemon.go:167-198`（bootstrap 复用 helper）、`:341`/`:367`（调用者 arity）
- Test: `internal/daemon/handlers_test.go`（改写 `TestCreateRule_RejectsUnresolvableHost`，新增两个测试）

**Interfaces:**
- Consumes: `resolver.PlausibleHostname` (Task 1)
- Produces:
  - `func partitionResolved(rules []nft.Rule) (applyable, unresolved []nft.Rule)` — 按 `DestHost=="" || DestIP!=""` 切分。
  - `reconcileOwners(ctx, mutate, metaFn, saveToDisk bool) (resolved, unresolved []nft.Rule, committed bool, err error)` — 新增 `unresolved` 返回；解析失败不再致命，只下发 applyable，把 unresolved 上交。

- [ ] **Step 1: 改写/新增失败测试**

在 `internal/daemon/handlers_test.go` 中，把 `TestCreateRule_RejectsUnresolvableHost`（现约 430-442 行）整体替换为下面两个测试（新契约：语法合法但暂不可解析 → 接受并跳过；语法即错 → 拒绝）：

```go
func TestCreateRule_AcceptsUnresolvableButValidHost(t *testing.T) {
	fake := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = fake
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		// nowhere.invalid 语法合法但解析不了：保持 DestIP 为空并报聚合错误
		return in, false, fmt.Errorf("dns: nowhere.invalid: no such host")
	}
	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "nowhere.invalid", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("status = %d, want 2xx (tolerant): %s", w.Code, w.Body.String())
	}
	// 规则入库（供刷新循环重试），但未下发到数据面（DestIP 空被跳过）
	if len(d.owners["tui"]) != 1 {
		t.Fatalf("rule should be stored in tui segment, got %+v", d.owners["tui"])
	}
	if len(fake.nftCalls) != 0 {
		t.Fatalf("unresolved rule must not reach dataplane, got %d apply calls", len(fake.nftCalls))
	}
}

func TestCreateRule_RejectsSyntacticallyInvalidHost(t *testing.T) {
	d := newTestDaemon(t)
	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "4212", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for numeric host", w.Code)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/daemon/ -run 'TestCreateRule_AcceptsUnresolvableButValidHost|TestCreateRule_RejectsSyntacticallyInvalidHost' -v`
Expected: FAIL —— Accepts 用例当前返回 400（严格拒绝），Rejects 用例当前返回 2xx（无语法校验）

- [ ] **Step 3: 加 partitionResolved、删 requireResolvedHosts**

在 `internal/daemon/dns.go` 中删除 `requireResolvedHosts`（现 27-37 行）整段，替换为：

```go
// partitionResolved splits rules into those safe to apply now (a literal IP,
// or a hostname that already resolved to an IP) and those still unresolved (a
// hostname with no IP yet). Unresolved rules are held back so one bad target
// can never block the rest; the refresh loop retries them, and callers that
// care surface them (e.g. as a node-level warning).
func partitionResolved(rules []nft.Rule) (applyable, unresolved []nft.Rule) {
	for _, r := range rules {
		if r.DestHost == "" || r.DestIP != "" {
			applyable = append(applyable, r)
		} else {
			unresolved = append(unresolved, r)
		}
	}
	return
}
```

- [ ] **Step 4: reconcileOwners 改为容错**

在 `internal/daemon/handlers.go` 把 `reconcileOwners` 的签名与解析/应用段改为（保留其余 d.mu 快照/commit 逻辑不变）：

签名行（现 93-98 行）改为：

```go
func (d *Daemon) reconcileOwners(
	ctx context.Context,
	mutate func(OwnerRuleset),
	metaFn func(*AgentMeta),
	saveToDisk bool,
) (resolved []nft.Rule, unresolved []nft.Rule, committed bool, err error) {
```

把现 108-146 行（`merged, err := ...` 到函数末）整体替换为：

```go
	merged, err := MergedRuleset(candidate)
	if err != nil {
		return nil, nil, false, err
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resolved, _, rerr := d.resolveFn(rctx, merged)
	cancel()
	// DNS is best-effort: resolveFn returns the best-effort slice even on
	// partial failure (unresolved hostnames keep their previous DestIP, empty
	// if never resolved). Hold back the still-unresolved ones instead of
	// failing the whole apply, so one bad target can't red the whole node.
	applyable, unresolved := partitionResolved(resolved)
	if len(unresolved) > 0 {
		log.Printf("reconcile: holding back %d rule(s) with unresolved target (retry in refresh loop): %v", len(unresolved), rerr)
	}

	// DNS-refresh callers skip apply+commit when nothing moved.
	if !saveToDisk && !rulesDiffer(prev, applyable) {
		return applyable, unresolved, false, nil
	}

	if err := d.applySerialized(ctx, applyable); err != nil {
		return nil, unresolved, false, fmt.Errorf("apply: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	meta := d.meta
	if metaFn != nil {
		metaFn(&meta)
	}
	if saveToDisk {
		if err := SaveState(d.statePath, candidate, meta); err != nil {
			return nil, unresolved, false, fmt.Errorf("save state: %w", err)
		}
	}
	d.owners = candidate
	d.meta = meta
	d.lastResolved = append([]nft.Rule(nil), applyable...)
	return applyable, unresolved, true, nil
```

确认 `handlers.go` 已 import `"log"`（`reconcileOwners` 现无 log，需要加；文件其它函数是否用 log 决定是否已在 import 块——若缺则加 `"log"`）。

- [ ] **Step 5: 更新 reconcileOwners 全部调用者 arity + 本地建/改语法校验**

`internal/daemon/dns.go` 的 `refreshOnce`（现 44 行）：

```go
	_, _, _, err := d.reconcileOwners(ctx, nil, nil, false)
```

`internal/daemon/daemon.go` 的 `SetPanelRuleset`（现 341 行，本任务暂不改其签名，忽略 unresolved）：

```go
	_, _, _, err := d.reconcileOwners(ctx,
```

`internal/daemon/daemon.go` 的 `clearTuiSegment`（现 367 行）：

```go
	_, _, _, err := d.reconcileOwners(ctx,
```

`internal/daemon/handlers.go` 的 `handleDeleteRule`（现 452 行）：

```go
	_, _, _, err := d.reconcileOwners(r.Context(),
```

`handleCreateRule`（现 333 行附近）：在构造 `rule` 之前、`if ip := net.ParseIP(req.ExitHost); ...` 处加语法校验，并更新调用 arity。把现 327-336 行替换为：

```go
	if net.ParseIP(req.ExitHost) == nil {
		if !resolver.PlausibleHostname(req.ExitHost) {
			http.Error(w, "出口地址非法："+req.ExitHost+" 不是合法 IP 或域名", http.StatusBadRequest)
			return
		}
		rule.DestHost = req.ExitHost
	} else {
		rule.DestIP = req.ExitHost
	}

	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			candidate["tui"] = append(candidate["tui"], rule)
		}, nil, true)
```

（注意：原代码是先 `if ip := net.ParseIP...` 再 `reconcileOwners`；替换后保持顺序。`rule` 变量已在上方声明。）

`handleUpdateRule`（现 384 行附近）：在 mutate 里改 ExitHost 之前，函数入口加一次校验；把现 384 行 `_, _, err :=` 改为 `_, _, _, err :=`，并在 `if ruleID, ok := parseRuleID(id); ok {` 之前（本地 hex 分支进入后）加：

```go
	if req.ExitHost != "" && net.ParseIP(req.ExitHost) == nil && !resolver.PlausibleHostname(req.ExitHost) {
		http.Error(w, "出口地址非法："+req.ExitHost+" 不是合法 IP 或域名", http.StatusBadRequest)
		return
	}
```

把这段放在"Local hex ID: update in tui segment"注释之前、`found := false` 之上（即已确认走本地分支之后）。

确认 `internal/daemon/handlers.go` import 块含 `"nft-forward/internal/resolver"`（若缺则加）。

- [ ] **Step 6: bootstrap 复用 partitionResolved**

`internal/daemon/daemon.go` 现 176-184 行的内联过滤：

```go
		var applyable []nft.Rule
		for _, r := range resolved {
			if r.DestHost == "" || r.DestIP != "" {
				applyable = append(applyable, r)
			}
		}
		resolved = applyable
```

替换为：

```go
		applyable, _ := partitionResolved(resolved)
		resolved = applyable
```

- [ ] **Step 7: 运行相关测试确认通过**

Run: `go test ./internal/daemon/ -run 'TestCreateRule|TestBootstrap|TestRefresh|TestHandler_CreateRule|TestHandler_UpdateRule|TestHandler_DeleteRule' -v`
Expected: PASS（含改写后的两个 create 测试、既有 bootstrap/refresh 测试）

- [ ] **Step 8: 全 daemon 包 + 构建**

Run: `go test ./internal/daemon/ && go build ./...`
Expected: PASS / 无错误

- [ ] **Step 9: 提交**

```bash
git add internal/daemon/
git commit -m "fix(daemon): apply best-effort and hold back unresolved rules instead of failing the whole apply"
```

---

### Task 4: warning 沿下发链回传

**Files:**
- Modify: `internal/wsproto/messages.go:107-111` (`ApplyAck` 加 `Warning`)
- Modify: `internal/daemon/daemon.go`（`SetPanelRuleset` 返回 warning + `summarizeUnresolved`；`OnApply` wiring）
- Modify: `internal/daemon/dialer.go:44`、`:400-408`（`OnApply` 签名 + 构造 `ApplyAck.Warning`）
- Modify: `internal/daemon/handlers.go:192-196` (`handleApplyRuleset` 响应带 warning)
- Modify: `internal/daemonclient/client.go` (`ApplyRuleset` 返回 warning)
- Modify: `internal/server/selfnode.go`（`Dispatch`/`sendLocalDefault`/`SendLocal` 返回 `(string, error)`）
- Modify: `internal/server/hub.go:305-341` (`SendApplyRuleset` 返回 `(string, error)`)
- Modify: `internal/server/server.go:193`（`dispatchToNode` 调用点 arity，warning 暂用 `_`）
- Test: `internal/daemon/handlers_test.go`（新增 warning 测试）、`internal/daemon/dialer_test.go`、`internal/server/selfnode_test.go`、`internal/server/hub_test.go`（更新桩签名）

**Interfaces:**
- Consumes: `reconcileOwners` 的 `unresolved` 返回 (Task 3)
- Produces:
  - `wsproto.ApplyAck{..., Warning string}`
  - `Daemon.SetPanelRuleset(ctx, rev, rules) (warning string, err error)`
  - `Dialer.Config.OnApply func(ctx, rev, rules) (warning string, err error)`
  - `daemonclient.Client.ApplyRuleset(rules) (warning string, err error)`
  - `Dispatcher.Dispatch(nodeID, rules, rev) (warning string, err error)`、`Dispatcher.SendLocal func([]nft.Rule) (string, error)`、`sendLocalDefault([]nft.Rule) (string, error)`
  - `Hub.SendApplyRuleset(nodeID, rules, rev) (warning string, err error)`

- [ ] **Step 1: 写失败测试（agent 产出 warning）**

在 `internal/daemon/handlers_test.go` 追加：

```go
func TestSetPanelRuleset_ReturnsWarningForUnresolved(t *testing.T) {
	d := newTestDaemon(t)
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		return in, false, fmt.Errorf("dns: bad.invalid: no such host")
	}
	warning, err := d.SetPanelRuleset(context.Background(), "rev1", []nft.Rule{
		{Proto: "tcp", SrcPort: 8080, DestHost: "bad.invalid", DestPort: 80},
	})
	if err != nil {
		t.Fatalf("SetPanelRuleset should not error on unresolved: %v", err)
	}
	if warning == "" {
		t.Fatal("expected non-empty warning for unresolved rule")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/daemon/ -run TestSetPanelRuleset_ReturnsWarningForUnresolved -v`
Expected: FAIL（`SetPanelRuleset` 目前返回单个 error，签名不匹配 → 编译失败）

- [ ] **Step 3: wsproto 加 Warning**

`internal/wsproto/messages.go` 的 `ApplyAck`：

```go
type ApplyAck struct {
	Rev     string `json:"rev"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}
```

- [ ] **Step 4: SetPanelRuleset 返回 warning + summarizeUnresolved**

`internal/daemon/daemon.go`，把 `SetPanelRuleset`（现 340-360 行）改为：

```go
func (d *Daemon) SetPanelRuleset(ctx context.Context, rev string, rules []nft.Rule) (string, error) {
	_, unresolved, _, err := d.reconcileOwners(ctx,
		func(candidate OwnerRuleset) {
			if len(rules) == 0 {
				delete(candidate, "panel")
			} else {
				candidate["panel"] = append([]nft.Rule(nil), rules...)
			}
		},
		func(meta *AgentMeta) {
			if rev != "" {
				meta.LastAppliedRev = rev
			}
		},
		true,
	)
	if err != nil {
		return "", d.classifyWriteError(err)
	}
	return summarizeUnresolved(unresolved), nil
}

// summarizeUnresolved renders a short, human-readable note naming the rules
// whose target could not be resolved, for display as a node-level warning.
func summarizeUnresolved(rules []nft.Rule) string {
	if len(rules) == 0 {
		return ""
	}
	const maxList = 5
	parts := make([]string, 0, len(rules))
	for i, r := range rules {
		if i == maxList {
			break
		}
		parts = append(parts, fmt.Sprintf("端口 %d → %s", r.SrcPort, r.DestHost))
	}
	if len(rules) > maxList {
		parts = append(parts, fmt.Sprintf("等共 %d 条", len(rules)))
	}
	return fmt.Sprintf("%d 条规则的目标无法解析：%s", len(rules), strings.Join(parts, "，"))
}
```

确认 `daemon.go` import 含 `"fmt"` 与 `"strings"`（若缺则加）。

- [ ] **Step 5: OnApply 签名 + dialer 构造 Warning**

`internal/daemon/dialer.go:44`：

```go
	OnApply  func(ctx context.Context, rev string, rules []nft.Rule) (warning string, err error)
```

`internal/daemon/dialer.go` 现 400-408 行：

```go
				ok := true
				errMsg := ""
				warning := ""
				if d.cfg.OnApply != nil {
					w, err := d.cfg.OnApply(ctx, ar.Rev, ar.Rules)
					warning = w
					if err != nil {
						ok = false
						errMsg = err.Error()
					}
				}
				ap, err := json.Marshal(wsproto.ApplyAck{Rev: ar.Rev, OK: ok, Error: errMsg, Warning: warning})
```

- [ ] **Step 6: self-node HTTP 对等回传 warning**

`internal/daemon/handlers.go` 的 `handleApplyRuleset`（现 192-196 行）：

```go
	warning, err := d.SetPanelRuleset(r.Context(), "", body.Rules)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
```

`internal/daemonclient/client.go` 的 `ApplyRuleset`：

```go
func (c *Client) ApplyRuleset(rules []nft.Rule) (string, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	body, err := json.Marshal(struct {
		Rules []nft.Rule `json:"rules"`
	}{Rules: rules})
	if err != nil {
		return "", err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/apply", body)
	if err != nil {
		return "", err
	}
	if code/100 != 2 {
		return "", fmt.Errorf("daemon apply: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var resp struct {
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(buf, &resp)
	return resp.Warning, nil
}
```

- [ ] **Step 7: Dispatcher / sendLocalDefault / SendLocal / Hub 返回 (string, error)**

`internal/server/selfnode.go`：

```go
	SendLocal func(rules []nft.Rule) (string, error) // nil → use default unix socket
```

```go
func (d *Dispatcher) Dispatch(nodeID int64, rules []nft.Rule, rev string) (string, error) {
	n, err := db.GetNode(d.DB, nodeID)
	if err != nil {
		return "", err
	}
	if n.NodeType == "self" {
		send := d.SendLocal
		if send == nil {
			send = sendLocalDefault
		}
		return send(rules)
	}
	if d.Hub == nil {
		return "", fmt.Errorf("hub not wired; cannot dispatch to remote node %d", nodeID)
	}
	return d.Hub.SendApplyRuleset(nodeID, rules, rev)
}

func sendLocalDefault(rules []nft.Rule) (string, error) {
	c, err := daemonclient.New(daemonclient.DefaultSocketPath)
	if err != nil {
		return "", err
	}
	return c.ApplyRuleset(rules)
}
```

`internal/server/hub.go` 的 `SendApplyRuleset`（现 305-341 行）：把 `func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {` 改为返回 `(string, error)`；各 `return`：
- 现 310 `return fmt.Errorf("node %d not connected", nodeID)` → `return "", fmt.Errorf(...)`
- 现 select 段：

```go
	select {
	case raw := <-ch:
		var ack wsproto.ApplyAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return "", fmt.Errorf("malformed apply_ack: %w", err)
		}
		if !ack.OK {
			return "", fmt.Errorf("apply rejected: %s", ack.Error)
		}
		return ack.Warning, nil
	case <-time.After(applyAckTimeout):
		return "", errors.New("apply_ack timeout")
	case <-ac.closed:
		return "", errors.New("connection closed before ack")
	}
```

- [ ] **Step 8: dispatchToNode 调用点 arity（warning 暂弃用）**

`internal/server/server.go:193`：

```go
	if _, err := s.Dispatcher.Dispatch(nodeID, rules, rev); err != nil {
```

（`MarkNodeApplied(s.DB, nodeID)` 现仍为 1 参，本任务不动，Task 5 改。）

- [ ] **Step 9: OnApply wiring 检查**

`internal/daemon/daemon.go:229` 的 `OnApply: d.SetPanelRuleset` 现在类型自动匹配（`SetPanelRuleset` 已返回 `(string, error)`），无需改。

- [ ] **Step 10: 更新受签名影响的测试桩**

`internal/daemon/dialer_test.go` 中 7 处 `OnApply: func(_ context.Context, rev string, rules []nft.Rule) error { return nil }`（行 125/153/189/223/257/300/374）逐一改为：

```go
		OnApply: func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
```

`internal/server/selfnode_test.go`：`:52` 的 `SendLocal: func(rules []nft.Rule) error {` 改为 `func(rules []nft.Rule) (string, error) {`，其函数体末尾 `return nil` 改为 `return "", nil`；`:57` `if err := disp.Dispatch(...); err != nil` 改为 `if _, err := disp.Dispatch(...); err != nil`；`:72` `err = disp.Dispatch(...)` 改为 `_, err = disp.Dispatch(...)`。

`internal/server/hub_test.go`：`:147` `done <- hub.SendApplyRuleset(...)` 改为在 goroutine 里 `_, e := hub.SendApplyRuleset(...); done <- e`（若 `done` 为 `chan error`）；`:288` `s.Dispatcher.SendLocal = func(rules []nft.Rule) error {` 改为 `func(rules []nft.Rule) (string, error) {`，函数体 `return nil` 改为 `return "", nil`。

- [ ] **Step 11: 运行测试确认通过**

Run: `go test ./internal/wsproto/ ./internal/daemon/ ./internal/daemonclient/ ./internal/server/ && go build ./...`
Expected: PASS / 无错误

- [ ] **Step 12: 提交**

```bash
git add internal/wsproto/ internal/daemon/ internal/daemonclient/ internal/server/
git commit -m "feat: carry unresolved-rule warning back from agent to panel on apply"
```

---

### Task 5: nodes.last_warning 列 + Mark 函数

**Files:**
- Create: `internal/db/migrations/0023_node_last_warning.sql`
- Modify: `internal/db/queries.go`（`Node.LastWarning` 字段、`nodeCols`、`scanNode`、`MarkNodeApplied`、`MarkNodeDispatchError`）
- Modify: `internal/db/grants.go`（内联 `Scan` 加 `&n.LastWarning`）
- Modify: `internal/server/server.go:197`（`MarkNodeApplied` 临时传 `""`，Task 6 再接真值）
- Test: `internal/db/queries_test.go`（若无则新建 warning 往返测试）

**Interfaces:**
- Produces:
  - `db.Node.LastWarning string`（json `last_warning`）
  - `db.MarkNodeApplied(d *sql.DB, id int64, warning string) error`
  - `db.MarkNodeDispatchError(d *sql.DB, id int64, msg string) error`（附带清空 `last_warning`）

- [ ] **Step 1: 写失败测试**

在 `internal/db/queries_test.go`（不存在则新建，`package db`，import `"testing"`）追加。用本包既有 helper `openTestDB(t)`（定义于 `traffic_test.go:211`，内部 `Open(":memory:")` 会跑迁移并注册清理）：

```go
func TestNodeLastWarningRoundTrip(t *testing.T) {
	d := openTestDB(t)

	n, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}

	if err := MarkNodeApplied(d, n.ID, "2 条规则的目标无法解析：端口 8080 → 4212"); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.LastWarning == "" {
		t.Fatal("last_warning should be set after MarkNodeApplied with warning")
	}
	if got.LastError.Valid {
		t.Fatal("last_error should be cleared on apply")
	}

	// 干净成功清 warning
	if err := MarkNodeApplied(d, n.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("last_warning should be cleared, got %q", got.LastWarning)
	}

	// 下发硬失败：置 error、清 warning
	_ = MarkNodeApplied(d, n.ID, "some warning")
	if err := MarkNodeDispatchError(d, n.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("dispatch error should clear warning, got %q", got.LastWarning)
	}
	if !got.LastError.Valid || got.LastError.String != "boom" {
		t.Fatalf("last_error = %+v, want boom", got.LastError)
	}
}
```

（关键是"跑过迁移的库 + UpsertSelfNode 拿到一行"。）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/db/ -run TestNodeLastWarningRoundTrip -v`
Expected: FAIL —— `n.LastWarning` 未定义 / `MarkNodeApplied` 参数不匹配

- [ ] **Step 3: 迁移文件**

Create `internal/db/migrations/0023_node_last_warning.sql`：

```sql
ALTER TABLE nodes ADD COLUMN last_warning TEXT NOT NULL DEFAULT '';
```

- [ ] **Step 4: Node 字段 + 三处对齐**

`internal/db/queries.go`，在 `Node` 结构体 `LastError` 字段（现 51 行）之后加：

```go
	LastWarning  string         `json:"last_warning"`
```

`nodeCols`（现 255 行）在 `last_error,` 之后插入 `last_warning,`：

```go
const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,relay_host_v6,online,agent_version,agent_sha,last_seen,last_apply_at,last_error,last_warning,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error,hidden,sort_order,rate_multiplier,unidirectional`
```

`scanNode` 的 `Scan`（现 274 行 `&lastSeen, &n.LastApplyAt, &n.LastError,`）改为：

```go
		&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
```

`internal/db/grants.go` 内联 `Scan`（现 117 行 `&lastSeen, &n.LastApplyAt, &n.LastError,`）同样改为：

```go
			&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
```

- [ ] **Step 5: Mark 函数**

`internal/db/queries.go` 的 `MarkNodeApplied` / `MarkNodeDispatchError`（现 681-693 行）：

```go
func MarkNodeApplied(d *sql.DB, id int64, warning string) error {
	_, err := d.Exec(`UPDATE nodes SET last_apply_at=?, last_error=NULL, last_warning=? WHERE id=?`, now(), warning, id)
	return err
}

func MarkNodeDispatchError(d *sql.DB, id int64, msg string) error {
	// A newer failed attempt supersedes any prior success's skip-state, so the
	// stale warning is cleared and only the error (red) is shown.
	_, err := d.Exec(`UPDATE nodes SET last_error=?, last_warning='' WHERE id=?`, msg, id)
	return err
}
```

- [ ] **Step 6: 更新 MarkNodeApplied 唯一调用点（临时空串）**

`internal/server/server.go:197`：

```go
	_ = db.MarkNodeApplied(s.DB, nodeID, "")
```

- [ ] **Step 7: 运行确认通过**

Run: `go test ./internal/db/ ./internal/server/ && go build ./...`
Expected: PASS / 无错误

- [ ] **Step 8: 提交**

```bash
git add internal/db/ internal/server/server.go
git commit -m "feat(db): add nodes.last_warning column and thread it through node marks"
```

---

### Task 6: dispatchToNode 落库 warning + 管理端 flash

**Files:**
- Modify: `internal/server/server.go:185-199` (`dispatchToNode`)、`:204-209` (`dispatchAfterMutation`)
- Test: 新增 `internal/server/dispatch_test.go`

**Interfaces:**
- Consumes: `Dispatcher.Dispatch` 的 warning 返回 (Task 4)、`db.MarkNodeApplied(d, id, warning)` (Task 5)

**说明：** `dispatchToNode` 有 15+ 调用者，**不改其 `error` 签名**。它内部捕获 warning 并落库；`dispatchAfterMutation` 在成功后**回读** `nodes.last_warning` 拼进 flash（省去 15 处连锁改动）。

- [ ] **Step 1: 写失败测试**

新建 `internal/server/dispatch_test.go`（用 self-node + SendLocal 桩返回 warning，验证 `dispatchToNode` 把 warning 落到 `last_warning`、不写 `last_error`）：

```go
package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func TestDispatchToNode_StoresWarningNotError(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	self, err := db.UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Dispatcher: &Dispatcher{
		DB:        d,
		SendLocal: func(rules []nft.Rule) (string, error) { return "1 条规则的目标无法解析：端口 8080 → bad.invalid", nil },
	}}
	if err := s.dispatchToNode(self.ID); err != nil {
		t.Fatalf("dispatch should succeed with warning: %v", err)
	}
	got, _ := db.GetNode(d, self.ID)
	if got.LastWarning == "" {
		t.Fatal("expected last_warning to be stored")
	}
	if got.LastError.Valid {
		t.Fatalf("last_error should stay clear, got %q", got.LastError.String)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run TestDispatchToNode_StoresWarningNotError -v`
Expected: FAIL（当前 `dispatchToNode` 丢弃 warning，`last_warning` 为空）

- [ ] **Step 3: dispatchToNode 捕获 warning 落库**

`internal/server/server.go` 的 `dispatchToNode`（现 185-199 行）：

```go
func (s *Server) dispatchToNode(nodeID int64) error {
	ruleHops, err := db.ActiveRuleHopsForPush(s.DB, nodeID)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	rules := buildRules(s.DB, ruleHops)
	rev := computeRev(rules)
	warning, err := s.Dispatcher.Dispatch(nodeID, rules, rev)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	_ = db.MarkNodeApplied(s.DB, nodeID, warning)
	return nil
}
```

- [ ] **Step 4: 管理端 mutation flash 回读 warning**

`internal/server/server.go` 的 `dispatchAfterMutation`（现 204-209 行）改为在成功后回读节点 `last_warning`：

```go
func (s *Server) dispatchAfterMutation(w http.ResponseWriter, nodeID int64, action string) {
	if err := s.dispatchToNode(nodeID); err != nil {
		setFlash(w, fmt.Sprintf("%s 已保存，但下发到节点失败：%v", action, err))
		log.Printf("dispatch node %d (%s): %v", nodeID, action, err)
		return
	}
	if n, err := db.GetNode(s.DB, nodeID); err == nil && n.LastWarning != "" {
		setFlash(w, fmt.Sprintf("%s 已保存，但 %s", action, n.LastWarning))
	}
}
```

（`dispatchToNode` 签名不变，`dispatchAfterFanout`/`redispatchNodes`/`apiDispatch` 等其余 15 处调用点无需改动。）

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/server/ && go build ./...`
Expected: PASS / 无错误

- [ ] **Step 6: 提交**

```bash
git add internal/server/server.go internal/server/dispatch_test.go
git commit -m "feat(server): store dispatch warning on node and surface it in the admin flash"
```

---

### Task 7: 前端琥珀警告

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx`（`HeaderStatus` 加琥珀分支 + 基本信息区琥珀说明行）
- Modify: `web/src/pages/nodes/List.jsx:393-395`（列表琥珀徽章）

**Interfaces:**
- Consumes: 节点 JSON 的 `last_warning`（plain string，Task 5）；`last_error`（`{String, Valid}`，`nullStr` 提取）

- [ ] **Step 1: HeaderStatus 加琥珀分支**

`web/src/pages/nodes/Detail.jsx` 的 `HeaderStatus`（现 429-440 行），在 `else if (nullStr(node.last_error))` 分支之后、`else if (node.online === 1)` 之前插入：

```jsx
  } else if (node.last_warning) {
    text = '警告'; cls = 'text-[#b25000] bg-[#fef6ec] border-[#f6d9ac]'; dot = '#e0892f'
```

即优先级：已禁用 > 错误(红) > 警告(琥珀) > 在线 > 离线。

- [ ] **Step 2: 基本信息区加琥珀说明行**

在 `Detail.jsx` 基本信息卡片渲染块中，当 `node.last_warning` 非空且无 `last_error` 时渲染一行琥珀说明（`last_error` 存在时红色优先，不重复显示）：

```jsx
{node.last_warning && !nullStr(node.last_error) && (
  <div className="mt-2 text-[12.5px] text-[#b25000] bg-[#fef6ec] border border-[#f6d9ac] rounded-lg px-3 py-2 break-all">
    {node.last_warning}
  </div>
)}
```

- [ ] **Step 3: List.jsx 琥珀徽章**

`web/src/pages/nodes/List.jsx`（现 393-395 行）：

```jsx
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red" title={lastErr}>错误</Badge>
  if (node.last_warning) return <Badge color="amber" title={node.last_warning}>警告</Badge>
```

（确认 `Badge` 支持 `color="amber"`；若不支持，用与升级"可能未生效"一致的 amber 样式——`Detail.jsx:237` 已有 `<Badge color="amber">`，故支持。）

- [ ] **Step 4: 构建前端确认无误**

Run: `cd web && npm run build`
Expected: 构建成功，无类型/语法错误

- [ ] **Step 5: 提交**

```bash
git add web/src/pages/nodes/Detail.jsx web/src/pages/nodes/List.jsx
git commit -m "feat(web): show amber warning when a node has unresolved rules"
```

---

### Task 8: 全量验证

**Files:** 无（验证）

- [ ] **Step 1: 全量测试**

Run: `go test ./internal/...`
Expected: 全 PASS

- [ ] **Step 2: 构建**

Run: `go build ./... && cd web && npm run build`
Expected: 均成功

- [ ] **Step 3: vet**

Run: `go vet ./internal/...`
Expected: 无告警

---

## Self-Review

**Spec coverage:**
- A 统一容错 → Task 3。B 节点级警告（last_warning + 回传 + 前端）→ Task 4/5/6/7。C 创建校验 → Task 1/2（面板）+ Task 3（本地）。状态语义（Mark 函数 + UI 优先级）→ Task 5/7。三处对齐 → Task 5 Step 4。测试改写 → Task 3 Step 1、Task 4 Step 10。已接受限制（warning 时效性）为文档记录项，无需代码任务。
- 覆盖完整，无遗漏。

**Placeholder scan:** 无 TBD/TODO；建库 helper 名称处给了"沿用现有/否则 Open(:memory:)"的明确回退，非占位。

**Type consistency:** `Dispatch`/`SendApplyRuleset`/`ApplyRuleset`/`SendLocal`/`sendLocalDefault`/`OnApply`/`SetPanelRuleset` 统一 `(warning string, err error)`；`reconcileOwners` 统一 `(resolved, unresolved []nft.Rule, committed bool, err error)`；`MarkNodeApplied(d, id, warning)`。前后一致。
