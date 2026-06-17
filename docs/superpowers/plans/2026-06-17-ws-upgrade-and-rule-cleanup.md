# WS 升级传输 + 去 path + 修陈旧升级错误 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or executing-plans. Steps use checkbox syntax.

**Goal:** 升级二进制经 WS 传输(救 po0 类无 HTTP 节点);规则列表/详情响应去掉 path;节点已最新时不再显示陈旧升级错误。

**Architecture:** A=WS 消息内嵌二进制(保留 HTTP 兼容);B=删 ruleView/ruleListItem 的 Path + 简化 buildRuleView;C=deriveUpgradeStatus 比对 serverVersion。

**Tech Stack:** Go(wsproto/server/daemon)、React。

---

## Task 1: 升级二进制经 WS 传输

**Files:** `internal/wsproto/messages.go`、`internal/server/api.go`、`internal/daemon/dialer.go`、`internal/daemon/upgrade.go`;测试 `internal/wsproto/messages_test.go`(或新建)、`internal/daemon/upgrade_test.go`(新建)。

- [ ] **Step 1: wsproto round-trip 失败测试** — 新建/追加 `internal/wsproto`(package wsproto)测试:
```go
func TestUpgradeDataRoundTrip(t *testing.T) {
	in := Upgrade{Version: "v1", SHA256: "abc", Size: 3, DownloadAt: "u", Data: []byte{1, 2, 3}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Upgrade
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if string(out.Data) != string(in.Data) || out.SHA256 != in.SHA256 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
```
(import `encoding/json`, `testing`.)

- [ ] **Step 2:** `go test ./internal/wsproto/ -run TestUpgradeDataRoundTrip` → FAIL(无 Data 字段).

- [ ] **Step 3: 协议加字段** — `internal/wsproto/messages.go` 的 `Upgrade` 结构体加:
```go
	// Data, when non-empty, carries the binary inline so daemons that cannot
	// reach the panel over HTTP still upgrade. DownloadAt remains the fallback.
	Data []byte `json:"data,omitempty"`
```

- [ ] **Step 4:** round-trip 测试 PASS.

- [ ] **Step 5: daemon upgradeBinary 失败测试** — 新建 `internal/daemon/upgrade_test.go`:
```go
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"nft-forward/internal/wsproto"
)

func TestUpgradeBinaryFromData(t *testing.T) {
	payload := []byte("hello-binary")
	sum := sha256.Sum256(payload)
	good := wsproto.Upgrade{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Data: payload}
	got, err := upgradeBinary(good)
	if err != nil {
		t.Fatalf("good data: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q", got)
	}
	bad := wsproto.Upgrade{SHA256: "deadbeef", Size: int64(len(payload)), Data: payload}
	if _, err := upgradeBinary(bad); err == nil {
		t.Fatal("sha mismatch should error")
	}
}
```

- [ ] **Step 6:** `go test ./internal/daemon/ -run TestUpgradeBinaryFromData` → FAIL(undefined).

- [ ] **Step 7: 实现 upgradeBinary + 改 handleUpgrade** — `internal/daemon/upgrade.go`:新增
```go
// upgradeBinary returns the new binary for u: the inline Data (sha-verified)
// when present, else an HTTP download from u.DownloadAt. Inline transport lets
// nodes that cannot reach the panel over HTTP still upgrade over the WS link.
func upgradeBinary(u wsproto.Upgrade) ([]byte, error) {
	if len(u.Data) > 0 {
		sum := sha256.Sum256(u.Data)
		if got := hex.EncodeToString(sum[:]); got != u.SHA256 {
			return nil, fmt.Errorf("sha256 mismatch: got %s, want %s", got, u.SHA256)
		}
		return u.Data, nil
	}
	return downloadBinary(u)
}
```
并把 `handleUpgrade` 里 `binary, err := downloadBinary(u)` 改为 `binary, err := upgradeBinary(u)`。(`crypto/sha256`、`encoding/hex`、`fmt` 已在该文件 import。)

- [ ] **Step 8: daemon 读取上限** — `internal/daemon/dialer.go` 第 259 行 `ws, _, err := websocket.Dial(...)` 之后、错误检查通过后,加:
```go
	// The panel may push the upgrade binary inline over WS (~13MB); the default
	// read limit (32KB) would reject it.
	ws.SetReadLimit(64 << 20)
```
确认插在 `if err != nil { ... return }` 之后、`ws` 可用处。

- [ ] **Step 9: 面板内嵌 Data** — `internal/server/api.go` 的 `apiUpgradeNode` 与 `apiUpgradeAllNodes`,构造 `wsproto.Upgrade{...}` 处加 `Data: selfBinaryBytes,`(保留 DownloadAt)。两处的 upgrade 字面量都加。

- [ ] **Step 10:** `go test ./internal/daemon/ ./internal/wsproto/ ./internal/server/` 全 PASS;`go build ./...` PASS.

- [ ] **Step 11: commit**
```bash
git add internal/wsproto/messages.go internal/server/api.go internal/daemon/dialer.go internal/daemon/upgrade.go internal/wsproto/*_test.go internal/daemon/upgrade_test.go
git commit -m "feat: push upgrade binary inline over the websocket link"
```

---

## Task 2: 规则响应移除 path

**Files:** `internal/server/shared.go`、`web/src/pages/rules/Detail.jsx`。

- [ ] **Step 1: 删 Path 字段** — `internal/server/shared.go`:`ruleView` 与 `ruleListItem` 结构体去掉 `Path string`(ruleListItem 的带 json tag `path`)。

- [ ] **Step 2: 简化 buildRuleView** — 把:
```go
func (s *Server) buildRuleView(r *db.Rule) ruleView {
	hops, _ := db.ListRuleHops(s.DB, r.ID)
	names := make([]string, 0, len(hops)+1)
	for _, h := range hops {
		n, err := db.GetNode(s.DB, h.NodeID)
		if err == nil {
			names = append(names, n.Name)
		} else {
			names = append(names, fmt.Sprintf("#%d", h.NodeID))
		}
	}
	exit := net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort))
	names = append(names, exit)
	entry := "—"
	var entryNodeID int64
	if len(hops) > 0 && r.EntryListenPort > 0 {
		entryNodeID = hops[0].NodeID
		if n, err := db.GetNode(s.DB, hops[0].NodeID); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(r.EntryListenPort))
		}
	}
	return ruleView{Rule: r, Path: strings.Join(names, " → "), Entry: entry, Exit: exit, EntryNodeID: entryNodeID}
}
```
改为:
```go
func (s *Server) buildRuleView(r *db.Rule) ruleView {
	hops, _ := db.ListRuleHops(s.DB, r.ID)
	exit := net.JoinHostPort(r.ExitHost, strconv.Itoa(r.ExitPort))
	entry := "—"
	var entryNodeID int64
	if len(hops) > 0 && r.EntryListenPort > 0 {
		entryNodeID = hops[0].NodeID
		if n, err := db.GetNode(s.DB, hops[0].NodeID); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(r.EntryListenPort))
		}
	}
	return ruleView{Rule: r, Entry: entry, Exit: exit, EntryNodeID: entryNodeID}
}
```

- [ ] **Step 3: buildRuleListItem 去 Path** — 把 `return ruleListItem{Rule: r, OwnerName: ownerName, Path: v.Path, Entry: v.Entry, Exit: v.Exit, EntryNodeID: v.EntryNodeID}` 改为去掉 `Path: v.Path,`。

- [ ] **Step 4: import 检查** — `strings` 现在可能仅被 path 的 `strings.Join` 使用;若 shared.go 其余无 `strings.`,移除 `strings` import。`fmt` 仍有多处 `fmt.Errorf`,保留。运行 `go build ./internal/server/` 据报错增删 import。

- [ ] **Step 5: 删详情页路径行** — `web/src/pages/rules/Detail.jsx` 删除这两行:
```jsx
            <span className="text-ink-soft font-semibold">路径</span>
            <span className="font-mono text-ink-soft"><SensText blurred={blurred}>{rule.path || '--'}</SensText></span>
```

- [ ] **Step 6: 验证** — `go build ./... && go test ./internal/server/`;`cd web && npm run build`。

- [ ] **Step 7: commit**
```bash
git add internal/server/shared.go web/src/pages/rules/Detail.jsx
git commit -m "refactor: drop unused rule path from responses and detail view"
```

---

## Task 3: 节点已最新时不显示陈旧升级错误

**Files:** `internal/server/upgrade_status.go`、`internal/server/api.go`、`internal/server/upgrade_status_test.go`。

- [ ] **Step 1: 改测试(先失败)** — `internal/server/upgrade_status_test.go`:把 `deriveUpgradeStatus(tc.node, tc.now)` 调用改为 `deriveUpgradeStatus(tc.node, "vSERVER", tc.now)`,并给现有用例的 node 设一个与 "vSERVER" 不同的 AgentVersion(它们已是 "v1"/"v2",传 server="vSERVER" 即可保持原路径)。新增用例:
```go
		{"current hides stale error", mk(base.Unix(), "v2", "error", "节点未连接", "vSERVER"), base, "none"},
```
(即 agent == serverVersion "vSERVER" → none。)其余用例 want 不变。

- [ ] **Step 2:** `go test ./internal/server/ -run TestDeriveUpgradeStatus` → FAIL(签名不符/新用例).

- [ ] **Step 3: 改 deriveUpgradeStatus** — `internal/server/upgrade_status.go`:签名加 `serverVersion string`;在 `if !n.LastUpgradeAt.Valid` 之后加:
```go
	// A node already on the panel's current version has nothing to report; any
	// stored failure/attempt is stale (e.g. it was upgraded out of band).
	if n.AgentVersion != "" && n.AgentVersion == serverVersion {
		return upgradeView{Status: "none"}
	}
```
其余 switch 不变。

- [ ] **Step 4: 调用方** — `internal/server/api.go` 的 `apiGetNode`:`"upgrade": deriveUpgradeStatus(n, time.Now())` 改为 `"upgrade": deriveUpgradeStatus(n, serverVersion(), time.Now())`。

- [ ] **Step 5:** `go test ./internal/server/ -run TestDeriveUpgradeStatus` → PASS;`go build ./...`。

- [ ] **Step 6: commit**
```bash
git add internal/server/upgrade_status.go internal/server/api.go internal/server/upgrade_status_test.go
git commit -m "fix: hide stale upgrade status once a node reaches the current version"
```

---

## Task 4: 全量验证

- [ ] `go test ./...` → PASS
- [ ] `go vet ./...` → 无输出
- [ ] `cd web && npm run build` → 成功

---

## Self-Review
- Spec A/B/C 三节均有对应 Task(1/2/3)。
- 无占位符;`upgradeBinary`、`deriveUpgradeStatus(n, serverVersion, now)`、`Upgrade.Data` 签名跨任务一致。
- 非目标:不自动化 po0 引导、不分块、不删 /v1/binary。
