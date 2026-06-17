# 节点 agent 升级状态展示 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 持久化每个节点最近一次 agent 升级的结果,并在节点详情页展示——尤其让「已确认接收但版本始终不变」的静默失败可见。

**Architecture:** 升级链路不变;在 `apiUpgradeNode`/`apiUpgradeAllNodes` 把 `SendUpgrade` 结果写入 `nodes` 新增的 4 列;节点详情接口用纯函数 `deriveUpgradeStatus` 算出展示状态(none/ok/error/pending/stuck),前端渲染。不改 daemon。

**Tech Stack:** Go(`internal/db`、`internal/server`)、SQLite migration、React(`web/src`)。

---

## File Structure

- `internal/db/migrations/0006_node_upgrade_status.sql` — 新建,给 nodes 加 4 列。
- `internal/db/queries.go` — Node 结构体 + `nodeCols` + `scanNode` 增 4 字段。
- `internal/db/rules.go` 或 `queries.go` — 新增 `RecordUpgradeResult`(放 queries.go,靠近其它 node 写函数)。
- `internal/server/upgrade_status.go` — 新建:`upgradeView`、`deriveUpgradeStatus`、`upgradeGrace`。
- `internal/server/upgrade_status_test.go` — 新建:派生函数表驱动测试。
- `internal/server/api.go` — `apiGetNode` 注入 `upgrade`;`apiUpgradeNode`/`apiUpgradeAllNodes` 记录结果。
- `internal/server/proto_compat_test.go` 旁 — 新建 `internal/server/upgrade_record_test.go`:记录写入与读回。
- `web/src/pages/nodes/Detail.jsx` — 「基本信息」区块加「升级状态」行。

---

## Task 1: nodes 表新增升级列 + Node 读写

**Files:**
- Create: `internal/db/migrations/0006_node_upgrade_status.sql`
- Modify: `internal/db/queries.go`(Node 结构体、`nodeCols`、`scanNode`)
- Test: `internal/server/upgrade_record_test.go`(新建)

- [ ] **Step 1: 写迁移文件**

Create `internal/db/migrations/0006_node_upgrade_status.sql`:

```sql
ALTER TABLE nodes ADD COLUMN last_upgrade_at INTEGER;
ALTER TABLE nodes ADD COLUMN last_upgrade_version TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_status TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_error TEXT;
```

迁移由 `internal/db/db.go` 的 `//go:embed migrations/*.sql` 按文件名排序自动应用,并记入 `schema_migrations`。

- [ ] **Step 2: Node 结构体加字段**

在 `internal/db/queries.go` 的 `Node` 结构体末尾(`CreatedAt int64` 之后)加:

```go
	LastUpgradeAt      sql.NullInt64 `json:"last_upgrade_at"`
	LastUpgradeVersion string        `json:"last_upgrade_version,omitempty"`
	LastUpgradeStatus  string        `json:"last_upgrade_status,omitempty"`
	LastUpgradeError   string        `json:"last_upgrade_error,omitempty"`
```

- [ ] **Step 3: nodeCols 追加 4 列**

将 `internal/db/queries.go` 的 `nodeCols` 常量(当前以 `...,port_range,created_at`结尾)改为:

```go
const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,online,agent_version,last_seen,last_apply_at,last_error,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error`
```

- [ ] **Step 4: scanNode 扫描 4 列**

在 `internal/db/queries.go` 的 `scanNode` 中,新增三个 NullString 局部变量并把 4 列加到 `r.Scan(...)` 末尾,扫描后回填。具体:在 `var agentVersion sql.NullString` 同处附近加 `var luVersion, luStatus, luError sql.NullString`;`r.Scan` 的最后一个参数 `&n.CreatedAt` 之后追加 `&n.LastUpgradeAt, &luVersion, &luStatus, &luError`;在 `return n, nil` 之前加:

```go
	n.LastUpgradeVersion = luVersion.String
	n.LastUpgradeStatus = luStatus.String
	n.LastUpgradeError = luError.String
```

(`n.LastUpgradeAt` 是 `sql.NullInt64`,直接扫描即可,无需局部变量。)

- [ ] **Step 5: 写失败测试**

Create `internal/server/upgrade_record_test.go`:

```go
package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestNodeUpgradeColumnsRoundTrip(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh node: no upgrade recorded yet.
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUpgradeAt.Valid {
		t.Fatalf("fresh node should have null last_upgrade_at, got %+v", got.LastUpgradeAt)
	}

	if err := db.RecordUpgradeResult(d, n.ID, "v1.2.3", "acked", ""); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastUpgradeAt.Valid || got.LastUpgradeVersion != "v1.2.3" || got.LastUpgradeStatus != "acked" || got.LastUpgradeError != "" {
		t.Fatalf("after acked record: %+v", got)
	}

	if err := db.RecordUpgradeResult(d, n.ID, "v1.2.3", "error", "节点未连接"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetNode(d, n.ID)
	if got.LastUpgradeStatus != "error" || got.LastUpgradeError != "节点未连接" {
		t.Fatalf("after error record: status=%q err=%q", got.LastUpgradeStatus, got.LastUpgradeError)
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestNodeUpgradeColumnsRoundTrip -v`
Expected: FAIL —— `db.RecordUpgradeResult` 未定义(下一任务实现)。先确认 schema/scan 部分编译通过:`go build ./...` 应通过;测试因缺函数编译失败属预期,Task 2 补齐后转 PASS。

- [ ] **Step 7: Commit**

```bash
git add internal/db/migrations/0006_node_upgrade_status.sql internal/db/queries.go internal/server/upgrade_record_test.go
git commit -m "feat: add node upgrade-status columns and scan them"
```

---

## Task 2: RecordUpgradeResult 写函数

**Files:**
- Modify: `internal/db/queries.go`(新增 `RecordUpgradeResult`)
- Test: `internal/server/upgrade_record_test.go`(Task 1 已写,本任务令其通过)

- [ ] **Step 1: 实现 RecordUpgradeResult**

在 `internal/db/queries.go` 中(靠近 `UpdateNodePortRange` 等 node 写函数)新增:

```go
// RecordUpgradeResult stores the outcome of the most recent upgrade push to a
// node. status is "acked" (daemon accepted and is restarting) or "error"
// (send/ack failure); errText is empty on acked. It overwrites the previous
// record — only the latest attempt is kept.
func RecordUpgradeResult(d DBTX, nodeID int64, version, status, errText string) error {
	_, err := d.Exec(
		`UPDATE nodes SET last_upgrade_at=?, last_upgrade_version=?, last_upgrade_status=?, last_upgrade_error=? WHERE id=?`,
		now(), version, status, errText, nodeID)
	return err
}
```

(`now()` 是 db 包既有的时间戳 helper,返回 Unix 秒。)

- [ ] **Step 2: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestNodeUpgradeColumnsRoundTrip -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/db/queries.go
git commit -m "feat: record latest node upgrade outcome to the nodes table"
```

---

## Task 3: deriveUpgradeStatus 派生函数

**Files:**
- Create: `internal/server/upgrade_status.go`
- Test: `internal/server/upgrade_status_test.go`(新建)

- [ ] **Step 1: 写失败测试**

Create `internal/server/upgrade_status_test.go`:

```go
package server

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
)

func TestDeriveUpgradeStatus(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	mk := func(at int64, ver, status, errText, agent string) *db.Node {
		n := &db.Node{AgentVersion: agent, LastUpgradeVersion: ver, LastUpgradeStatus: status, LastUpgradeError: errText}
		if at > 0 {
			n.LastUpgradeAt = sql.NullInt64{Int64: at, Valid: true}
		}
		return n
	}
	cases := []struct {
		name string
		node *db.Node
		now  time.Time
		want string
	}{
		{"never", mk(0, "", "", "", "v1"), base, "none"},
		{"ok", mk(base.Unix(), "v2", "acked", "", "v2"), base, "ok"},
		{"error", mk(base.Unix(), "v2", "error", "节点未连接", "v1"), base, "error"},
		{"pending within grace", mk(base.Unix(), "v2", "acked", "", "v1"), base.Add(30 * time.Second), "pending"},
		{"stuck past grace", mk(base.Unix(), "v2", "acked", "", "v1"), base.Add(5 * time.Minute), "stuck"},
	}
	for _, tc := range cases {
		got := deriveUpgradeStatus(tc.node, tc.now)
		if got.Status != tc.want {
			t.Errorf("%s: status=%q want %q", tc.name, got.Status, tc.want)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestDeriveUpgradeStatus -v`
Expected: FAIL —— `deriveUpgradeStatus`/`upgradeView` 未定义。

- [ ] **Step 3: 实现 upgrade_status.go**

Create `internal/server/upgrade_status.go`:

```go
package server

import (
	"time"

	"nft-forward/internal/db"
)

// upgradeGrace is how long after an acked upgrade we still treat an unchanged
// agent_version as "in progress" (the daemon restarts and reconnects). Past it,
// an unchanged version means the restart almost certainly failed.
const upgradeGrace = 90 * time.Second

// upgradeView is the derived upgrade state shown on the node detail page.
type upgradeView struct {
	At      int64  `json:"at,omitempty"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"` // none | ok | error | pending | stuck
	Error   string `json:"error,omitempty"`
}

// deriveUpgradeStatus turns a node's stored last-upgrade columns plus its
// live agent_version into a display status. It surfaces the silent failure:
// an acked upgrade whose target version never took, past the grace window.
func deriveUpgradeStatus(n *db.Node, now time.Time) upgradeView {
	if !n.LastUpgradeAt.Valid {
		return upgradeView{Status: "none"}
	}
	v := upgradeView{At: n.LastUpgradeAt.Int64, Version: n.LastUpgradeVersion, Error: n.LastUpgradeError}
	switch {
	case n.LastUpgradeStatus == "error":
		v.Status = "error"
	case n.LastUpgradeVersion != "" && n.AgentVersion == n.LastUpgradeVersion:
		v.Status = "ok"
	case now.Unix()-n.LastUpgradeAt.Int64 <= int64(upgradeGrace/time.Second):
		v.Status = "pending"
	default:
		v.Status = "stuck"
	}
	return v
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestDeriveUpgradeStatus -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/upgrade_status.go internal/server/upgrade_status_test.go
git commit -m "feat: derive node upgrade display status with a silent-failure signal"
```

---

## Task 4: 接入 API(详情注入 + 升级记录)

**Files:**
- Modify: `internal/server/api.go`(`apiGetNode`、`apiUpgradeNode`、`apiUpgradeAllNodes`)
- Test: `internal/server/upgrade_record_test.go`(追加 handler-level 断言)

- [ ] **Step 1: 追加失败测试**

在 `internal/server/upgrade_record_test.go` 追加(验证 `apiGetNode` 响应含派生 upgrade,且失败的 `SendUpgrade`——节点未连接——被记录为 error):

```go
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

func TestApiUpgradeNodeRecordsError(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := HashPassword("pw")
	aid, err := db.CreateUser(d, "admin1", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cookieTok, err := db.CreateSession(d, aid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: cookieTok}

	s, _ := New(d)

	// Node is not connected -> SendUpgrade fails -> must be recorded as error.
	req := httptest.NewRequest("POST", "/api/nodes/"+itoa(n.ID)+"/upgrade", bytes.NewReader([]byte("{}")))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	// The handler returns 500 for a not-connected node; the point is the record.
	got, _ := db.GetNode(d, n.ID)
	if got.LastUpgradeStatus != "error" || got.LastUpgradeError == "" {
		t.Fatalf("expected recorded error after failed upgrade, got status=%q err=%q (http=%d)", got.LastUpgradeStatus, got.LastUpgradeError, rec.Code)
	}

	// apiGetNode must expose a derived upgrade object.
	req2 := httptest.NewRequest("GET", "/api/nodes/"+itoa(n.ID), nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get node: http=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		Upgrade upgradeView `json:"upgrade"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Upgrade.Status != "error" {
		t.Fatalf("apiGetNode upgrade.status=%q want error", resp.Upgrade.Status)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
```

NOTE: 若 `strconv` 已在文件 import 中则复用;`db.CreateUser`/`db.CreateSession`/`sessionCookie`/`HashPassword` 均为既有符号(见 `my_chains_test.go`、`proto_compat_test.go`)。若 admin 路由需要的角色/中间件不同,按 `handlers_admin_test.go` 里的既有 admin 登录方式对齐。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestApiUpgradeNodeRecordsError -v`
Expected: FAIL —— `apiGetNode` 响应无 `upgrade` 字段,且失败升级未被记录。

- [ ] **Step 3: apiGetNode 注入 upgrade**

在 `internal/server/api.go` 的 `apiGetNode` 中,构造 `resp` map 处加一行(`n` 即已取到的节点):

```go
	resp := map[string]any{
		"node": n, "rule_hops": ruleHops, "panel_url": panelURL,
		"panel_url_configured": panelURL != "",
		"server_version":       serverVersion(),
		"upgrade":              deriveUpgradeStatus(n, time.Now()),
	}
```

确认 `internal/server/api.go` 已 import `"time"`;未 import 则补上。

- [ ] **Step 4: apiUpgradeNode 记录结果**

在 `internal/server/api.go` 的 `apiUpgradeNode` 中,把 `SendUpgrade` 调用改为先取 err、记录、再分支:

```go
	err = s.Hub.SendUpgrade(id, wsproto.Upgrade{
		Version: serverVersion(), SHA256: selfBinarySHA,
		Size: int64(len(selfBinaryBytes)), DownloadAt: panelURL + "/v1/binary",
	})
	status, errText := "acked", ""
	if err != nil {
		status, errText = "error", err.Error()
	}
	db.RecordUpgradeResult(s.DB, id, serverVersion(), status, errText)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
```

(此处 `err` 需为已声明变量;若原代码用 `if err := ...; err != nil` 内联声明,改成函数体内先 `err :=` 再判断,如上。)

- [ ] **Step 5: apiUpgradeAllNodes 逐节点记录**

在 `internal/server/api.go` 的 `apiUpgradeAllNodes` 循环里,把发送与计数改为同时记录:

```go
	for _, n := range nodes {
		if n.AgentVersion == serverVersion() {
			continue
		}
		err := s.Hub.SendUpgrade(n.ID, upgrade)
		status, errText := "acked", ""
		if err != nil {
			status, errText = "error", err.Error()
			fail++
		} else {
			ok++
		}
		db.RecordUpgradeResult(s.DB, n.ID, serverVersion(), status, errText)
	}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestApiUpgradeNodeRecordsError -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server/api.go internal/server/upgrade_record_test.go
git commit -m "feat: record upgrade outcomes and expose derived status on node detail"
```

---

## Task 5: 前端详情页展示升级状态

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx`

- [ ] **Step 1: 取出 upgrade 派生对象**

在 `web/src/pages/nodes/Detail.jsx` 第 35-39 行附近(`const agentOutdated = ...` 之后)加:

```jsx
  const up = data.upgrade || { status: 'none' }
  const showUpgrade = up.status !== 'none'
```

- [ ] **Step 2: 「Agent 版本」行去掉 last,新增「升级状态」行**

把「基本信息」区块里的 Agent 版本 `InfoRow`(当前带 `last`)改为 `last={!showUpgrade}`,并在其后新增升级状态行。即把:

```jsx
              <InfoRow label="Agent 版本" mono valueClass="text-[12.5px]" last>
                {node.agent_version
                  ? <>{node.agent_version} {agentOutdated ? <Badge color="amber">非最新</Badge> : <Badge color="green">最新</Badge>}</>
                  : <span className="text-ink-mut">未知</span>}
              </InfoRow>
```

替换为:

```jsx
              <InfoRow label="Agent 版本" mono valueClass="text-[12.5px]" last={!showUpgrade}>
                {node.agent_version
                  ? <>{node.agent_version} {agentOutdated ? <Badge color="amber">非最新</Badge> : <Badge color="green">最新</Badge>}</>
                  : <span className="text-ink-mut">未知</span>}
              </InfoRow>
              {showUpgrade && (
                <InfoRow label="升级状态" valueClass="text-[12.5px]" last>
                  {up.status === 'ok' && <><Badge color="green">升级成功</Badge> <span className="ml-1 text-ink-mut">{up.version} · {fmtTime(up.at)}</span></>}
                  {up.status === 'error' && <><Badge color="red">升级失败</Badge> <span className="ml-1 text-ink-mut break-all">{up.error}</span></>}
                  {up.status === 'pending' && <><Badge color="blue">升级中</Badge> <span className="ml-1 text-ink-mut">已推送 {up.version} · {fmtTime(up.at)}</span></>}
                  {up.status === 'stuck' && <><Badge color="amber">可能未生效</Badge> <span className="ml-1 text-ink-mut">已确认接收 {up.version}（{fmtTime(up.at)}），当前仍为 {node.agent_version || '未知'}，可能重启失败</span></>}
                </InfoRow>
              )}
```

(`Badge`、`fmtTime`、`InfoRow` 均已在该文件可用。)

- [ ] **Step 3: 构建确认无报错**

Run: `cd web && npm run build`
Expected: 构建成功,无 lint/未使用变量错误。

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/nodes/Detail.jsx
git commit -m "feat: show node upgrade status on the detail page"
```

---

## Task 6: 全量验证

**Files:** 无(仅验证)

- [ ] **Step 1: Go 全量测试**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 2: go vet**

Run: `go vet ./...`
Expected: 无输出

- [ ] **Step 3: 前端构建**

Run: `cd web && npm run build`
Expected: 成功

---

## Self-Review

- **Spec 覆盖**:数据模型(Task 1)、`RecordUpgradeResult`(Task 2)、`deriveUpgradeStatus` 五态+宽限期(Task 3)、API 注入与逐节点记录(Task 4)、前端展示(Task 5)——spec 各节均有对应任务。
- **占位符**:无 TBD/TODO,每个改动步骤给出完整代码。
- **类型一致**:`RecordUpgradeResult(d DBTX, nodeID int64, version, status, errText string)`、`deriveUpgradeStatus(n *db.Node, now time.Time) upgradeView`、`upgradeView{At,Version,Status,Error}`、Node 新字段名在各任务间一致。
- **非目标**:不改 daemon、不抓 journalctl、不留历史、不动未路由的 SSR upgrade handler。
