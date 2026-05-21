# Host Daemon — Phase C TUI 客户端化 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** TUI 不再直接调用 `store.Load` / `store.Save` / `nft.Apply`，改为通过新增的 `internal/daemonclient` 包对本机 daemon socket 发起 HTTP-over-unix-socket 调用。TUI 启动时连不上 daemon → 报错并提示用户启动 daemon service，**不 fallback** 到直接管 nftables。

**Architecture:** 新 `internal/daemonclient/` 提供独立 `Client` 类型 + 镜像版 `OwnerRuleset`（不引用 `internal/daemon`，避免 TUI 间接耦合 daemon 内部）。TUI 保留 DNS 解析（仍通过 `nft.ResolveHosts`），把解析后的 ruleset POST 到 `/v1/ruleset/tui`；daemon 当前 Bootstrap+Apply 逻辑**不动**（它已经接受 resolved rules，state.json 写已 resolved 的内容）。**server / agent 仍走旧 in-process / HTTP 路径**。

**Tech Stack:** Go 1.22+ stdlib only。

**Spec:** `docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md` Phase C 节

---

## 关键决策（在 spec 基础上落地）

| # | 决策 | 取舍 |
|---|---|---|
| 1 | **DNS 解析仍由 TUI 做** | spec 隐含 daemon 应有 `/v1/resolve` endpoint，但 Phase A/B 没实现。TUI 保留 `nft.ResolveHosts`，POST 已 resolved 的 rules — daemon 端代码完全不动，Phase C 最小化 |
| 2 | **`daemonclient` 不引用 `internal/daemon`** | 在 daemonclient 内 mirror 定义 `OwnerRuleset`，两边各管各的。TUI 只 import daemonclient |
| 3 | **TUI 启动不再 prompt 安装 `nft-forward.service`** | 持久化职责由 daemon 接管（daemon 有自己的 systemd unit），TUI 不再需要装老的 nft-forward.service |
| 4 | **TUI 不再做 root + nft preflight** | daemon 自己做了；TUI 只看 socket 能不能连。连不上→报错，不 fallback |
| 5 | **`cmd/nft-forward --apply` 暂不动** | 它仍走 `store.Load` + `nft.Apply` 直接路径，是旧 `nft-forward.service` 的 ExecStart。Phase E (install.sh) 会替换 service 表面 |
| 6 | **`internal/store` 包不删** | `cmd/nft-forward --apply` 和未迁移的 internal/agent 仍引用。删除是 Phase E 的事 |

---

## File Structure

| 路径 | 操作 | 职责 |
|---|---|---|
| `internal/daemonclient/client.go` | **新增** | `Client` struct + `New(socketPath)` + `Health()` / `GetRuleset()` / `PostRuleset(owner, rules)` |
| `internal/daemonclient/types.go` | **新增** | `OwnerRuleset = map[string][]nft.Rule` mirror + payload types |
| `internal/daemonclient/client_test.go` | **新增** | httptest unix socket server + 各方法 round-trip |
| `internal/tui/tui.go` | **改** | `model` 加 `client` 字段；`commit`/`refresh` 用 client；`Run` 签名改为 `Run(client daemonClient)`；删 `store.Load/Save`、`nft.Apply` 直接调用 |
| `internal/tui/tui_test.go` | **改** | 用 fake client 替代 store/nft；调整需要 client 参数的测试调用 |
| `cmd/nft-forward/main.go` | **改** | `runTUI` 替换为：构造 daemonclient → Health 检查 → 失败时报错 + 提示启动 daemon → 不 prompt 安装 nft-forward.service → 不调 store.Load/nft.Apply → `tui.Run(client)` |
| `docs/daemon-manual-verification.md` | **改** | "已知限制"列表更新（TUI 已接入；剩 server/agent 未接入） |

---

## Task 1: `internal/daemonclient/` 包

新独立包，提供本机 unix-socket daemon client。**不引用 `internal/daemon`** —— OwnerRuleset 在这里 mirror 定义。

**Files:**
- Create: `internal/daemonclient/types.go`
- Create: `internal/daemonclient/client.go`
- Create: `internal/daemonclient/client_test.go`

- [ ] **Step 1: 写 `internal/daemonclient/types.go`**

```go
package daemonclient

import "nft-forward/internal/nft"

// OwnerRuleset mirrors the daemon's internal type but lives here so client
// callers (TUI today, server/agent later) do not need to import
// internal/daemon. Both sides serialize to the same JSON shape, so a
// type-level mismatch is impossible by construction.
type OwnerRuleset map[string][]nft.Rule

// segmentPayload is the body of POST /v1/ruleset/{owner}.
type segmentPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// fullPayload is the body of GET /v1/ruleset.
type fullPayload struct {
	Owners OwnerRuleset `json:"owners"`
}
```

- [ ] **Step 2: 写 `internal/daemonclient/client.go`**

```go
package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"nft-forward/internal/nft"
)

// DefaultSocketPath matches what the daemon listens on by default. Tests
// pass a custom path; production callers use this constant.
const DefaultSocketPath = "/var/run/nft-forward.sock"

// Client speaks HTTP over a unix socket to the local nft-forward daemon.
// It is safe to share across goroutines; the underlying http.Client and
// transport already handle concurrent requests.
type Client struct {
	socketPath string
	http       *http.Client
}

// New constructs a Client bound to socketPath.
func New(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

// Health hits GET /v1/health and returns nil on 200 with {"ok":true}.
// Any transport error, non-200 status, or ok=false produces an error
// describing the failure so the caller can surface a precise message.
func (c *Client) Health() error {
	resp, err := c.http.Get("http://unix/v1/health")
	if err != nil {
		return fmt.Errorf("dial daemon socket %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon health returned status %d: %s", resp.StatusCode, body)
	}
	var got map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return fmt.Errorf("decode health body: %w", err)
	}
	if !got["ok"] {
		return fmt.Errorf("daemon health returned ok=false")
	}
	return nil
}

// GetRuleset fetches the full segmented ruleset currently held by daemon.
func (c *Client) GetRuleset() (OwnerRuleset, error) {
	resp, err := c.http.Get("http://unix/v1/ruleset")
	if err != nil {
		return nil, fmt.Errorf("GET /v1/ruleset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /v1/ruleset status %d: %s", resp.StatusCode, body)
	}
	var got fullPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return nil, fmt.Errorf("decode ruleset: %w", err)
	}
	if got.Owners == nil {
		got.Owners = OwnerRuleset{}
	}
	return got.Owners, nil
}

// PostRuleset replaces the daemon's ruleset segment for owner with rules.
// Passing an empty slice clears the segment. Returns an error with the
// daemon's response body on non-2xx so the caller can show conflict /
// validation messages verbatim.
func (c *Client) PostRuleset(owner string, rules []nft.Rule) error {
	if owner == "" {
		return fmt.Errorf("owner must not be empty")
	}
	if rules == nil {
		rules = []nft.Rule{}
	}
	b, err := json.Marshal(segmentPayload{Rules: rules})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	url := "http://unix/v1/ruleset/" + owner
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
```

- [ ] **Step 3: 写 `internal/daemonclient/client_test.go`**

```go
package daemonclient

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// shortSockDir mirrors internal/daemon's helper: macOS sun_path is capped
// at 104 bytes and t.TempDir under /var/folders is often too long.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nftc-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mockServer starts an HTTP server bound to a unix socket inside a short
// temp dir and serves handlerFn. Returns the socket path so the test can
// build a Client. The server is closed automatically via t.Cleanup.
func mockServer(t *testing.T, handlerFn http.HandlerFunc) string {
	t.Helper()
	sockPath := filepath.Join(shortSockDir(t), "mock.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handlerFn}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); l.Close() })
	return sockPath
}

func TestHealth_OK(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c := New(sock)
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHealth_FailsWhenSocketMissing(t *testing.T) {
	c := New(filepath.Join(shortSockDir(t), "nope.sock"))
	if err := c.Health(); err == nil {
		t.Fatal("expected error when socket does not exist")
	}
}

func TestHealth_FailsOnNon200(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c := New(sock)
	err := c.Health()
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestHealth_FailsOnOkFalse(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	})
	c := New(sock)
	err := c.Health()
	if err == nil || !strings.Contains(err.Error(), "ok=false") {
		t.Fatalf("expected ok=false error, got %v", err)
	}
}

func TestGetRuleset_RoundTrip(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owners":{"tui":[{"id":"r1","proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80}]}}`))
	})
	c := New(sock)
	got, err := c.GetRuleset()
	if err != nil {
		t.Fatalf("GetRuleset: %v", err)
	}
	if len(got["tui"]) != 1 || got["tui"][0].ID != "r1" {
		t.Fatalf("unexpected ruleset: %+v", got)
	}
}

func TestGetRuleset_EmptyOwnersReturnsNonNilMap(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owners":{}}`))
	})
	c := New(sock)
	got, err := c.GetRuleset()
	if err != nil {
		t.Fatalf("GetRuleset: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil OwnerRuleset")
	}
	if len(got) != 0 {
		t.Fatalf("expected zero owners, got %+v", got)
	}
}

func TestPostRuleset_SendsBodyAndOwnerInPath(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"count":1}`))
	})
	c := New(sock)
	err := c.PostRuleset("tui", []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 80},
	})
	if err != nil {
		t.Fatalf("PostRuleset: %v", err)
	}
	if capturedPath != "/v1/ruleset/tui" {
		t.Errorf("path = %q, want /v1/ruleset/tui", capturedPath)
	}
	var got segmentPayload
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" {
		t.Fatalf("body did not carry rule: %+v", got)
	}
}

func TestPostRuleset_NilRulesNormalizesToEmpty(t *testing.T) {
	var capturedBody []byte
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"count":0}`))
	})
	c := New(sock)
	if err := c.PostRuleset("tui", nil); err != nil {
		t.Fatalf("PostRuleset: %v", err)
	}
	if !strings.Contains(string(capturedBody), `"rules":[]`) {
		t.Fatalf("expected empty rules array in body, got %s", capturedBody)
	}
}

func TestPostRuleset_PropagatesConflict(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "port tcp/80 already claimed by owner \"panel\"", http.StatusConflict)
	})
	c := New(sock)
	err := c.PostRuleset("tui", []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "tcp/80") {
		t.Fatalf("error should mention 409 and conflicting port; got: %v", err)
	}
}

func TestPostRuleset_EmptyOwnerRejectedClientSide(t *testing.T) {
	c := New("/tmp/not-used.sock")
	err := c.PostRuleset("", []nft.Rule{{ID: "x"}})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("expected owner-empty error, got %v", err)
	}
}
```

- [ ] **Step 4: 跑包测试**

Run:
```bash
go test ./internal/daemonclient/ -v -count=1
go test ./internal/daemonclient/ -race -count=1
```
Expected: 9 个测试 PASS，无 race

- [ ] **Step 5: Commit**

```bash
git add internal/daemonclient/types.go internal/daemonclient/client.go internal/daemonclient/client_test.go
git commit -m "daemonclient: HTTP-over-unix-socket client for local daemon

New package speaks to the daemon's HTTP API through a unix socket so
consumers (the TUI today, server/agent later) get a typed Health /
GetRuleset / PostRuleset surface without re-implementing the
transport themselves. OwnerRuleset is mirrored here rather than
imported from internal/daemon so callers do not transitively pull in
daemon-internal code; both sides serialize to the same JSON shape so
type-level drift is impossible by construction. Errors on non-2xx
responses carry the daemon's body verbatim, letting the TUI render
a conflict (409) message exactly as the daemon described it."
```

---

## Task 2: TUI refactor — 用 daemonclient

`model` struct 加 `client` 字段；`commit`/`refresh` 调 client；`Run` 签名改成 `Run(client *daemonclient.Client)`；删 `store.Load`、`store.Save`、`nft.Apply` 直接调用。**保留 `nft.ResolveHosts`**（TUI 仍做 DNS 解析，把 resolved rules POST 给 daemon）。

**Files:**
- Modify: `internal/tui/tui.go`
- Modify: `internal/tui/tui_test.go`

- [ ] **Step 1: 改 `internal/tui/tui.go` 顶部 import 块**

把现有 import 块：

```go
import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/store"
	"nft-forward/internal/systemd"
)
```

替换为：

```go
import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)
```

`store` 和 `systemd` 不再使用（commit 走 client，refresh 走 client；systemd prompt 移到 cmd 入口或删除 — 本 task 删 model 里所有 systemd 引用）。

- [ ] **Step 2: 给 TUI 定义内部 daemonClient interface（accept-interface 模式）**

在 tui.go 文件顶部、`type viewMode int` 定义**之前**，插入：

```go
// daemonClient is the subset of daemonclient.Client the TUI relies on.
// Declared locally so the TUI test suite can substitute a fake; the
// return type uses daemonclient.OwnerRuleset because Go's interface
// matching is strict on named-vs-unnamed map types — *daemonclient.Client
// declares OwnerRuleset, so the TUI's interface must use the same name
// for the structural match to hold.
type daemonClient interface {
	GetRuleset() (daemonclient.OwnerRuleset, error)
	PostRuleset(owner string, rules []nft.Rule) error
}
```

最终 tui.go 顶部 imports（需要新增 `"nft-forward/internal/daemonclient"`）：

```go
import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)
```

- [ ] **Step 3: 改 `model` struct 加 client 字段**

定位 `type model struct { ... }`。在末尾（`height int` 之后）加 client 字段：

```go
type model struct {
	mode   viewMode
	rules  []nft.Rule
	cursor int

	inputs       []textinput.Model
	focusedInput int
	protoIdx     int

	status string
	err    string

	width  int
	height int

	client daemonClient
}
```

- [ ] **Step 4: 改 `Run` 函数签名 + `initialModel`**

把：

```go
func Run(rules []nft.Rule) error {
	p := tea.NewProgram(initialModel(rules), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func initialModel(rules []nft.Rule) model {
	return model{mode: viewList, rules: rules, inputs: buildInputs()}
}
```

替换为：

```go
// Run starts the TUI bound to the given daemon client. Caller (cmd) is
// responsible for verifying the daemon is reachable before invoking Run.
func Run(client daemonClient) error {
	rules, err := loadInitialRules(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, rules), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func initialModel(client daemonClient, rules []nft.Rule) model {
	return model{
		mode:   viewList,
		rules:  rules,
		inputs: buildInputs(),
		client: client,
	}
}

// loadInitialRules fetches the tui owner segment from the daemon. A nil
// segment (daemon has no tui rules yet) becomes an empty slice so the
// rest of the TUI does not have to nil-check.
func loadInitialRules(client daemonClient) ([]nft.Rule, error) {
	owners, err := client.GetRuleset()
	if err != nil {
		return nil, fmt.Errorf("加载规则失败: %w", err)
	}
	rules := owners["tui"]
	if rules == nil {
		rules = []nft.Rule{}
	}
	return rules, nil
}
```

- [ ] **Step 5: 改 `refresh` 方法**

定位：

```go
func (m *model) refresh() {
	rules, err := store.Load()
	if err != nil {
		m.err = err.Error()
		return
	}
	m.rules = rules
	m.status = "已从磁盘重新加载"
}
```

替换为：

```go
func (m *model) refresh() {
	owners, err := m.client.GetRuleset()
	if err != nil {
		m.err = err.Error()
		return
	}
	rules := owners["tui"]
	if rules == nil {
		rules = []nft.Rule{}
	}
	m.rules = rules
	m.status = "已从 daemon 重新加载"
}
```

- [ ] **Step 6: 改 `commit` 函数**

定位：

```go
func commit(rules []nft.Rule) ([]nft.Rule, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, resolver.New())
	if dnsErr != nil {
		return nil, dnsErr
	}
	for _, rl := range resolved {
		if rl.DestIP == "" {
			return nil, fmt.Errorf("%s/%d: 无法解析目标域名 %s", rl.Proto, rl.SrcPort, rl.DestHost)
		}
	}
	if err := nft.Apply(resolved); err != nil {
		return nil, err
	}
	if err := store.Save(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}
```

替换为：

```go
func commit(client daemonClient, rules []nft.Rule) ([]nft.Rule, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, resolver.New())
	if dnsErr != nil {
		return nil, dnsErr
	}
	for _, rl := range resolved {
		if rl.DestIP == "" {
			return nil, fmt.Errorf("%s/%d: 无法解析目标域名 %s", rl.Proto, rl.SrcPort, rl.DestHost)
		}
	}
	if err := client.PostRuleset("tui", resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}
```

- [ ] **Step 7: 改所有 `commit(...)` 的调用点**

`commit` 函数被 4 处调用：`submitEdit`、`submitAdd`、`updateConfirmDelete`、`updateConfirmClear`。每处都要把 `commit(next)` 改成 `commit(m.client, next)`。

逐处定位并改：

**`submitEdit`** 函数体中：
```go
	applied, err := commit(next)
```
改为：
```go
	applied, err := commit(m.client, next)
```

**`submitAdd`** 函数体中：
```go
	applied, err := commit(next)
```
改为：
```go
	applied, err := commit(m.client, next)
```

**`updateConfirmDelete`** 函数体中：
```go
		applied, err := commit(next)
```
改为：
```go
		applied, err := commit(m.client, next)
```

**`updateConfirmClear`** 函数体中：
```go
		applied, err := commit(nil)
```
改为：
```go
		applied, err := commit(m.client, nil)
```

- [ ] **Step 8: 编译验证**

Run:
```bash
go build ./internal/tui/
```
Expected: 编译通过

如果失败（如某个 `commit(` 调用没改全），逐处定位修复。

- [ ] **Step 9: 改 `internal/tui/tui_test.go`**

测试文件需要：
1. 加 `fakeDaemonClient` 类型实现 `daemonClient` interface
2. 现有测试调用 `initialModel(rules)` 的地方改为 `initialModel(&fakeDaemonClient{}, rules)`
3. 现有测试调用 `enterAddMode()` / `enterEditMode()` 不受影响（它们是 model method）
4. 已有的 `commit` 函数测试如果有也要改

打开 `internal/tui/tui_test.go` 看现状（**implementer 要先 cat 文件确认当前结构**），然后做最小改动让它编译通过 + 测试 PASS。

预期改动：
- 加 fake client struct
- 改 `initialModel(rules)` → `initialModel(fc, rules)` （rules 仍是第二参）

**通用 fake：**

```go
type fakeDaemonClient struct {
	owners       daemonclient.OwnerRuleset
	postedOwner  string
	postedRules  []nft.Rule
	postErr      error
}

func (f *fakeDaemonClient) GetRuleset() (daemonclient.OwnerRuleset, error) {
	if f.owners == nil {
		return daemonclient.OwnerRuleset{}, nil
	}
	return f.owners, nil
}

func (f *fakeDaemonClient) PostRuleset(owner string, rules []nft.Rule) error {
	if f.postErr != nil {
		return f.postErr
	}
	f.postedOwner = owner
	f.postedRules = append([]nft.Rule(nil), rules...)
	return nil
}
```

> 把 fakeDaemonClient 加入 tui_test.go 顶部（package 声明之后、第一个测试函数之前）。

> 把 tui_test.go 顶部 import 块加上 `"nft-forward/internal/daemonclient"`（如果尚未含）。

然后定位所有 `initialModel(` 调用，把单参数版改为双参数版传入 `&fakeDaemonClient{}`：

```bash
grep -n "initialModel(" internal/tui/tui_test.go
```

每个匹配处把 `initialModel(rules)` 形式改为 `initialModel(&fakeDaemonClient{}, rules)`，把 `initialModel(nil)` 改为 `initialModel(&fakeDaemonClient{}, nil)`。

- [ ] **Step 10: 跑 tui 包测试**

Run:
```bash
go test ./internal/tui/ -v -count=1 2>&1 | tail -20
```
Expected: 全部测试 PASS

如果有失败，定位并修复。

- [ ] **Step 11: 跑整体测试 + race + vet 确认 cmd 暂时编译失败（预期）**

Run:
```bash
go test ./internal/tui/ ./internal/daemon/ ./internal/daemonclient/ -count=1
go test ./internal/tui/ ./internal/daemon/ ./internal/daemonclient/ -race -count=1
```
Expected: 这三个包全 PASS

```bash
go build ./cmd/nft-forward/ 2>&1 | head -5
```
Expected: 编译失败 — `tui.Run(rules)` 老签名不匹配新的 `tui.Run(client)`。这是预期的中间态，下个 task 修。

- [ ] **Step 12: Commit**

```bash
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "tui: route ruleset reads and writes through daemon client

The TUI no longer touches store.Load / store.Save / nft.Apply
directly. commit() POSTs the resolved ruleset to the daemon's tui
owner segment; refresh() pulls the latest snapshot from the daemon
the same way; initialModel boots from a single GetRuleset call.
DNS resolution stays inside the TUI so it can present DNS failures
inline before any state hits the daemon, keeping the user feedback
loop intact. The model takes a daemonClient interface defined
locally so the test suite can substitute a fake without dragging
httptest into the TUI tests."
```

# 严格规则

1. 改 2 个文件（tui.go、tui_test.go）。**其他文件字节级不变**。
2. 严格按上面贴的代码复制 — 不要"优化"或 reformat。
3. Commit message 严格使用上面给的内容（无 phase / step / round 等过程信息）。
4. 任何 step 失败立即停下报告 BLOCKED + log。

---

## Task 3: cmd/nft-forward `runTUI` 改造

`runTUI` 不再 store.Load / nft.Apply / prompt 安装 nft-forward.service。改为：构造 daemonclient → Health 检查 → 失败时报错 + 提示用户启动 daemon service → 成功时 `tui.Run(client)`。

**Files:**
- Modify: `cmd/nft-forward/main.go`

- [ ] **Step 1: 改 `cmd/nft-forward/main.go` 顶部 import**

把 import 块：

```go
import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"nft-forward/internal/daemon"
	"nft-forward/internal/nft"
	"nft-forward/internal/store"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/systemd"
	"nft-forward/internal/tui"
)
```

替换为：

```go
import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"nft-forward/internal/daemon"
	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/store"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/systemd"
	"nft-forward/internal/tui"
)
```

> `store` / `systemd` / `sysdeps` / `bufio` / `strings` 仍保留 — `runApply` / `runInstallService` / `runUninstall` 还用它们。

- [ ] **Step 2: 替换 `runTUI` 函数**

定位 `runTUI`：

```go
func runTUI() int {
	if err := preflight(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}

	if !systemd.Installed() {
		if promptPersist() {
			if err := systemd.Install(); err != nil {
				fmt.Fprintln(os.Stderr, "安装持久化服务失败:", err)
				return 1
			}
			fmt.Println("已启用开机持久化：systemd 单元 nft-forward.service")
		}
	}

	rules, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载规则失败:", err)
		return 1
	}
	if err := nft.Apply(rules); err != nil {
		fmt.Fprintln(os.Stderr, "应用规则失败:", err)
		return 1
	}

	if err := tui.Run(rules); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
		return 1
	}
	return 0
}
```

替换为：

```go
func runTUI() int {
	client := daemonclient.New(daemonclient.DefaultSocketPath)
	if err := client.Health(); err != nil {
		fmt.Fprintln(os.Stderr, "无法连接 nft-forward daemon:", err)
		fmt.Fprintln(os.Stderr, "请先启动 daemon：sudo systemctl start nft-forward-daemon.service")
		fmt.Fprintln(os.Stderr, "或者临时：sudo nft-forward daemon")
		return 1
	}

	if err := tui.Run(client); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 3: `preflight` / `promptPersist` / `promptYes` 仍保留**

`preflight` 被 `runDaemon` 内联使用？查看 daemon subcommand 实现。如果 `preflight` 函数仅被 `runTUI` 使用而 `runTUI` 不再调它，会变成 dead code，go build 时虽不报错但 `go vet` 也不警告（unexported 函数 unused 不 lint）。

策略：先**不删** preflight 等函数（lossless 改动），后续 Phase E 整理 cmd 时再清。

但要确认：`runDaemon` 内部已经有自己的 nft / sysdeps preflight 检查 — 不依赖 `preflight()` 函数。是的（看现有 daemon subcommand 代码）。

所以本 task **不动** `preflight` / `promptPersist` / `promptYes`，让它们暂时未引用。

- [ ] **Step 4: 编译 + 跑所有测试 + race + vet**

Run:
```bash
go build ./cmd/nft-forward/
go test ./... -count=1
go test ./internal/daemon/ ./internal/daemonclient/ ./internal/tui/ -race -count=1
go vet ./...
```
Expected:
- 编译通过
- 全包测试 PASS
- 无 race
- vet 可能警告 `preflight` / `promptPersist` / `promptYes` 未引用 — 这是可接受的中间态（unexported unused 不算 vet warning，只有 unused import 是 vet warning，函数 unused 是 lint 而非 vet）

如果 vet 有 warning，决定：可接受（保留 fn）或必须清（删 fn）。**默认保留**，Phase E 统一清理。

- [ ] **Step 5: Commit**

```bash
git add cmd/nft-forward/main.go
git commit -m "cmd: TUI entry now connects to the daemon instead of touching nftables

runTUI no longer runs the root / sysdeps / ip_forward preflight, no
longer offers to install the legacy nft-forward.service, and no
longer hands rules.json contents to nft.Apply. It opens a
daemonclient against the default socket, fails fast with a clear
'start the daemon' message if it cannot reach it, and otherwise
hands the client straight to tui.Run. preflight / promptPersist /
promptYes stay defined for now — they are still called by runApply
and runInstallService, which will retire in a later cleanup pass."
```

> **注意**：commit message 提到 `preflight / promptPersist / promptYes` "仍被 runApply / runInstallService 调用"。这需要 implementer 在 commit 前验证：grep 看这三个函数实际被谁调用，再写准确的 message。如果它们已经无引用，commit message 改为 "stay defined for now — a later cleanup pass will retire them along with runApply / runInstallService"。

> 实际上 `preflight` 当前只被 `runTUI` 调用；`promptPersist` 也只被 `runTUI`；`promptYes` 被 `promptPersist`。删 `runTUI` 调用后这三个变 unused。Commit message 应反映这点：

**修正后的 commit message**：

```
cmd: TUI entry now connects to the daemon instead of touching nftables

runTUI no longer runs the root / sysdeps / ip_forward preflight, no
longer offers to install the legacy nft-forward.service, and no
longer hands rules.json contents to nft.Apply. It opens a
daemonclient against the default socket, fails fast with a clear
'start the daemon' message if it cannot reach it, and otherwise
hands the client straight to tui.Run. preflight / promptPersist /
promptYes stay defined for now even though no caller remains — a
later cleanup pass will retire them alongside the legacy systemd
unit they used to advertise.
```

# 严格规则

1. 改 1 个文件（cmd/nft-forward/main.go）。**其他文件字节级不变**。
2. **不删** preflight / promptPersist / promptYes 函数（让它们暂时 unused）。
3. Commit message 严格使用修正后的版本（无 phase / step / round 等过程信息）。
4. 任何 step 失败立即停下报告 BLOCKED + log。

---

## Task 4: docs 更新

`docs/daemon-manual-verification.md` 已知限制列表中"TUI 仍走旧路径"这一条要改 — TUI 已经接入。

**Files:**
- Modify: `docs/daemon-manual-verification.md`

- [ ] **Step 1: 改 daemon-manual-verification.md "已知限制" 章节**

定位文件末尾 `## 已知限制` 章节：

```markdown
## 已知限制

- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI / server / agent 仍走旧路径**，与 daemon 并存 — 同机同时跑 daemon 和旧 TUI/agent **会冲突**（都想独占本机 nftables 表），验证时只跑 daemon
```

替换为：

```markdown
## 已知限制

- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI 已接入 daemon** — 运行 `sudo nft-forward`（默认子命令）会通过 unix socket 与 daemon 对话；daemon 没起会立即报错，不再 fallback 直接管 nftables
- **server / agent 仍走旧路径** — `nft-server` / `nft-agent` 二进制各自直接操作 nftables，与 daemon 并存会撞表。生产部署目前仍只能选一种：要么单机跑 daemon + TUI，要么跑 server/agent 集群（不要混用）
```

- [ ] **Step 2: 验证 markdown 仍合法**

Run:
```bash
awk 'BEGIN{n=0} /^```/{n++} END{print "code fences:", n, (n%2==0 ? "OK" : "UNBALANCED")}' docs/daemon-manual-verification.md
```
Expected: `code fences: <even> OK`

- [ ] **Step 3: Commit**

```bash
git add docs/daemon-manual-verification.md
git commit -m "docs: note TUI now reaches nftables through the daemon

The 'known limits' section used to lump TUI in with server/agent as
still bypassing the daemon. TUI is the first real client now —
sudo nft-forward speaks to the daemon over the unix socket, and
fails loudly if the daemon is not running rather than silently
managing nftables itself. server/agent stay on their old paths and
still must not be mixed with the daemon on the same host."
```

---

## End-to-end gate (after all tasks)

- [ ] **Gate 1: 全包测试 PASS**

Run:
```bash
go test ./... -count=1
```

- [ ] **Gate 2: race detector 干净**

Run:
```bash
go test ./internal/daemon/ ./internal/daemonclient/ ./internal/tui/ -race -count=1
```

- [ ] **Gate 3: vet 干净**

Run:
```bash
go vet ./...
```

- [ ] **Gate 4: 手动 e2e（root + Linux host）**

按 `docs/daemon-manual-verification.md` 前 6 步起 daemon、curl 验证 socket。然后**另起一个终端**：

```bash
sudo ./build/nft-forward
```

- 不报"无法连接 daemon"
- TUI 启动，看到刚才 curl POST 进去的 tui owner 规则
- 在 TUI 加一条规则、删一条规则 → 都成功
- 切回 curl 验证：`curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset` 反映改动
- `sudo nft list table ip nft_forward` 反映改动

如果 daemon 没起，先停掉再跑 TUI：

```bash
sudo ./build/nft-forward
```

应该立即报错并提示：`请先启动 daemon：sudo systemctl start nft-forward-daemon.service`

---

## Phase C 完成后的状态

- 新 `internal/daemonclient/` 包提供本机 daemon 的 HTTP-over-unix-socket client
- TUI 是 daemon 的第一个真实 client；删除了 TUI 内部的 store / nft.Apply / nft-forward.service 安装逻辑
- 用户路径：装 daemon service → 跑 TUI（连 daemon） → 改规则 → daemon 写 nftables + state.json
- **server / agent 仍走旧路径**（接入留到 Phase D）

**下一阶段（Phase D — server / agent client 化 + 删除旧 cmd）**：
- `internal/server/pusher` / `internal/server/poller` 通过 daemonclient（本机走 unix socket，远程走 HTTP）；删除 `local://` sentinel
- `internal/agent` 缩成 thin HTTP→socket proxy（或者直接合并入 daemon 的 HTTP-enable 模式）
- 删除 `cmd/nft-agent` 和 `cmd/nft-server` 两个 binary 目录，归并到 `cmd/nft-forward` 的 server / 远程 daemon subcommand 下

---

## Self-Review Checklist

- [x] 每个 task 自包含可独立 review/revert
- [x] Commit message 解释 WHY，无过程信息（Phase / Task / Step / Round / 引用前 task）
- [x] Spec Phase C 覆盖：daemonclient(T1) / TUI 改造(T2) / cmd 改造(T3) / docs(T4)
- [x] 无 TBD / TODO / "implement later"（preflight/promptPersist/promptYes 暂留是显式决策不是 TODO）
- [x] 每步都给出具体代码 / 命令
- [x] Type 一致：`Client`、`OwnerRuleset`、`daemonClient` interface、`fakeDaemonClient`、`segmentPayload`、`fullPayload`、`commit(client, rules)` 全文统一
- [x] **server / agent 旧路径字节级不变**（Phase C 不接 server/agent，留 Phase D）
- [x] `cmd/nft-forward --apply` 不动（仍走 store.Load + nft.Apply）— Phase E install.sh 重写时同步处理
