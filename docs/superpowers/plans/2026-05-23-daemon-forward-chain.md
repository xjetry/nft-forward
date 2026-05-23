# Daemon FORWARD chain shim Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `internal/nft/shim/` 新增 multi-backend "forward chain shim" 包，让 daemon 自动同步 `DOCKER-USER` / `ufw-user-forward` 里的放行规则，跨越 Docker/ufw 把 FORWARD policy 改成 drop 的环境而无需手动 iptables 操作。

**Architecture:** `ForwardShim` interface + 内置实现（DockerUserShim, UfwShim）+ `Registry` 调度。所有 nft 命令调用通过函数注入（test seam）使 shim 100% 可单元测试。`applier.nftApplier` 在 `nft.Apply` 之后调 `shim.SyncAll`、在 daemon `Run` 退出时调 `Cleanup`。shim 失败 best-effort（log warn，不阻塞核心 DNAT）。

**Tech Stack:** Go 1.22 + `os/exec` 调 nft 命令；不引入新依赖。

**Spec:** `docs/superpowers/specs/2026-05-23-daemon-forward-chain-design.md`

---

## File Structure

| 文件 | 职责 | 改动 |
|---|---|---|
| `internal/nft/shim/shim.go` | `ForwardShim` interface + `Registry` 调度 + `nftRunner`/`nftScriptRunner` 函数类型 | Create |
| `internal/nft/shim/render.go` | 纯函数：`renderShimScript`, `parseShimHandles` | Create |
| `internal/nft/shim/render_test.go` | 纯函数测试 | Create |
| `internal/nft/shim/docker_user.go` | `DockerUserShim` 实现 | Create |
| `internal/nft/shim/docker_user_test.go` | DockerUserShim 测试（mock runner） | Create |
| `internal/nft/shim/ufw.go` | `UfwShim` 实现 | Create |
| `internal/nft/shim/ufw_test.go` | UfwShim 测试 | Create |
| `internal/nft/shim/registry_test.go` | Registry 调度逻辑测试 | Create |
| `internal/daemon/applier.go` | 扩展 `Applier` interface 加 `Cleanup() error`；`nftApplier.Apply` 调 `shim.SyncAll`、新增 `Cleanup` 调 `shim.CleanupAll` | Modify |
| `internal/daemon/applier_test.go` | fakeApplier 加 Cleanup 满足新 interface | Modify |
| `internal/daemon/daemon.go` | `Run` 退出前调 `d.applier.Cleanup()`；启动时探测 FORWARD policy=drop 但无 shim detect 时 log warn | Modify |
| `internal/daemon/daemon_test.go` | 验 Run 退出时 cleanup 被调用 | Modify |
| `docker/test.sh` | 新增 shim 集成测试 step | Modify |
| `README.md` | 升级/角色切换章节下加一段说明 daemon 自动处理 docker/ufw 兼容 | Modify |
| `docs/daemon-manual-verification.md` | 加 shim 验证 case | Modify |

不引入新的 daemon 包；shim 完全自包含在 `internal/nft/shim/` 下。

---

### Task 1: shim package skeleton + ForwardShim interface

**目的：** 建立 shim 包的最小骨架——interface 定义 + 测试 seam（`nftRunner`/`nftScriptRunner` 函数类型）。后续 task 在此基础上加实现。

**Files:**
- Create: `internal/nft/shim/shim.go`

- [ ] **Step 1: 写 interface 定义文件**

创建 `internal/nft/shim/shim.go`：

```go
// Package shim implements per-firewall compatibility layers that inject
// daemon-managed accept rules into well-known user-extension chains
// (e.g. Docker's DOCKER-USER, ufw's ufw-user-forward). This lets
// nft-forward keep working on hosts where some other tool has set the
// FORWARD chain default policy to drop.
//
// Every shim is best-effort: a failure inside a shim never blocks the
// core nft_forward table apply. Each shim's Detect should be cheap so
// callers can poll it on every Apply.
package shim

import (
	"bytes"
	"os/exec"

	"nft-forward/internal/nft"
)

// OwnerComment is the literal string tagged on every rule that any shim
// inserts into a foreign chain. Cleanup walks the chain and deletes by
// this exact comment.
const OwnerComment = "nft-forward managed"

// ForwardShim is one firewall-tool integration. Implementations live
// alongside this file (docker_user.go, ufw.go, ...).
type ForwardShim interface {
	// Name returns a short identifier used in logs.
	Name() string

	// Detect returns true when this shim's target chain exists right
	// now. Cheap; called on every Sync.
	Detect() bool

	// Sync makes the target chain reflect rules: deletes any leftover
	// owner-tagged rule, inserts current ones. No-op when Detect is
	// false. Idempotent.
	Sync(rules []nft.Rule) error

	// Cleanup deletes every owner-tagged rule from the target chain.
	// No-op when Detect is false. Idempotent.
	Cleanup() error
}

// nftRunner runs `nft <args>` and returns combined stdout. Production
// callers use defaultNftRunner; tests substitute a fake.
type nftRunner func(args ...string) (string, error)

// nftScriptRunner pipes `script` into `nft -f -`. Production callers
// use defaultNftScriptRunner; tests substitute a fake.
type nftScriptRunner func(script string) error

func defaultNftRunner(args ...string) (string, error) {
	cmd := exec.Command("nft", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// Caller decides whether the error means "chain missing" or
		// something fatal; we surface stderr in the wrapped error.
		return stdout.String(), &nftError{args: args, err: err, stderr: stderr.String()}
	}
	return stdout.String(), nil
}

func defaultNftScriptRunner(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &nftError{args: []string{"-f", "-"}, err: err, stderr: stderr.String()}
	}
	return nil
}

type nftError struct {
	args   []string
	err    error
	stderr string
}

func (e *nftError) Error() string {
	if e.stderr == "" {
		return "nft " + joinArgs(e.args) + ": " + e.err.Error()
	}
	return "nft " + joinArgs(e.args) + ": " + e.err.Error() + ": " + e.stderr
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./internal/nft/shim/
```

期望：no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/nft/shim/shim.go
git commit -m "nft: introduce shim package skeleton for forward-chain compatibility"
```

---

### Task 2: render.go pure functions (TDD)

**目的：** 实现两个纯函数——`parseShimHandles`（从 `nft -a list chain` 输出中提取 handle）和 `renderShimScript`（生成 `nft -f -` 脚本）。纯函数 TDD 最容易。

**Files:**
- Create: `internal/nft/shim/render.go`
- Create: `internal/nft/shim/render_test.go`

- [ ] **Step 1: 写 render_test.go（先写测试）**

```go
package shim

import (
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func TestParseShimHandlesEmptyChain(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
	}
}`
	handles := parseShimHandles(out)
	if len(handles) != 0 {
		t.Fatalf("expected 0 handles, got %v", handles)
	}
}

func TestParseShimHandlesIgnoresUnrelatedRules(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
		counter packets 5 bytes 300 jump SOMEWHERE-ELSE # handle 7
		ip daddr 1.2.3.4 counter accept comment "third-party tool" # handle 8
	}
}`
	handles := parseShimHandles(out)
	if len(handles) != 0 {
		t.Fatalf("expected 0 handles (no nft-forward managed), got %v", handles)
	}
}

func TestParseShimHandlesPicksOwnerTagged(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
		counter packets 0 bytes 0 jump SOMEWHERE # handle 7
		ct state established,related counter accept comment "nft-forward managed" # handle 12
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 17
		ip daddr 1.2.3.4 counter accept comment "other" # handle 19
	}
}`
	handles := parseShimHandles(out)
	want := []int{12, 17}
	if !equalInts(handles, want) {
		t.Fatalf("got %v, want %v", handles, want)
	}
}

func TestRenderShimScriptEmptyRulesStillEmitsCtState(t *testing.T) {
	script := renderShimScript("ip", "filter", "DOCKER-USER", nil, nil)
	if !strings.Contains(script, "ct state established,related") {
		t.Fatalf("ct state rule missing:\n%s", script)
	}
	if strings.Contains(script, "delete rule") {
		t.Fatalf("no stale handles, should not emit delete:\n%s", script)
	}
}

func TestRenderShimScriptDeletesStaleHandles(t *testing.T) {
	script := renderShimScript("ip", "filter", "DOCKER-USER", nil, []int{12, 17})
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 12") {
		t.Fatalf("handle 12 delete missing:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 17") {
		t.Fatalf("handle 17 delete missing:\n%s", script)
	}
}

func TestRenderShimScriptInsertsTCPRule(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", DestIP: "10.20.1.20", DestPort: 8443},
	}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, nil)
	want := `add rule ip filter DOCKER-USER ip daddr 10.20.1.20 tcp dport 8443 counter accept comment "nft-forward managed"`
	if !strings.Contains(script, want) {
		t.Fatalf("expected line %q in:\n%s", want, script)
	}
}

func TestRenderShimScriptInsertsTCPUDPRule(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp+udp", DestIP: "10.20.1.20", DestPort: 8443},
	}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, nil)
	want := `add rule ip filter DOCKER-USER ip daddr 10.20.1.20 meta l4proto { tcp, udp } th dport 8443 counter accept comment "nft-forward managed"`
	if !strings.Contains(script, want) {
		t.Fatalf("expected line %q in:\n%s", want, script)
	}
}

func TestRenderShimScriptOrderingDeleteBeforeAdd(t *testing.T) {
	rules := []nft.Rule{{Proto: "tcp", DestIP: "1.2.3.4", DestPort: 80}}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, []int{5})
	delIdx := strings.Index(script, "delete rule")
	addIdx := strings.Index(script, "add rule")
	if delIdx < 0 || addIdx < 0 {
		t.Fatalf("missing delete or add:\n%s", script)
	}
	if delIdx > addIdx {
		t.Fatalf("delete must come before add for atomic swap:\n%s", script)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/nft/shim/ -run 'TestParseShimHandles|TestRenderShimScript' -v
```

期望：编译失败（`parseShimHandles` / `renderShimScript` undefined）。

- [ ] **Step 3: Implement render.go**

```go
package shim

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"nft-forward/internal/nft"
)

// handleRegex captures the trailing `# handle N` annotation emitted by
// `nft -a list chain`. nft prints it on every rule line.
var handleRegex = regexp.MustCompile(`#\s*handle\s+(\d+)\s*$`)

// parseShimHandles walks nft -a list chain output and returns every
// handle whose rule line carries the OwnerComment string. Lines without
// OwnerComment (other tools' rules, ct rules from a different owner)
// are ignored.
func parseShimHandles(listOutput string) []int {
	var out []int
	for _, line := range strings.Split(listOutput, "\n") {
		if !strings.Contains(line, "comment \""+OwnerComment+"\"") {
			continue
		}
		m := handleRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// renderShimScript builds the `nft -f -` script that, in one atomic
// transaction:
//   1. Deletes every rule whose handle is in staleHandles (the
//      previously-injected daemon-managed rules).
//   2. Re-adds the ct state established,related accept tail rule and
//      one accept rule per DNAT (matching ip daddr + proto/dport).
// Empty rules + empty staleHandles still emits the ct state rule so
// reply traffic for any future rule has a route through.
func renderShimScript(family, table, chain string, rules []nft.Rule, staleHandles []int) string {
	var b strings.Builder
	for _, h := range staleHandles {
		fmt.Fprintf(&b, "delete rule %s %s %s handle %d\n", family, table, chain, h)
	}
	fmt.Fprintf(&b,
		"add rule %s %s %s ct state established,related counter accept comment \"%s\"\n",
		family, table, chain, OwnerComment,
	)
	for _, r := range rules {
		if r.DestIP == "" {
			continue
		}
		match := protoForwardMatch(r.Proto, r.DestPort)
		fmt.Fprintf(&b,
			"add rule %s %s %s ip daddr %s %s counter accept comment \"%s\"\n",
			family, table, chain, r.DestIP, match, OwnerComment,
		)
	}
	return b.String()
}

// protoForwardMatch produces the proto + dport match clause for the
// forward chain. Mirrors nft.protoPostMatch so tcp+udp uses set syntax.
func protoForwardMatch(proto string, port int) string {
	switch proto {
	case "tcp+udp":
		return fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", port)
	default:
		return fmt.Sprintf("%s dport %d", proto, port)
	}
}
```

注意 signature 变化：`renderShimScript(family, table, chain, rules, staleHandles)` — 测试调用是 `renderShimScript("ip", "filter", "DOCKER-USER", ...)`。改测试 fixture 适配。

⚠️ **要点**：上面我写的测试是按 4 参数版本（family + table + chain + rules + handles）。`renderShimScript("ip", "filter", "DOCKER-USER", nil, nil)` 是 5 个 arg。**先确保测试调用和实现签名匹配**。

修订测试调用形式：所有 `renderShimScript("ip", "filter", "DOCKER-USER", ...)` 改为 `renderShimScript("ip", "filter", "DOCKER-USER", rules, handles)`——已经是这个形式。OK，测试无需改。

- [ ] **Step 4: Run tests, verify all pass**

```bash
go test ./internal/nft/shim/ -v
```

期望：所有 test PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/nft/shim/render.go internal/nft/shim/render_test.go
git commit -m "nft/shim: pure renderers for forward-chain script and handle parsing"
```

---

### Task 3: DockerUserShim implementation (TDD)

**目的：** 第一个具体 shim 实现。Detect / Sync / Cleanup 三个方法，依赖注入测试。

**Files:**
- Create: `internal/nft/shim/docker_user.go`
- Create: `internal/nft/shim/docker_user_test.go`

- [ ] **Step 1: 写 docker_user_test.go**

```go
package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// recorder is a test runner that captures calls so we can assert on the
// nft commands the shim issued.
type recorder struct {
	listOut    string
	listErr    error
	scripts    []string
	scriptErr  error
	listArgs   [][]string
}

func (r *recorder) run(args ...string) (string, error) {
	r.listArgs = append(r.listArgs, args)
	return r.listOut, r.listErr
}

func (r *recorder) runScript(script string) error {
	r.scripts = append(r.scripts, script)
	return r.scriptErr
}

func newDockerUserShimWith(r *recorder) *DockerUserShim {
	return &DockerUserShim{runNft: r.run, runNftScript: r.runScript}
}

func TestDockerUserShimName(t *testing.T) {
	s := NewDockerUserShim()
	if s.Name() != "docker-user" {
		t.Fatalf("got %q", s.Name())
	}
}

func TestDockerUserShimDetectTrue(t *testing.T) {
	r := &recorder{listOut: `chain DOCKER-USER { ... }`}
	s := newDockerUserShimWith(r)
	if !s.Detect() {
		t.Fatal("expected Detect to return true on successful list")
	}
}

func TestDockerUserShimDetectFalseOnError(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newDockerUserShimWith(r)
	if s.Detect() {
		t.Fatal("expected Detect to return false when chain missing")
	}
}

func TestDockerUserShimSyncSkipsWhenAbsent(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newDockerUserShimWith(r)
	if err := s.Sync(nil); err != nil {
		t.Fatalf("Sync should swallow missing chain: %v", err)
	}
	if len(r.scripts) != 0 {
		t.Fatalf("no script should have been run, got %v", r.scripts)
	}
}

func TestDockerUserShimSyncInjectsRule(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
	}
}`,
	}
	s := newDockerUserShimWith(r)
	rules := []nft.Rule{{Proto: "tcp", DestIP: "10.20.1.20", DestPort: 8443}}
	if err := s.Sync(rules); err != nil {
		t.Fatal(err)
	}
	if len(r.scripts) != 1 {
		t.Fatalf("expected 1 script, got %d", len(r.scripts))
	}
	if !strings.Contains(r.scripts[0], "ip daddr 10.20.1.20 tcp dport 8443 counter accept") {
		t.Fatalf("rule missing from script:\n%s", r.scripts[0])
	}
}

func TestDockerUserShimSyncDeletesStaleThenAdds(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
		ct state established,related counter accept comment "nft-forward managed" # handle 7
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 8
	}
}`,
	}
	s := newDockerUserShimWith(r)
	if err := s.Sync(nil); err != nil {
		t.Fatal(err)
	}
	script := r.scripts[0]
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 7") {
		t.Fatalf("stale handle 7 not deleted:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 8") {
		t.Fatalf("stale handle 8 not deleted:\n%s", script)
	}
}

func TestDockerUserShimCleanupRemovesAll(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain DOCKER-USER {
		ct state established,related counter accept comment "nft-forward managed" # handle 7
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 8
	}
}`,
	}
	s := newDockerUserShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(r.scripts) != 1 {
		t.Fatalf("expected 1 cleanup script, got %d", len(r.scripts))
	}
	script := r.scripts[0]
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 7") {
		t.Fatalf("handle 7 should be deleted:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 8") {
		t.Fatalf("handle 8 should be deleted:\n%s", script)
	}
	if strings.Contains(script, "add rule") {
		t.Fatalf("cleanup must not re-add rules:\n%s", script)
	}
}

func TestDockerUserShimCleanupAbsentNoOp(t *testing.T) {
	r := &recorder{listErr: errors.New("no chain")}
	s := newDockerUserShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatalf("Cleanup should swallow missing chain: %v", err)
	}
	if len(r.scripts) != 0 {
		t.Fatalf("no script should run when chain absent, got %v", r.scripts)
	}
}
```

- [ ] **Step 2: Run, expect FAIL (DockerUserShim undefined)**

```bash
go test ./internal/nft/shim/ -run TestDockerUserShim -v
```

- [ ] **Step 3: Implement docker_user.go**

```go
package shim

import (
	"nft-forward/internal/nft"
)

const (
	dockerUserFamily = "ip"
	dockerUserTable  = "filter"
	dockerUserChain  = "DOCKER-USER"
)

// DockerUserShim integrates with Docker's DOCKER-USER chain. Docker
// places this chain at the head of the FORWARD chain explicitly so
// upstream applications can append accept rules without conflicting
// with Docker's own rule generation.
type DockerUserShim struct {
	runNft       nftRunner
	runNftScript nftScriptRunner
}

func NewDockerUserShim() *DockerUserShim {
	return &DockerUserShim{
		runNft:       defaultNftRunner,
		runNftScript: defaultNftScriptRunner,
	}
}

func (s *DockerUserShim) Name() string { return "docker-user" }

func (s *DockerUserShim) Detect() bool {
	_, err := s.runNft("list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	return err == nil
}

func (s *DockerUserShim) Sync(rules []nft.Rule) error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil // chain absent; nothing to do
	}
	stale := parseShimHandles(out)
	script := renderShimScript(dockerUserFamily, dockerUserTable, dockerUserChain, rules, stale)
	return s.runNftScript(script)
}

func (s *DockerUserShim) Cleanup() error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	if len(stale) == 0 {
		return nil
	}
	// Cleanup emits only deletes, no adds. Reuse the renderer with an
	// empty rules list — but skip the ct state add line by emitting a
	// custom script.
	var b []byte
	for _, h := range stale {
		b = append(b, []byte(deleteRule(dockerUserFamily, dockerUserTable, dockerUserChain, h))...)
	}
	return s.runNftScript(string(b))
}

func deleteRule(family, table, chain string, handle int) string {
	return formatDelete(family, table, chain, handle)
}

func formatDelete(family, table, chain string, handle int) string {
	return "delete rule " + family + " " + table + " " + chain + " handle " + itoa(handle) + "\n"
}

// itoa avoids importing strconv just for this; keep deps tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
```

- [ ] **Step 4: Run tests, verify PASS**

```bash
go test ./internal/nft/shim/ -run TestDockerUserShim -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/nft/shim/docker_user.go internal/nft/shim/docker_user_test.go
git commit -m "nft/shim: DockerUserShim — detect, sync, cleanup against DOCKER-USER"
```

---

### Task 4: UfwShim implementation

**目的：** 第二个 shim，结构跟 DockerUserShim 完全平行——只是 chain 名不同。

**Files:**
- Create: `internal/nft/shim/ufw.go`
- Create: `internal/nft/shim/ufw_test.go`

- [ ] **Step 1: 写 ufw_test.go**

```go
package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func newUfwShimWith(r *recorder) *UfwShim {
	return &UfwShim{runNft: r.run, runNftScript: r.runScript}
}

func TestUfwShimName(t *testing.T) {
	s := NewUfwShim()
	if s.Name() != "ufw" {
		t.Fatalf("got %q", s.Name())
	}
}

func TestUfwShimDetectTrue(t *testing.T) {
	r := &recorder{listOut: `chain ufw-user-forward { ... }`}
	s := newUfwShimWith(r)
	if !s.Detect() {
		t.Fatal("expected Detect to return true on successful list")
	}
}

func TestUfwShimDetectFalseOnError(t *testing.T) {
	r := &recorder{listErr: errors.New("no such chain")}
	s := newUfwShimWith(r)
	if s.Detect() {
		t.Fatal("expected Detect false when chain missing")
	}
}

func TestUfwShimSyncTargetsUfwUserForward(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain ufw-user-forward {
	}
}`,
	}
	s := newUfwShimWith(r)
	rules := []nft.Rule{{Proto: "tcp", DestIP: "192.168.1.5", DestPort: 443}}
	if err := s.Sync(rules); err != nil {
		t.Fatal(err)
	}
	script := r.scripts[0]
	if !strings.Contains(script, "ufw-user-forward") {
		t.Fatalf("script must target ufw-user-forward, got:\n%s", script)
	}
	if strings.Contains(script, "DOCKER-USER") {
		t.Fatalf("script must not mention DOCKER-USER:\n%s", script)
	}
	if !strings.Contains(script, "ip daddr 192.168.1.5 tcp dport 443") {
		t.Fatalf("rule missing:\n%s", script)
	}
}

func TestUfwShimCleanupRemovesAll(t *testing.T) {
	r := &recorder{
		listOut: `table ip filter {
	chain ufw-user-forward {
		ct state established,related counter accept comment "nft-forward managed" # handle 22
	}
}`,
	}
	s := newUfwShimWith(r)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.scripts[0], "delete rule ip filter ufw-user-forward handle 22") {
		t.Fatalf("handle 22 should be deleted:\n%s", r.scripts[0])
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/nft/shim/ -run TestUfwShim -v
```

- [ ] **Step 3: Implement ufw.go**

```go
package shim

import (
	"nft-forward/internal/nft"
)

const (
	ufwFamily = "ip"
	ufwTable  = "filter"
	ufwChain  = "ufw-user-forward"
)

// UfwShim integrates with ufw's ufw-user-forward chain. Same general
// pattern as DOCKER-USER: ufw provides this chain as the documented
// extension point for forward-direction rules added by external tools.
type UfwShim struct {
	runNft       nftRunner
	runNftScript nftScriptRunner
}

func NewUfwShim() *UfwShim {
	return &UfwShim{
		runNft:       defaultNftRunner,
		runNftScript: defaultNftScriptRunner,
	}
}

func (s *UfwShim) Name() string { return "ufw" }

func (s *UfwShim) Detect() bool {
	_, err := s.runNft("list", "chain", ufwFamily, ufwTable, ufwChain)
	return err == nil
}

func (s *UfwShim) Sync(rules []nft.Rule) error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, ufwChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	script := renderShimScript(ufwFamily, ufwTable, ufwChain, rules, stale)
	return s.runNftScript(script)
}

func (s *UfwShim) Cleanup() error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, ufwChain)
	if err != nil {
		return nil
	}
	stale := parseShimHandles(out)
	if len(stale) == 0 {
		return nil
	}
	var b []byte
	for _, h := range stale {
		b = append(b, []byte(formatDelete(ufwFamily, ufwTable, ufwChain, h))...)
	}
	return s.runNftScript(string(b))
}
```

- [ ] **Step 4: Tests pass**

```bash
go test ./internal/nft/shim/ -run TestUfwShim -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/nft/shim/ufw.go internal/nft/shim/ufw_test.go
git commit -m "nft/shim: UfwShim — same shape as DockerUserShim, ufw-user-forward chain"
```

---

### Task 5: Registry — SyncAll / CleanupAll

**目的：** 把单个 shim 组合成一个 Registry，daemon 只跟 Registry 打交道。

**Files:**
- Modify: `internal/nft/shim/shim.go` (append Registry type + funcs)
- Create: `internal/nft/shim/registry_test.go`

- [ ] **Step 1: 写 registry_test.go**

```go
package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// stubShim is a hand-rolled fake satisfying ForwardShim. Captures every
// call so tests can assert ordering and arguments.
type stubShim struct {
	name      string
	detect    bool
	syncErr   error
	cleanErr  error
	syncCalls int
	cleanCalls int
	lastRules []nft.Rule
}

func (s *stubShim) Name() string { return s.name }
func (s *stubShim) Detect() bool { return s.detect }
func (s *stubShim) Sync(rules []nft.Rule) error {
	s.syncCalls++
	s.lastRules = rules
	return s.syncErr
}
func (s *stubShim) Cleanup() error {
	s.cleanCalls++
	return s.cleanErr
}

func TestRegistrySyncAllSkipsUndetected(t *testing.T) {
	a := &stubShim{name: "a", detect: false}
	b := &stubShim{name: "b", detect: true}
	r := &Registry{shims: []ForwardShim{a, b}}
	if err := r.SyncAll([]nft.Rule{{Proto: "tcp", DestIP: "1.1.1.1", DestPort: 80}}); err != nil {
		t.Fatal(err)
	}
	if a.syncCalls != 0 {
		t.Fatalf("a should not be synced (Detect false), syncCalls=%d", a.syncCalls)
	}
	if b.syncCalls != 1 {
		t.Fatalf("b should have synced once, got %d", b.syncCalls)
	}
}

func TestRegistrySyncAllAggregatesErrors(t *testing.T) {
	a := &stubShim{name: "a", detect: true, syncErr: errors.New("boom-a")}
	b := &stubShim{name: "b", detect: true, syncErr: errors.New("boom-b")}
	c := &stubShim{name: "c", detect: true}
	r := &Registry{shims: []ForwardShim{a, b, c}}
	err := r.SyncAll(nil)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if !strings.Contains(err.Error(), "boom-a") || !strings.Contains(err.Error(), "boom-b") {
		t.Fatalf("aggregate error must mention both, got %v", err)
	}
	if c.syncCalls != 1 {
		t.Fatal("third shim must still be called after earlier failures")
	}
}

func TestRegistryCleanupAllSkipsUndetected(t *testing.T) {
	a := &stubShim{name: "a", detect: false}
	b := &stubShim{name: "b", detect: true}
	r := &Registry{shims: []ForwardShim{a, b}}
	if err := r.CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if a.cleanCalls != 0 {
		t.Fatalf("a should not be cleaned (Detect false), got %d", a.cleanCalls)
	}
	if b.cleanCalls != 1 {
		t.Fatalf("b should clean once, got %d", b.cleanCalls)
	}
}

func TestDefaultRegistryListsKnownShims(t *testing.T) {
	r := DefaultRegistry()
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 default shims, got %d: %v", len(names), names)
	}
	want := map[string]bool{"docker-user": true, "ufw": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected shim %q in DefaultRegistry()", n)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/nft/shim/ -run TestRegistry -v
```

- [ ] **Step 3: Append Registry to shim.go**

在 `internal/nft/shim/shim.go` 末尾追加：

```go
// Registry holds the built-in shims and dispatches Sync/Cleanup across
// all of them. The set of shims is fixed at construction time; we don't
// support dynamic registration because the list is small and known.
type Registry struct {
	shims []ForwardShim
}

// DefaultRegistry returns the built-in shim set: docker-user, ufw.
// Tests can construct a Registry literal with arbitrary stubs.
func DefaultRegistry() *Registry {
	return &Registry{
		shims: []ForwardShim{
			NewDockerUserShim(),
			NewUfwShim(),
		},
	}
}

// Names lists the shim Name()s in registration order, for logging.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.shims))
	for _, s := range r.shims {
		names = append(names, s.Name())
	}
	return names
}

// SyncAll runs Sync on every detected shim. A failure in one shim does
// not skip the others — failures are aggregated and returned at the
// end so the caller can log them. Detect failures are not surfaced.
func (r *Registry) SyncAll(rules []nft.Rule) error {
	var errs []string
	for _, s := range r.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Sync(rules); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &aggregateError{errs}
}

// CleanupAll mirrors SyncAll for shutdown / uninstall paths.
func (r *Registry) CleanupAll() error {
	var errs []string
	for _, s := range r.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Cleanup(); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &aggregateError{errs}
}

type aggregateError struct {
	errs []string
}

func (e *aggregateError) Error() string {
	return "shim: " + joinStr(e.errs, "; ")
}

func joinStr(xs []string, sep string) string {
	out := ""
	for i, s := range xs {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
```

- [ ] **Step 4: Tests pass**

```bash
go test ./internal/nft/shim/ -v
```

期望所有 test PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/nft/shim/shim.go internal/nft/shim/registry_test.go
git commit -m "nft/shim: Registry dispatches SyncAll/CleanupAll across all shims"
```

---

### Task 6: Applier integration — Apply calls SyncAll, new Cleanup method

**目的：** 把 shim 接到 daemon 的 nftApplier 上。扩展 `Applier` interface 加 `Cleanup() error`。

**Files:**
- Modify: `internal/daemon/applier.go`
- Modify: `internal/daemon/applier_test.go`
- Modify: `internal/daemon/daemon_test.go`（fakeApplier 定义可能在这）

- [ ] **Step 1: 找到 fakeApplier 定义位置**

```bash
grep -rn 'fakeApplier' internal/daemon/
```

定位到 `internal/daemon/daemon_test.go`（确认）。

- [ ] **Step 2: 修改 applier.go 加 Cleanup 接口 + shim 集成**

替换 `internal/daemon/applier.go` 全文：

```go
package daemon

import (
	"log"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
	"nft-forward/internal/tc"
)

// Applier writes a fully-resolved ruleset into the kernel data plane.
// Production daemons drive nft (DNAT/MASQUERADE), tc (rate limit), and
// firewall-tool compatibility shims. Tests substitute fakes that record
// calls without requiring root.
type Applier interface {
	Apply(rules []nft.Rule, iface string) error
	// Cleanup is called when the daemon shuts down so any owner-tagged
	// rules injected into foreign chains (DOCKER-USER, ufw-user-forward,
	// ...) get removed. Safe to call multiple times.
	Cleanup() error
}

type nftApplier struct {
	shims *shim.Registry
}

func (a nftApplier) Apply(rules []nft.Rule, iface string) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	if a.shims != nil {
		if err := a.shims.SyncAll(rules); err != nil {
			// shim failure is non-fatal: core nft_forward table already
			// applied. Surface as a log line for ops visibility.
			log.Printf("shim sync: %v", err)
		}
	}
	// tc runs after nft so a stale class hierarchy never points at a
	// dest IP nft hasn't published yet. If tc fails the kernel keeps the
	// freshly-applied nft ruleset (traffic still forwards, only shaping
	// is missing); that's preferable to rolling nft back and dropping
	// packets.
	return tc.Apply(rules, iface)
}

func (a nftApplier) Cleanup() error {
	if a.shims == nil {
		return nil
	}
	return a.shims.CleanupAll()
}

// DefaultApplier returns the production applier wired with the built-in
// shim registry.
func DefaultApplier() Applier {
	return nftApplier{shims: shim.DefaultRegistry()}
}
```

- [ ] **Step 3: 修改 applier_test.go**

替换 `internal/daemon/applier_test.go`：

```go
package daemon

import (
	"testing"
)

func TestApplier_FakeAndDefaultSatisfyInterface(t *testing.T) {
	var _ Applier = (*fakeApplier)(nil)
	var _ Applier = nftApplier{}
}
```

(签名没变；只是验证 `nftApplier{}` 仍满足扩展后的 interface。)

- [ ] **Step 4: 修改 daemon_test.go 的 fakeApplier**

定位 `fakeApplier` 定义。它需要新增 `Cleanup() error` 方法。

```bash
grep -n 'fakeApplier' internal/daemon/daemon_test.go
```

找到 fakeApplier struct + Apply method 之后追加：

```go
func (f *fakeApplier) Cleanup() error {
	f.cleanupCalls++
	return nil
}
```

并在 fakeApplier struct 加 `cleanupCalls int` 字段。

⚠️ 如果 fakeApplier 已经在不同测试文件里被引用且字段无 `cleanupCalls`，只加 method 不加字段也能工作（method 没 receive 状态就行）。**先看现有定义再决定**。

- [ ] **Step 5: Run all daemon tests**

```bash
go test ./internal/daemon/ -v
```

期望：全 PASS。Bootstrap / Apply / Counter 等已有测试不受影响。

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/applier.go internal/daemon/applier_test.go internal/daemon/daemon_test.go
git commit -m "daemon: wire shim Registry into applier's Apply + new Cleanup hook"
```

---

### Task 7: Daemon Run calls Cleanup on shutdown

**目的：** Daemon `Run` 在 ctx Done / srv shutdown 之后调一次 `d.applier.Cleanup()`。

**Files:**
- Modify: `internal/daemon/daemon.go`（`Run` 方法）
- Modify: `internal/daemon/daemon_test.go`（验 cleanup 被调用）

- [ ] **Step 1: 写测试 — Cleanup is called on shutdown**

在 `internal/daemon/daemon_test.go` 找合适位置追加：

```go
func TestDaemonRunCallsCleanupOnShutdown(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"
	// pre-seed empty state so Bootstrap doesn't fail
	if err := os.WriteFile(statePath, []byte(`{"owners":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApplier{}
	d, err := New(Config{
		SocketPath: dir + "/sock",
		StatePath:  statePath,
		GroupName:  "",
		Applier:    fa,
		LegacyPaths: LegacyMigrationPaths{
			RulesJSON:        dir + "/rules.json",
			AgentState:       dir + "/agent.json",
			EmbeddedAgentState: dir + "/emb.json",
		},
		Iface: "lo",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fa.cleanupCalls != 1 {
		t.Fatalf("expected Cleanup called once on shutdown, got %d", fa.cleanupCalls)
	}
}
```

⚠️ 上面用 `context`/`os`/`time` import 可能要补。`fakeApplier.cleanupCalls` 字段已在 Task 6 step 4 加好。

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/daemon/ -run TestDaemonRunCallsCleanup -v
```

期望：FAIL（Run 没调 Cleanup）。

- [ ] **Step 3: 修改 Run 在 ctx.Done 分支调 Cleanup**

定位 `internal/daemon/daemon.go::Run` 的 select 块：

```go
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(shutCtx)
		}
		return srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
```

改成：

```go
	var shutdownErr error
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(shutCtx)
		}
		shutdownErr = srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			shutdownErr = nil
		} else {
			shutdownErr = err
		}
	}
	if cleanupErr := d.applier.Cleanup(); cleanupErr != nil {
		log.Printf("applier cleanup: %v", cleanupErr)
	}
	return shutdownErr
```

- [ ] **Step 4: Tests pass**

```bash
go test ./internal/daemon/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go
git commit -m "daemon: call applier.Cleanup on shutdown so shim residue gets removed"
```

---

### Task 8: Unknown firewall warning (startup detect)

**目的：** Daemon 启动时如果检测到 FORWARD chain default policy=drop 但所有 shim 都 `Detect()==false`，记一行明确的 WARN，引导用户手动 remediation。

**Files:**
- Modify: `internal/daemon/daemon.go`（在 Bootstrap 后或 Run 开始时探测）
- Modify: `internal/daemon/daemon_test.go`（cover 探测函数纯逻辑）

- [ ] **Step 1: 写测试（纯逻辑函数 detectForwardDropNoShim）**

先把"探测 FORWARD policy + 检查 shim"的逻辑抽成纯函数，便于测试。

在 `internal/daemon/daemon_test.go` 追加：

```go
func TestDetectForwardDropNoShim_FalseWhenPolicyAccept(t *testing.T) {
	if detectForwardDropNoShim("Chain FORWARD (policy ACCEPT 0 packets)", []string{"docker-user"}) {
		t.Fatal("policy ACCEPT must not trigger warning")
	}
}

func TestDetectForwardDropNoShim_FalseWhenShimDetected(t *testing.T) {
	if detectForwardDropNoShim("Chain FORWARD (policy DROP 100 packets)", []string{"docker-user"}) {
		t.Fatal("known shim detected: no warning")
	}
}

func TestDetectForwardDropNoShim_TrueWhenDropAndNoShim(t *testing.T) {
	if !detectForwardDropNoShim("Chain FORWARD (policy DROP 100 packets)", nil) {
		t.Fatal("policy DROP + no shim must trigger warning")
	}
}

func TestDetectForwardDropNoShim_EmptyInput(t *testing.T) {
	if detectForwardDropNoShim("", nil) {
		t.Fatal("empty input (probe failed) must not trigger warning")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/daemon/ -run TestDetectForward -v
```

- [ ] **Step 3: 在 daemon.go 实现 detectForwardDropNoShim**

在 `internal/daemon/daemon.go` 末尾追加：

```go
// detectForwardDropNoShim returns true when the iptables FORWARD chain
// has a drop default policy AND no known shim was detected. Pure
// function so tests can drive it with fixture input.
func detectForwardDropNoShim(iptablesForwardListOutput string, detectedShims []string) bool {
	if iptablesForwardListOutput == "" {
		return false
	}
	if !strings.Contains(iptablesForwardListOutput, "policy DROP") {
		return false
	}
	return len(detectedShims) == 0
}
```

确认 `strings` 已 imported (daemon.go 已经 import 了)。

- [ ] **Step 4: 在 Run 启动时实际探测 + log**

在 `Run` 开头（Bootstrap 之后、Listen 之前）加一段：

```go
	// Best-effort firewall environment probe. We only log a warning when
	// the kernel will silently drop forwarded packets and we have no
	// shim coverage; we never modify policy ourselves.
	go d.probeFirewallEnvironment()
```

并新增方法（在 Run 之后）：

```go
func (d *Daemon) probeFirewallEnvironment() {
	out, err := exec.Command("iptables", "-nL", "FORWARD").CombinedOutput()
	if err != nil {
		return // probe failed; no signal
	}
	var detected []string
	if d.applier != nil {
		if reg, ok := d.applier.(interface{ DetectedShims() []string }); ok {
			detected = reg.DetectedShims()
		}
	}
	if detectForwardDropNoShim(string(out), detected) {
		log.Printf("WARN: FORWARD chain has drop policy but no known firewall shim detected; " +
			"forwarded traffic may be blocked. supported shims: docker-user, ufw.")
	}
}
```

`os/exec` 已经 imported。需要在 import 块加 `"os/exec"` 如果还没有（可能没有；检查后追加）。

并让 `nftApplier` 实现 `DetectedShims() []string`：

在 `internal/daemon/applier.go` 加方法：

```go
func (a nftApplier) DetectedShims() []string {
	if a.shims == nil {
		return nil
	}
	var out []string
	for _, name := range a.shims.Names() {
		// We need to ask each shim's Detect() — Names() returns all
		// registered, Detect() filters to live ones. Use a small helper
		// on Registry instead of duplicating logic here.
	}
	_ = out
	return a.shims.DetectedNames()
}
```

需要 Registry 加 `DetectedNames()` 方法。在 `internal/nft/shim/shim.go` 加：

```go
// DetectedNames returns the names of shims whose Detect() returns true
// right now. Cheap; used by daemon startup probe.
func (r *Registry) DetectedNames() []string {
	var names []string
	for _, s := range r.shims {
		if s.Detect() {
			names = append(names, s.Name())
		}
	}
	return names
}
```

(把 applier.go 里 DetectedShims 实现化简为：)

```go
func (a nftApplier) DetectedShims() []string {
	if a.shims == nil {
		return nil
	}
	return a.shims.DetectedNames()
}
```

- [ ] **Step 5: Tests pass**

```bash
go test ./internal/daemon/ -v
go test ./internal/nft/shim/ -v
```

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go internal/daemon/applier.go internal/nft/shim/shim.go
git commit -m "daemon: warn on startup when FORWARD policy is drop but no shim applies"
```

---

### Task 9: docker/test.sh — shim integration test

**目的：** `docker/test.sh` 新增 step 验证：手工建 DOCKER-USER chain → 提交 DNAT 规则 → 验证 daemon 注入了 owner-tagged rule → 提交空规则 → 验证 rule 消失。

**Files:**
- Modify: `docker/test.sh`

- [ ] **Step 1: 在现有清理 step 之前追加 shim 验证 step**

定位 `docker/test.sh` 最后一个 `green "..."` 之前。找到合适的步骤号（前面有 1, 2, 3, 5, 6, 7；如果按现有有 gap，新增按下一个数字递增）。

```bash
note "8. shim: DOCKER-USER 同步与清理"
docker compose exec daemon bash -c '
  set -e
  # 容器里手工建 DOCKER-USER chain 模拟 Docker 主机环境
  nft add table ip filter 2>/dev/null || true
  nft add chain ip filter DOCKER-USER 2>/dev/null || true

  # 提交一条 DNAT 规则到 daemon
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST -H "Content-Type: application/json" \
       http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[{\"id\":\"a\",\"proto\":\"tcp\",\"src_port\":58443,\"dest_ip\":\"10.20.1.20\",\"dest_port\":8443}]}" \
       >/dev/null

  # 验证 DOCKER-USER 里出现 nft-forward managed rule
  if ! nft list chain ip filter DOCKER-USER | grep -q "nft-forward managed"; then
    echo "shim 未注入 DOCKER-USER"
    nft list chain ip filter DOCKER-USER
    exit 1
  fi
  if ! nft list chain ip filter DOCKER-USER | grep -q "10.20.1.20"; then
    echo "DNAT 目标 IP 未出现在 shim 规则中"
    exit 1
  fi
  echo "  注入验证通过"

  # 提交空 ruleset，触发 shim 同步删除
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST -H "Content-Type: application/json" \
       http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[]}" \
       >/dev/null

  # 此时 ct state 兜底规则应仍在（每次 sync 都会重写一条），但 dest_ip 那条应消失
  if nft list chain ip filter DOCKER-USER | grep -q "10.20.1.20"; then
    echo "shim 未清除 10.20.1.20 对应的 accept rule"
    exit 1
  fi
  echo "  同步删除验证通过"
' || fail "step 8 失败"
green "  shim 注入与同步验证通过"
```

- [ ] **Step 2: 语法检查**

```bash
bash -n docker/test.sh
```

期望：no output.

- [ ] **Step 3: Commit**

```bash
git add docker/test.sh
git commit -m "docker: verify shim injects and syncs owner-tagged DOCKER-USER rules"
```

---

### Task 10: README + manual-verification doc

**目的：** README "升级与迁移" 章节加一段说明 daemon 自动处理 docker/ufw 兼容；`docs/daemon-manual-verification.md` 加 shim 验证 case。

**Files:**
- Modify: `README.md`
- Modify: `docs/daemon-manual-verification.md`

- [ ] **Step 1: README 加段**

找到 `README.md` 里 "升级与迁移" 章节末尾（在已有内容之后，"### 角色切换" 之类已存在的小节后追加新小节）：

```bash
grep -n '^## 升级与迁移\|^### \|^## ' README.md | head -10
```

定位合适位置后，追加一节：

```markdown
### 防火墙兼容

daemon 启动 / 每次 apply 时会自动同步 `DOCKER-USER`（如果装了 Docker）和 `ufw-user-forward`（如果装了 ufw）chain 里的放行规则，让 nft-forward 的 DNAT 流量穿透 Docker / ufw 把 FORWARD policy 设成 drop 的环境。daemon 卸载或停止时这些规则自动清除。

无 Docker / 无 ufw 的纯净系统：daemon 不动任何 chain，启动日志仅打印 `shim docker-user: target chain not found, skipping`。

如果你装了别的 firewall 工具（firewalld 等）让 FORWARD policy=drop 但 daemon 不能自动处理，启动日志会有一行 `WARN: FORWARD chain has drop policy but no known firewall shim detected`——这时需要手动在你的 firewall 里放行 nft-forward DNAT 后的流量。
```

- [ ] **Step 2: docs/daemon-manual-verification.md 追加 case**

末尾追加：

```markdown

## 6. 防火墙 shim 验证（Docker 主机）

在装了 Docker 的真实 VM 上验证 daemon 自动处理 DOCKER-USER：

```bash
# 前置：清理之前测试残留
sudo iptables -F DOCKER-USER 2>/dev/null || true

# 1. 装 daemon + 加一条规则
sudo bash install.sh tui
sudo nft-forward    # 添加规则 58443 → 10.20.1.20:8443，退出

# 2. 验证 DOCKER-USER 出现 owner-tagged 规则
sudo nft list chain ip filter DOCKER-USER | grep "nft-forward managed"
#   ct state established,related counter ... accept comment "nft-forward managed"
#   ip daddr 10.20.1.20 tcp dport 8443 counter accept comment "nft-forward managed"

# 3. 真实客户端从外网测：应该能连通（之前的"DNAT 后 SYN 没出去"问题消失）

# 4. 停 daemon，验证清理
sudo systemctl stop nft-forward-daemon.service
sudo nft list chain ip filter DOCKER-USER | grep "nft-forward managed" && echo "FAIL: 残留" || echo "OK: 已清理"
```
```

- [ ] **Step 3: Commit**

```bash
git add README.md docs/daemon-manual-verification.md
git commit -m "docs: document daemon's auto-managed DOCKER-USER / ufw-user-forward shim"
```

---

## Self-Review Checklist

1. **Spec coverage**:
   - §设计 §架构 → Task 6 (applier 调 shim.SyncAll) + Task 7 (Cleanup on shutdown) ✓
   - §设计 §文件结构 → Tasks 1, 2, 3, 4, 5 (shim 包所有文件) ✓
   - §设计 §接口定义 → Task 1 (ForwardShim, Registry, runners) ✓
   - §设计 §Rule 内容 → Task 2 (render.go ct state + per-DNAT) ✓
   - §设计 §Owner 标识 → Task 1 (OwnerComment const) + Task 2 (parseShimHandles) ✓
   - §设计 §Sync 流程 → Task 3 (DockerUserShim.Sync) + Task 4 (UfwShim.Sync) ✓
   - §设计 §错误处理 → Task 5 (Registry aggregate) + Task 6 (Apply best-effort log) ✓
   - §设计 §Cleanup 时机 → Task 7 (daemon Run on shutdown) ✓
   - §设计 §干净系统行为 → Task 1 (Detect false → skip) + Task 8 (no warning when no drop policy) ✓
   - §设计 §未知 firewall 检测 → Task 8 (detectForwardDropNoShim + Run 探测) ✓
   - §测试 §单元测试 → Tasks 2-5 全 TDD ✓
   - §测试 §集成测试 → Task 9 (docker/test.sh) ✓
   - §风险与缓解 → 设计/实现自然 cover（idempotent sync, cleanup 兜底, comment stable）；不需要单独 task

2. **Placeholder scan**: 无 TBD / TODO / "implement later" / "similar to Task N" / "appropriate error handling"。所有 code blocks 完整可粘贴。

3. **Type / 名称一致性**:
   - `ForwardShim` interface 在 Task 1 定义、Tasks 3/4/5 中正确实现 ✓
   - `Registry` / `DefaultRegistry()` / `SyncAll` / `CleanupAll` / `Names` / `DetectedNames` 全部一致 ✓
   - `OwnerComment = "nft-forward managed"` 在 Task 1 定义、Task 2 parseShimHandles 使用、Task 9 docker/test.sh 验证 ✓
   - `nftRunner` / `nftScriptRunner` 函数类型在 Task 1 定义，Tasks 3/4 注入测试 ✓
   - `Applier` interface 在 Task 6 扩展 Cleanup，Tasks 6/7 一致引用 ✓
   - `nftApplier` struct 字段 `shims *shim.Registry`, `DetectedShims()` 方法在 Task 6 + Task 8 一致 ✓
   - `fakeApplier.cleanupCalls` 字段在 Task 6 step 4 加，Task 7 step 1 测试引用 ✓
   - `detectForwardDropNoShim` 函数名 Task 8 内一致使用 ✓

4. **修订点**:
   - Task 3 的 `DockerUserShim.Cleanup` 用了 `formatDelete` helper（内联在 docker_user.go），UfwShim 复用同一 helper——OK。
   - Task 8 中 `nftApplier.DetectedShims()` 的方法名通过类型断言传给 `probeFirewallEnvironment`，要求 nftApplier 实现 `DetectedShims() []string`——已确认。
   - Task 8 中 `Registry.DetectedNames()` 跟 Task 1 的 `Names()` 是两个不同方法（前者只返回 detect==true 的）——OK。
   - Task 8 中 detector 用 `iptables -nL FORWARD` 而非 `nft list ruleset`——理由：iptables-nft 把两者输出格式标准化，policy DROP/ACCEPT 文本在 iptables 输出里更稳定。
