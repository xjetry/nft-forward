# 每转发可选「内核态 / 用户态 TCP 转发模式」实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给每条转发加一个 `mode` 选项——内核态(现有 nftables DNAT,零拷贝)或用户态(daemon 内嵌的薄 TCP 分段中继),端到端贯通 daemon / TUI / Web 面板。

**Architecture:** 新增 `internal/forward` 包,把数据平面从单一 `Applier` 升级为 `Dataplane` 编排器(`Partition` 纯函数分流 + kernel 后端 nft/tc + userspace 后端中继 + firewall 集成)。转发层刻意做薄(取 realm/gost 的薄中继之精华,不做优雅排空/完整事务);架构与交互层做完整、好用。

**Tech Stack:** Go 1.26;nftables/iproute2;`golang.org/x/time/rate`(令牌桶);chi(面板 HTTP);bubbletea(TUI);modernc sqlite(面板 DB)。

**Spec:** `docs/superpowers/specs/2026-06-02-userspace-tcp-forward-mode-design.md`

**关键约束(CLAUDE.md):** commit message / 代码注释里**禁止**出现任务编号、方案代号、审阅轮次等过程信息。注释只解释 WHY 与 invariant。本计划的 `### Task N` 仅为计划结构,**不得**写进 commit/注释。

---

## Task 1: 数据模型 `nft.Rule.Mode` + `EffectiveMode` + `Validate` 矩阵

**Files:**
- Modify: `internal/nft/nft.go`
- Test: `internal/nft/nft_test.go`

- [ ] **Step 1: 写失败测试(校验矩阵 + EffectiveMode)**

追加到 `internal/nft/nft_test.go`:

```go
func TestEffectiveMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ModeKernel},
		{ModeKernel, ModeKernel},
		{ModeUserspace, ModeUserspace},
		{"bogus", ModeKernel}, // 未知值归一为 kernel(防御性默认)
	}
	for _, c := range cases {
		got := Rule{Mode: c.in}.EffectiveMode()
		if got != c.want {
			t.Errorf("EffectiveMode(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestValidate_ModeMatrix(t *testing.T) {
	base := Rule{SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443}
	mk := func(proto, mode string) Rule { r := base; r.Proto = proto; r.Mode = mode; return r }

	ok := []Rule{
		mk("tcp", ""), mk("tcp", ModeKernel), mk("udp", ModeKernel), mk("tcp+udp", ModeKernel),
		mk("tcp", ModeUserspace), mk("tcp+udp", ModeUserspace),
	}
	for _, r := range ok {
		if err := Validate(r); err != nil {
			t.Errorf("Validate(%s/%s) unexpected error: %v", r.Proto, r.Mode, err)
		}
	}

	bad := []Rule{
		mk("udp", ModeUserspace),   // UDP 不支持用户态
		mk("tcp", "weird"),         // 非法 mode
	}
	for _, r := range bad {
		if err := Validate(r); err == nil {
			t.Errorf("Validate(%s/%s) expected error, got nil", r.Proto, r.Mode)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/ -run 'TestEffectiveMode|TestValidate_ModeMatrix' -v`
Expected: 编译失败(`ModeKernel`/`ModeUserspace` 未定义,`EffectiveMode` 未定义)。

- [ ] **Step 3: 实现 — 加常量、字段、EffectiveMode**

在 `internal/nft/nft.go` 的 `const` 块(`TableName`/`TableFamily` 旁)追加:

```go
const (
	ModeKernel    = "kernel"
	ModeUserspace = "userspace"
)
```

在 `Rule` 结构体末尾(`BandwidthMbps` 后)加字段:

```go
	// Mode selects the data plane for this forward: "" / "kernel" = nftables
	// DNAT (zero-copy); "userspace" = the embedded TCP split-relay (TCP only).
	Mode string `json:"mode,omitempty"`
```

在 `Rule` 方法区(`Display` 附近)加:

```go
// EffectiveMode normalizes the mode: an empty or unrecognized value means
// kernel, so old state files and old-panel pushes (no mode field) default to
// the existing zero-copy behavior. This is the single source of the default.
func (r Rule) EffectiveMode() string {
	if r.Mode == ModeUserspace {
		return ModeUserspace
	}
	return ModeKernel
}
```

- [ ] **Step 4: 实现 — 扩展 Validate**

在 `Validate` 函数 `return nil` 之前插入:

```go
	switch r.Mode {
	case "", ModeKernel, ModeUserspace:
	default:
		return fmt.Errorf("转发模式必须为 kernel 或 userspace")
	}
	if r.EffectiveMode() == ModeUserspace && r.Proto == "udp" {
		return fmt.Errorf("UDP 不支持用户态转发")
	}
```

- [ ] **Step 5: 跑测试确认通过 + 全包回归**

Run: `go test ./internal/nft/ -v`
Expected: PASS(含既有用例)。

- [ ] **Step 6: 提交**

```bash
git add internal/nft/nft.go internal/nft/nft_test.go
git commit -m "nft: add per-rule forwarding mode (kernel/userspace) to the rule model

Rule.Mode selects kernel nftables DNAT (default) or the userspace TCP
relay. EffectiveMode normalizes empty/unknown to kernel so old state and
old-panel pushes keep working. Validate rejects udp+userspace."
```

---

## Task 2: `internal/forward` 包 — `Counter` 类型 + `Partition` 纯函数

**Files:**
- Create: `internal/forward/counter.go`
- Create: `internal/forward/partition.go`
- Test: `internal/forward/partition_test.go`

- [ ] **Step 1: 写失败测试**

创建 `internal/forward/partition_test.go`:

```go
package forward

import (
	"testing"

	"nft-forward/internal/nft"
)

func ports(rules []nft.Rule) map[string]bool {
	m := map[string]bool{}
	for _, r := range rules {
		m[r.Proto+"/"+itoaTest(r.SrcPort)] = true
	}
	return m
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestPartition_KernelPassthrough(t *testing.T) {
	in := []nft.Rule{
		{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
		{ID: "b", Proto: "udp", SrcPort: 53, DestIP: "10.0.0.2", DestPort: 53, Mode: nft.ModeKernel},
	}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 || len(u) != 0 {
		t.Fatalf("want 2 kernel / 0 userspace, got %d/%d", len(k), len(u))
	}
}

func TestPartition_UserspaceTCP(t *testing.T) {
	in := []nft.Rule{{ID: "a", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 0 || len(u) != 1 || u[0].Proto != "tcp" {
		t.Fatalf("want 0 kernel / 1 tcp userspace, got %d/%d", len(k), len(u))
	}
}

func TestPartition_TCPUDPUserspaceSplits(t *testing.T) {
	in := []nft.Rule{{ID: "a", Proto: "tcp+udp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}
	k, u, err := Partition(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 1 || k[0].Proto != "udp" || k[0].EffectiveMode() != nft.ModeKernel {
		t.Fatalf("want udp kernel half, got %+v", k)
	}
	if len(u) != 1 || u[0].Proto != "tcp" || u[0].EffectiveMode() != nft.ModeUserspace {
		t.Fatalf("want tcp userspace half, got %+v", u)
	}
}

func TestPartition_OverlapRejected(t *testing.T) {
	// tcp+udp kernel on 8443 occupies tcp/8443 AND udp/8443; a tcp userspace
	// rule on 8443 then collides on tcp/8443.
	in := []nft.Rule{
		{ID: "a", Proto: "tcp+udp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443},
		{ID: "b", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.2", DestPort: 443, Mode: nft.ModeUserspace},
	}
	if _, _, err := Partition(in); err == nil {
		t.Fatal("expected overlap error, got nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/forward/ -run TestPartition -v`
Expected: 编译失败(包不存在 / `Partition` 未定义)。

- [ ] **Step 3: 实现 Counter**

创建 `internal/forward/counter.go`:

```go
// Package forward is the data plane: it reconciles a resolved rule set onto
// the kernel (nftables DNAT + tc) and userspace (embedded TCP relay) backends
// behind one Dataplane surface. The forwarding layer is intentionally thin —
// the relay is a plain bidirectional copy, with only the minimum lifecycle
// machinery needed for correctness.
package forward

// Counter is the unified per-rule traffic counter across both backends. It is
// the data plane's public counter contract (the kernel backend maps
// nft.Counter into it; the userspace backend produces it directly). Bytes
// counts the inbound (client->target) direction only, matching nft prerouting
// counter semantics so both modes accrue tenant quota identically.
type Counter struct {
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	Bytes      int64  `json:"bytes"`
	Packets    int64  `json:"packets"`
}
```

- [ ] **Step 4: 实现 Partition**

创建 `internal/forward/partition.go`:

```go
package forward

import (
	"fmt"

	"nft-forward/internal/nft"
)

// Partition splits resolved rules into the kernel and userspace rule sets.
// A tcp+udp userspace rule is split into a udp kernel rule and a tcp userspace
// rule (same target/bandwidth). It returns an error when two rules' effective
// (proto, port) tuples overlap — treating tcp+udp as occupying both tcp/port
// and udp/port — which also catches the latent tcp+udp-vs-tcp ambiguity that
// the owner-level merge (keyed by the literal proto string) cannot see.
//
// Callers pass already-resolved, already-Validated rules; a stray
// udp+userspace rule (which Validate rejects) is handled defensively.
func Partition(rules []nft.Rule) (kernel, userspace []nft.Rule, err error) {
	claimed := map[string]string{} // "tcp/8443" -> who claimed it

	claim := func(proto string, port int, who string) error {
		protos := []string{proto}
		if proto == "tcp+udp" {
			protos = []string{"tcp", "udp"}
		}
		for _, p := range protos {
			key := fmt.Sprintf("%s/%d", p, port)
			if prev, dup := claimed[key]; dup {
				return fmt.Errorf("端口 %s 同时被 %s 与 %s 占用", key, prev, who)
			}
			claimed[key] = who
		}
		return nil
	}

	for _, r := range rules {
		who := fmt.Sprintf("规则 %s (%s/%d, %s)", r.ID, r.Proto, r.SrcPort, r.EffectiveMode())
		if r.EffectiveMode() == nft.ModeKernel {
			if cerr := claim(r.Proto, r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			kernel = append(kernel, r)
			continue
		}
		switch r.Proto {
		case "tcp":
			if cerr := claim("tcp", r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			userspace = append(userspace, r)
		case "tcp+udp":
			if cerr := claim("tcp+udp", r.SrcPort, who); cerr != nil {
				return nil, nil, cerr
			}
			udp := r
			udp.Proto = "udp"
			udp.Mode = nft.ModeKernel
			kernel = append(kernel, udp)
			tcp := r
			tcp.Proto = "tcp"
			tcp.Mode = nft.ModeUserspace
			userspace = append(userspace, tcp)
		default:
			return nil, nil, fmt.Errorf("规则 %s: 协议 %s 不能用用户态", r.ID, r.Proto)
		}
	}
	return kernel, userspace, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/forward/ -run TestPartition -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/forward/counter.go internal/forward/partition.go internal/forward/partition_test.go
git commit -m "forward: add the data-plane package with Counter and Partition

Partition splits resolved rules by mode (tcp+udp userspace -> udp kernel +
tcp userspace) and rejects overlapping effective (proto,port) tuples.
Counter is the unified per-rule counter contract for both backends."
```

---

## Task 3: 用户态中继 — `relay.go` + `userspace.go`(loopback 单测,无需 root)

**Files:**
- Create: `internal/forward/relay.go`
- Create: `internal/forward/userspace.go`
- Test: `internal/forward/userspace_test.go`
- Modify: `go.mod` / `go.sum`(加 `golang.org/x/time/rate`)

- [ ] **Step 1: 加依赖**

Run:
```bash
go get golang.org/x/time/rate@latest
go mod tidy
```
Expected: `go.mod` 出现 `golang.org/x/time vX.Y.Z`。

- [ ] **Step 2: 写失败测试(loopback 端到端 + 计数 + reconcile 增删)**

创建 `internal/forward/userspace_test.go`:

```go
package forward

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

// freePort grabs an ephemeral port number, then releases it so the relay can
// bind it. Small TOCTOU window, acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// echoServer accepts one-shot connections and echoes everything back.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestUserspace_LoopbackEchoAndCounter(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	listen := freePort(t)
	b := newUserspaceBackend()
	defer b.Close()

	rule := nft.Rule{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace}
	if err := b.Reconcile([]nft.Rule{rule}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello-relay")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: got %q", buf)
	}

	// Inbound bytes must be counted (client->upstream direction).
	deadline := time.Now().Add(time.Second)
	for {
		cs := b.Counters()
		if len(cs) == 1 && cs[0].ListenPort == listen && cs[0].Bytes >= int64(len(msg)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("counter not updated: %+v", cs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserspace_ReconcileAddRemove(t *testing.T) {
	b := newUserspaceBackend()
	defer b.Close()

	p1, p2 := freePort(t), freePort(t)
	r1 := nft.Rule{ID: "1", Proto: "tcp", SrcPort: p1, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}
	r2 := nft.Rule{ID: "2", Proto: "tcp", SrcPort: p2, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}

	if err := b.Reconcile([]nft.Rule{r1, r2}); err != nil {
		t.Fatal(err)
	}
	if len(b.listeners) != 2 {
		t.Fatalf("want 2 listeners, got %d", len(b.listeners))
	}
	// Drop r1.
	if err := b.Reconcile([]nft.Rule{r2}); err != nil {
		t.Fatal(err)
	}
	if len(b.listeners) != 1 {
		t.Fatalf("want 1 listener after removal, got %d", len(b.listeners))
	}
	if _, ok := b.listeners[p2]; !ok {
		t.Fatalf("listener %d should remain", p2)
	}
	// p1 must now be bindable again (listener actually closed).
	probe, err := net.Listen("tcp4", fmt.Sprintf(":%d", p1))
	if err != nil {
		t.Fatalf("port %d not released: %v", p1, err)
	}
	probe.Close()
}

func TestUserspace_TargetHotUpdate(t *testing.T) {
	// New connections must reach the updated target without a listener restart.
	a, stopA := echoServer(t)
	defer stopA()
	b2, stopB := echoServer(t)
	defer stopB()

	listen := freePort(t)
	be := newUserspaceBackend()
	defer be.Close()

	ahost, aps, _ := net.SplitHostPort(a)
	ap, _ := strconv.Atoi(aps)
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: ahost, DestPort: ap, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatal(err)
	}
	// Retarget to b.
	bhost, bps, _ := net.SplitHostPort(b2)
	bp, _ := strconv.Atoi(bps)
	if err := be.Reconcile([]nft.Rule{{ID: "x", Proto: "tcp", SrcPort: listen, DestIP: bhost, DestPort: bp, Mode: nft.ModeUserspace}}); err != nil {
		t.Fatal(err)
	}
	// A fresh connection should still echo (proves the listener survived and
	// dials the new target).
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo after retarget: %v", err)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/forward/ -run TestUserspace -v`
Expected: 编译失败(`newUserspaceBackend` 等未定义)。

- [ ] **Step 4: 实现 relay.go**

创建 `internal/forward/relay.go`:

```go
package forward

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync/atomic"

	"golang.org/x/time/rate"

	"nft-forward/internal/nft"
)

// relayBufSize is the copy buffer per direction. Wrapping the source reader
// (below) makes io.CopyBuffer take the generic buffered path instead of
// splice — required so byte accounting updates continuously over the life of
// a connection, not only at close. This mirrors realm's behavior when its
// traffic counter is enabled; we always count (for quota), so we always copy.
const relayBufSize = 64 * 1024

type target struct{ addr string }

func targetAddr(r nft.Rule) string {
	return net.JoinHostPort(r.DestIP, strconv.Itoa(r.DestPort))
}

// makeLimiter converts a Mbps cap into a byte/sec token-bucket limiter, or nil
// when unlimited. Burst must be >= the largest single WaitN call (one buffer).
func makeLimiter(mbps int) *rate.Limiter {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := float64(mbps) * 1e6 / 8.0
	burst := int(bytesPerSec)
	if burst < relayBufSize {
		burst = relayBufSize
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// meteredReader rate-limits and/or counts each Read. limPtr may hold nil
// (unlimited); counter may be nil (don't count this direction).
type meteredReader struct {
	src     io.Reader
	limPtr  *atomic.Pointer[rate.Limiter]
	counter *atomic.Int64
	ctx     context.Context
}

func (r *meteredReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		if r.limPtr != nil {
			if lim := r.limPtr.Load(); lim != nil {
				if werr := lim.WaitN(r.ctx, n); werr != nil {
					return n, werr
				}
			}
		}
		if r.counter != nil {
			r.counter.Add(int64(n))
		}
	}
	return n, err
}

// relayCopy copies src->dst. When limPtr or counter is non-nil it wraps src so
// each chunk is paced/counted; otherwise it is a plain buffered copy.
func relayCopy(ctx context.Context, dst io.Writer, src io.Reader, limPtr *atomic.Pointer[rate.Limiter], counter *atomic.Int64) {
	var r io.Reader = src
	if limPtr != nil || counter != nil {
		r = &meteredReader{src: src, limPtr: limPtr, counter: counter, ctx: ctx}
	}
	buf := make([]byte, relayBufSize)
	_, _ = io.CopyBuffer(dst, r, buf)
}

// halfCloseWrite propagates a one-directional EOF so protocols that signal end
// of stream by closing one half keep working.
func halfCloseWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
```

- [ ] **Step 5: 实现 userspace.go**

创建 `internal/forward/userspace.go`:

```go
package forward

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"

	"nft-forward/internal/nft"
)

// listener is one userspace TCP forward: a net.Listener plus the hot-updatable
// dial target and rate limiter shared by all of its connections.
type listener struct {
	port  int
	ln    net.Listener
	tgt   atomic.Pointer[target]
	lim   atomic.Pointer[rate.Limiter]
	bytes atomic.Int64

	conns  sync.Map // net.Conn -> struct{}; only so close() can tear them down
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func openListener(r nft.Rule) (*listener, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", r.SrcPort))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &listener{port: r.SrcPort, ln: ln, ctx: ctx, cancel: cancel}
	l.tgt.Store(&target{addr: targetAddr(r)})
	l.lim.Store(makeLimiter(r.BandwidthMbps))
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

func (l *listener) acceptLoop() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed (or fatal): stop accepting
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(conn)
		}()
	}
}

func (l *listener) handle(client net.Conn) {
	l.conns.Store(client, struct{}{})
	defer func() { l.conns.Delete(client); client.Close() }()

	tgt := l.tgt.Load()
	if tgt == nil {
		return
	}
	upstream, err := net.DialTimeout("tcp4", tgt.addr, dialTimeout)
	if err != nil {
		return
	}
	l.conns.Store(upstream, struct{}{})
	defer func() { l.conns.Delete(upstream); upstream.Close() }()

	done := make(chan struct{}, 2)
	// Inbound (client->upstream): rate-limited + counted, matching nft
	// prerouting counter semantics.
	go func() {
		relayCopy(l.ctx, upstream, client, &l.lim, &l.bytes)
		halfCloseWrite(upstream)
		done <- struct{}{}
	}()
	// Return path (upstream->client): unshaped + uncounted (parity: nft
	// counts only the marked forward direction).
	go func() {
		relayCopy(l.ctx, client, upstream, nil, nil)
		halfCloseWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// close stops accepting, force-closes in-flight connections, and waits for all
// goroutines. No graceful drain — the forwarding layer is intentionally thin.
func (l *listener) close() {
	l.cancel()
	_ = l.ln.Close()
	l.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
	l.wg.Wait()
}

// userspaceBackend keeps one listener per userspace TCP rule, keyed by port.
type userspaceBackend struct {
	mu        sync.Mutex
	listeners map[int]*listener
}

func newUserspaceBackend() *userspaceBackend {
	return &userspaceBackend{listeners: map[int]*listener{}}
}

// Reconcile makes the running listener set match rules. New listeners open
// first (make-before-break: a bind failure rolls back the just-opened ones and
// leaves the previous set intact); targets/limits hot-update without restart;
// removed listeners are closed.
func (b *userspaceBackend) Reconcile(rules []nft.Rule) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	desired := make(map[int]nft.Rule, len(rules))
	for _, r := range rules {
		desired[r.SrcPort] = r
	}

	var opened []*listener
	for port, r := range desired {
		if _, ok := b.listeners[port]; ok {
			continue
		}
		l, err := openListener(r)
		if err != nil {
			for _, ol := range opened {
				ol.close()
				delete(b.listeners, ol.port)
			}
			return fmt.Errorf("监听 tcp/%d 失败: %w", port, err)
		}
		b.listeners[port] = l
		opened = append(opened, l)
	}

	for port, r := range desired {
		l := b.listeners[port]
		l.tgt.Store(&target{addr: targetAddr(r)})
		l.lim.Store(makeLimiter(r.BandwidthMbps))
	}

	for port, l := range b.listeners {
		if _, ok := desired[port]; !ok {
			l.close()
			delete(b.listeners, port)
		}
	}
	return nil
}

func (b *userspaceBackend) Counters() []Counter {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Counter, 0, len(b.listeners))
	for _, l := range b.listeners {
		out = append(out, Counter{Proto: "tcp", ListenPort: l.port, Bytes: l.bytes.Load()})
	}
	return out
}

func (b *userspaceBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for port, l := range b.listeners {
		l.close()
		delete(b.listeners, port)
	}
}
```

加 `dialTimeout` 常量到 `relay.go` 的 `const` 块旁(或 userspace.go 顶部),例如在 relay.go 加:

```go
import "time"
// ...
const dialTimeout = 10 * time.Second
```

> 注意:`relay.go` 已 import 多个包;把 `time` 加进 import 并加上 `dialTimeout` 常量。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/forward/ -run TestUserspace -v`
Expected: PASS(echo 往返、计数、增删端口释放、目标热更新)。

- [ ] **Step 7: 提交**

```bash
git add internal/forward/relay.go internal/forward/userspace.go internal/forward/userspace_test.go go.mod go.sum
git commit -m "forward: add the thin userspace TCP relay backend

A listener per userspace rule, bidirectional buffered copy with half-close,
optional shared token-bucket on the inbound direction, atomic target/limiter
hot-update, inbound byte counting. Reconcile is make-before-break; close is
direct (no graceful drain) per the thin-forwarding value."
```

---

## Task 4: 防火墙 shim 泛化(FORWARD + INPUT 放行)

**Files:**
- Modify: `internal/nft/shim/shim.go`(`FirewallState`/`ListenPort`、`ForwardShim.Sync`、`Registry.SyncAll`)
- Modify: `internal/nft/shim/render.go`(`renderInputShimScript`)
- Modify: `internal/nft/shim/docker_user.go`(`Sync(state)`)
- Modify: `internal/nft/shim/ufw.go`(`Sync(state)` 双链 + `Cleanup` 双链)
- Test: `internal/nft/shim/render_test.go`、`internal/nft/shim/ufw_test.go`、`internal/nft/shim/docker_user_test.go`、`internal/nft/shim/registry_test.go`

- [ ] **Step 1: 写失败测试(INPUT 渲染 + ufw 双链)**

追加到 `internal/nft/shim/render_test.go`:

```go
func TestRenderInputShimScript(t *testing.T) {
	ports := []ListenPort{{Proto: "tcp", Port: 8443}, {Proto: "tcp", Port: 9000}}
	out := renderInputShimScript("ip", "filter", "ufw-user-input", ports, []int{5})
	if !strings.Contains(out, "delete rule ip filter ufw-user-input handle 5") {
		t.Errorf("missing stale delete:\n%s", out)
	}
	if !strings.Contains(out, `tcp dport 8443 counter accept comment "`+OwnerComment+`"`) {
		t.Errorf("missing input accept for 8443:\n%s", out)
	}
	if !strings.Contains(out, `tcp dport 9000 counter accept comment "`+OwnerComment+`"`) {
		t.Errorf("missing input accept for 9000:\n%s", out)
	}
}
```

追加到 `internal/nft/shim/ufw_test.go`(沿用该文件已有的 fake runner 写法;若已有 `newUfwShimWithFakes` 之类辅助,复用之。下面假设可直接构造并替换 `runNft`/`runNftScript`):

```go
func TestUfwSync_InputChainGetsListenPorts(t *testing.T) {
	var scripts []string
	s := &UfwShim{
		runNft: func(args ...string) (string, error) {
			// Pretend both chains exist and carry no stale handles.
			return "", nil
		},
		runNftScript: func(script string) error {
			scripts = append(scripts, script)
			return nil
		},
	}
	err := s.Sync(FirewallState{
		ForwardRules: []nft.Rule{{Proto: "tcp", DestIP: "10.0.0.1", DestPort: 443}},
		ListenPorts:  []ListenPort{{Proto: "tcp", Port: 8443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(scripts, "\n")
	if !strings.Contains(joined, "ufw-user-forward") || !strings.Contains(joined, "ip daddr 10.0.0.1") {
		t.Errorf("forward chain not synced:\n%s", joined)
	}
	if !strings.Contains(joined, "ufw-user-input") || !strings.Contains(joined, "tcp dport 8443") {
		t.Errorf("input chain not synced:\n%s", joined)
	}
}
```

> 实现前先看 `ufw_test.go`/`docker_user_test.go` 现有 fake 构造方式,保持一致。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/shim/ -run 'TestRenderInputShimScript|TestUfwSync_InputChain' -v`
Expected: 编译失败(`ListenPort`/`FirewallState`/`renderInputShimScript` 未定义,`Sync` 签名不符)。

- [ ] **Step 3: 实现 — shim.go 类型与接口**

在 `internal/nft/shim/shim.go` 的 `ForwardShim` 定义上方加:

```go
// FirewallState carries what the shims need to make the host pass our traffic:
// FORWARD accepts for kernel DNAT targets, and INPUT accepts for userspace TCP
// listen ports.
type FirewallState struct {
	ForwardRules []nft.Rule
	ListenPorts  []ListenPort
}

// ListenPort is one userspace listener the firewall must allow inbound.
type ListenPort struct {
	Proto string // "tcp"
	Port  int
}
```

把接口方法签名改为:

```go
	// Sync makes the target chain(s) reflect state: deletes leftover
	// owner-tagged rules, inserts current ones. No-op when Detect is false.
	Sync(state FirewallState) error
```

把 `Registry.SyncAll` 签名改为接收 `state FirewallState`,内部 `s.Sync(state)`:

```go
func (r *Registry) SyncAll(state FirewallState) error {
	var errs []string
	for _, s := range r.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Sync(state); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &aggregateError{errs}
}
```

- [ ] **Step 4: 实现 — render.go 增加 INPUT 渲染**

在 `internal/nft/shim/render.go` 末尾加:

```go
// renderInputShimScript builds the nft -f script for an INPUT-type chain:
// delete stale owner-tagged rules, then one accept per userspace listen port.
// No ct-state rule (inbound NEW is exactly what we allow) and no ip daddr.
func renderInputShimScript(family, table, chain string, ports []ListenPort, staleHandles []int) string {
	var b strings.Builder
	for _, h := range staleHandles {
		fmt.Fprintf(&b, "delete rule %s %s %s handle %d\n", family, table, chain, h)
	}
	for _, p := range ports {
		fmt.Fprintf(&b,
			"add rule %s %s %s %s dport %d counter accept comment \"%s\"\n",
			family, table, chain, p.Proto, p.Port, OwnerComment,
		)
	}
	return b.String()
}
```

- [ ] **Step 5: 实现 — docker_user.go 改签名**

把 `DockerUserShim.Sync` 改为:

```go
func (s *DockerUserShim) Sync(state FirewallState) error {
	out, err := s.runNft("-a", "list", "chain", dockerUserFamily, dockerUserTable, dockerUserChain)
	if err != nil {
		return nil // chain absent; nothing to do
	}
	stale := parseShimHandles(out)
	script := renderShimScript(dockerUserFamily, dockerUserTable, dockerUserChain, state.ForwardRules, stale)
	return s.runNftScript(script)
}
```

(Docker 不管宿主 INPUT,忽略 `state.ListenPorts`。)

- [ ] **Step 6: 实现 — ufw.go 双链**

在常量块加 `ufwInputChain = "ufw-user-input"`,并改写 `Sync`/`Cleanup`,新增 `syncChain` 辅助:

```go
const (
	ufwFamily     = "ip"
	ufwTable      = "filter"
	ufwChain      = "ufw-user-forward"
	ufwInputChain = "ufw-user-input"
)

func (s *UfwShim) Sync(state FirewallState) error {
	if err := s.syncChain(ufwChain, func(stale []int) string {
		return renderShimScript(ufwFamily, ufwTable, ufwChain, state.ForwardRules, stale)
	}); err != nil {
		return err
	}
	return s.syncChain(ufwInputChain, func(stale []int) string {
		return renderInputShimScript(ufwFamily, ufwTable, ufwInputChain, state.ListenPorts, stale)
	})
}

// syncChain lists one chain (skipping when absent), parses owner-tagged stale
// handles, and runs the built script in one atomic nft -f transaction.
func (s *UfwShim) syncChain(chain string, build func(stale []int) string) error {
	out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, chain)
	if err != nil {
		return nil // chain absent
	}
	stale := parseShimHandles(out)
	return s.runNftScript(build(stale))
}

func (s *UfwShim) Cleanup() error {
	for _, chain := range []string{ufwChain, ufwInputChain} {
		out, err := s.runNft("-a", "list", "chain", ufwFamily, ufwTable, chain)
		if err != nil {
			continue
		}
		stale := parseShimHandles(out)
		if len(stale) == 0 {
			continue
		}
		var script string
		for _, h := range stale {
			script += formatDelete(ufwFamily, ufwTable, chain, h)
		}
		if err := s.runNftScript(script); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 7: 迁移既有 shim 测试到新签名**

把 `docker_user_test.go`、`ufw_test.go`、`registry_test.go` 中所有 `.Sync(rules)` / `SyncAll(rules)` 调用改为传 `FirewallState{ForwardRules: rules}`(原断言不变——FORWARD 链行为未变)。

- [ ] **Step 8: 跑全包测试**

Run: `go test ./internal/nft/shim/ -v`
Expected: PASS。

- [ ] **Step 9: 提交**

```bash
git add internal/nft/shim/
git commit -m "shim: carry both forward rules and userspace listen ports

Generalize the firewall integration to a FirewallState: docker-user keeps
adding FORWARD accepts; ufw now also accepts the userspace TCP listen ports
into ufw-user-input so the embedded relay is reachable when INPUT defaults
to drop."
```

---

## Task 5: `kernel` 后端 + `firewall` + `Dataplane` 编排器

**Files:**
- Create: `internal/forward/kernel.go`
- Create: `internal/forward/firewall.go`
- Create: `internal/forward/dataplane.go`
- Test: `internal/forward/dataplane_test.go`

- [ ] **Step 1: 写失败测试(回滚 + 计数合并)**

创建 `internal/forward/dataplane_test.go`:

```go
package forward

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"nft-forward/internal/nft"
)

type fakeKernel struct {
	err    error
	rules  []nft.Rule
	counts []Counter
}

func (k *fakeKernel) Reconcile(rules []nft.Rule) error {
	if k.err != nil {
		return k.err
	}
	k.rules = append([]nft.Rule(nil), rules...)
	return nil
}
func (k *fakeKernel) Counters() ([]Counter, error) { return k.counts, nil }

// newTestDataplane builds a Dataplane with an injectable kernel, a real
// userspace backend (loopback-safe), and a no-op firewall.
func newTestDataplane(k kernelReconciler) *Dataplane {
	return &Dataplane{kernel: k, userspace: newUserspaceBackend(), fw: firewall{shims: nil}}
}

func TestDataplane_KernelFailureRollsBackUserspace(t *testing.T) {
	dp := newTestDataplane(&fakeKernel{err: errors.New("nft boom")})
	defer dp.Close(context.Background())

	l, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	rules := []nft.Rule{{ID: "x", Proto: "tcp", SrcPort: port, DestIP: "127.0.0.1", DestPort: 9, Mode: nft.ModeUserspace}}
	if err := dp.Reconcile(context.Background(), rules); err == nil {
		t.Fatal("expected kernel error to surface")
	}
	// Userspace must have rolled back to the previous (empty) set.
	if n := len(dp.userspace.listeners); n != 0 {
		t.Fatalf("userspace not rolled back: %d listeners remain", n)
	}
	// Port must be free again.
	probe, err := net.Listen("tcp4", ":"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port not released after rollback: %v", err)
	}
	probe.Close()
}

func TestDataplane_CountersMerged(t *testing.T) {
	dp := newTestDataplane(&fakeKernel{counts: []Counter{{Proto: "udp", ListenPort: 53, Bytes: 100}}})
	defer dp.Close(context.Background())

	cs, err := dp.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Proto != "udp" {
		t.Fatalf("want merged kernel counter, got %+v", cs)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/forward/ -run TestDataplane -v`
Expected: 编译失败(`kernelReconciler`/`Dataplane`/`firewall` 未定义)。

- [ ] **Step 3: 实现 kernel.go**

创建 `internal/forward/kernel.go`:

```go
package forward

import (
	"nft-forward/internal/nft"
	"nft-forward/internal/tc"
)

// kernelReconciler is the kernel backend seam; the Dataplane test injects a
// fake (the real one shells nft/tc and needs root).
type kernelReconciler interface {
	Reconcile(rules []nft.Rule) error
	Counters() ([]Counter, error)
}

type kernelBackend struct {
	iface string
}

// Reconcile applies the atomic nftables ruleset, then rebuilds the tc HTB tree.
// nft is atomic (keeps the old table on failure); tc runs after so a stale
// class never points at an unpublished dest IP. A tc failure surfaces an error
// but leaves nft applied — traffic keeps forwarding, only shaping is missing.
func (k kernelBackend) Reconcile(rules []nft.Rule) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	return tc.Apply(rules, k.iface)
}

func (k kernelBackend) Counters() ([]Counter, error) {
	cs, err := nft.Counters()
	if err != nil {
		return nil, err
	}
	out := make([]Counter, 0, len(cs))
	for _, c := range cs {
		out = append(out, Counter{Proto: c.Proto, ListenPort: c.ListenPort, Bytes: c.Bytes, Packets: c.Packets})
	}
	return out, nil
}
```

- [ ] **Step 4: 实现 firewall.go**

创建 `internal/forward/firewall.go`:

```go
package forward

import (
	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
)

// firewall drives the shim registry with both rule sets. Best-effort: a shim
// failure never fails a reconcile (the core nft table is already applied).
type firewall struct {
	shims *shim.Registry
}

func (f firewall) Sync(forwardRules []nft.Rule, listenPorts []shim.ListenPort) error {
	if f.shims == nil {
		return nil
	}
	return f.shims.SyncAll(shim.FirewallState{ForwardRules: forwardRules, ListenPorts: listenPorts})
}

func (f firewall) Cleanup() error {
	if f.shims == nil {
		return nil
	}
	return f.shims.CleanupAll()
}

func (f firewall) DetectedNames() []string {
	if f.shims == nil {
		return nil
	}
	return f.shims.DetectedNames()
}
```

- [ ] **Step 5: 实现 dataplane.go**

创建 `internal/forward/dataplane.go`:

```go
package forward

import (
	"context"
	"log"
	"sync"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
)

// Dataplane orchestrates the kernel and userspace backends plus firewall
// integration behind one Reconcile/Counters/Close surface. It keeps a single
// rollback anchor (lastUserspace): nft is atomic so the kernel needs none, but
// a kernel failure after a successful userspace step would otherwise strand
// the userspace layer ahead of the daemon's logical state (refreshOnce only
// re-applies on an actual resolved-IP change, so it would not self-correct).
type Dataplane struct {
	kernel    kernelReconciler
	userspace *userspaceBackend
	fw        firewall

	mu            sync.Mutex
	lastUserspace []nft.Rule
}

// Config wires dependencies. Shims defaults to the built-in registry.
type Config struct {
	Iface string
	Shims *shim.Registry
}

func New(cfg Config) *Dataplane {
	shims := cfg.Shims
	if shims == nil {
		shims = shim.DefaultRegistry()
	}
	return &Dataplane{
		kernel:    kernelBackend{iface: cfg.Iface},
		userspace: newUserspaceBackend(),
		fw:        firewall{shims: shims},
	}
}

// Reconcile partitions rules and applies userspace first, then kernel, then
// firewall (best-effort). A hard kernel failure rolls userspace back to the
// last good set.
func (d *Dataplane) Reconcile(ctx context.Context, rules []nft.Rule) error {
	kernelRules, userspaceRules, err := Partition(rules)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.userspace.Reconcile(userspaceRules); err != nil {
		return err
	}
	if err := d.kernel.Reconcile(kernelRules); err != nil {
		if rbErr := d.userspace.Reconcile(d.lastUserspace); rbErr != nil {
			log.Printf("dataplane: 内核应用失败后用户态回滚亦失败: %v", rbErr)
		}
		return err
	}
	if err := d.fw.Sync(kernelRules, listenPortsOf(userspaceRules)); err != nil {
		log.Printf("dataplane: 防火墙同步: %v", err)
	}
	d.lastUserspace = append([]nft.Rule(nil), userspaceRules...)
	return nil
}

func (d *Dataplane) Counters() ([]Counter, error) {
	kc, err := d.kernel.Counters()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	uc := d.userspace.Counters()
	d.mu.Unlock()
	return append(kc, uc...), nil
}

func (d *Dataplane) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.userspace.Close()
	return d.fw.Cleanup()
}

// DetectedShims exposes firewall detection for the daemon's startup
// FORWARD-policy warning.
func (d *Dataplane) DetectedShims() []string {
	return d.fw.DetectedNames()
}

func listenPortsOf(rules []nft.Rule) []shim.ListenPort {
	out := make([]shim.ListenPort, 0, len(rules))
	for _, r := range rules {
		out = append(out, shim.ListenPort{Proto: "tcp", Port: r.SrcPort})
	}
	return out
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/forward/ -v`
Expected: PASS(回滚、计数合并、Task 2/3 用例全过)。

- [ ] **Step 7: 提交**

```bash
git add internal/forward/kernel.go internal/forward/firewall.go internal/forward/dataplane.go internal/forward/dataplane_test.go
git commit -m "forward: add kernel backend, firewall step, and Dataplane orchestrator

Dataplane reconciles userspace-then-kernel (eliminating the kernel->userspace
flip blackhole), drives the firewall best-effort, and keeps one lastUserspace
rollback anchor for the kernel-failure path. Counters merge both backends."
```

---

## Task 6: daemon 接线 `Applier` → `Dataplane`(含计数统一与测试迁移)

**Files:**
- Create: `internal/daemon/dataplane.go`(消费侧接口)
- Delete: `internal/daemon/applier.go`、`internal/daemon/applier_test.go`
- Modify: `internal/daemon/daemon.go`(Config/New/probe)
- Modify: `internal/daemon/handlers.go`(`dp` 字段、`applySerialized(ctx,...)`、`closeSerialized(ctx)`)
- Modify: `internal/daemon/counters.go`(`forward.Counter`)
- Modify: `internal/daemon/dns.go`(`refreshOnce` 调用带 ctx)
- Modify: `internal/daemon/handlers_test.go`(`fakeDataplane`、`newTestServer`、`countersFn` 类型)
- Modify: `internal/daemon/daemon_test.go`(`Dataplane:` 注入、nil 检查)

- [ ] **Step 1: 定义消费侧接口 + 删 applier.go**

创建 `internal/daemon/dataplane.go`:

```go
package daemon

import (
	"context"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
)

// Dataplane is the data-plane seam the daemon depends on. Production wires
// *forward.Dataplane; tests substitute a fake. The daemon owns this
// (consumer-defined) interface so the dependency points daemon -> forward.
type Dataplane interface {
	Reconcile(ctx context.Context, rules []nft.Rule) error
	Counters() ([]forward.Counter, error)
	Close(ctx context.Context) error
}
```

Run: `git rm internal/daemon/applier.go internal/daemon/applier_test.go`

- [ ] **Step 2: handlers.go — 字段与串行化方法**

`Daemon` 结构体里 `applier Applier` 改为 `dp Dataplane`;`countersFn` 字段类型改为 `func() ([]forward.Counter, error)`(import `nft-forward/internal/forward`)。

`applySerialized` / `cleanupSerialized` 改写为:

```go
// applySerialized runs dp.Reconcile under applierMu so the DNS refresh loop and
// the unix-socket / dialer write paths never touch the data plane concurrently.
func (d *Daemon) applySerialized(ctx context.Context, resolved []nft.Rule) error {
	d.applierMu.Lock()
	defer d.applierMu.Unlock()
	return d.dp.Reconcile(ctx, resolved)
}

// closeSerialized runs dp.Close under the same applierMu so a shutdown close
// can't overlap an in-flight refresh-loop reconcile.
func (d *Daemon) closeSerialized(ctx context.Context) error {
	d.applierMu.Lock()
	defer d.applierMu.Unlock()
	return d.dp.Close(ctx)
}
```

更新 `applySerialized` 的全部调用点加 `ctx` 实参(共 4 处;`Bootstrap` 在 Step 3 处理):
- `setOwnerRuleset`(handlers.go:211):`d.applySerialized(ctx, resolved)`(已有 `ctx` 形参)。
- `demoteToTui`(handlers.go:305):`d.applySerialized(ctx, resolved)`(已有 `ctx`)。
- `refreshOnce`(dns.go:65):`d.applySerialized(ctx, resolved)`(`refreshOnce(ctx ...)` 已有 `ctx`)。
- `Bootstrap`(daemon.go:136):见 Step 3。

- [ ] **Step 3: daemon.go — Config / New / Bootstrap / Run / probe**

`Config`:删除 `Applier Applier` 与 `CountersFn func() ([]nft.Counter, error)`,加 `Dataplane Dataplane`。

`New`:

```go
	iface := cfg.Iface
	if iface == "" {
		iface = tc.DefaultIface()
		if iface == "" {
			iface = "eth0"
		}
	}
	if cfg.Dataplane == nil {
		cfg.Dataplane = forward.New(forward.Config{Iface: iface})
	}
	// ... 其余默认值不变 ...
	return &Daemon{
		socketPath:  cfg.SocketPath,
		statePath:   cfg.StatePath,
		groupName:   cfg.GroupName,
		dp:          cfg.Dataplane,
		legacyPaths: cfg.LegacyPaths,
		iface:       iface,
		countersFn:  cfg.Dataplane.Counters,
		resolveFn:   defaultResolver(resolver.New()),
		connectURL:  cfg.ConnectURL,
		connectTok:  cfg.ConnectToken,
	}, nil
```

(import `nft-forward/internal/forward`;移除不再使用的 `defaultCounters` 引用。)

`Bootstrap`:`d.applySerialized(ctx, resolved)`(已有 `ctx`)。

`Run` 关停处:`d.cleanupSerialized()` → `d.closeSerialized(context.Background())`(Close 立即返回,无需截止期)。

`probeFirewallEnvironment`:把 `d.applier.(interface{ DetectedShims() []string })` 改为 `d.dp.(interface{ DetectedShims() []string })`,并把前面的 `if d.applier != nil` 改为 `if d.dp != nil`。

- [ ] **Step 4: counters.go — forward.Counter**

`handleCounters` 与 `defaultCounters`:

```go
package daemon

import (
	"encoding/json"
	"net/http"

	"nft-forward/internal/forward"
)

func (d *Daemon) handleCounters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counters, err := d.countersFn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if counters == nil {
		counters = []forward.Counter{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"counters": counters})
}
```

(删除 `defaultCounters`;计数现由 `dp.Counters` 提供,经 `New` 默认到 `countersFn`。)

- [ ] **Step 5: 迁移测试 — fakeDataplane**

`handlers_test.go`:把 `fakeApplier` 整体替换为 `fakeDataplane`(保留字段名 `nftCalls`/`cleanupCalls`/`err` 以最小化断言改动):

```go
type fakeDataplane struct {
	mu           sync.Mutex
	nftCalls     [][]nft.Rule // records each Reconcile's rule slice
	cleanupCalls int
	err          error // Reconcile error
}

func (f *fakeDataplane) Reconcile(ctx context.Context, rules []nft.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nftCalls = append(f.nftCalls, append([]nft.Rule(nil), rules...))
	return f.err
}
func (f *fakeDataplane) Counters() ([]forward.Counter, error) { return nil, nil }
func (f *fakeDataplane) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	return nil
}
```

- `newTestServer` 里 `applier: &fakeApplier{}` → `dp: &fakeDataplane{}`;`countersFn` 的 panic 默认函数签名改为 `func() ([]forward.Counter, error)`。
- 全局把 `&fakeApplier{` → `&fakeDataplane{`、`Applier:` → `Dataplane:`、`fakeApplier{err:` → `fakeDataplane{err:`。
- 原 `fakeApplier{tcErr: ...}` 的那条用例(tc 失败语义)改为 `fakeDataplane{err: fmt.Errorf("tc broke")}`——daemon 视角只关心 Reconcile 是否报错;nft-ok/tc-fail 的分层语义已在 `forward/kernel.go` 覆盖。
- `d.countersFn = func() ([]nft.Counter, error)` 的三处 → `func() ([]forward.Counter, error)`,内部构造 `forward.Counter{...}`。

`daemon_test.go`:
- 全局 `&fakeApplier{}` → `&fakeDataplane{}`、`Applier:` → `Dataplane:`。
- `fa.nftCalls` / `fa.cleanupCalls` 断言保持。
- `New(Config{})` 后的 `if d.applier == nil` 检查 → `if d.dp == nil { t.Fatal("dp nil after New(Config{})") }`。

新建 `internal/daemon/dataplane_test.go`(替代删掉的 applier_test.go 的接口断言):

```go
package daemon

import (
	"testing"

	"nft-forward/internal/forward"
)

func TestDataplane_Implementations(t *testing.T) {
	var _ Dataplane = (*fakeDataplane)(nil)
	var _ Dataplane = (*forward.Dataplane)(nil)
}
```

- [ ] **Step 6: 跑 daemon 全包测试**

Run: `go test ./internal/daemon/ -v`
Expected: PASS(全部迁移用例通过)。

- [ ] **Step 7: 全仓构建 + vet**

Run: `go build ./... && go vet ./...`
Expected: 无错误。

- [ ] **Step 8: 提交**

```bash
git add -A internal/daemon/
git commit -m "daemon: depend on the forward.Dataplane instead of the bare Applier

Replace the nft+tc+shim Applier seam with the Dataplane (Reconcile/Counters/
Close), threading context through the apply path and serving /v1/counters
from the merged kernel+userspace counters. Tests use a fakeDataplane."
```

---

## Task 7: 状态迁移 `state.json` v3 → v4(显式化 Mode)

**Files:**
- Modify: `internal/daemon/state.go`
- Test: `internal/daemon/state_test.go`

- [ ] **Step 1: 写失败测试**

追加到 `internal/daemon/state_test.go`:

```go
func TestLoadState_V3FillsKernelMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// v3 file: a rule with no "mode" field.
	v3 := `{"version":3,"owners":{"tui":[{"id":"a","proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]},"agent_meta":{}}`
	if err := os.WriteFile(path, []byte(v3), 0o600); err != nil {
		t.Fatal(err)
	}
	owners, _, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	r := owners["tui"][0]
	if r.Mode != nft.ModeKernel {
		t.Fatalf("v3 rule should be normalized to kernel, got %q", r.Mode)
	}
}

func TestSaveLoad_V4RoundTripUserspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := OwnerRuleset{"tui": {{ID: "a", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}}
	if err := SaveState(path, in, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	out, _, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if out["tui"][0].Mode != nft.ModeUserspace {
		t.Fatalf("userspace mode lost on round trip: %+v", out["tui"][0])
	}
}
```

(确保 `state_test.go` 已 import `nft-forward/internal/nft`、`os`、`path/filepath`。)

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run 'TestLoadState_V3FillsKernelMode|TestSaveLoad_V4RoundTripUserspace' -v`
Expected: FAIL(v3 现被当作 `stateSchemaVersion`==3 直接读,Mode 为空;或版本号已不匹配)。

- [ ] **Step 3: 实现 — 版本号 + v3 归一分支**

`internal/daemon/state.go`:
- `const stateSchemaVersion = 3` → `= 4`。
- 加 v3 旧布局类型(紧邻 `legacyV2File`):

```go
// legacyV3File is the pre-v4 layout: owner-segmented rules + agent_meta, but
// rules had no per-rule mode. Migration stamps Mode="kernel" on every rule so
// the on-disk data is explicit after upgrade. (The normalization is also done
// defensively at ingest via Rule.EffectiveMode, so wire pushes are covered.)
type legacyV3File struct {
	Version   int          `json:"version"`
	Owners    OwnerRuleset `json:"owners"`
	AgentMeta AgentMeta    `json:"agent_meta"`
}
```

- 在 `LoadState` 的 `switch probe.Version` 中,把原 `case 3` 的位置改为 `case stateSchemaVersion`(=4)读 `stateFile`;新增 `case 3` 分支:

```go
	case 3:
		var v3 legacyV3File
		if err := json.Unmarshal(b, &v3); err != nil {
			return nil, AgentMeta{}, fmt.Errorf("parse v3 state: %w", err)
		}
		if v3.Owners == nil {
			v3.Owners = OwnerRuleset{}
		}
		normalizeModes(v3.Owners)
		return v3.Owners, v3.AgentMeta, nil
```

- `default` 的报错信息更新为 `want 4, 3, 2, or 1`。
- 加归一辅助:

```go
// normalizeModes stamps the kernel default onto any rule whose mode is empty,
// making upgraded state explicit on disk.
func normalizeModes(owners OwnerRuleset) {
	for owner, rules := range owners {
		for i := range rules {
			rules[i].Mode = rules[i].EffectiveMode()
		}
		owners[owner] = rules
	}
}
```

- [ ] **Step 4: 跑测试确认通过 + 全包**

Run: `go test ./internal/daemon/ -v`
Expected: PASS(含既有 v1/v2 迁移用例)。

- [ ] **Step 5: 提交**

```bash
git add internal/daemon/state.go internal/daemon/state_test.go
git commit -m "daemon: bump state to v4, stamping kernel mode onto pre-mode rules

v3 files (rules without a mode field) load with mode explicitly normalized
to kernel; SaveState writes v4. Wire pushes are also normalized at ingest
via EffectiveMode, so this is purely about making on-disk data explicit."
```

---

## Task 8: 面板端到端(wsproto / DB / server / 模板)

**Files:**
- Modify: `internal/wsproto/messages.go`(`Forward.Mode`)
- Create: `internal/db/migrations/0005_forward_mode.sql`
- Modify: `internal/db/queries.go`(`Forward.Mode`、`forwardCols`、`scanForward`、`CreateForward`)
- Modify: `internal/server/hub.go`(register_local INSERT 加 mode)
- Modify: `internal/server/handlers_admin.go`(snapshot 导入加 `Mode`)
- Modify: `internal/server/server.go`(`buildRules` 透传 mode;admin `createForward` 读 mode)
- Modify: `internal/server/handlers_my.go`(tenant `createForward` 读 mode + 校验)
- Modify: `internal/server/templates/forwards.html`、`my_forwards.html`、`node_detail.html`
- Test: `internal/db/queries_test.go`、`internal/server/handlers_admin_test.go`

- [ ] **Step 1: wsproto.Forward 加 Mode**

`internal/wsproto/messages.go` 的 `Forward` 结构体加:

```go
	Mode string `json:"mode,omitempty"`
```

- [ ] **Step 2: DB 迁移 0005**

创建 `internal/db/migrations/0005_forward_mode.sql`:

```sql
-- Per-forward data plane selector: 'kernel' (nftables DNAT, default) or
-- 'userspace' (embedded TCP relay). The composite invariant
-- "userspace => proto='tcp'" is enforced at the HTTP handler + nft.Validate
-- (SQLite cannot ALTER TABLE ADD CONSTRAINT, and rebuilding the forwards
-- table for one CHECK is not worth it); panel proto is already only tcp/udp.
ALTER TABLE forwards ADD COLUMN mode TEXT NOT NULL DEFAULT 'kernel'
    CHECK(mode IN ('kernel','userspace'));
```

- [ ] **Step 3: 写失败测试(DB 往返 mode)**

追加到 `internal/db/queries_test.go`(复用其建库/建节点辅助;若有 `newTestDB`/`mustNode` 之类,沿用):

```go
func TestForward_ModeRoundTrip(t *testing.T) {
	d := newTestDB(t) // 沿用文件内既有辅助
	nodeID := mustCreateNode(t, d) // 沿用文件内既有辅助
	f := &Forward{NodeID: nodeID, Proto: "tcp", ListenPort: 8443, TargetIP: "10.0.0.1", TargetPort: 443, Mode: "userspace"}
	id, err := CreateForward(d, f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "userspace" {
		t.Fatalf("mode lost: %q", got.Mode)
	}
}
```

> 若该文件没有 `newTestDB`/`mustCreateNode`,改用文件内实际的等价辅助函数名。

- [ ] **Step 4: 跑测试确认失败**

Run: `go test ./internal/db/ -run TestForward_ModeRoundTrip -v`
Expected: FAIL(`Forward` 无 `Mode` 字段 / 列未读写)。

- [ ] **Step 5: 实现 — db.Forward + 列 + 读写**

`internal/db/queries.go`:
- `Forward` 结构体加 `Mode string`(放在 `Comment` 后)。
- `forwardCols` 末尾加 `,mode`:
  ```go
  const forwardCols = `id,node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,disabled,last_bytes,total_bytes,created_at,mode`
  ```
- `scanForward` 的 `r.Scan(...)` 末尾加 `&f.Mode`(在 `&f.CreatedAt` 之后)。
- `CreateForward` 的 INSERT 加 `mode` 列与值:
  ```go
  res, err := d.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode) VALUES (?,?,?,?,?,?,?,?,?,?)`,
      f.NodeID, f.TenantID, f.TunnelID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, now(), normalizeForwardMode(f.Mode))
  ```
- 加辅助(空值归一为 kernel,保证 NOT NULL CHECK 通过):
  ```go
  func normalizeForwardMode(m string) string {
      if m == "userspace" {
          return "userspace"
      }
      return "kernel"
  }
  ```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/db/ -v`
Expected: PASS。

- [ ] **Step 7: server 透传 — buildRules + register_local + snapshot import**

`internal/server/server.go` `buildRules` 里构造 `nft.Rule` 处加 `Mode: f.Mode`:

```go
		rule := nft.Rule{
			Proto:         f.Proto,
			SrcPort:       f.ListenPort,
			DestPort:      f.TargetPort,
			Comment:       f.Comment,
			BandwidthMbps: bw,
			Mode:          f.Mode,
		}
```

`internal/server/hub.go` `handleRegisterLocal` 的 INSERT 加 mode 列(从 `f.Mode`):

```go
		res, err := tx.Exec(
			`INSERT INTO forwards(node_id, tenant_id, tunnel_id, proto, listen_port, target_ip, target_port, comment, created_at, mode) VALUES (?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, time.Now().Unix(), normalizeRegisterMode(f.Mode))
```

并在 hub.go 加一个等价归一(或复用 db 的——但 hub 不应反向依赖私有函数,这里就地加):

```go
func normalizeRegisterMode(m string) string {
	if m == "userspace" {
		return "userspace"
	}
	return "kernel"
}
```

`internal/server/handlers_admin.go` snapshot 导入的 `db.Forward{...}` 加 `Mode: f.Mode`:

```go
		if _, err := db.CreateForward(s.DB, &db.Forward{
			NodeID:     nodeID,
			Proto:      f.Proto,
			ListenPort: f.ListenPort,
			TargetIP:   f.TargetIP,
			TargetPort: f.TargetPort,
			Comment:    f.Comment,
			Mode:       f.Mode,
		}); err != nil {
```

- [ ] **Step 8: server 表单 handler 读 mode + 校验**

`internal/server/server.go` admin `createForward`:在读 `comment` 后加 `mode := strings.TrimSpace(r.FormValue("mode"))`;给 `testRule` 与 `f` 都设 `Mode: mode`(`nft.Validate(testRule)` 会拦 `udp+userspace`):

```go
	mode := strings.TrimSpace(r.FormValue("mode"))
	f := &db.Forward{
		NodeID:     nodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
		Mode:       mode,
	}
	testRule := nft.Rule{
		Proto:    proto,
		SrcPort:  listenPort,
		DestPort: targetPort,
		Mode:     mode,
	}
```

`internal/server/handlers_my.go` `tenantCreateForward`:读 `mode := strings.TrimSpace(r.FormValue("mode"))`;在 `validateAgainstTunnel` 之后加显式校验并写 `f.Mode`:

```go
	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode == "userspace" && proto == "udp" {
		setFlash(w, "UDP 不支持用户态转发")
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	// ... 既有 f := &db.Forward{...} 处加: ...
	f := &db.Forward{
		NodeID:     tunnel.NodeID,
		Proto:      proto,
		ListenPort: listenPort,
		TargetIP:   targetIP,
		TargetPort: targetPort,
		Comment:    comment,
		Mode:       mode,
	}
```

- [ ] **Step 9: 模板 — 表单加 mode 选择 + 列表显示**

`internal/server/templates/forwards.html`:
- 表头(第 8 行)`<th>协议</th>` 后加 `<th>模式</th>`。
- 数据行对应位置加 `<td>{{.Mode}}</td>`(找到渲染 `.Proto` 的那一行,在其后加)。
- 新增表单(第 41 行 proto select 后)加:
  ```html
  <label>模式</label><select name="mode"><option value="kernel">内核态(零拷贝)</option><option value="userspace">用户态(TCP)</option></select>
  ```

`internal/server/templates/my_forwards.html`:
- 表格表头加「模式」列、数据行加 `{{.Mode}}`(对照该文件实际表头结构)。
- 第 44 行 proto select 后加同样的 mode select。
- (可选「好用」增强)在第 56-72 的内联 JS 里,当 mode=userspace 时禁用 udp 选项;若超出当前范围,留作 §10 之后的交互打磨,不阻塞本任务。

`internal/server/templates/node_detail.html`:
- 第 34 行表头 `<th>协议</th>` 后加 `<th>模式</th>`;数据行 `{{upper .Proto}}` 那行(第 39 行附近)后加 `<td>{{.Mode}}</td>`。

- [ ] **Step 10: server 层回归(核心校验已被单测覆盖)**

`udp+userspace` 的拒绝在两条路径上都已有更底层的测试兜底:
- admin `createForward` 调 `nft.Validate(testRule)`,该校验矩阵已由 **Task 1** 的
  `TestValidate_ModeMatrix` 直接覆盖;
- tenant `tenantCreateForward` 有显式 `if mode=="userspace" && proto=="udp"` 分支。

因此本步**不强制**新增 server HTTP 集成测试(server 测试脚手架较重,贸然臆造易引入
假代码)。要求:`go test ./internal/server/` **编译通过且既有用例零回归**。
若执行者熟悉该包既有的 `httptest`/会话脚手架,可按其真实 helper 补一条
「POST /forwards 带 udp+userspace → `ListForwards` 仍为 0」的用例(可选增强)。

- [ ] **Step 11: 全仓测试 + 构建**

Run: `go test ./... && go build ./...`
Expected: PASS。

- [ ] **Step 12: 提交**

```bash
git add -A internal/wsproto/ internal/db/ internal/server/
git commit -m "panel: thread forwarding mode end-to-end through the web panel

Add forwards.mode (migration 0005) and carry it through Forward CRUD, the
register_local import, the TUI-snapshot import, buildRules, and the admin +
tenant create forms. The userspace=>tcp invariant is enforced at the handler
and by nft.Validate."
```

---

## Task 9: TUI 模式选择器

**Files:**
- Modify: `internal/tui/tui.go`
- Test: `internal/tui/tui_test.go`

- [ ] **Step 1: 写失败测试(提交用户态规则携带 mode + 拒 udp+userspace)**

追加到 `internal/tui/tui_test.go`(沿用文件内的 fake `daemonClient` 与构造方式):

```go
func TestSubmitAdd_CarriesUserspaceMode(t *testing.T) {
	fc := &fakeClient{} // 沿用既有 fake
	m := initialModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 0          // tcp
	m.modeIdx = 1           // userspace
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(mm.rules) != 1 || mm.rules[0].Mode != nft.ModeUserspace {
		t.Fatalf("rule should carry userspace mode: %+v", mm.rules)
	}
}

func TestSubmitAdd_RejectsUDPUserspace(t *testing.T) {
	fc := &fakeClient{}
	m := initialModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 1 // udp
	m.modeIdx = 1  // userspace
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	if nm.(model).err == "" {
		t.Fatal("expected validation error for udp+userspace")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestSubmitAdd_CarriesUserspaceMode|TestSubmitAdd_RejectsUDPUserspace' -v`
Expected: 编译失败(`modeIdx`/`fMode` 未定义)。

- [ ] **Step 3: 实现 — 字段常量 + model 字段 + 选项**

`internal/tui/tui.go`:
- 字段常量块改为 6 项(在 `fComment` 后追加 `fMode`):
  ```go
  const (
      fProto    = 0
      fSrcPort  = 1
      fDestIP   = 2
      fDestPort = 3
      fComment  = 4
      fMode     = 5
  )
  ```
- 加 `var modeOptions = []string{nft.ModeKernel, nft.ModeUserspace}`(在 `protoOptions` 旁)。
- `model` 结构体加 `modeIdx int`(在 `protoIdx` 旁)。

- [ ] **Step 4: 实现 — buildInputs / enterAdd / enterEdit**

- `buildInputs` 在返回的 slice 末尾加一个占位 textinput(与 fProto 同样的占位用法),使索引 `fMode=5` 有效:
  ```go
  return []textinput.Model{
      protoPlaceholder,
      mk("监听端口 1-65535", 12),
      mk("目标 IPv4 或域名", 32),
      mk("目标端口", 12),
      mk("可选备注", 40),
      mk("", 0), // fMode 占位:由 modeIdx 选择器渲染
  }
  ```
- `enterAddMode` 末尾加 `m.modeIdx = 0`(默认 kernel)。
- `enterEditMode` 里,从 `r.Mode` 解析 `modeIdx`:
  ```go
  m.modeIdx = 0
  for i, md := range modeOptions {
      if md == r.EffectiveMode() {
          m.modeIdx = i
          break
      }
  }
  ```

- [ ] **Step 5: 实现 — updateAdd/updateEdit 处理 mode 选择器左右键**

在 `updateAdd` 与 `updateEdit` 里,把现有「`if m.focusedInput == fProto` 处理左右键」扩展为也处理 `fMode`:

```go
	if m.focusedInput == fProto {
		switch msg.String() {
		case "left", "h":
			m.protoIdx = (m.protoIdx - 1 + len(protoOptions)) % len(protoOptions)
		case "right", "l":
			m.protoIdx = (m.protoIdx + 1) % len(protoOptions)
		}
		return m, nil
	}
	if m.focusedInput == fMode {
		switch msg.String() {
		case "left", "h":
			m.modeIdx = (m.modeIdx - 1 + len(modeOptions)) % len(modeOptions)
		case "right", "l":
			m.modeIdx = (m.modeIdx + 1) % len(modeOptions)
		}
		return m, nil
	}
```

- [ ] **Step 6: 实现 — submitAdd/submitEdit 写 Mode**

在 `submitAdd` 与 `submitEdit` 构造 `nft.Rule` 处加 `Mode: modeOptions[m.modeIdx]`(`nft.Validate` 会拦 udp+userspace,沿用既有 `if err := nft.Validate(r); err != nil` 分支):

```go
	r := nft.Rule{
		ID:       nft.NewRuleID(), // submitEdit 用 m.rules[m.cursor].ID
		Proto:    proto,
		SrcPort:  srcPort,
		DestPort: destPort,
		Comment:  comment,
		Mode:     modeOptions[m.modeIdx],
	}
```

- [ ] **Step 7: 实现 — viewForm 渲染 mode 选择器 + 列表显示**

- `viewForm` 的 `labels` 加第 6 项 `"模式       "`;循环里 `if i == fProto` 渲染 proto selector,加 `else if i == fMode` 渲染 mode selector。新增 `renderModeSelector`(仿 `renderProtoSelector`,把 `protoOptions`/`m.protoIdx` 换成 `modeOptions`/`m.modeIdx`,`focused := m.focusedInput == fMode`)。
- `viewForm` 底部 help:当 `m.focusedInput == fProto || m.focusedInput == fMode` 时显示「← → 切换 • ...」。
- (列表显示)`viewList` 数据行可在协议列后追加模式标识——最简做法:在 proto 单元格文本上,userspace 规则前缀一个标记。例如把 `strings.ToLower(r.Proto)` 改为:
  ```go
  protoCell := strings.ToLower(r.Proto)
  if r.EffectiveMode() == nft.ModeUserspace {
      protoCell += " (U)"
  }
  ```
  并用 `protoCell` 渲染该列(列宽 `colProto` 已是 8,容得下 `tcp (U)`;若 `tcp+udp (U)` 超宽会被 `truncateCell` 截断,可接受;如需更宽留作交互打磨)。

- [ ] **Step 8: 跑 TUI 全包测试**

Run: `go test ./internal/tui/ -v`
Expected: PASS(含既有用例;若既有用例因 `fComment` 不再是末项而断言 focus 循环长度,需相应更新为 6)。

- [ ] **Step 9: 提交**

```bash
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "tui: add a mode selector to the add/edit form

A kernel/userspace pill selector mirroring the proto selector; submit carries
Rule.Mode and nft.Validate rejects udp+userspace. The list marks userspace
forwards with a (U) tag."
```

---

## Task 10: 整体回归与冒烟

**Files:** 无(验证 + 文档)

- [ ] **Step 1: 全仓测试 + 构建 + vet**

Run:
```bash
go test ./... && go vet ./... && go build ./...
```
Expected: 全 PASS,无 vet 警告,二进制构建成功。

- [ ] **Step 2: 竞态检测(中继并发路径)**

Run: `go test -race ./internal/forward/ -v`
Expected: PASS,无 data race(覆盖 accept/handle/reconcile/counters 并发)。

- [ ] **Step 3: 手动冒烟(需 root 的主机或本地 Linux)**

在一台 Linux 测试机:
1. 起 daemon;经 unix socket `POST /v1/ruleset/tui` 写一条
   `{"proto":"tcp","src_port":<高位空闲端口>,"dest_ip":"<可达IP>","dest_port":<端口>,"mode":"userspace"}`。
2. `ss -ltnp | grep <src_port>` 确认 daemon 在监听(内核态规则不会出现在 ss)。
3. 经该端口拉一段数据,`GET /v1/counters` 确认该端口有 `bytes` 增长。
4. 再写一条 `mode":"kernel"` 规则,确认 `nft list table ip nft_forward` 有其 DNAT、`ss` 无其监听。
5. 删除用户态规则,确认监听器消失、端口可重新绑定。

记录结果(通过/问题)。**不在生产链路或用户未授权的主机上做**(见项目网络测试范围约束)。

- [ ] **Step 4: 最终提交(若冒烟触发任何小修)**

```bash
git add -A
git commit -m "forward: finalize userspace mode after end-to-end smoke"
```

---

## 自检对照(spec 覆盖)

- 数据模型 `Rule.Mode`/`EffectiveMode`/校验矩阵 → Task 1。
- `forward.Partition`/`Counter` → Task 2。
- 用户态 relay(薄拷贝、半关、令牌桶、原子热更新、计数、直接关闭)→ Task 3。
- shim FORWARD+INPUT 泛化 → Task 4。
- kernel 后端 / firewall / Dataplane / 单一回滚 → Task 5。
- daemon `Applier`→`Dataplane`、计数统一、测试迁移 → Task 6。
- 状态 v3→v4 + 摄入归一 → Task 7(归一)+ Task 1(`EffectiveMode`)。
- 面板端到端(wsproto/DB/server/模板/四处写入点)→ Task 8。
- TUI mode 选择器 → Task 9。
- 回归/竞态/冒烟 → Task 10。
- 兼容性(旧 panel↔新 daemon、新 panel↔旧 daemon)→ 由 `EffectiveMode` 摄入归一(Task 1/7)+ `omitempty` 字段(Task 1/8)保证,Task 6 集成测试与 Task 10 回归覆盖。
