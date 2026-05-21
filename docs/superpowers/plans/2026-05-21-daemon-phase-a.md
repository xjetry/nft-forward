# Host Daemon — Phase A 骨架 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 `internal/daemon` 包 + `nft-forward daemon` 子命令：新增一个独立守护进程，监听 unix socket，提供 HTTP-over-unix-socket API 接收 ruleset 提交，调用 `nft.Apply` 写本机 nftables，并把 ruleset 持久化到 `/var/lib/nft-forward/state.json`。**Phase A 完全不动现有 TUI / server / agent 代码与行为**；daemon 是新增的并行路径，旧三套二进制继续按现状跑。

**Architecture:** Daemon = HTTP server + Applier interface + state.json persistence + unix socket listener。Applier 抽象让单测可以替换 fake，避免对 nftables 内核 / root 权限的依赖。Phase A 的 state 是 **owner-less 扁平 `[]nft.Rule`**（owner-segment 模型留到 Phase B）。

**Tech Stack:** Go 1.22+、`net/http`、`net.Listen("unix", ...)`、stdlib only — 不引入新依赖。

**Spec:** `docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md`（Phase A 对应"实现规模与迁移路径"节 A 阶段）

---

## File Structure

| 路径 | 操作 | 职责 |
|---|---|---|
| `internal/daemon/applier.go` | **新增** | `Applier` interface + 生产实现（包装 `nft.Apply`） |
| `internal/daemon/state.go` | **新增** | `LoadState` / `SaveState`：读写 `state.json` |
| `internal/daemon/handlers.go` | **新增** | HTTP handlers：`/v1/health`、`/v1/ruleset` GET/POST |
| `internal/daemon/socket.go` | **新增** | Unix socket listener：创建 + chmod 0660 + 可选 chown group |
| `internal/daemon/daemon.go` | **新增** | `Daemon` struct + `Config` + `New` + `Bootstrap` + `Run` + `RunWithSignals` |
| `internal/daemon/*_test.go` | **新增** | 对应每个文件的单测 |
| `cmd/nft-forward/main.go` | **改** | 加 `daemon` 子命令分发（保留所有现有 flag） |
| `docs/daemon-phase-a.md` | **新增** | 手动 smoke test 指南 |

---

## Task 1: Applier 抽象 + state.json 读写

最底层基础：把"调用 nft 应用规则"抽到 interface（生产用真 `nft.Apply`，测试用 fake），并实现 state 文件的 atomic 持久化。

**Files:**
- Create: `internal/daemon/applier.go`
- Create: `internal/daemon/state.go`
- Create: `internal/daemon/applier_test.go`
- Create: `internal/daemon/state_test.go`

- [ ] **Step 1: 写 applier_test.go**

```go
package daemon

import (
	"testing"

	"nft-forward/internal/nft"
)

type fakeApplier struct {
	last []nft.Rule
	err  error
}

func (f *fakeApplier) Apply(rules []nft.Rule) error {
	f.last = append([]nft.Rule(nil), rules...)
	return f.err
}

func TestApplier_FakeAndDefaultSatisfyInterface(t *testing.T) {
	var _ Applier = (*fakeApplier)(nil)
	var _ Applier = nftApplier{}
}
```

- [ ] **Step 2: 写 applier.go 让 Step 1 通过**

```go
package daemon

import "nft-forward/internal/nft"

// Applier hides the concrete nftables call so the daemon can be
// exercised in unit tests without root or a real kernel ruleset.
type Applier interface {
	Apply(rules []nft.Rule) error
}

type nftApplier struct{}

func (nftApplier) Apply(rules []nft.Rule) error { return nft.Apply(rules) }

// DefaultApplier returns the production Applier backed by internal/nft.
func DefaultApplier() Applier { return nftApplier{} }
```

- [ ] **Step 3: 跑 Applier 单测**

Run:
```bash
go test ./internal/daemon/ -run TestApplier -v
```
Expected: `PASS`

- [ ] **Step 4: 写 state_test.go**

```go
package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"nft-forward/internal/nft"
)

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	rules, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadState missing: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected empty, got %d", len(rules))
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	in := []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 8080, DestIP: "1.2.3.4", DestPort: 80, Comment: "demo"},
		{ID: "r2", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
	}
	if err := SaveState(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 2 || out[0].ID != "r1" || out[1].SrcPort != 53 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestLoadState_WrongVersionErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"rules":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected version error")
	}
}

func TestSaveState_AtomicViaTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := SaveState(path, []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 1, DestPort: 1}}); err != nil {
		t.Fatal(err)
	}
	// 临时文件不应该残留
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file leaked: stat err = %v", err)
	}
}
```

- [ ] **Step 5: 写 state.go 让 Step 4 通过**

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"nft-forward/internal/nft"
)

const stateSchemaVersion = 1

type stateFile struct {
	Version int        `json:"version"`
	Rules   []nft.Rule `json:"rules"`
}

// LoadState reads ruleset state from path. Missing file returns
// (nil, nil) so callers can treat first boot as empty state.
func LoadState(path string) ([]nft.Rule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if sf.Version != stateSchemaVersion {
		return nil, fmt.Errorf("unsupported state version %d (want %d)", sf.Version, stateSchemaVersion)
	}
	return sf.Rules, nil
}

// SaveState writes the ruleset atomically: write to path+".tmp" first,
// then rename. A reader seeing path always sees a fully written file.
func SaveState(path string, rules []nft.Rule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	sf := stateFile{Version: stateSchemaVersion, Rules: rules}
	b, err := json.MarshalIndent(&sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 6: 跑全部单测**

Run:
```bash
go test ./internal/daemon/ -v
```
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/applier.go internal/daemon/applier_test.go \
        internal/daemon/state.go   internal/daemon/state_test.go
git commit -m "daemon: introduce Applier abstraction and atomic state.json persistence

Applier hides the nft.Apply call behind an interface so daemon logic
can be unit-tested without root or a live nftables kernel module.
state.json uses MarshalIndent + write-temp-then-rename so a concurrent
reader never observes a torn file. Version field guards against
future schema migrations failing silently."
```

---

## Task 2: HTTP handlers — health + ruleset

把 daemon 的 HTTP 表面写完整，**仍未挂任何 transport**（socket / TCP）。`httptest.NewServer` 就能完整测。

**Files:**
- Create: `internal/daemon/handlers.go`
- Create: `internal/daemon/handlers_test.go`

- [ ] **Step 1: 写 handlers_test.go**

```go
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nft-forward/internal/nft"
)

// newTestServer wires a Daemon with provided applier and a temp state file
// into an httptest.Server. Returns the daemon (for inspection) and a teardown.
func newTestServer(t *testing.T, applier Applier) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		applier:   applier,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		mu:        sync.Mutex{},
	}
	return d, httptest.NewServer(d.Handler())
}

func TestHandler_Health(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got["ok"] {
		t.Fatalf("expected ok=true, got %v", got)
	}
}

func TestHandler_GetRulesetEmpty(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got rulesetPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 0 {
		t.Fatalf("expected empty rules, got %d", len(got.Rules))
	}
}

func TestHandler_PostRuleset_AppliesAndSavesAndIsReadable(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if len(fa.last) != 1 || fa.last[0].SrcPort != 8080 {
		t.Fatalf("Apply not called with expected rule: %+v", fa.last)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 {
		t.Fatalf("state not saved, got %+v", saved)
	}

	// GET should now reflect the new rule.
	resp2, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got rulesetPayload
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" {
		t.Fatalf("GET after POST mismatch: %+v", got.Rules)
	}
}

func TestHandler_PostRuleset_ApplyErrorReturns500AndDoesNotSave(t *testing.T) {
	fa := &fakeApplier{err: errors.New("nft failed")}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":1,"dest_ip":"1.2.3.4","dest_port":1}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// state file should not have been created since apply failed.
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("state was saved despite apply error: %+v", saved)
	}
}

func TestHandler_PostRuleset_BadJSON(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_PutRulesetNotAllowed(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/ruleset", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// Reference unused symbols to keep imports tidy in case the suite is split.
var _ = nft.Rule{}
```

> **关于 `Daemon` 类型**：上面的测试用 `&Daemon{...}` 直接构造，因为 daemon.go 里 struct 字段 export 不是必需 — 当前文件（handlers.go）和测试在同一个包内。`mu sync.Mutex` 是 zero-value-usable。Task 4 会引入 `New(Config)` 构造器作为对外入口；这里的直接构造仅用于 in-package 测试。

- [ ] **Step 2: 写 handlers.go 让 Step 1 通过**

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"sync"

	"nft-forward/internal/nft"
)

// Daemon holds the in-memory ruleset + applier wiring used by both the
// HTTP handlers and the lifecycle code in daemon.go.
// Fields are intentionally unexported and assembled via New() (see daemon.go);
// the in-package tests in handlers_test.go construct Daemon directly because
// they only need the handler surface.
type Daemon struct {
	socketPath string
	statePath  string
	groupName  string
	applier    Applier

	mu    sync.Mutex
	rules []nft.Rule
}

type rulesetPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// Handler returns the HTTP mux serving all daemon endpoints.
// Mounted on whichever transport (unix socket or TCP) daemon.Run sets up.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/ruleset", d.handleRuleset)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (d *Daemon) handleRuleset(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleGetRuleset(w, r)
	case http.MethodPost:
		d.handlePostRuleset(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Daemon) handleGetRuleset(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	out := append([]nft.Rule{}, d.rules...)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, rulesetPayload{Rules: out})
}

func (d *Daemon) handlePostRuleset(w http.ResponseWriter, r *http.Request) {
	var p rulesetPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Apply first; if the kernel rejects the ruleset, we must NOT persist it,
	// otherwise a daemon restart would re-apply a known-bad set.
	if err := d.applier.Apply(p.Rules); err != nil {
		http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := SaveState(d.statePath, p.Rules); err != nil {
		http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.rules = p.Rules
	writeJSON(w, http.StatusOK, map[string]int{"count": len(p.Rules)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 3: 跑 handlers 测试**

Run:
```bash
go test ./internal/daemon/ -v -run TestHandler
```
Expected: 全部 PASS

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/handlers.go internal/daemon/handlers_test.go
git commit -m "daemon: add HTTP handlers for health and full ruleset replace

Endpoints are mounted on whichever transport the daemon lifecycle
wires later (unix socket today, optionally HTTP for remote panel push
in a later phase). Apply runs before SaveState — a kernel rejection
must not poison the on-disk state, otherwise a restart would re-apply
a known-bad ruleset forever."
```

---

## Task 3: Unix socket listener

把 listener 创建（含权限 / group 处理 / stale socket 清理）单独提出来，daemon lifecycle 里调用。

**Files:**
- Create: `internal/daemon/socket.go`
- Create: `internal/daemon/socket_test.go`

- [ ] **Step 1: 写 socket_test.go**

```go
package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenSocket_CreatesSocketWith0660(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := ListenSocket(sockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("perm = %v, want 0660", info.Mode().Perm())
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("file is not a socket: %v", info.Mode())
	}
}

func TestListenSocket_ReplacesStaleFile(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	if err := os.WriteFile(sockPath, []byte("stale leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ListenSocket(sockPath, "")
	if err != nil {
		t.Fatalf("ListenSocket failed despite stale file: %v", err)
	}
	defer l.Close()
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatal("stale file was not replaced with a socket")
	}
}

func TestListenSocket_NonexistentGroupIsNotFatal(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := ListenSocket(sockPath, "definitely-no-such-group-xyz")
	if err != nil {
		t.Fatalf("missing group should not error: %v", err)
	}
	defer l.Close()
	// 仍然是一个有效 socket
	conn, dialErr := net.Dial("unix", sockPath)
	if dialErr != nil {
		t.Fatalf("dial failed: %v", dialErr)
	}
	conn.Close()
}
```

- [ ] **Step 2: 写 socket.go 让 Step 1 通过**

```go
package daemon

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
)

// ListenSocket binds a unix-domain socket at path with mode 0660 and
// (best-effort) the given group ownership. A stale file at path is
// removed first so daemon restarts after an unclean shutdown still
// succeed. groupName == "" or a missing group is non-fatal — the
// socket is still created, just with whatever default group the
// daemon's effective UID maps to.
func ListenSocket(path, groupName string) (net.Listener, error) {
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	if groupName != "" {
		if g, lookupErr := user.LookupGroup(groupName); lookupErr == nil {
			gid, _ := strconv.Atoi(g.Gid)
			// Ignore chown errors — non-root tests will fail this but the
			// socket itself is fine. Production runs as root.
			_ = os.Chown(path, os.Geteuid(), gid)
		}
	}
	return l, nil
}
```

- [ ] **Step 3: 跑 socket 测试**

Run:
```bash
go test ./internal/daemon/ -v -run TestListenSocket
```
Expected: 全部 PASS

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/socket.go internal/daemon/socket_test.go
git commit -m "daemon: unix socket listener with permissions and stale-file cleanup

Mode 0660 + best-effort group chown lets users in a nft-forward
group reach the daemon without sudo. Stale-file removal makes daemon
restart after an unclean shutdown (kill -9, OOM) succeed without
manual rm. A missing group is non-fatal so first boots before
install.sh creates the group still work."
```

---

## Task 4: Daemon lifecycle (Config / New / Bootstrap / Run / RunWithSignals)

把前三 task 拼起来，提供对外的 daemon entry point。

**Files:**
- Create: `internal/daemon/daemon.go`
- Create: `internal/daemon/daemon_test.go`

- [ ] **Step 1: 写 daemon_test.go**

```go
package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

func TestNew_DefaultsApplied(t *testing.T) {
	d := New(Config{})
	if d.socketPath != DefaultSocketPath {
		t.Errorf("socketPath default = %q, want %q", d.socketPath, DefaultSocketPath)
	}
	if d.statePath != DefaultStatePath {
		t.Errorf("statePath default = %q, want %q", d.statePath, DefaultStatePath)
	}
	if d.groupName != DefaultGroupName {
		t.Errorf("groupName default = %q, want %q", d.groupName, DefaultGroupName)
	}
	if d.applier == nil {
		t.Fatal("applier nil after New(Config{})")
	}
}

func TestNew_ExplicitOverrides(t *testing.T) {
	fa := &fakeApplier{}
	d := New(Config{
		SocketPath: "/tmp/x.sock",
		StatePath:  "/tmp/x.json",
		GroupName:  "g",
		Applier:    fa,
	})
	if d.socketPath != "/tmp/x.sock" || d.statePath != "/tmp/x.json" || d.groupName != "g" {
		t.Fatalf("overrides not applied: %+v", d)
	}
	if d.applier != fa {
		t.Fatal("custom applier not used")
	}
}

func TestBootstrap_LoadsAndApplies(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 8080},
	}); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApplier{}
	d := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(t.TempDir(), "s.sock"),
		Applier:    fa,
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(fa.last) != 1 || fa.last[0].SrcPort != 80 {
		t.Fatalf("Bootstrap did not apply state: %+v", fa.last)
	}
	if len(d.rules) != 1 {
		t.Fatalf("in-memory rules not populated: %+v", d.rules)
	}
}

func TestBootstrap_EmptyStateIsFine(t *testing.T) {
	d := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "missing.json"),
		SocketPath: filepath.Join(t.TempDir(), "s.sock"),
		Applier:    &fakeApplier{},
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap on empty state: %v", err)
	}
}

func TestRun_AcceptsSocketTrafficAndShutsDown(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	statePath := filepath.Join(t.TempDir(), "state.json")
	fa := &fakeApplier{}
	d := New(Config{
		SocketPath: sockPath,
		StatePath:  statePath,
		GroupName:  "",
		Applier:    fa,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Run did not exit within 3s after cancel")
		}
	})

	// 等 socket 出现，最多 1s
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket never appeared: %v", err)
	}

	// 通过 unix-socket dial 提交一条 ruleset
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	body := `{"rules":[{"id":"rZ","proto":"tcp","src_port":9090,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := client.Post("http://unix/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if len(fa.last) != 1 || fa.last[0].ID != "rZ" {
		t.Fatalf("applier did not see POSTed rule: %+v", fa.last)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].ID != "rZ" {
		t.Fatalf("state.json not persisted as expected: %+v", saved)
	}
}
```

- [ ] **Step 2: 写 daemon.go 让 Step 1 通过**

```go
package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	DefaultSocketPath = "/var/run/nft-forward.sock"
	DefaultStatePath  = "/var/lib/nft-forward/state.json"
	DefaultGroupName  = "nft-forward"
)

// Config wires the daemon's external dependencies. All fields have
// production defaults applied by New so a zero-value Config "just works".
type Config struct {
	SocketPath string
	StatePath  string
	GroupName  string
	Applier    Applier
}

// New constructs a Daemon ready to Bootstrap and Run. Fields not set on
// Config are filled from the Default* constants. Applier defaults to
// the production nft-backed implementation.
func New(cfg Config) *Daemon {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	if cfg.StatePath == "" {
		cfg.StatePath = DefaultStatePath
	}
	if cfg.GroupName == "" {
		cfg.GroupName = DefaultGroupName
	}
	if cfg.Applier == nil {
		cfg.Applier = DefaultApplier()
	}
	return &Daemon{
		socketPath: cfg.SocketPath,
		statePath:  cfg.StatePath,
		groupName:  cfg.GroupName,
		applier:    cfg.Applier,
	}
}

// Bootstrap loads persisted state and re-applies it so the kernel ruleset
// reflects the last known good configuration immediately on daemon startup.
// Must be called before Run.
func (d *Daemon) Bootstrap() error {
	rules, err := LoadState(d.statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if len(rules) > 0 {
		if err := d.applier.Apply(rules); err != nil {
			return fmt.Errorf("apply persisted state: %w", err)
		}
	}
	d.mu.Lock()
	d.rules = rules
	d.mu.Unlock()
	return nil
}

// Run is the main lifecycle: bootstrap → listen → serve → block until ctx is
// cancelled. The socket file is removed on exit so subsequent runs do not
// hit a stale file. Returns nil on clean shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Bootstrap(); err != nil {
		return err
	}
	l, err := ListenSocket(d.socketPath, d.groupName)
	if err != nil {
		return err
	}
	defer os.Remove(d.socketPath)
	srv := &http.Server{Handler: d.Handler()}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// RunWithSignals is the production entry point: SIGINT / SIGTERM trigger a
// graceful shutdown via Run's context cancellation.
func (d *Daemon) RunWithSignals() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("nft-forward daemon: listening on %s", d.socketPath)
	return d.Run(ctx)
}
```

- [ ] **Step 3: 跑 daemon 测试**

Run:
```bash
go test ./internal/daemon/ -v
```
Expected: 全部 PASS（含 Task 1-3 全部测试 + 本 task 的）

- [ ] **Step 4: 跑 race detector 检查并发访问**

Run:
```bash
go test ./internal/daemon/ -race
```
Expected: 无 DATA RACE 报告

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go
git commit -m "daemon: lifecycle entry points Bootstrap, Run, RunWithSignals

Bootstrap re-applies persisted state so kernel ruleset matches state.json
the moment daemon starts — restarts are transparent to traffic.
Run serves HTTP over the unix socket and removes the socket file on
exit. RunWithSignals adds SIGINT/SIGTERM handling so systemctl stop
triggers graceful Shutdown."
```

---

## Task 5: `cmd/nft-forward daemon` subcommand

把 daemon 入口接到二进制。**必须保持所有现有 flag（`--apply` / `--install-service` / `--uninstall-service` / 默认 TUI）的行为不变** —— Phase A 不替换任何旧路径。

**Files:**
- Modify: `cmd/nft-forward/main.go`

- [ ] **Step 1: 看一下当前 main 函数结构以确认插入点**

Run:
```bash
sed -n '17,40p' cmd/nft-forward/main.go
```
Expected: 看到 `main()` 函数内是 `flag.BoolVar` 三个 flag + `flag.Parse()` + `switch` 选择 mode。`daemon` subcommand 要在 `flag.Parse()` **之前**截走 `os.Args[1] == "daemon"`，否则全局 flag 会消费它。

- [ ] **Step 2: 改 main.go：在 main() 第一句加 subcommand 截获**

完整改写后的 `cmd/nft-forward/main.go`：

```go
package main

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

func main() {
	// Subcommand dispatch must precede flag.Parse() so the global flag set
	// does not try to consume subcommand-specific args.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		os.Exit(runDaemon())
	}

	var (
		applyOnly  bool
		uninstall  bool
		installSvc bool
	)
	flag.BoolVar(&applyOnly, "apply", false, "加载 rules.json 并应用到内核后退出（开机由 systemd 调用）")
	flag.BoolVar(&installSvc, "install-service", false, "安装 systemd 单元以实现开机持久化后退出")
	flag.BoolVar(&uninstall, "uninstall-service", false, "卸载 systemd 持久化单元后退出")
	flag.Parse()

	switch {
	case applyOnly:
		os.Exit(runApply())
	case installSvc:
		os.Exit(runInstallService())
	case uninstall:
		os.Exit(runUninstall())
	default:
		os.Exit(runTUI())
	}
}

func runDaemon() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nft-forward daemon 必须以 root 身份运行")
		return 1
	}

	var (
		socketPath string
		statePath  string
		groupName  string
	)
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}

	if err := sysdeps.Ensure("nftables"); err != nil {
		fmt.Fprintln(os.Stderr, "依赖检查失败:", err)
		return 1
	}
	if !nft.Available() {
		fmt.Fprintln(os.Stderr, "nft 命令不可用，请先安装 nftables")
		return 1
	}
	if err := nft.Probe(); err != nil {
		fmt.Fprintln(os.Stderr, "nft 检测失败:", err)
		return 1
	}
	if !nft.IPForwardEnabled() {
		if err := nft.EnableIPForward(); err != nil {
			fmt.Fprintln(os.Stderr, "启用 ip_forward 失败:", err)
			return 1
		}
	}

	d := daemon.New(daemon.Config{
		SocketPath: socketPath,
		StatePath:  statePath,
		GroupName:  groupName,
	})
	if err := d.RunWithSignals(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon 运行失败:", err)
		return 1
	}
	return 0
}

func runApply() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if !nft.Available() {
		fmt.Fprintln(os.Stderr, "未找到 nft 命令")
		return 1
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
	fmt.Printf("nft-forward: 已应用 %d 条规则\n", len(rules))
	return 0
}

func runInstallService() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if err := systemd.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "安装失败:", err)
		return 1
	}
	fmt.Println("已安装 systemd 单元 nft-forward.service；规则将在开机时自动恢复")
	return 0
}

func runUninstall() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if err := systemd.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "卸载失败:", err)
		return 1
	}
	fmt.Println("已移除 systemd 单元；rules.json 与当前内核规则保持不变")
	return 0
}

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

func preflight() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("必须以 root 身份运行（尝试: sudo %s）", os.Args[0])
	}

	if err := sysdeps.Ensure("nftables"); err != nil {
		return err
	}

	if err := nft.Probe(); err != nil {
		return err
	}

	if !nft.IPForwardEnabled() {
		fmt.Println("net.ipv4.ip_forward 未启用，正在启用...")
		if err := nft.EnableIPForward(); err != nil {
			return fmt.Errorf("启用 ip_forward 失败: %w", err)
		}
	}
	return nil
}

func promptPersist() bool {
	fmt.Println("尚未配置开机持久化。")
	fmt.Println("启用后将把本程序复制到 /usr/local/sbin/nft-forward，")
	fmt.Println("并注册 systemd 单元，使保存的规则在每次开机时自动恢复。")
	return promptYes("现在启用持久化？[Y/n]: ", true)
}

func promptYes(prompt string, defaultYes bool) bool {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return defaultYes
	}
	return ans == "y" || ans == "yes"
}
```

> **改动只增不减**：除了 main() 顶端加 `daemon` subcommand 截获 + 新增 `runDaemon()` 函数 + import `internal/daemon`，其它全部行为与原文件相同。已有的 TUI / `--apply` / `--install-service` / `--uninstall-service` 路径**字节级不变**。

- [ ] **Step 3: 验证编译通过**

Run:
```bash
go build -o /tmp/nft-forward-build ./cmd/nft-forward
ls -l /tmp/nft-forward-build && rm /tmp/nft-forward-build
```
Expected: 产出可执行文件、无编译错误

- [ ] **Step 4: 验证 daemon subcommand 不破坏现有 flag 解析**

Run:
```bash
go build -o /tmp/nft-forward-build ./cmd/nft-forward
/tmp/nft-forward-build --help 2>&1 | head -10
/tmp/nft-forward-build daemon --help 2>&1 | head -10
rm /tmp/nft-forward-build
```
Expected:
- `--help` 输出含 `-apply` / `-install-service` / `-uninstall-service` 三个 flag（现状）
- `daemon --help` 输出 `-socket` / `-state` / `-group` 三个新 flag

- [ ] **Step 5: 跑全部包测试确认无回归**

Run:
```bash
go test ./...
```
Expected: 全部 PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/nft-forward/main.go
git commit -m "cmd: add 'nft-forward daemon' subcommand entry

Dispatches before flag.Parse() so the daemon subcommand owns its own
FlagSet (socket / state / group) without colliding with the existing
TUI flag surface. All pre-existing entry points (--apply,
--install-service, --uninstall-service, default TUI) are unchanged."
```

---

## Task 6: 手动 smoke test 指南

骨架阶段没有 user-facing 功能（既不接入 TUI 也不接入 server/agent），但可以用 curl + unix socket 端到端验证 daemon。写成长期维护的 `docs/daemon-manual-verification.md`，daemon 后续演进时更新而不是重写。

**Files:**
- Create: `docs/daemon-manual-verification.md`

- [ ] **Step 1: 写文档**

```markdown
# Daemon 手动验证

直接用 `curl --unix-socket` 打 daemon 的 unix socket API 端到端验证。daemon 当前不接入 TUI / server / agent — 后两者仍走旧路径。

## 前置

- Linux + nftables（与现有 nft-forward 一致）
- Go 1.22+
- root 终端（daemon 必须 root 跑）

## Build

```bash
go build -trimpath -ldflags="-s -w" -o ./build/nft-forward ./cmd/nft-forward
```

## 跑 daemon

```bash
sudo ./build/nft-forward daemon \
    --socket=/tmp/nft-forward.sock \
    --state=/tmp/nft-forward-state.json \
    --group=""
```

预期 stdout：

```
nft-forward daemon: listening on /tmp/nft-forward.sock
```

## 在另一个终端验证

```bash
# 1. health
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/health
# → {"ok":true}

# 2. 取空 ruleset
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"rules":[]}

# 3. 提交一条 ruleset
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rT","proto":"tcp","src_port":19090,"dest_ip":"127.0.0.1","dest_port":22}]}' \
     http://unix/v1/ruleset
# → {"count":1}

# 4. 验证内核确实写入了
sudo nft list table ip nft_forward | grep 19090
# → 应看到 'tcp dport 19090 counter ... dnat to 127.0.0.1:22'

# 5. 再 GET 一次
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"rules":[{"id":"rT",...}]}

# 6. 验证 state 文件
cat /tmp/nft-forward-state.json
# → 含 version: 1 + rules 数组

# 7. Ctrl-C 终止 daemon → /tmp/nft-forward.sock 应被清理
ls /tmp/nft-forward.sock
# → No such file or directory
```

## Restart 恢复验证

1. 重新启动 daemon（同样的 `--state` 路径）
2. 不发任何 POST，立即 `sudo nft list table ip nft_forward` — 应该看到上一次的 rule（Bootstrap 从 state.json 重放）

## 已知限制

- **无 owner segmentation** — 整套 ruleset 由后写的 POST 完全替换前一次提交。owner 分段是后续要做的能力
- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI / server / agent 仍走旧路径**，与 daemon 并存 — 同机同时跑 daemon 和旧 TUI/agent **会冲突**（都想独占本机 nftables 表），验证时只跑 daemon
```

- [ ] **Step 2: Commit**

```bash
git add docs/daemon-manual-verification.md
git commit -m "docs: manual verification recipe for the daemon

Walks an operator through build → run daemon → curl socket → verify
nftables → restart → state recovery. Documents the four current
limits (no owner segmentation, no remote transport, no auth beyond
socket permissions, TUI/server/agent unmigrated) so reviewers know
what's deliberately unbuilt today."
```

---

## End-to-end gate (after all tasks)

- [ ] **Gate 1: 单元测试无回归**

Run:
```bash
go test ./...
```
Expected: PASS

- [ ] **Gate 2: race detector 干净**

Run:
```bash
go test ./internal/daemon/ -race
```
Expected: 无 DATA RACE 报告

- [ ] **Gate 3: 手动 smoke 走通（按 `docs/daemon-phase-a.md`）**

在一台 root + nftables 可用的 Linux host 上执行文档里的 7 个 curl 步骤 + restart 恢复验证，每步预期输出一致。

- [ ] **Gate 4: 现有路径未回归**

```bash
# TUI 仍能起（root + 真 nft 环境）
sudo ./build/nft-forward --help

# --apply 仍能工作
sudo ./build/nft-forward --apply 2>&1 | head -2
```
Expected: 与 Phase A 前版本表面一致

---

## Phase A 完成后的状态

- `internal/daemon` 包齐全：applier / state / handlers / socket / lifecycle
- `nft-forward daemon` 子命令可独立运行，持有 `/var/run/nft-forward.sock`
- daemon 重启可从 `/var/lib/nft-forward/state.json` 恢复
- **TUI / server / agent 行为完全不变**（仍走旧 in-process / HTTP 路径）
- 没有 owner segmentation —— **同时跑 daemon 和旧 TUI 会冲突**，验证时只跑一个

**下一阶段（Phase B — owner segmentation）将做的事**：在 state schema 加 `owners` map、把 POST endpoint 改为 `/v1/ruleset/{owner}` 全量替换该 segment、daemon 内部 merge 多 owner 后 apply、加跨 owner 端口冲突检测、加旧三份 state 文件的导入迁移。Phase B 完成后，TUI 和 server-push 可在同台机器共存（但仍未实际接入，接入是 C/D）。

---

## Self-Review Checklist

- [x] 每个 task 自包含、单独可 review/revert
- [x] 所有 commit message 解释 WHY，无过程信息（无 "Phase X / Step Y / Task N / Round Z / based on" 用语）
- [x] Spec Phase A 范围全覆盖：daemon 包(1-4) / cmd subcommand(5) / smoke 文档(6)
- [x] 无 TBD / TODO / "implement later"
- [x] 每步都有具体代码或命令
- [x] Type / 函数名一致：`Applier`、`Daemon`、`Config`、`New`、`Bootstrap`、`Run`、`RunWithSignals`、`Handler`、`ListenSocket`、`LoadState`、`SaveState`、`DefaultSocketPath` / `DefaultStatePath` / `DefaultGroupName` 全文统一
- [x] 现有路径（TUI / `--apply` / `--install-service` / `--uninstall-service`）字节级不变 — main.go 改动只增不减
