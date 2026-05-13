# DDNS / 域名目标转发实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 nft-forward 在 TUI、agent、server 三种形态下都能把 hostname/DDNS 域名当作转发目标，并在底层 IP 变化时自动重建 nftables 规则，无需人工介入。

**Architecture:**
- `nft.Rule` 新增可选 `DestHost` 字段持久化用户原始输入；`DestIP` 仍是 nft 规则真正下发的解析后 IPv4。
- 新增 `internal/resolver` 包封装 host 语法判定 + 带超时的 IPv4 解析（可注入 lookup 函数以便 TDD）。
- **由 agent 与 TUI 本地解析**：每个执行端各自跑一个 60s 周期的 resolver goroutine，IP 变化即重建规则——这样断网/重启场景下解析不依赖 server，最贴合 DDNS 弹性需求。
- Server 只放宽校验、把 `target_ip` 作为可选 hostname 透传给 agent；多租户路径下 tunnel 的 `target_cidr_allow` 仍强制 IP，避免租户绕过 CIDR 白名单。

**Tech Stack:** Go 1.26、`net.LookupHost`（stdlib，无新依赖）、modernc sqlite（仅放宽校验，新增列）、nftables。

---

## 文件结构

- **新增** `internal/resolver/resolver.go`：host 判定 + `Lookup(ctx, host) (string, error)` 单 IPv4 解析；包级 `LookupFunc` 变量便于注入。
- **新增** `internal/resolver/resolver_test.go`：单元测试 host 判定 + 注入桩 lookup 验证错误路径。
- **改** `internal/nft/nft.go`：`Rule` 加 `DestHost string`；`Validate` 区分「host-mode」与「ip-mode」；新增 `ResolveHosts(rules []Rule, lookup) ([]Rule, bool, error)` 用桩可测。
- **新增** `internal/nft/nft_test.go`：覆盖 Validate 两种模式、`ResolveHosts` 的 change 检测。
- **改** `internal/agent/agent.go`：`ApplyRules` 前先 ResolveHosts；新增 `dnsLoop` 后台 goroutine（启动一次）；`Bootstrap` 也要先解析；状态文件继续保存解析后的 `nft.Rule`（包含 DestHost + DestIP，重启时 DestIP 先用旧值兜底再触发解析）。
- **改** `internal/tui/tui.go`：输入校验放宽，允许 hostname；新增简单 polling goroutine（M0 不做也可）；列表展示 `DestHost (→ DestIP)`。
- **改** `internal/store/store.go`：保持兼容（`DestHost` 是 omitempty 字段，老配置仍可读）。
- **改** `internal/server/handlers_my.go`：`validateAgainstTunnel` 拆出对 IPv4-only 的硬要求（tenant 路径仍强制）；admin 路径放宽。
- **改** `internal/server/server.go`：admin 直建 forward 时允许 hostname。
- **改** `internal/server/pusher.go`：把 `f.TargetIP` 按「是 IP 还是 host」分发到 `DestIP` / `DestHost`。
- **新增** `internal/db/migrations/0003_target_host.sql`：放宽 `target_ip` 含义（不动 schema，仅注释 + 兼容查询）—— **决定**：不加列，让 `target_ip` 同时承载 IP 或 host；上层应用区分。零迁移最简。
- **改** `README.md`：「目标地址支持 hostname / DDNS 域名」段落。

---

## 设计决策（提前锁定）

1. **不加 DB 列**：`forwards.target_ip` 同时承载 IPv4 或 hostname。理由：避免迁移；server 不参与解析，只透传；上层 `nft.Rule` 字段已区分。
2. **agent 端解析**：解析逻辑只放在 agent / TUI（运行 nftables 的进程），server 不解析。理由：弹性（server 离线时 agent 仍能感知 DDNS 变化），且避免 server / agent 看到不同 DNS 视图。
3. **解析周期 60s**：硬编码 + 通过 env `NFT_FORWARD_DNS_INTERVAL=30s` 可调。理由：DDNS TTL 通常 1-5 分钟，60s 是合理默认。
4. **多租户安全**：当 tunnel `target_cidr_allow` 非空时，禁止 hostname（绕过 CIDR 白名单的攻击面）。Admin 直建 forward 不受此限。
5. **解析失败策略**：保留上一次成功的 IP；只在日志告警，不下线规则。这样上游 DNS 抽风不会中断现有连接。
6. **host 判定**：`net.ParseIP(s) == nil` 即视为 host，再走 RFC 1123-ish 字符白名单（字母数字、`-`、`.`）。

---

## Task 1: 新增 resolver 包

**Files:**
- Create: `internal/resolver/resolver.go`
- Test: `internal/resolver/resolver_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/resolver/resolver_test.go
package resolver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1.2.3.4", false},
		{"home.example.ddns.net", true},
		{"::1", false},
		{"localhost", true},
		{"bad host", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsHostname(c.in); got != c.want {
			t.Errorf("IsHostname(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLookupUsesInjectedFunc(t *testing.T) {
	called := 0
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			called++
			return []string{"10.0.0.1"}, nil
		},
		Timeout: time.Second,
	}
	ip, err := r.LookupIPv4(context.Background(), "x.example")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.1" {
		t.Fatalf("got %q", ip)
	}
	if called != 1 {
		t.Fatalf("called=%d", called)
	}
}

func TestLookupSkipsIPv6(t *testing.T) {
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"::1", "2001:db8::1", "192.0.2.5"}, nil
		},
	}
	ip, err := r.LookupIPv4(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.5" {
		t.Fatalf("got %q", ip)
	}
}

func TestLookupNoIPv4(t *testing.T) {
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"::1"}, nil
		},
	}
	_, err := r.LookupIPv4(context.Background(), "x")
	if !errors.Is(err, ErrNoIPv4) {
		t.Fatalf("err=%v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/resolver/...`
Expected: `undefined: IsHostname`, `undefined: Resolver` 等编译错误。

- [ ] **Step 3: 写最小实现**

```go
// internal/resolver/resolver.go
package resolver

import (
	"context"
	"errors"
	"net"
	"time"
)

var ErrNoIPv4 = errors.New("no IPv4 address for host")

// IsHostname returns true when s looks like a DNS name rather than a literal IP.
// Empty or syntactically invalid strings return false so callers reject them early.
func IsHostname(s string) bool {
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

// Resolver wraps net.LookupHost so tests can stub the network call.
type Resolver struct {
	Lookup  func(ctx context.Context, host string) ([]string, error)
	Timeout time.Duration
}

func New() *Resolver {
	return &Resolver{
		Lookup:  func(ctx context.Context, host string) ([]string, error) { return net.DefaultResolver.LookupHost(ctx, host) },
		Timeout: 3 * time.Second,
	}
}

// LookupIPv4 returns the first IPv4 address for host, ignoring IPv6 results.
// We return only IPv4 because nft-forward's nftables rules live in the `ip` family.
func (r *Resolver) LookupIPv4(ctx context.Context, host string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && ip.To4() != nil {
			return ip.To4().String(), nil
		}
	}
	return "", ErrNoIPv4
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/resolver/... -v`
Expected: 全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/resolver
git commit -m "feat(resolver): add IPv4 hostname resolver with injectable lookup"
```

---

## Task 2: 扩展 nft.Rule 与校验

**Files:**
- Modify: `internal/nft/nft.go:19-61`
- Test: `internal/nft/nft_test.go` (new)

- [ ] **Step 1: 写失败测试**

```go
// internal/nft/nft_test.go
package nft

import "testing"

func TestValidateAcceptsIPOnly(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateAcceptsHostOnly(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestHost: "home.example.net", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateRejectsNeither(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestPort: 80}
	if err := Validate(r); err == nil {
		t.Fatal("expected error when both DestIP and DestHost empty")
	}
}

func TestValidateRejectsBadHost(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestHost: "bad host name!", DestPort: 80}
	if err := Validate(r); err == nil {
		t.Fatal("expected error on invalid host")
	}
}

func TestRenderRulesetUsesDestIP(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp", SrcPort: 80, DestHost: "home.example.net",
		DestIP: "10.0.0.5", DestPort: 80,
	}})
	if !contains(out, "dnat to 10.0.0.5:80") {
		t.Fatalf("renderer must use DestIP, got:\n%s", out)
	}
	if contains(out, "home.example.net") {
		t.Fatalf("renderer leaked host into nft script:\n%s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/...`
Expected: FAIL — `DestHost` 字段不存在 / Validate 拒绝合法 host。

- [ ] **Step 3: 改 Rule 与 Validate**

```go
// internal/nft/nft.go (struct)
type Rule struct {
	ID            string `json:"id"`
	Proto         string `json:"proto"`
	SrcPort       int    `json:"src_port"`
	DestIP        string `json:"dest_ip,omitempty"`
	DestHost      string `json:"dest_host,omitempty"`
	DestPort      int    `json:"dest_port"`
	Comment       string `json:"comment,omitempty"`
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
}
```

```go
// internal/nft/nft.go (Validate)
func Validate(r Rule) error {
	switch r.Proto {
	case "tcp", "udp":
	default:
		return fmt.Errorf("协议必须为 tcp 或 udp")
	}
	if r.SrcPort < 1 || r.SrcPort > 65535 {
		return fmt.Errorf("监听端口必须在 1-65535 之间")
	}
	if r.DestPort < 1 || r.DestPort > 65535 {
		return fmt.Errorf("目标端口必须在 1-65535 之间")
	}
	hasHost := r.DestHost != ""
	hasIP := r.DestIP != ""
	if !hasHost && !hasIP {
		return fmt.Errorf("目标必须填 IPv4 或域名")
	}
	if hasIP {
		ip := net.ParseIP(r.DestIP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("目标 IP 必须为有效的 IPv4")
		}
	}
	if hasHost {
		if !resolver.IsHostname(r.DestHost) {
			return fmt.Errorf("目标域名格式非法")
		}
	}
	return nil
}
```

加 import `"nft-forward/internal/resolver"`。

`Display` 也调整：

```go
func (r Rule) Display() string {
	target := r.DestIP
	if r.DestHost != "" {
		if r.DestIP != "" {
			target = fmt.Sprintf("%s (→ %s)", r.DestHost, r.DestIP)
		} else {
			target = r.DestHost
		}
	}
	suffix := ""
	if r.Comment != "" {
		suffix = "  # " + r.Comment
	}
	return fmt.Sprintf("%s  %5d  →  %s:%d%s",
		strings.ToUpper(r.Proto), r.SrcPort, target, r.DestPort, suffix)
}
```

`RenderRuleset` 不改：`DestIP` 已经是 nft 真正用的字段；测试用例已验证。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/nft/... ./internal/resolver/... -v`
Expected: 全 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/nft
git commit -m "feat(nft): allow Rule to carry hostname alongside resolved IP"
```

---

## Task 3: ResolveHosts helper

**Files:**
- Modify: `internal/nft/nft.go` (append)
- Test: `internal/nft/nft_test.go` (append)

- [ ] **Step 1: 写失败测试**

追加到 `internal/nft/nft_test.go`：

```go
import (
	"context"
	"errors"
	"nft-forward/internal/resolver"
)

func TestResolveHostsFillsDestIP(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"203.0.113.7"}, nil
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed=true on first resolve")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("got %q", out[0].DestIP)
	}
}

func TestResolveHostsNoChangeWhenSame(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"203.0.113.7"}, nil
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestIP: "203.0.113.7", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected changed=false when IP unchanged")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("got %q", out[0].DestIP)
	}
}

func TestResolveHostsKeepsOldIPOnError(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return nil, errors.New("dns down")
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestIP: "203.0.113.7", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if changed {
		t.Fatal("expected changed=false on failure")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("stale IP should be preserved, got %q", out[0].DestIP)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/...`
Expected: `undefined: ResolveHosts`。

- [ ] **Step 3: 写实现**

追加到 `internal/nft/nft.go`：

```go
import (
	"context"
	"strings"

	"nft-forward/internal/resolver"
)

// ResolveHosts walks rules; for any rule with DestHost set it asks r to look
// up the IPv4 and writes it into a copy. Returns:
//   - out: the resolved rule slice (callers should use this in nft.Apply)
//   - changed: true when at least one DestIP differs from the input
//   - err: aggregated lookup failure (non-nil when at least one host failed to
//     resolve, but out still contains the best-effort state — failed entries
//     keep their previous DestIP so live traffic isn't torn down by DNS hiccups)
func ResolveHosts(ctx context.Context, rules []Rule, r *resolver.Resolver) ([]Rule, bool, error) {
	out := make([]Rule, len(rules))
	copy(out, rules)
	changed := false
	var errs []string
	for i := range out {
		if out[i].DestHost == "" {
			continue
		}
		ip, err := r.LookupIPv4(ctx, out[i].DestHost)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", out[i].DestHost, err))
			continue
		}
		if ip != out[i].DestIP {
			changed = true
			out[i].DestIP = ip
		}
	}
	if len(errs) > 0 {
		return out, changed, fmt.Errorf("dns: %s", strings.Join(errs, "; "))
	}
	return out, changed, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/nft/... -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/nft
git commit -m "feat(nft): add ResolveHosts to fill DestIP from DestHost"
```

---

## Task 4: agent 集成解析 + 后台 ticker

**Files:**
- Modify: `internal/agent/agent.go` 全文调整

- [ ] **Step 1: 改 Agent 结构与 ApplyRules**

在 `Agent` 加 resolver、stop chan：

```go
type Agent struct {
	cfg      Config
	mu       sync.Mutex
	rules    []nft.Rule
	resolver *resolver.Resolver
	stopDNS  chan struct{}
}

func New(cfg Config) *Agent {
	return &Agent{
		cfg:      cfg,
		resolver: resolver.New(),
		stopDNS:  make(chan struct{}),
	}
}
```

import 处加 `"context"` 和 `"nft-forward/internal/resolver"`。

`ApplyRules` 在校验后、`nft.Apply` 前插入解析：

```go
func (a *Agent) ApplyRules(rules []nft.Rule) error {
	for _, rl := range rules {
		if err := nft.Validate(rl); err != nil {
			return fmt.Errorf("invalid rule %s/%d: %w", rl.Proto, rl.SrcPort, err)
		}
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, a.resolver)
	if dnsErr != nil {
		log.Printf("warn: dns: %v", dnsErr)
	}
	// Reject only when a host-only rule has no usable IP after resolution; an
	// IP-only rule that never needed DNS must still apply cleanly.
	for _, rl := range resolved {
		if rl.DestIP == "" {
			return fmt.Errorf("rule %s/%d: 无法解析目标域名 %s", rl.Proto, rl.SrcPort, rl.DestHost)
		}
	}
	if err := nft.Apply(resolved); err != nil {
		return err
	}
	if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
		return fmt.Errorf("tc: %w", err)
	}
	if err := saveState(a.cfg.StatePath, resolved); err != nil {
		log.Printf("warn: saveState: %v", err)
	}
	a.mu.Lock()
	a.rules = resolved
	a.mu.Unlock()
	return nil
}
```

- [ ] **Step 2: 在 Bootstrap 中也走解析**

```go
func (a *Agent) Bootstrap() error {
	rules, err := loadState(a.cfg.StatePath)
	if err != nil {
		return err
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, a.resolver)
	if dnsErr != nil {
		log.Printf("warn: dns at bootstrap: %v", dnsErr)
	}
	a.mu.Lock()
	a.rules = resolved
	a.mu.Unlock()
	if err := nft.Apply(resolved); err != nil {
		return err
	}
	if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 3: 加 dnsLoop 后台 goroutine**

新增方法：

```go
// dnsLoop periodically re-resolves any DestHost-bearing rules. When a target
// IP moves (typical DDNS event), we rebuild the nftables ruleset in place so
// new flows hit the new backend without operator intervention.
func (a *Agent) dnsLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.stopDNS:
			return
		case <-t.C:
			a.mu.Lock()
			snap := append([]nft.Rule(nil), a.rules...)
			a.mu.Unlock()
			if !hasHost(snap) {
				continue
			}
			resolved, changed, err := nft.ResolveHosts(context.Background(), snap, a.resolver)
			if err != nil {
				log.Printf("dns refresh: %v", err)
			}
			if !changed {
				continue
			}
			if err := nft.Apply(resolved); err != nil {
				log.Printf("dns refresh apply: %v", err)
				continue
			}
			if err := tc.Apply(resolved, a.cfg.Iface); err != nil {
				log.Printf("dns refresh tc: %v", err)
			}
			a.mu.Lock()
			a.rules = resolved
			a.mu.Unlock()
			_ = saveState(a.cfg.StatePath, resolved)
			log.Printf("dns refresh: %d rule(s) re-applied", len(resolved))
		}
	}
}

func hasHost(rules []nft.Rule) bool {
	for _, r := range rules {
		if r.DestHost != "" {
			return true
		}
	}
	return false
}
```

在 `Serve` 开头启动 ticker（读取 env，缺省 60s）：

```go
func (a *Agent) Serve() error {
	go a.dnsLoop(dnsInterval())
	// ...existing mux setup...
}

func dnsInterval() time.Duration {
	if s := os.Getenv("NFT_FORWARD_DNS_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}
```

`os` 已在 import；如缺则补。

- [ ] **Step 4: build 检查**

Run: `go build ./...`
Expected: 编译通过。

- [ ] **Step 5: 提交**

```bash
git add internal/agent
git commit -m "feat(agent): resolve DestHost rules and re-apply on DNS change"
```

---

## Task 5: server 端放宽 + pusher 透传

**Files:**
- Modify: `internal/server/pusher.go:108-131`
- Modify: `internal/server/handlers_my.go:211-234`
- Modify: `internal/server/server.go:270-290` (admin create)

- [ ] **Step 1: pusher 区分 IP / host 透传**

```go
// pusher.go 内 rules 组装段
import "nft-forward/internal/resolver"
// ...
for _, f := range forwards {
	bw := 0
	if f.TunnelID.Valid { /* unchanged */ }
	rule := nft.Rule{
		Proto:         f.Proto,
		SrcPort:       f.ListenPort,
		DestPort:      f.TargetPort,
		Comment:       f.Comment,
		BandwidthMbps: bw,
	}
	if resolver.IsHostname(f.TargetIP) {
		rule.DestHost = f.TargetIP
	} else {
		rule.DestIP = f.TargetIP
	}
	rules = append(rules, rule)
}
```

- [ ] **Step 2: 多租户校验严格（tunnel 有 CIDR 限制 → 必须 IP）**

`validateAgainstTunnel` 改为：

```go
func validateAgainstTunnel(t *db.Tunnel, proto string, listenPort int, target string, targetPort int) error {
	// proto / listenPort / targetPort 校验保持
	// ...
	if target == "" {
		return errors.New("目标地址不能为空")
	}
	ip := net.ParseIP(target)
	if ip == nil {
		// hostname path
		if !resolver.IsHostname(target) {
			return errors.New("目标地址格式非法")
		}
		if strings.TrimSpace(t.TargetCIDRAllow) != "" {
			return errors.New("该通道限制了目标 CIDR，仅允许 IPv4 目标")
		}
		return nil
	}
	if ip.To4() == nil {
		return errors.New("目标地址必须为 IPv4")
	}
	if !targetIPInCIDR(ip, t.TargetCIDRAllow) {
		return fmt.Errorf("目标地址不在允许的 CIDR 内（%s）", t.TargetCIDRAllow)
	}
	return nil
}
```

补 `import "nft-forward/internal/resolver"` 与 `"strings"`（若未导入）。

- [ ] **Step 3: admin 直建 forward 放宽**

`internal/server/server.go:274-290` 附近创建 forward 的入口去掉 `net.ParseIP` 死校验，改为：

```go
if !resolver.IsHostname(targetIP) && net.ParseIP(targetIP) == nil {
	setFlash(w, "目标地址必须是 IPv4 或合法域名")
	http.Redirect(w, r, "/admin/forwards", http.StatusSeeOther)
	return
}
```

（具体路径以现有 admin 入口为准；用 grep 确认。）

- [ ] **Step 4: build 与全量 test**

Run: `go build ./... && go test ./...`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server
git commit -m "feat(server): accept hostname as forward target; keep tunnel CIDR strict"
```

---

## Task 6: TUI 输入放宽 + 后台 refresh

**Files:**
- Modify: `internal/tui/tui.go`

- [ ] **Step 1: 输入校验放宽**

`internal/tui/tui.go:182-200` 附近：现有把输入塞进 `DestIP`，改为：

```go
destInput := strings.TrimSpace(m.inputs[fDestIP].Value())
rule := nft.Rule{
	ID:       nft.NewRuleID(),
	Proto:    proto,
	SrcPort:  srcPort,
	DestPort: destPort,
	Comment:  comment,
}
if resolver.IsHostname(destInput) {
	rule.DestHost = destInput
} else {
	rule.DestIP = destInput
}
if err := nft.Validate(rule); err != nil { /* unchanged */ }
```

加 import `"nft-forward/internal/resolver"`。

- [ ] **Step 2: 在 apply 路径加解析**

TUI 现在直接 `nft.Apply(rules)`；改为：

```go
r := resolver.New()
resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, r)
if dnsErr != nil {
	m.status = "DNS 解析告警: " + dnsErr.Error()
}
if err := nft.Apply(resolved); err != nil { /* ... */ }
```

import `"context"`。

- [ ] **Step 3: 启动一个 60s polling tea.Cmd**

bubbletea 模式下，加一个 `tickMsg` 周期触发解析；如果 IP 变化则重新 Apply 并刷新视图。最小可工作版本即可，若实现复杂可降级为「TUI 模式下仅在写入时解析，不做后台轮询」并在 README 中标注「TUI 单机模式 DDNS 仅在保存时刷新；如需后台自动追踪，改用 server+agent 形态」。

> **决定**：M1 先做「保存时解析」，把后台 polling 留作 Task 9 后续改进。理由：TUI 用例场景多为一次配置后基本不变，agent 模式才是 DDNS 主战场。

- [ ] **Step 4: build**

Run: `go build ./...`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/tui
git commit -m "feat(tui): accept hostname target and resolve before apply"
```

---

## Task 7: 集成验证（手工）

**Files:** 无代码改动；产出验证脚本 `scripts/ddns-smoke.sh`（可选）。

- [ ] **Step 1: 构建三件套**

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-forward ./cmd/nft-forward
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-agent   ./cmd/nft-agent
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-server  ./cmd/nft-server
```

确认三个二进制均出。

- [ ] **Step 2: 静态分析**

```bash
go vet ./...
```

Expected: 无输出。

- [ ] **Step 3: 端到端冒烟（需 Linux + root + nft）**

在测试 Linux VM：

1. 启 agent，设置 `NFT_FORWARD_DNS_INTERVAL=5s` 跑前台。
2. 用 curl 向 `/v1/apply` POST 一条规则，`dest_host = "test.example"`，先把 `test.example` 在本地 `/etc/hosts` 指到 10.0.0.1。
3. `nft list ruleset` 应看到 `dnat to 10.0.0.1:N`。
4. 改 `/etc/hosts` 把 `test.example` 指到 10.0.0.2，等 ≤5s。
5. 再 `nft list ruleset`，应已更新到 10.0.0.2。
6. agent stderr 应有 `dns refresh: 1 rule(s) re-applied`。

无 Linux VM 时此步打 N/A 标记，由 reviewer 补做。

- [ ] **Step 4: 更新 README**

在 `README.md` 「目标地址 / 转发」相关段落（含 admin 创建 forward 描述）补一段：

```markdown
### 域名 / DDNS 目标

转发目标除 IPv4 外，也接受域名（如 `home.example.ddns.net`）。
- agent 会以 `NFT_FORWARD_DNS_INTERVAL`（默认 60s）的周期解析；
  IP 变化即重建 nftables 规则。
- 解析失败时保留上一次成功的 IP，避免短暂 DNS 抽风中断转发。
- 多租户场景下，若 tunnel 设置了 `target_cidr_allow`，
  仅允许 IPv4 目标（域名无法静态验证 CIDR 归属）。
```

- [ ] **Step 5: 提交 + 收尾**

```bash
git add README.md
git commit -m "docs: explain DDNS target behavior and DNS refresh interval"
```

---

## Self-Review

- ✅ **Spec coverage:** Task 1 给出 resolver；Task 2-3 给出 `Rule` + `ResolveHosts`；Task 4 给 agent；Task 5 给 server；Task 6 给 TUI；Task 7 验证 & 文档。所有 brainstorming 决策点（agent-side、60s、CIDR 严格、失败保留旧 IP）都映射到代码或 README。
- ✅ **Placeholder scan:** 无「TBD/TODO/类似于 Task N」。Task 6 显式降级 TUI 后台 polling 并写明原因。
- ✅ **Type consistency:** `Rule.DestHost`、`resolver.Resolver.LookupIPv4`、`nft.ResolveHosts` 三处签名一致；agent 与 TUI 使用同一接口。

---

**执行选项：**

1. **Subagent-Driven（推荐）** — 我每个 task 派一个新 subagent，task 之间做 review。
2. **Inline Execution** — 在当前会话直接逐 task 推进，到 checkpoint 停下确认。

你选哪种？
