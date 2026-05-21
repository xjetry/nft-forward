# Host Daemon — Phase B 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 引入 **owner-segmented ruleset** + **旧 state 文件迁移**：daemon 内部把规则按 owner 分段持有（`tui` / `panel` 等），nftables ruleset = 所有 owner 的合并；跨 owner 端口冲突 daemon 拒绝。同时把现有三种旧 state 文件（`/etc/nft-forward/rules.json`、`/var/lib/nft-forward/agent-state.json`、`/var/lib/nft-forward/embedded-agent-state.json`）在 daemon 启动时自动导入到对应 owner segment。

**Architecture:** state.json schema 升 v2（兼容读 v1）；新增 `OwnerRuleset` 数据模型 + merge/conflict 函数；HTTP 接口 `POST /v1/ruleset/{owner}` 替换 Phase A 的 `POST /v1/ruleset`（旧端点删除）；`GET /v1/ruleset` 返回 segmented 视图；Bootstrap 流程加 legacy migration。**TUI / server / agent 仍走旧路径不变**（接入在 Phase C/D）。

**Tech Stack:** Go 1.22+，stdlib only。仍不引入新依赖。

**Spec:** `docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md` Phase B 节

---

## 与 Spec 的微小修正

Spec 里说 TUI 旧 state 路径是 `/var/lib/nft-forward/rules.json`，但实际 `internal/store/store.go` 用的是 `/etc/nft-forward/rules.json`（可被 `NFT_FORWARD_CONFIG` 环境变量覆盖）。Plan 按代码现状走，迁移源为 `/etc/nft-forward/rules.json`。

Spec 里说"迁移完后删除旧文件"，Plan 改为**重命名为 `.migrated` 后缀**：删除不可逆，rename 留住可被人工检查的备份。Phase B 完成 + 用户确认生产环境无问题后可手动清理。

---

## File Structure

| 路径 | 操作 | 职责 |
|---|---|---|
| `internal/daemon/state.go` | **改** | schema v2：`Owners map[string][]nft.Rule`；LoadState 兼容读 v1 |
| `internal/daemon/state_test.go` | **改** | 加 v1→v2 兼容读、v2 round-trip、empty owners |
| `internal/daemon/owners.go` | **新增** | `OwnerRuleset` 类型 alias + `MergedRuleset` + 跨 owner conflict 检测 |
| `internal/daemon/owners_test.go` | **新增** | merge 排序稳定、conflict 报错带 owner 名 |
| `internal/daemon/handlers.go` | **改** | `POST /v1/ruleset/{owner}` + `GET /v1/ruleset` segmented + 旧端点 410 |
| `internal/daemon/handlers_test.go` | **改** | per-owner POST、conflict 409、GET segmented、旧 endpoint 410、bad path |
| `internal/daemon/daemon.go` | **改** | `Daemon.rules []nft.Rule` → `Daemon.owners OwnerRuleset`；Bootstrap 调用 migration |
| `internal/daemon/daemon_test.go` | **改** | Bootstrap 测试用 owner-segmented state；新增 migration e2e |
| `internal/daemon/migrate.go` | **新增** | `migrateLegacyState(daemonStatePath) error`：检测三个旧路径并导入 |
| `internal/daemon/migrate_test.go` | **新增** | 三种旧文件单独存在、组合存在、不存在；冲突时 panel segment 优先 embedded |
| `docs/daemon-manual-verification.md` | **改** | curl 命令更新到 `/v1/ruleset/tui` + 输出格式 segmented |

---

## Task 1: state schema v2（兼容读 v1）

state.json 内部表达从扁平 `[]nft.Rule` 变为 `map[string][]nft.Rule`（owner → rules）。LoadState 兼容读 v1（旧字段 `rules` 视为 `tui` segment）— v1 内容不会丢，只是被理解为"全部归 tui 拥有"。SaveState 写 v2。

**Files:**
- Modify: `internal/daemon/state.go`
- Modify: `internal/daemon/state_test.go`

- [ ] **Step 1: 改 `internal/daemon/state_test.go` 加 v1 / v2 / empty owners 测试**

完整替换文件内容（保留 round-trip + missing-file + atomic 测试，新增 v1/v2 测试）：

```go
package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"nft-forward/internal/nft"
)

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	owners, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadState missing: %v", err)
	}
	if len(owners) != 0 {
		t.Fatalf("expected empty, got %d owners", len(owners))
	}
}

func TestSaveLoad_RoundTrip_V2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "t1", Proto: "tcp", SrcPort: 8080, DestIP: "1.2.3.4", DestPort: 80, Comment: "demo"},
		},
		"panel": []nft.Rule{
			{ID: "p1", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
			{ID: "p2", Proto: "tcp+udp", SrcPort: 443, DestHost: "example.com", DestIP: "203.0.113.5", DestPort: 8443, BandwidthMbps: 100, Comment: "with bandwidth"},
		},
	}
	if err := SaveState(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\nin = %+v\nout = %+v", in, out)
	}
}

func TestLoadState_V1CompatibilityReadsAsTuiSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// v1 schema: top-level "version":1 + "rules":[...] (flat list)
	v1 := []byte(`{
		"version": 1,
		"rules": [
			{"id":"x1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}
		]
	}`)
	if err := os.WriteFile(path, v1, 0o640); err != nil {
		t.Fatal(err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 owner segment, got %d: %+v", len(out), out)
	}
	tui, ok := out["tui"]
	if !ok {
		t.Fatalf("expected 'tui' owner from v1 migration, got owners: %v", keysOf(out))
	}
	if len(tui) != 1 || tui[0].ID != "x1" {
		t.Fatalf("tui segment after v1 read: %+v", tui)
	}
}

func TestLoadState_UnknownVersionErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"owners":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected version error for v99")
	}
}

func TestSaveState_AtomicViaTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := OwnerRuleset{"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 1, DestPort: 1}}}
	if err := SaveState(path, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file leaked: stat err = %v", err)
	}
}

func TestSaveState_EmptyOwnersWritesValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, OwnerRuleset{}); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty owners, got %+v", out)
	}
}

func keysOf(m OwnerRuleset) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: 跑测试看它们如期 fail**（OwnerRuleset 类型还没定义、LoadState 签名还是旧的）

Run:
```bash
go test ./internal/daemon/ -count=1 -run TestSaveLoad_RoundTrip_V2 2>&1 | head -8
```
Expected: 编译失败（`undefined: OwnerRuleset` 或类似）

- [ ] **Step 3: 改 `internal/daemon/state.go`**

完整替换文件内容：

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

// stateSchemaVersion is bumped whenever the on-disk layout changes in a way
// that requires migration. LoadState accepts older versions and converts in
// memory; SaveState always writes the current version.
const stateSchemaVersion = 2

// OwnerRuleset is the in-memory representation: each known controller
// ("tui", "panel", future additions) owns a slice of rules. The daemon
// merges all owners into one ruleset before calling Applier.Apply.
type OwnerRuleset map[string][]nft.Rule

// stateFile is the on-disk JSON layout for the current schema version.
// New fields go here; reading older versions converts into this shape.
type stateFile struct {
	Version int          `json:"version"`
	Owners  OwnerRuleset `json:"owners"`
}

// legacyV1File is the Phase A on-disk layout. We keep this type defined
// purely to recognize and migrate it; we do not write v1 anymore.
type legacyV1File struct {
	Version int        `json:"version"`
	Rules   []nft.Rule `json:"rules"`
}

// LoadState reads ruleset state from path. Missing file returns an empty
// OwnerRuleset (not nil, so callers can range / index without nil checks).
// v1 files are read transparently and exposed as a single "tui" segment —
// in practice v1 only ever contained rules submitted through the bare
// /v1/ruleset endpoint, which was used by manual smoke tests; assigning
// them to "tui" preserves the data without forcing users to re-submit.
func LoadState(path string) (OwnerRuleset, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return OwnerRuleset{}, nil
	}
	if err != nil {
		return nil, err
	}

	// Peek at the version field first so we don't decode v1 into a v2 shape
	// (which would silently drop the "rules" field).
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("parse state version: %w", err)
	}

	switch probe.Version {
	case stateSchemaVersion:
		var sf stateFile
		if err := json.Unmarshal(b, &sf); err != nil {
			return nil, fmt.Errorf("parse v%d state: %w", stateSchemaVersion, err)
		}
		if sf.Owners == nil {
			sf.Owners = OwnerRuleset{}
		}
		return sf.Owners, nil
	case 1:
		var v1 legacyV1File
		if err := json.Unmarshal(b, &v1); err != nil {
			return nil, fmt.Errorf("parse v1 state: %w", err)
		}
		out := OwnerRuleset{}
		if len(v1.Rules) > 0 {
			out["tui"] = v1.Rules
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported state version %d (want %d or 1)", probe.Version, stateSchemaVersion)
	}
}

// SaveState writes owners atomically at the filesystem-rename level: write
// to path+".tmp" first, then rename. A reader seeing path always observes a
// fully written file. On rename failure the temp file is removed best-effort
// so a retried call does not silently overwrite a stale leftover.
//
// This is not crash-safe at the OS level: no fsync is performed, so a
// system crash between WriteFile and the next page-cache flush can lose
// the latest contents. For daemon state this is acceptable — a crash
// either way means the kernel ruleset has to be reconciled on recovery.
func SaveState(path string, owners OwnerRuleset) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if owners == nil {
		owners = OwnerRuleset{}
	}
	sf := stateFile{Version: stateSchemaVersion, Owners: owners}
	b, err := json.MarshalIndent(&sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
```

- [ ] **Step 4: 跑 state 测试**

Run:
```bash
go test ./internal/daemon/ -count=1 -run "TestLoadState|TestSaveState|TestSaveLoad" -v
```
Expected: 6 个 state 相关测试 PASS（其他 daemon/handlers/applier/socket 测试此时**会编译失败**，因为它们引用了旧 `[]nft.Rule` 签名 — 留给后续 task 修，本 task 仅验证 state.go 本身的正确性）

如果其他测试编译失败导致整个 `go test ./internal/daemon/` 报错，是预期内 — 用 `-run` 范围跑即可。

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/state.go internal/daemon/state_test.go
git commit -m "daemon: state.json schema v2 with owner-segmented ruleset

State now persists rules grouped by owner ('tui', 'panel', future)
rather than as a single flat slice, so multiple controllers can write
to the same daemon without overwriting each other. v1 files are read
transparently — the rules they contain are exposed as a single 'tui'
segment, preserving any data submitted via the Phase A smoke-test
endpoint. SaveState always writes v2."
```

---

## Task 2: owners.go — merge + 跨 owner 冲突检测

Daemon 把所有 owner 的 rules 合并成一份扁平 `[]nft.Rule` 交给 `Applier.Apply`。合并时检查同 `(Proto, SrcPort)` 是否出现在多个 owner 里 — 这是用户跨 owner 撞端口（例如 TUI 加 tcp/80，panel 又推 tcp/80），daemon 拒绝。同 owner 内部冲突由 client 自身负责，daemon 同样在合并时被动检测（防御性）。

**Files:**
- Create: `internal/daemon/owners.go`
- Create: `internal/daemon/owners_test.go`

- [ ] **Step 1: 写 `internal/daemon/owners_test.go`**

```go
package daemon

import (
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func TestMergedRuleset_EmptyOwnersReturnsEmpty(t *testing.T) {
	got, err := MergedRuleset(OwnerRuleset{})
	if err != nil {
		t.Fatalf("merge empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestMergedRuleset_SingleOwnerPassesThrough(t *testing.T) {
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 80},
			{ID: "b", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d: %+v", len(got), got)
	}
}

func TestMergedRuleset_MultiOwnerDeterministicOrder(t *testing.T) {
	// Owners come out sorted by name so the same input always yields the
	// same ruleset — important because nft.Apply diffs against current
	// kernel and a flapping order would cause spurious replace cycles.
	in := OwnerRuleset{
		"panel": []nft.Rule{{ID: "p1", Proto: "tcp", SrcPort: 90, DestIP: "1.0.0.0", DestPort: 90}},
		"tui":   []nft.Rule{{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(got))
	}
	// 'panel' < 'tui' alphabetically → panel rules first.
	if got[0].ID != "p1" || got[1].ID != "t1" {
		t.Fatalf("merge order not deterministic by owner name: %+v", got)
	}
}

func TestMergedRuleset_CrossOwnerSamePortConflicts(t *testing.T) {
	in := OwnerRuleset{
		"tui":   []nft.Rule{{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
		"panel": []nft.Rule{{ID: "b", Proto: "tcp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80}},
	}
	_, err := MergedRuleset(in)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tcp/80") {
		t.Errorf("error should name the conflicting port; got: %s", msg)
	}
	// Must name both owners so the user knows who already has it.
	if !strings.Contains(msg, "tui") || !strings.Contains(msg, "panel") {
		t.Errorf("error should name both owners; got: %s", msg)
	}
}

func TestMergedRuleset_SameOwnerSamePortConflicts(t *testing.T) {
	// Defensive: a buggy client shouldn't be able to crash daemon by
	// submitting two rules with the same port in one POST.
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
			{ID: "b", Proto: "tcp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80},
		},
	}
	_, err := MergedRuleset(in)
	if err == nil {
		t.Fatal("expected intra-owner conflict error")
	}
	if !strings.Contains(err.Error(), "tcp/80") {
		t.Errorf("error should name the conflicting port; got: %s", err.Error())
	}
}

func TestMergedRuleset_DifferentProtoSamePortOK(t *testing.T) {
	// tcp/80 and udp/80 are independent in nftables; the dedup key
	// must include proto.
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
			{ID: "b", Proto: "udp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80},
		},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("tcp+udp on same port should not conflict: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both rules, got %d", len(got))
	}
}
```

- [ ] **Step 2: 写 `internal/daemon/owners.go`**

```go
package daemon

import (
	"fmt"
	"sort"

	"nft-forward/internal/nft"
)

// MergedRuleset flattens all owner segments into a single ruleset suitable
// for nft.Apply. Owners are sorted by name so the output is deterministic
// across runs — nft.Apply diffs against the kernel and a flapping order
// would cause unnecessary replace cycles.
//
// A (proto, src_port) collision either within one owner or across owners
// is rejected with an error that names the port and the colliding owners,
// so the rejected client can surface a clear message to the user.
func MergedRuleset(owners OwnerRuleset) ([]nft.Rule, error) {
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	sort.Strings(names)

	type holder struct {
		owner string
		rule  nft.Rule
	}
	// (proto, src_port) → first owner that took it.
	seen := make(map[string]holder)
	merged := make([]nft.Rule, 0)

	for _, name := range names {
		for _, r := range owners[name] {
			key := fmt.Sprintf("%s/%d", r.Proto, r.SrcPort)
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf(
					"port %s already claimed by owner %q (rule %q); rejecting owner %q (rule %q)",
					key, prev.owner, prev.rule.ID, name, r.ID,
				)
			}
			seen[key] = holder{owner: name, rule: r}
			merged = append(merged, r)
		}
	}
	return merged, nil
}
```

- [ ] **Step 3: 跑 owners 测试**

Run:
```bash
go test ./internal/daemon/ -count=1 -run TestMergedRuleset -v
```
Expected: 6 个测试 PASS

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/owners.go internal/daemon/owners_test.go
git commit -m "daemon: merge owner segments and detect cross-owner port conflicts

MergedRuleset flattens OwnerRuleset into one slice sorted by owner
name so nft.Apply receives a deterministic ruleset (avoids spurious
replace cycles when nothing changed). The merge rejects any (proto,
src_port) tuple that appears in more than one owner segment — or
twice within the same segment — with an error naming both the
existing claimant and the rejected submitter so the client can
surface a precise message to the user."
```

---

## Task 3: handlers — per-owner POST + segmented GET + Daemon owners field

Daemon struct 的 `rules []nft.Rule` 字段改为 `owners OwnerRuleset`；handlers 路由从 `POST /v1/ruleset` 改为 `POST /v1/ruleset/{owner}`；GET 返回 segmented payload；旧扁平 endpoint 返回 410 Gone。

**Files:**
- Modify: `internal/daemon/daemon.go`（仅 Daemon struct 的字段类型 + 已有 Bootstrap 内的字段写法）
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemon/handlers_test.go`
- Modify: `internal/daemon/daemon_test.go`（修字段引用）

- [ ] **Step 1: 确认 `Daemon` struct 当前位置**

Phase A 把 `Daemon` struct 定义在 `internal/daemon/handlers.go` 顶部（约第 13-23 行）。下一步会完整替换 `handlers.go`，包含新版 struct 定义（字段 `rules []nft.Rule` → `owners OwnerRuleset`）以及删除旧的 `rulesetPayload` 类型。

Run（仅 inspection，不做修改）：
```bash
grep -n "^type Daemon struct\|^type rulesetPayload" internal/daemon/handlers.go
```
Expected: 看到 `Daemon` 和 `rulesetPayload` 两个 type 都在 handlers.go。

- [ ] **Step 2: 改 `internal/daemon/handlers.go` — 路由和 payload**

完整替换 handlers.go（包括上一步删 `rulesetPayload`）：

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"nft-forward/internal/nft"
)

// Daemon holds the in-memory owner-segmented ruleset and applier wiring
// shared by the HTTP handlers and the lifecycle code.
// Fields are unexported; production callers go through New().
type Daemon struct {
	socketPath string
	statePath  string
	groupName  string
	applier    Applier

	mu     sync.Mutex
	owners OwnerRuleset
}

// segmentPayload is the body of POST /v1/ruleset/{owner} — replaces the
// entire ruleset segment owned by {owner}.
type segmentPayload struct {
	Rules []nft.Rule `json:"rules"`
}

// fullPayload is the body of GET /v1/ruleset — every owner segment in
// one response so the caller can inspect the full daemon state.
type fullPayload struct {
	Owners OwnerRuleset `json:"owners"`
}

// Handler returns the HTTP mux serving all daemon endpoints.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/ruleset", d.handleRulesetRoot)
	mux.HandleFunc("/v1/ruleset/", d.handleRulesetOwner)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRulesetRoot serves GET /v1/ruleset (segmented payload) and rejects
// POST/PUT/etc explicitly: the flat POST that existed in Phase A is gone,
// callers MUST use /v1/ruleset/{owner} now.
func (d *Daemon) handleRulesetRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.mu.Lock()
		out := cloneOwners(d.owners)
		d.mu.Unlock()
		writeJSON(w, http.StatusOK, fullPayload{Owners: out})
	case http.MethodPost:
		// The flat endpoint is intentionally removed — return 410 with a
		// directive so existing clients (manual smoke tests, scripts) get
		// a clear pointer to the new shape rather than a generic 404.
		http.Error(w, "use POST /v1/ruleset/{owner} to write owner-scoped ruleset", http.StatusGone)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRulesetOwner serves POST /v1/ruleset/{owner}. Empty owner segment
// is allowed in body (clears the segment). Path may not end with a slash.
func (d *Daemon) handleRulesetOwner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	owner := strings.TrimPrefix(r.URL.Path, "/v1/ruleset/")
	if owner == "" || strings.ContainsAny(owner, "/") {
		http.Error(w, "owner segment required: POST /v1/ruleset/{owner}", http.StatusBadRequest)
		return
	}

	var p segmentPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Build the would-be new owner map, then merge to detect conflicts
	// before touching kernel state.
	candidate := cloneOwners(d.owners)
	if len(p.Rules) == 0 {
		delete(candidate, owner)
	} else {
		candidate[owner] = append([]nft.Rule(nil), p.Rules...)
	}
	merged, err := MergedRuleset(candidate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := d.applier.Apply(merged); err != nil {
		http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := SaveState(d.statePath, candidate); err != nil {
		// Kernel ruleset is already updated by the Apply above; the disk
		// state lags behind. A daemon restart would reload the old state
		// and Apply that, rolling the kernel back. We accept this rare
		// window because SaveState failure is extremely unlikely outside
		// of a disk full / read-only fs situation, and reporting 500 lets
		// the client retry or escalate.
		http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.owners = candidate
	writeJSON(w, http.StatusOK, map[string]int{"count": len(p.Rules)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cloneOwners returns a deep-enough copy that the caller can mutate the
// map (delete/replace keys) without affecting the original. Rule slices
// themselves are shallow-copied — Rule is a value type so this is safe.
func cloneOwners(src OwnerRuleset) OwnerRuleset {
	if src == nil {
		return OwnerRuleset{}
	}
	out := make(OwnerRuleset, len(src))
	for k, v := range src {
		out[k] = append([]nft.Rule(nil), v...)
	}
	return out
}
```

- [ ] **Step 3: 改 `internal/daemon/handlers_test.go`**

完整替换文件内容（保留 health + 重构 ruleset 测试）：

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
)

func newTestServer(t *testing.T, applier Applier) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		applier:   applier,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		mu:        sync.Mutex{},
		owners:    OwnerRuleset{},
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

func TestHandler_GetRuleset_EmptyReturnsEmptyOwnersMap(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got fullPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Owners) != 0 {
		t.Fatalf("expected empty owners, got %+v", got.Owners)
	}
}

func TestHandler_PostOwnerSegment_AppliesAndSavesAndIsReadable(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(fa.last) != 1 || fa.last[0].SrcPort != 8080 {
		t.Fatalf("Apply not called with merged ruleset: %+v", fa.last)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved["tui"]) != 1 {
		t.Fatalf("state segment not saved: %+v", saved)
	}

	resp2, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got fullPayload
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Owners["tui"]) != 1 || got.Owners["tui"][0].ID != "r1" {
		t.Fatalf("GET after POST mismatch: %+v", got.Owners)
	}
}

func TestHandler_PostOwnerSegment_CrossOwnerPortConflictReturns409(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	// tui claims tcp/80 first
	resp1, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"t1","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("seed tui failed: %d", resp1.StatusCode)
	}

	// panel tries to take the same tcp/80 — must 409
	resp2, err := http.Post(srv.URL+"/v1/ruleset/panel", "application/json",
		strings.NewReader(`{"rules":[{"id":"p1","proto":"tcp","src_port":80,"dest_ip":"2.0.0.0","dest_port":80}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp2.StatusCode)
	}
	// tui segment must still be there, panel must NOT have been recorded
	if len(d.owners["tui"]) != 1 || len(d.owners["panel"]) != 0 {
		t.Fatalf("state mutated despite conflict: %+v", d.owners)
	}
}

func TestHandler_PostOwnerSegment_ApplyErrorReturns500AndDoesNotMutate(t *testing.T) {
	fa := &fakeApplier{err: errors.New("nft failed")}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"r1","proto":"tcp","src_port":1,"dest_ip":"1.0.0.0","dest_port":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("d.owners mutated despite apply error: %+v", d.owners)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("state was saved despite apply error: %+v", saved)
	}
}

func TestHandler_PostOwnerSegment_EmptyRulesClearsSegment(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"x","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]}`))
	// clear
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("empty rules POST should drop owner key, got: %+v", d.owners)
	}
}

func TestHandler_PostFlatRulesetReturns410Gone(t *testing.T) {
	// The Phase A flat endpoint is removed; clients must migrate.
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json",
		strings.NewReader(`{"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d", resp.StatusCode)
	}
}

func TestHandler_BadJSONOnOwnerEndpoint(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_MissingOwnerInPathRejected(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	// trailing slash without owner
	resp, err := http.Post(srv.URL+"/v1/ruleset/", "application/json",
		strings.NewReader(`{"rules":[]}`))
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
```

- [ ] **Step 4: 改 `internal/daemon/daemon_test.go` — 调字段引用 + Bootstrap 测试改 owner-segmented**

把现有的 `d.rules` 引用全部改为 `d.owners`（field rename），并把 Bootstrap 测试用的 SaveState 调用改为 owner-segmented。完整替换文件内容：

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

func TestBootstrap_LoadsOwnerSegmentsAndAppliesMerged(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 8080}},
		"panel": []nft.Rule{
			{ID: "p1", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApplier{}
	d := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    fa,
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(fa.last) != 2 {
		t.Fatalf("Bootstrap should apply merged ruleset (2 rules), got: %+v", fa.last)
	}
	if len(d.owners["tui"]) != 1 || len(d.owners["panel"]) != 1 {
		t.Fatalf("in-memory owners not populated: %+v", d.owners)
	}
}

func TestBootstrap_EmptyStateIsFine(t *testing.T) {
	d := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "missing.json"),
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    &fakeApplier{},
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap on empty state: %v", err)
	}
}

func TestRun_AcceptsSocketTrafficAndShutsDown(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
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

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	body := `{"rules":[{"id":"rZ","proto":"tcp","src_port":9090,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := client.Post("http://unix/v1/ruleset/tui", "application/json", strings.NewReader(body))
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
	if len(saved["tui"]) != 1 || saved["tui"][0].ID != "rZ" {
		t.Fatalf("state.json not persisted as expected: %+v", saved)
	}
}
```

- [ ] **Step 5: 改 `internal/daemon/daemon.go` — Bootstrap 用 OwnerRuleset**

定位现有 Bootstrap 函数，把:

```go
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
```

替换为：

```go
func (d *Daemon) Bootstrap() error {
	owners, err := LoadState(d.statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	merged, err := MergedRuleset(owners)
	if err != nil {
		// Persisted state should never contain a conflict — it was written
		// by us after a passing merge. If we see one now the file was
		// hand-edited or corrupted; refuse to start rather than risk
		// flapping the kernel.
		return fmt.Errorf("persisted state has conflict: %w", err)
	}
	if len(merged) > 0 {
		if err := d.applier.Apply(merged); err != nil {
			return fmt.Errorf("apply persisted state: %w", err)
		}
	}
	d.mu.Lock()
	d.owners = owners
	d.mu.Unlock()
	return nil
}
```

- [ ] **Step 6: 跑全部 daemon 测试**

Run:
```bash
go test ./internal/daemon/ -v -count=1
```
Expected: 全部 PASS（state 6 + owners 6 + handlers 10 + daemon 5 + applier 1 + socket 3 = ~31 个）

- [ ] **Step 7: 跑 race detector**

Run:
```bash
go test ./internal/daemon/ -race -count=1
```
Expected: 无 DATA RACE

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go \
        internal/daemon/handlers.go internal/daemon/handlers_test.go
git commit -m "daemon: owner-scoped ruleset endpoints and merged apply pipeline

Daemon now holds rules in an OwnerRuleset map keyed by controller
(tui, panel, ...) instead of a flat slice. POST /v1/ruleset/{owner}
replaces just that owner's segment; GET /v1/ruleset returns the full
segmented view. The flat Phase A POST /v1/ruleset returns 410 with a
directive pointing at the new shape, so smoke-test scripts surface a
clear migration message rather than a generic 404. Cross-owner port
conflicts surface as 409 Conflict with the colliding owner named,
and kernel state is never mutated when the candidate ruleset fails
merge or apply."
```

---

## Task 4: legacy state migration

Daemon Bootstrap 时先尝试导入旧 store 文件。三个旧路径：

- `/etc/nft-forward/rules.json`（TUI 用 `internal/store`）→ `owners.tui`
- `/var/lib/nft-forward/agent-state.json`（agent）→ `owners.panel`
- `/var/lib/nft-forward/embedded-agent-state.json`（server 嵌入式 agent）→ `owners.panel`（与 agent-state.json 冲突时 embedded 胜出，因为它是 server-side 权威）

迁移完后**重命名**旧文件为 `.migrated` 后缀（不删，留人工备份），并写入 daemon 自己的 state.json (v2)。

**Files:**
- Create: `internal/daemon/migrate.go`
- Create: `internal/daemon/migrate_test.go`
- Modify: `internal/daemon/daemon.go`（Bootstrap 在 LoadState 之前先调 migrate）

- [ ] **Step 1: 写 `internal/daemon/migrate_test.go`**

```go
package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"nft-forward/internal/nft"
)

// writeLegacyRules dumps rules to path in the old `[]nft.Rule` schema used
// by both store.Save and agent.saveState.
func writeLegacyRules(t *testing.T, path string, rules []nft.Rule) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate_NoLegacyFilesIsNoOp(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "etc", "nft-forward", "rules.json"),
		Agent:         filepath.Join(root, "var", "lib", "nft-forward", "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "var", "lib", "nft-forward", "embedded-agent-state.json"),
	}
	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners) != 0 {
		t.Fatalf("expected empty migration result, got %+v", owners)
	}
}

func TestMigrate_TuiFileOnly(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{
		{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["tui"]) != 1 || owners["tui"][0].ID != "t1" {
		t.Fatalf("tui segment after migration: %+v", owners)
	}
	if _, exists := owners["panel"]; exists {
		t.Fatalf("panel should be absent when no agent file: %+v", owners)
	}
	// migrated → renamed
	if _, err := os.Stat(cfg.TUI); !os.IsNotExist(err) {
		t.Errorf("legacy TUI file still present after migration: %v", err)
	}
	if _, err := os.Stat(cfg.TUI + ".migrated"); err != nil {
		t.Errorf("expected %s.migrated to exist: %v", cfg.TUI, err)
	}
}

func TestMigrate_AgentFileOnly(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "a1", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "a1" {
		t.Fatalf("panel segment after migration: %+v", owners)
	}
	if _, err := os.Stat(cfg.Agent + ".migrated"); err != nil {
		t.Errorf("agent file should be renamed: %v", err)
	}
}

func TestMigrate_EmbeddedAgentWinsOverAgent(t *testing.T) {
	// If both agent-state.json (remote-pushed) and embedded-agent-state.json
	// (server's localhost view) exist, server is authoritative for the
	// panel segment because the embedded path is what server has been
	// actively maintaining.
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "from-agent", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})
	writeLegacyRules(t, cfg.EmbeddedAgent, []nft.Rule{
		{ID: "from-embedded", Proto: "tcp", SrcPort: 100, DestIP: "3.0.0.0", DestPort: 100},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "from-embedded" {
		t.Fatalf("panel should equal embedded rules: %+v", owners["panel"])
	}
}

func TestMigrate_AllThreeFiles(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{
		{ID: "tui-1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
	})
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "agent-1", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})
	writeLegacyRules(t, cfg.EmbeddedAgent, []nft.Rule{
		{ID: "embedded-1", Proto: "tcp", SrcPort: 100, DestIP: "3.0.0.0", DestPort: 100},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["tui"]) != 1 || owners["tui"][0].ID != "tui-1" {
		t.Errorf("tui segment: %+v", owners["tui"])
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "embedded-1" {
		t.Errorf("panel segment should equal embedded: %+v", owners["panel"])
	}
	// All three files renamed
	for _, p := range []string{cfg.TUI, cfg.Agent, cfg.EmbeddedAgent} {
		if _, err := os.Stat(p + ".migrated"); err != nil {
			t.Errorf("expected %s.migrated to exist: %v", p, err)
		}
	}
}

func TestMigrate_EmptyLegacyFilesProduceNoSegment(t *testing.T) {
	// A zero-byte / "[]" file should not create an empty owner segment;
	// that would pollute GET /v1/ruleset output with always-empty keys.
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, exists := owners["tui"]; exists {
		t.Fatalf("empty legacy file should not create owner key: %+v", owners)
	}
	// file should still be renamed (it was processed)
	if _, err := os.Stat(cfg.TUI + ".migrated"); err != nil {
		t.Errorf("expected migration marker on empty file too: %v", err)
	}
}
```

- [ ] **Step 2: 写 `internal/daemon/migrate.go`**

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"nft-forward/internal/nft"
)

// Default* paths for the three legacy state files that may exist on hosts
// that previously ran nft-forward (TUI), nft-agent, or nft-server (with its
// embedded agent). The daemon imports these into owner segments on first
// boot so users do not lose their existing rules.
const (
	DefaultLegacyTUIPath           = "/etc/nft-forward/rules.json"
	DefaultLegacyAgentPath         = "/var/lib/nft-forward/agent-state.json"
	DefaultLegacyEmbeddedAgentPath = "/var/lib/nft-forward/embedded-agent-state.json"
)

// LegacyMigrationPaths bundles the three legacy state file locations the
// daemon checks at boot. Each is a string so callers (notably tests) can
// override individual fields.
type LegacyMigrationPaths struct {
	TUI           string
	Agent         string
	EmbeddedAgent string
}

// DefaultLegacyPaths returns the production legacy path set.
func DefaultLegacyPaths() LegacyMigrationPaths {
	return LegacyMigrationPaths{
		TUI:           DefaultLegacyTUIPath,
		Agent:         DefaultLegacyAgentPath,
		EmbeddedAgent: DefaultLegacyEmbeddedAgentPath,
	}
}

// migrateLegacyState reads any of the three legacy state files that exist
// at the given paths and returns them as a partially-populated OwnerRuleset.
// Each processed file is renamed to "<path>.migrated" so the daemon can be
// re-run idempotently and an operator has a clear breadcrumb of what was
// imported. Rules from agent-state.json and embedded-agent-state.json both
// land in the "panel" segment; if both are non-empty, embedded wins because
// it represents the controller's authoritative view.
//
// Empty files (no rules) are still renamed (they have been consumed), but
// do not produce an owner key — we don't want GET /v1/ruleset to expose
// always-empty owners after migration.
func migrateLegacyState(p LegacyMigrationPaths) (OwnerRuleset, error) {
	out := OwnerRuleset{}

	tuiRules, err := readLegacyRules(p.TUI)
	if err != nil {
		return nil, fmt.Errorf("read legacy tui state %s: %w", p.TUI, err)
	}
	if tuiRules != nil { // file existed
		if len(tuiRules) > 0 {
			out["tui"] = tuiRules
		}
		if err := os.Rename(p.TUI, p.TUI+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy tui state: %w", err)
		}
	}

	agentRules, err := readLegacyRules(p.Agent)
	if err != nil {
		return nil, fmt.Errorf("read legacy agent state %s: %w", p.Agent, err)
	}
	if agentRules != nil {
		if len(agentRules) > 0 {
			out["panel"] = agentRules
		}
		if err := os.Rename(p.Agent, p.Agent+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy agent state: %w", err)
		}
	}

	embeddedRules, err := readLegacyRules(p.EmbeddedAgent)
	if err != nil {
		return nil, fmt.Errorf("read legacy embedded agent state %s: %w", p.EmbeddedAgent, err)
	}
	if embeddedRules != nil {
		// embedded-agent-state.json wins over agent-state.json — server is
		// authoritative for the panel segment when both exist.
		if len(embeddedRules) > 0 {
			out["panel"] = embeddedRules
		}
		if err := os.Rename(p.EmbeddedAgent, p.EmbeddedAgent+".migrated"); err != nil {
			return nil, fmt.Errorf("rename legacy embedded state: %w", err)
		}
	}

	return out, nil
}

// readLegacyRules reads a legacy `[]nft.Rule` JSON array file. Returns
// (nil, nil) when the file does not exist so the caller can distinguish
// "no migration needed" from "migration produced empty result".
func readLegacyRules(path string) ([]nft.Rule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return []nft.Rule{}, nil
	}
	var rules []nft.Rule
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return rules, nil
}
```

- [ ] **Step 3: 把 migration 接进 `Bootstrap`**

修改 `internal/daemon/daemon.go` 的 `Bootstrap` 函数。**先**做 legacy migration（只在 state.json 不存在时进行 — 避免每次重启重复迁移），**然后**调 LoadState。同时把 `Config` 加 `LegacyPaths` 字段以便测试注入。

修改 `Config` struct（在 `daemon.go` 里）—— 把现有：

```go
type Config struct {
	SocketPath string
	StatePath  string
	GroupName  string
	Applier    Applier
}
```

替换为：

```go
type Config struct {
	SocketPath string
	StatePath  string
	GroupName  string
	Applier    Applier

	// LegacyPaths configures where to look for the three pre-daemon state
	// files (TUI rules.json, agent-state.json, embedded-agent-state.json).
	// Production defaults populated by New; tests inject a temp dir.
	LegacyPaths LegacyMigrationPaths
}
```

修改 `New` 函数 — 在现有默认值填充处加：

```go
	if cfg.LegacyPaths == (LegacyMigrationPaths{}) {
		cfg.LegacyPaths = DefaultLegacyPaths()
	}
```

> Where to put it: right after the `if cfg.Applier == nil { cfg.Applier = DefaultApplier() }` line, before the `return &Daemon{...}`.

同时把 `Daemon` struct（在 `handlers.go`）加一个字段 `legacyPaths LegacyMigrationPaths`：

```go
type Daemon struct {
	socketPath string
	statePath  string
	groupName  string
	applier    Applier
	legacyPaths LegacyMigrationPaths

	mu     sync.Mutex
	owners OwnerRuleset
}
```

并在 `New` 返回 Daemon 时填充：

```go
	return &Daemon{
		socketPath:  cfg.SocketPath,
		statePath:   cfg.StatePath,
		groupName:   cfg.GroupName,
		applier:     cfg.Applier,
		legacyPaths: cfg.LegacyPaths,
	}
```

替换 `Bootstrap` 函数为：

```go
func (d *Daemon) Bootstrap() error {
	// If daemon's own state.json does not exist, this is potentially a
	// first-boot upgrade from the pre-daemon binaries — try importing
	// their legacy state files. We only attempt migration on first boot
	// so a later legacy file showing up after the daemon has been running
	// (e.g. a stale leftover) does not silently overwrite live state.
	if _, err := os.Stat(d.statePath); os.IsNotExist(err) {
		migrated, mErr := migrateLegacyState(d.legacyPaths)
		if mErr != nil {
			return fmt.Errorf("migrate legacy state: %w", mErr)
		}
		if len(migrated) > 0 {
			if err := SaveState(d.statePath, migrated); err != nil {
				return fmt.Errorf("save migrated state: %w", err)
			}
		}
	}

	owners, err := LoadState(d.statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	merged, err := MergedRuleset(owners)
	if err != nil {
		return fmt.Errorf("persisted state has conflict: %w", err)
	}
	if len(merged) > 0 {
		if err := d.applier.Apply(merged); err != nil {
			return fmt.Errorf("apply persisted state: %w", err)
		}
	}
	d.mu.Lock()
	d.owners = owners
	d.mu.Unlock()
	return nil
}
```

注意 `daemon.go` 顶部 import 块要加 `"os"`（应已有）。

- [ ] **Step 4: 加 Bootstrap migration 集成测试到 `daemon_test.go`**

在文件末尾追加：

```go
func TestBootstrap_MigratesLegacyTuiFile(t *testing.T) {
	root := t.TempDir()
	tuiPath := filepath.Join(root, "etc", "nft-forward", "rules.json")
	if err := os.MkdirAll(filepath.Dir(tuiPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// drop a v1-style legacy TUI rules.json (flat array)
	legacy := []byte(`[{"id":"legacy1","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]`)
	if err := os.WriteFile(tuiPath, legacy, 0o640); err != nil {
		t.Fatal(err)
	}

	fa := &fakeApplier{}
	statePath := filepath.Join(root, "state.json")
	d := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    fa,
		LegacyPaths: LegacyMigrationPaths{
			TUI:           tuiPath,
			Agent:         filepath.Join(root, "no-such-agent.json"),
			EmbeddedAgent: filepath.Join(root, "no-such-embedded.json"),
		},
	})

	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(d.owners["tui"]) != 1 || d.owners["tui"][0].ID != "legacy1" {
		t.Fatalf("legacy TUI rule not imported into tui segment: %+v", d.owners)
	}
	if len(fa.last) != 1 {
		t.Fatalf("Apply should see merged ruleset (1 rule): %+v", fa.last)
	}
	// state.json now exists with v2 schema
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json not created post-migration: %v", err)
	}
	// legacy file renamed
	if _, err := os.Stat(tuiPath + ".migrated"); err != nil {
		t.Fatalf("legacy file should be renamed: %v", err)
	}
}

func TestBootstrap_NoMigrationWhenStateAlreadyExists(t *testing.T) {
	// A second boot must NOT re-process legacy files. We simulate this
	// by writing both a daemon state.json (v2) AND a legacy TUI file
	// at the legacy path; Bootstrap should load only the v2 file.
	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "from-v2", Proto: "tcp", SrcPort: 90, DestIP: "9.0.0.0", DestPort: 90}},
	}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "rules.json")
	if err := os.WriteFile(legacyPath, []byte(`[{"id":"ghost","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]`), 0o640); err != nil {
		t.Fatal(err)
	}

	fa := &fakeApplier{}
	d := New(Config{
		StatePath:   statePath,
		SocketPath:  filepath.Join(shortSockDir(t), "s.sock"),
		Applier:     fa,
		LegacyPaths: LegacyMigrationPaths{TUI: legacyPath},
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if d.owners["tui"][0].ID != "from-v2" {
		t.Fatalf("expected v2 state, got: %+v", d.owners["tui"])
	}
	// Legacy file untouched — daemon must not migrate on subsequent boots.
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file should still exist (not migrated): %v", err)
	}
	if _, err := os.Stat(legacyPath + ".migrated"); !os.IsNotExist(err) {
		t.Fatalf(".migrated marker should NOT exist when state already there: %v", err)
	}
}
```

- [ ] **Step 5: 跑全部 daemon 测试 + race**

Run:
```bash
go test ./internal/daemon/ -v -count=1
go test ./internal/daemon/ -race -count=1
```
Expected: 全部 PASS（约 31 个 + 6 个新 migration + 2 个 Bootstrap migration = ~39 个），无 race

- [ ] **Step 6: 跑全部包测试无回归**

Run:
```bash
go test ./... -count=1
```
Expected: 所有包 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/migrate.go internal/daemon/migrate_test.go \
        internal/daemon/daemon.go  internal/daemon/daemon_test.go \
        internal/daemon/handlers.go
git commit -m "daemon: migrate legacy state files into owner segments on first boot

Bootstrap now checks for the three pre-daemon state files
(/etc/nft-forward/rules.json from TUI store, agent-state.json from
nft-agent, embedded-agent-state.json from nft-server's embedded
agent) and imports them into 'tui' and 'panel' owner segments when
the daemon's own state.json does not yet exist. Each consumed file
is renamed to '<path>.migrated' rather than deleted so an operator
can audit what was imported. embedded-agent-state.json wins over
agent-state.json for the panel segment because it represents the
server's authoritative live view. Migration runs once on first boot;
subsequent restarts use the daemon's own state.json and ignore any
legacy file that may reappear (stale leftover, manual restore, etc)."
```

---

## Task 5: 更新 `docs/daemon-manual-verification.md`

API 表面变了，curl 命令也得跟上。同时澄清"已知限制"列表 — Phase B 完成后，owner segmentation 已落地，剩下三项限制（远程接入、auth、TUI/server/agent 未接入）继续标。

**Files:**
- Modify: `docs/daemon-manual-verification.md`

- [ ] **Step 1: 用下面新内容完全替换 `docs/daemon-manual-verification.md`**

```markdown
# Daemon 手动验证

直接用 `curl --unix-socket` 打 daemon 的 unix socket API 端到端验证。daemon 当前不接入 TUI / server / agent — 后者仍走旧路径。

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

# 2. 取空 segmented ruleset
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"owners":{}}

# 3. 提交一条 tui-owned rule
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rT","proto":"tcp","src_port":19090,"dest_ip":"127.0.0.1","dest_port":22}]}' \
     http://unix/v1/ruleset/tui
# → {"count":1}

# 4. 提交一条 panel-owned rule
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rP","proto":"udp","src_port":19091,"dest_ip":"127.0.0.1","dest_port":53}]}' \
     http://unix/v1/ruleset/panel
# → {"count":1}

# 5. 验证内核确实写入了两条（来自不同 owner 的 merge）
sudo nft list table ip nft_forward | grep -E "1909[01]"
# → 应看到两行：dport 19090 → 127.0.0.1:22 和 dport 19091 → 127.0.0.1:53

# 6. 再 GET 一次 — 应该看到两个 owner segment
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"owners":{"panel":[{...}],"tui":[{...}]}}

# 7. 跨 owner 端口冲突 — 让 panel 抢走 tui 已占的 tcp/19090，应被拒
curl -s -o /dev/null -w '%{http_code}\n' --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"steal","proto":"tcp","src_port":19090,"dest_ip":"2.2.2.2","dest_port":22}]}' \
     http://unix/v1/ruleset/panel
# → 409

# 8. 清空 tui segment（POST 空数组）
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[]}' \
     http://unix/v1/ruleset/tui
# → {"count":0}

# 9. 旧的扁平 POST endpoint 已经下线（returns 410 Gone）
curl -s -o /dev/null -w '%{http_code}\n' --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[]}' \
     http://unix/v1/ruleset
# → 410

# 10. 验证 state 文件 schema = v2 + owner-segmented
cat /tmp/nft-forward-state.json
# → {"version":2,"owners":{"panel":[{...}]}}

# 11. Ctrl-C 终止 daemon → /tmp/nft-forward.sock 应被清理
ls /tmp/nft-forward.sock
# → No such file or directory
```

## Restart 恢复验证

1. 重新启动 daemon（同样的 `--state` 路径）
2. 不发任何 POST，立即 `sudo nft list table ip nft_forward` — 应该看到上一次 GET 看到的所有 owner rule（Bootstrap 从 state.json 重放 merged ruleset）

## 旧 state 文件迁移验证

如果你的机器之前跑过 nft-forward TUI / nft-agent / nft-server，对应的 `rules.json` / `agent-state.json` / `embedded-agent-state.json` 会在 daemon **第一次启动**时被导入到对应 owner segment：

- `/etc/nft-forward/rules.json` → `tui` segment
- `/var/lib/nft-forward/agent-state.json` → `panel` segment
- `/var/lib/nft-forward/embedded-agent-state.json` → `panel` segment（与上一条同时存在时此条优先）

每个被处理过的文件会重命名为 `<原路径>.migrated`（不删，留人工备份）。

后续 daemon 重启不会重复迁移：只要 `--state` 指向的文件已存在，迁移就被跳过。

## 已知限制

- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI / server / agent 仍走旧路径**，与 daemon 并存 — 同机同时跑 daemon 和旧 TUI/agent **会冲突**（都想独占本机 nftables 表），验证时只跑 daemon
```

- [ ] **Step 2: 验证 markdown code fence 平衡**

Run:
```bash
awk 'BEGIN{n=0} /^```/{n++} END{print "code fences:", n, (n%2==0 ? "OK" : "UNBALANCED")}' docs/daemon-manual-verification.md
```
Expected: `code fences: <even number> OK`

- [ ] **Step 3: Commit**

```bash
git add docs/daemon-manual-verification.md
git commit -m "docs: refresh daemon verification recipe for owner-segmented API

The verification walk now exercises POST /v1/ruleset/{owner} for both
'tui' and 'panel' segments, confirms the merged ruleset reaches
nftables, asserts that a cross-owner port grab returns 409 Conflict,
checks that the flat Phase A endpoint now returns 410 Gone, and adds
a section on legacy state file migration so an operator upgrading
from nft-forward TUI / nft-agent / nft-server understands what gets
imported on first boot of the daemon."
```

---

## End-to-end gate (after all tasks)

- [ ] **Gate 1: 全部测试 PASS**

Run:
```bash
go test ./... -count=1
```
Expected: 所有包 PASS

- [ ] **Gate 2: race detector 干净**

Run:
```bash
go test ./internal/daemon/ -race -count=1
```
Expected: 无 DATA RACE

- [ ] **Gate 3: 手动 smoke 走通（按 `docs/daemon-manual-verification.md` 11 步）**

在 root + nftables host 上一步步跑 curl，每步预期输出一致。

- [ ] **Gate 4: 现有路径未回归**

```bash
sudo ./build/nft-forward --help              # 三个旧 flag 仍在
sudo ./build/nft-forward --apply 2>&1 | head # store.Load → nft.Apply 仍工作
```

---

## Phase B 完成后的状态

- daemon 内部按 owner 持有规则（map），合并后一次 Apply
- `POST /v1/ruleset/{owner}` 接受任意 owner 名，端口冲突拒绝
- state.json 升 v2；v1 兼容读
- 第一次启动自动导入旧 TUI / agent / embedded-agent state，旧文件 rename 为 `.migrated`
- **TUI / server / agent 行为完全不变**（仍走旧 in-process / HTTP 路径）

**下一阶段（Phase C — TUI client）将做的事**：在 `internal/daemonclient/` 新增 HTTP-over-unix-socket / HTTP client；改 `internal/tui` 把所有 `store.Load` / `nft.Apply` / `store.Save` 调用换成 daemonclient 调用（POST `/v1/ruleset/tui`）；TUI 启动时连不上 daemon 直接报错（不 fallback）。Phase C 完成后 TUI 是 daemon 的第一个真实 client，能与 daemon 并存。

---

## Self-Review Checklist

- [x] 每个 task self-contained，独立 review/revert 可行
- [x] Commit message 解释 WHY，无过程信息（无 "Phase X / Step Y / Round Z / Task N / based on" 等用语）
- [x] Spec Phase B 范围全覆盖：state v2(T1) / merge & conflict(T2) / handlers per-owner(T3) / legacy migration(T4) / docs(T5)
- [x] 无 TBD / TODO / "implement later"
- [x] 每步都给出具体代码或命令
- [x] Type / 函数名一致：`OwnerRuleset`、`MergedRuleset`、`LegacyMigrationPaths`、`DefaultLegacyPaths`、`migrateLegacyState`、`segmentPayload`、`fullPayload`、`cloneOwners`、`d.owners` 全文统一
- [x] 与 Phase A 兼容：旧 v1 state 自动升级；旧 endpoint 留下 410 + 迁移提示
- [x] TUI/server/agent 旧路径**字节级不变**（Phase B 不接入 client，仅 daemon 自身演进）
