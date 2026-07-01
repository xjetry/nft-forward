# 节点 IPv6 出口校验修复 + 协议栈标签 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复创建规则时 IPv6 出口地址被通用格式错误拦截的 bug，并让节点的 v4/v6 协议栈可以被自动探测、上报，并在三处 UI 里以标签形式展示。

**Architecture:** (A) `parseExit` 增加裸 IPv6 格式提示，复用 `RegenerateRule` 里已有的节点能力校验。(B) agent 端用 UDP-dial 技巧探测自身出口 v4/v6 地址，通过 `wsproto.Hello` 上报；服务端按地址族分工填充 `relay_host`/`relay_host_v6`（`connectIP` 权威覆盖自己观测到的地址族，agent 自探只补另一个地址族）。(C) 协议栈标签直接由 `relay_host`/`relay_host_v6` 是否非空推导，组合节点新增三个只读计算字段（首跳 entry、尾跳 exit v6），前端在列表/详情/规则入口下拉框三处展示。

**Tech Stack:** Go（`internal/server`、`internal/db`、`internal/daemon`、`internal/wsproto`），React（`web/src`，Vite，无既有测试框架）。

## Global Constraints

- 不新增数据库列/migration —— 所有协议栈数据都从既有的 `relay_host`/`relay_host_v6` 推导，组合节点的三个新字段是纯内存计算（不进 `nodeCols`/`scanNode`/`grants.go`）。
- 自动探测/填充永远遵守「仅在字段当前为空时才写入」的门控 —— 绝不覆盖管理员手动设置的值。
- `wsproto.Hello` 新增字段必须 `omitempty`，服务端要对旧 agent（不带这两个字段）优雅回退。
- 提交信息、代码注释禁止出现任务编号、方案代号等过程性文字，只写 WHY 和不变量。
- 参考 spec：`docs/superpowers/specs/2026-07-02-node-ipv6-stack-design.md`

---

### Task 1: `parseExit` 裸 IPv6 格式提示

**Files:**
- Modify: `internal/server/shared.go:119-133`
- Test: `internal/server/shared_test.go` (new file)

**Interfaces:**
- Produces: `looksLikeBareIPv6(raw string) bool`（内部函数，仅本文件使用）；`parseExit` 签名不变（`func parseExit(raw string) (string, int, error)`）。

- [ ] **Step 1: 写失败测试**

创建 `internal/server/shared_test.go`：

```go
package server

import "testing"

func TestParseExitBareIPv6Hint(t *testing.T) {
	_, _, err := parseExit("2001:db8::1:1080")
	if err == nil {
		t.Fatal("expected error for bare IPv6 without brackets")
	}
	want := "IPv6 地址需要用方括号包裹，例如 [::1]:1080"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestParseExitBracketedIPv6Succeeds(t *testing.T) {
	host, port, err := parseExit("[2001:db8::1]:1080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "2001:db8::1" || port != 1080 {
		t.Fatalf("got host=%q port=%d, want host=2001:db8::1 port=1080", host, port)
	}
}

func TestParseExitGenericFormatError(t *testing.T) {
	_, _, err := parseExit("not-an-address")
	if err == nil {
		t.Fatal("expected error for malformed input")
	}
	want := "出口需为 host:port 形式"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestParseExitValidIPv4(t *testing.T) {
	host, port, err := parseExit("10.0.0.1:80")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "10.0.0.1" || port != 80 {
		t.Fatalf("got host=%q port=%d, want host=10.0.0.1 port=80", host, port)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run TestParseExit -v`
Expected: `TestParseExitBareIPv6Hint` FAIL（当前会拿到 `出口需为 host:port 形式`，不是方括号提示），其余三个测试 PASS（现有逻辑已经支持）。

- [ ] **Step 3: 实现 `looksLikeBareIPv6` 并修改 `parseExit`**

`internal/server/shared.go:119-133` 现状：

```go
func parseExit(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("出口需为 host:port 形式")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("出口端口非法")
	}
	if host == "" {
		return "", 0, fmt.Errorf("出口地址不能为空")
	}
	return host, port, nil
}
```

改为：

```go
func parseExit(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		if looksLikeBareIPv6(raw) {
			return "", 0, fmt.Errorf("IPv6 地址需要用方括号包裹，例如 [::1]:1080")
		}
		return "", 0, fmt.Errorf("出口需为 host:port 形式")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("出口端口非法")
	}
	if host == "" {
		return "", 0, fmt.Errorf("出口地址不能为空")
	}
	return host, port, nil
}

// looksLikeBareIPv6 reports whether raw is very likely an IPv6 literal
// missing the brackets host:port syntax requires: multiple colons with no
// leading '[' isn't ambiguous with any valid IPv4/hostname:port form.
func looksLikeBareIPv6(raw string) bool {
	return !strings.HasPrefix(raw, "[") && strings.Count(raw, ":") >= 2
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/server/... -run TestParseExit -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/shared.go internal/server/shared_test.go
git commit -m "fix: hint IPv6 exits need brackets instead of a generic format error"
```

---

### Task 2: `wsproto.Hello` 探测字段 + daemon 出口地址探测

**Files:**
- Modify: `internal/wsproto/messages.go:73-84`
- Modify: `internal/daemon/dialer.go:274-290`
- Test: `internal/daemon/dialer_test.go`（追加）

**Interfaces:**
- Produces: `wsproto.Hello.ProbedV4 string`、`wsproto.Hello.ProbedV6 string`（均 `json:"...,omitempty"`）；`probeOutboundIP(network, target string) string`；`probeOutboundIPs() (v4, v6 string)`（daemon 包内部函数，Task 3 不需要，Task 3 只消费 `hello.ProbedV4`/`hello.ProbedV6`）。

- [ ] **Step 1: 写失败测试**

在 `internal/daemon/dialer_test.go` 末尾追加：

```go
func TestProbeOutboundIP(t *testing.T) {
	got := probeOutboundIP("udp4", "127.0.0.1:9")
	if got != "127.0.0.1" {
		t.Fatalf("probeOutboundIP(udp4, loopback) = %q, want 127.0.0.1", got)
	}
	if got := probeOutboundIP("udp4", "not-a-valid-target"); got != "" {
		t.Fatalf("probeOutboundIP with malformed target = %q, want empty", got)
	}
}

func TestDialerHelloIncludesProbedV4(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(frames[0].Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ProbedV4 == "" {
		t.Error("expected ProbedV4 to be populated (host must have a default v4 route)")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/daemon/... -run 'TestProbeOutboundIP|TestDialerHelloIncludesProbedV4' -v`
Expected: 编译失败（`probeOutboundIP` 未定义，`wsproto.Hello` 无 `ProbedV4` 字段）

- [ ] **Step 3: 给 `wsproto.Hello` 加字段**

`internal/wsproto/messages.go:73-84` 现状：

```go
type Hello struct {
	NodeToken    string `json:"node_token"`
	AgentVersion string `json:"agent_version"`
	// AgentSHA is the sha256 of the running nft-agent binary — the identity the
	// panel compares against the agent it would push to decide whether a push is
	// needed at all. Empty from agents that predate the split.
	AgentSHA       string `json:"agent_sha,omitempty"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
	PortRange      string `json:"port_range,omitempty"`
}
```

改为：

```go
type Hello struct {
	NodeToken    string `json:"node_token"`
	AgentVersion string `json:"agent_version"`
	// AgentSHA is the sha256 of the running nft-agent binary — the identity the
	// panel compares against the agent it would push to decide whether a push is
	// needed at all. Empty from agents that predate the split.
	AgentSHA       string `json:"agent_sha,omitempty"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
	PortRange      string `json:"port_range,omitempty"`
	// ProbedV4/ProbedV6 are this agent's own best-guess outbound address per
	// family, re-probed fresh on every hello. The panel only uses these to
	// seed the family its own connection-observed address didn't cover —
	// see hub.go's fillNodeRelayHosts. Empty from agents that predate this probe.
	ProbedV4 string `json:"probed_v4,omitempty"`
	ProbedV6 string `json:"probed_v6,omitempty"`
}
```

- [ ] **Step 4: daemon 端加探测函数并接入 Hello 构造**

在 `internal/daemon/dialer.go` 的常量块（`dialerBackoffMax` 附近，约第 23-30 行）后加：

```go
const (
	probeV4Target = "8.8.8.8:80"
	probeV6Target = "[2001:4860:4860::8888]:80"
)
```

在文件末尾（`jitter` 函数后，`doProbe` 函数前或后均可，放在 `doProbe` 后）加：

```go
// probeOutboundIP dials target over the given UDP network ("udp4" or "udp6")
// and returns the local address the OS routing table picked for it. A UDP
// dial never sends a packet — this is a pure local route lookup, so it works
// even when target itself is unreachable or firewalled. Returns "" if the
// family has no usable route (e.g. no IPv6 connectivity at all).
func probeOutboundIP(network, target string) string {
	conn, err := net.DialTimeout(network, target, 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// probeOutboundIPs returns this host's best-guess v4/v6 outbound addresses.
// Re-probed fresh on every call (cheap — no packets sent) so a network change
// between reconnects is picked up without an agent restart.
func probeOutboundIPs() (v4, v6 string) {
	return probeOutboundIP("udp4", probeV4Target), probeOutboundIP("udp6", probeV6Target)
}
```

`internal/daemon/dialer.go:274-284`（`runOnce` 里构造 Hello 的地方）现状：

```go
	_, currentMeta := d.cfg.GetState()
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:      d.cfg.Token,
		AgentVersion:   d.cfg.AgentVersion,
		AgentSHA:       d.cfg.AgentSHA,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		LastAppliedRev: currentMeta.LastAppliedRev,
		PortRange:      d.cfg.PortRange,
	})
```

改为：

```go
	_, currentMeta := d.cfg.GetState()
	probedV4, probedV6 := probeOutboundIPs()
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:      d.cfg.Token,
		AgentVersion:   d.cfg.AgentVersion,
		AgentSHA:       d.cfg.AgentSHA,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		LastAppliedRev: currentMeta.LastAppliedRev,
		PortRange:      d.cfg.PortRange,
		ProbedV4:       probedV4,
		ProbedV6:       probedV6,
	})
```

`internal/daemon/dialer.go` 已经 import 了 `net` 和 `time`（`doProbe` 已在用），无需改动 import 块。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/daemon/... -run 'TestProbeOutboundIP|TestDialerHelloIncludesProbedV4' -v`
Expected: PASS

- [ ] **Step 6: 跑完整 daemon 包测试防止回归**

Run: `go test ./internal/daemon/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/wsproto/messages.go internal/daemon/dialer.go internal/daemon/dialer_test.go
git commit -m "feat: agent self-probes outbound v4/v6 addresses and reports them in hello"
```

---

### Task 3: hub.go 按地址族权威填充 relay_host / relay_host_v6

**Files:**
- Modify: `internal/server/hub.go:1-23`（import 块）、`internal/server/hub.go:148-159`
- Test: `internal/server/hub_test.go`（追加）

**Interfaces:**
- Consumes: `wsproto.Hello.ProbedV4`/`ProbedV6`（Task 2 产出）
- Produces: `fillNodeRelayHosts(d *sql.DB, node *db.Node, connectIP, probedV4, probedV6 string)`（hub.go 内部函数）

- [ ] **Step 1: 写失败测试**

在 `internal/server/hub_test.go` 末尾追加：

```go
func TestHubFillsRelayHostByFamily(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		ProbedV6: "2001:db8::1",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "127.0.0.1" {
		t.Errorf("RelayHost = %q, want 127.0.0.1 (from connectIP, this conn is v4)", got.RelayHost)
	}
	if got.RelayHostV6 != "2001:db8::1" {
		t.Errorf("RelayHostV6 = %q, want 2001:db8::1 (from agent self-probe, connectIP didn't cover v6)", got.RelayHostV6)
	}
}

func TestHubNeverOverwritesManualRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		ProbedV4: "198.51.100.1",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.9" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.9 (manual value must not be overwritten)", got.RelayHost)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run 'TestHubFillsRelayHostByFamily|TestHubNeverOverwritesManualRelayHost' -v`
Expected: `TestHubFillsRelayHostByFamily` FAIL（`RelayHostV6` 目前永远是空，因为 hub.go 从不写它）；`TestHubNeverOverwritesManualRelayHost` 应该已经 PASS（现有 `if node.RelayHost == ""` 门控已经生效）。

- [ ] **Step 3: hub.go 加 `net` import**

`internal/server/hub.go` 第 1-23 行现状：

```go
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)
```

改为（加一行 `"net"`）：

```go
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)
```

- [ ] **Step 4: 替换自动填充逻辑**

`internal/server/hub.go:148-154` 现状：

```go
	connectIP := extractIP(r)
	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, connectIP); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	if node.RelayHost == "" && connectIP != "" {
		_ = db.UpdateNodeRelayHost(h.DB, node.ID, connectIP)
	}
```

改为：

```go
	connectIP := extractIP(r)
	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, connectIP); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	fillNodeRelayHosts(h.DB, node, connectIP, hello.ProbedV4, hello.ProbedV6)
```

在 `extractIP` 函数（约第 394-409 行）后加新函数：

```go
// fillNodeRelayHosts seeds relay_host/relay_host_v6 for a node that hasn't
// had them set yet. connectIP (the address the panel observed this WS
// connection arrive from) is authoritative for whichever family it belongs
// to — it reflects the address as seen after any NAT, unlike a locally
// self-probed address. The agent's self-probed address only fills the
// OTHER family, the one this connection didn't use. Never overwrites a
// manually-configured value (only fires when the DB field is still empty).
func fillNodeRelayHosts(d *sql.DB, node *db.Node, connectIP, probedV4, probedV6 string) {
	connectIsV6 := false
	if ip := net.ParseIP(connectIP); ip != nil {
		connectIsV6 = ip.To4() == nil
	}
	if node.RelayHost == "" {
		if !connectIsV6 && connectIP != "" {
			_ = db.UpdateNodeRelayHost(d, node.ID, connectIP)
		} else if probedV4 != "" {
			_ = db.UpdateNodeRelayHost(d, node.ID, probedV4)
		}
	}
	if node.RelayHostV6 == "" {
		if connectIsV6 && connectIP != "" {
			_ = db.UpdateNodeRelayHostV6(d, node.ID, connectIP)
		} else if probedV6 != "" {
			_ = db.UpdateNodeRelayHostV6(d, node.ID, probedV6)
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/server/... -run 'TestHubFillsRelayHostByFamily|TestHubNeverOverwritesManualRelayHost' -v`
Expected: PASS

- [ ] **Step 6: 跑完整 server 包测试防止回归**

Run: `go test ./internal/server/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server/hub.go internal/server/hub_test.go
git commit -m "feat: fill node relay_host_v6 from agent self-probe, keep connectIP authoritative for its own family"
```

---

### Task 4: `db.Node` 协议栈只读字段 + `ResolveCompositeRelayStack`

**Files:**
- Modify: `internal/db/queries.go:37-64`（Node 结构体）、`internal/db/queries.go:351-365` 后追加新函数
- Test: `internal/db/relay_stack_test.go`（new file）

**Interfaces:**
- Produces: `Node.EntryRelayHost`、`Node.EntryRelayHostV6`、`Node.ExitRelayHostV6`（均 `string`, `json:"...,omitempty"`）；`ResolveCompositeRelayStack(d *sql.DB, nodes []*Node)`；`resolveCompositeRelayStack(nodes []*Node, hops []*NodeHop)`（未导出，纯函数，供单测直接调用）

- [ ] **Step 1: 写失败测试**

创建 `internal/db/relay_stack_test.go`：

```go
package db

import "testing"

func TestResolveCompositeRelayStack(t *testing.T) {
	mk := func(id int64, typ, v4, v6 string) *Node {
		return &Node{ID: id, NodeType: typ, RelayHost: v4, RelayHostV6: v6}
	}
	hop := func(comp, child int64) *NodeHop { return &NodeHop{NodeID: comp, HopNodeID: child} }

	tests := []struct {
		name        string
		nodes       []*Node
		hops        []*NodeHop
		compID      int64
		wantEntry   string
		wantEntryV6 string
		wantExitV6  string
	}{
		{
			name:      "single hop composite mirrors that node",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1"), mk(9, "composite", "", "")},
			hops:      []*NodeHop{hop(9, 1)},
			compID:    9,
			wantEntry: "10.0.0.1", wantEntryV6: "2001:db8::1", wantExitV6: "2001:db8::1",
		},
		{
			name: "multi-hop: entry from first, exit v6 from last",
			nodes: []*Node{
				mk(1, "remote", "10.0.0.1", ""),
				mk(2, "remote", "10.0.0.2", "2001:db8::2"),
				mk(9, "composite", "", ""),
			},
			hops:      []*NodeHop{hop(9, 1), hop(9, 2)},
			compID:    9,
			wantEntry: "10.0.0.1", wantEntryV6: "", wantExitV6: "2001:db8::2",
		},
		{
			name:      "composite with no hops stays empty",
			nodes:     []*Node{mk(9, "composite", "", "")},
			hops:      nil,
			compID:    9,
			wantEntry: "", wantEntryV6: "", wantExitV6: "",
		},
		{
			name:      "non-composite node is left untouched",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1")},
			hops:      nil,
			compID:    1,
			wantEntry: "", wantEntryV6: "", wantExitV6: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveCompositeRelayStack(tt.nodes, tt.hops)
			var got *Node
			for _, n := range tt.nodes {
				if n.ID == tt.compID {
					got = n
				}
			}
			if got.EntryRelayHost != tt.wantEntry {
				t.Errorf("EntryRelayHost = %q, want %q", got.EntryRelayHost, tt.wantEntry)
			}
			if got.EntryRelayHostV6 != tt.wantEntryV6 {
				t.Errorf("EntryRelayHostV6 = %q, want %q", got.EntryRelayHostV6, tt.wantEntryV6)
			}
			if got.ExitRelayHostV6 != tt.wantExitV6 {
				t.Errorf("ExitRelayHostV6 = %q, want %q", got.ExitRelayHostV6, tt.wantExitV6)
			}
		})
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/db/... -run TestResolveCompositeRelayStack -v`
Expected: 编译失败（`Node` 无 `EntryRelayHost` 等字段，`resolveCompositeRelayStack` 未定义）

- [ ] **Step 3: 给 `Node` 结构体加字段**

`internal/db/queries.go:37-64` 现状（结尾部分）：

```go
	LastUpgradeAt      sql.NullInt64 `json:"last_upgrade_at"`
	LastUpgradeVersion string        `json:"last_upgrade_version,omitempty"`
	LastUpgradeStatus  string        `json:"last_upgrade_status,omitempty"`
	LastUpgradeError   string        `json:"last_upgrade_error,omitempty"`
	RateMultiplier     float64       `json:"rate_multiplier"`
	Unidirectional     bool          `json:"unidirectional"`
}
```

改为：

```go
	LastUpgradeAt      sql.NullInt64 `json:"last_upgrade_at"`
	LastUpgradeVersion string        `json:"last_upgrade_version,omitempty"`
	LastUpgradeStatus  string        `json:"last_upgrade_status,omitempty"`
	LastUpgradeError   string        `json:"last_upgrade_error,omitempty"`
	RateMultiplier     float64       `json:"rate_multiplier"`
	Unidirectional     bool          `json:"unidirectional"`
	// EntryRelayHost/EntryRelayHostV6/ExitRelayHostV6 are not real columns —
	// ResolveCompositeRelayStack fills them in-memory for composite nodes only
	// (entry = first hop's own relay fields, exit = last hop's v6 relay field),
	// the same pattern RateMultiplier above uses for hop aggregation. Single/
	// self nodes leave them empty; callers fall back to the node's own
	// RelayHost/RelayHostV6 in that case.
	EntryRelayHost   string `json:"entry_relay_host,omitempty"`
	EntryRelayHostV6 string `json:"entry_relay_host_v6,omitempty"`
	ExitRelayHostV6  string `json:"exit_relay_host_v6,omitempty"`
}
```

- [ ] **Step 4: 加 `ResolveCompositeRelayStack`**

`internal/db/queries.go:349-365` 现状：

```go
// ResolveCompositeRateMultiplier sets each composite node's RateMultiplier to
// the sum of its hops' TrafficMultiplier values.
func ResolveCompositeRateMultiplier(d *sql.DB, nodes []*Node) {
	hops, err := ListAllNodeHops(d)
	if err != nil {
		return
	}
	sums := make(map[int64]float64)
	for _, h := range hops {
		sums[h.NodeID] += h.TrafficMultiplier
	}
	for _, n := range nodes {
		if n.NodeType == "composite" {
			n.RateMultiplier = sums[n.ID]
		}
	}
}
```

在这个函数后面（第 365 行 `}` 之后）追加：

```go

// ResolveCompositeRelayStack fills each composite node's EntryRelayHost/
// EntryRelayHostV6/ExitRelayHostV6 from its hop chain's first and last node —
// see the Node struct's doc comment for why these aren't real columns.
func ResolveCompositeRelayStack(d *sql.DB, nodes []*Node) {
	hops, err := ListAllNodeHops(d)
	if err != nil {
		return
	}
	resolveCompositeRelayStack(nodes, hops)
}

// resolveCompositeRelayStack is the pure aggregation, split out so tests
// don't need a DB. hops must already be ordered by (node_id, position) —
// ListAllNodeHops guarantees this — so chain[0]/chain[len-1] are the first
// and last hop of each composite.
func resolveCompositeRelayStack(nodes []*Node, hops []*NodeHop) {
	byID := make(map[int64]*Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	chains := make(map[int64][]*NodeHop)
	for _, h := range hops {
		chains[h.NodeID] = append(chains[h.NodeID], h)
	}
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		chain := chains[n.ID]
		if len(chain) == 0 {
			continue
		}
		if first := byID[chain[0].HopNodeID]; first != nil {
			n.EntryRelayHost = first.RelayHost
			n.EntryRelayHostV6 = first.RelayHostV6
		}
		if last := byID[chain[len(chain)-1].HopNodeID]; last != nil {
			n.ExitRelayHostV6 = last.RelayHostV6
		}
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/db/... -run TestResolveCompositeRelayStack -v`
Expected: PASS

- [ ] **Step 6: 跑完整 db 包测试防止回归**

Run: `go test ./internal/db/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/db/queries.go internal/db/relay_stack_test.go
git commit -m "feat: derive composite node entry/exit relay-host stack from its hop chain"
```

---

### Task 5: 接入 4 处调用点

**Files:**
- Modify: `internal/server/api.go:281-283`（`apiListNodes`）
- Modify: `internal/server/api.go:474-482`（`apiGetNode` composite 分支）
- Modify: `internal/server/api.go:1128-1130`（`apiListRules`，管理员）
- Modify: `internal/server/api.go:2053-2055`（`apiMyListRules`，用户）
- Test: `internal/server/relay_stack_wiring_test.go`（new file）

**Interfaces:**
- Consumes: `db.ResolveCompositeRelayStack`（Task 4 产出）

- [ ] **Step 1: 写失败测试**

创建 `internal/server/relay_stack_wiring_test.go`：

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestApiListNodesIncludesCompositeRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, n := range resp.Nodes {
		if int64(n["id"].(float64)) == comp.ID {
			found = n
		}
	}
	if found == nil {
		t.Fatal("composite node not found in /api/nodes response")
	}
	if found["entry_relay_host"] != "1.1.1.1" {
		t.Errorf("entry_relay_host = %v, want 1.1.1.1", found["entry_relay_host"])
	}
	if found["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", found["exit_relay_host_v6"])
	}
}

func TestApiGetNodeCompositeIncludesRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHostV6(d, a.ID, "2001:db8::1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	cookie := loginAsAdmin(t, d)
	s, _ := New(d)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/nodes/%d", comp.ID), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Node map[string]any `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Node["entry_relay_host_v6"] != "2001:db8::1" {
		t.Errorf("entry_relay_host_v6 = %v, want 2001:db8::1", resp.Node["entry_relay_host_v6"])
	}
	if resp.Node["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", resp.Node["exit_relay_host_v6"])
	}
}

func TestApiMyListRulesNodesIncludeCompositeRelayStack(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHostV6(d, b.ID, "2001:db8::2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my/rules", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, n := range resp.Nodes {
		if int64(n["id"].(float64)) == comp.ID {
			found = n
		}
	}
	if found == nil {
		t.Fatal("composite node not found in /api/my/rules nodes")
	}
	if found["exit_relay_host_v6"] != "2001:db8::2" {
		t.Errorf("exit_relay_host_v6 = %v, want 2001:db8::2", found["exit_relay_host_v6"])
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run 'TestApiListNodesIncludesCompositeRelayStack|TestApiGetNodeCompositeIncludesRelayStack|TestApiMyListRulesNodesIncludeCompositeRelayStack' -v`
Expected: 三个测试全部 FAIL（响应里没有这些字段）

- [ ] **Step 3: `apiListNodes` 接入**

`internal/server/api.go:275-283` 现状：

```go
func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.ResolveCompositeOnline(s.DB, nodes)
	db.ResolveCompositeRateMultiplier(s.DB, nodes)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
```

改为：

```go
func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.ResolveCompositeOnline(s.DB, nodes)
	db.ResolveCompositeRateMultiplier(s.DB, nodes)
	db.ResolveCompositeRelayStack(s.DB, nodes)
	panelURL, _ := db.GetSetting(s.DB, "panel_url")
```

- [ ] **Step 4: `apiGetNode` composite 分支接入**

`internal/server/api.go:474-481` 现状：

```go
		// Online is aggregated from children; reuse the same node list.
		db.ResolveCompositeOnline(s.DB, all)
		for _, c := range all {
			if c.ID == n.ID {
				n.Online = c.Online
				break
			}
		}
```

改为：

```go
		// Online is aggregated from children; reuse the same node list.
		db.ResolveCompositeOnline(s.DB, all)
		db.ResolveCompositeRelayStack(s.DB, all)
		for _, c := range all {
			if c.ID == n.ID {
				n.Online = c.Online
				n.EntryRelayHost = c.EntryRelayHost
				n.EntryRelayHostV6 = c.EntryRelayHostV6
				n.ExitRelayHostV6 = c.ExitRelayHostV6
				break
			}
		}
```

- [ ] **Step 5: `apiListRules`（管理员）接入**

`internal/server/api.go:1124-1130` 现状：

```go
		rules, _ = db.ListAllRules(s.DB)
	}
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeRateMultiplier(s.DB, nodes)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
```

改为：

```go
		rules, _ = db.ListAllRules(s.DB)
	}
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	db.ResolveCompositeRateMultiplier(s.DB, nodes)
	db.ResolveCompositeRelayStack(s.DB, nodes)
	nodeByID := buildMap(nodes, func(n *db.Node) int64 { return n.ID })
```

- [ ] **Step 6: `apiMyListRules`（用户）接入**

`internal/server/api.go:2048-2055` 现状：

```go
func (s *Server) apiMyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	db.FillRuleTraffic(s.DB, rules)
	idx := landingIndex(s.landingNodesFor(u, false))
	grantedNodes, _, _ := db.ListNodesForUser(s.DB, u.ID)
	db.ResolveCompositeRateMultiplier(s.DB, grantedNodes)
	grantedByID := buildMap(grantedNodes, func(n *db.Node) int64 { return n.ID })
```

改为：

```go
func (s *Server) apiMyListRules(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	rules, _ := db.ListRulesByUser(s.DB, u.ID)
	db.FillRuleTraffic(s.DB, rules)
	idx := landingIndex(s.landingNodesFor(u, false))
	grantedNodes, _, _ := db.ListNodesForUser(s.DB, u.ID)
	db.ResolveCompositeRateMultiplier(s.DB, grantedNodes)
	db.ResolveCompositeRelayStack(s.DB, grantedNodes)
	grantedByID := buildMap(grantedNodes, func(n *db.Node) int64 { return n.ID })
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go test ./internal/server/... -run 'TestApiListNodesIncludesCompositeRelayStack|TestApiGetNodeCompositeIncludesRelayStack|TestApiMyListRulesNodesIncludeCompositeRelayStack' -v`
Expected: PASS

- [ ] **Step 8: 跑完整 server 包测试防止回归**

Run: `go test ./internal/server/...`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/server/api.go internal/server/relay_stack_wiring_test.go
git commit -m "feat: expose composite node relay-host stack in node/rule list endpoints"
```

---

### Task 6: 前端 `NodeStackBadge` + `nodeStack` 派生函数

**Files:**
- Modify: `web/src/components/ui.jsx`（`NodeTypeBadge` 定义后，约第 66-67 行）

**Interfaces:**
- Consumes: node 对象的 `node_type`/`relay_host`/`relay_host_v6`/`entry_relay_host`/`entry_relay_host_v6`/`exit_relay_host_v6` 字段（Task 4/5 产出）
- Produces: `nodeStack(node) -> { entryV4, entryV6, exitV6 }`；`<NodeStackBadge node={node} />`（均从 `web/src/components/ui.jsx` 导出）

- [ ] **Step 1: 加派生函数与组件**

`web/src/components/ui.jsx` 里 `NodeTypeBadge` 定义（约第 62-66 行）：

```jsx
export function NodeTypeBadge({ type }) {
  if (type === 'composite') return <Badge color="violet">{nodeTypeIcon.composite}组合</Badge>
  if (type === 'self') return <Badge color="blue">{nodeTypeIcon.self}自身</Badge>
  return <Badge color="green">{nodeTypeIcon.single}单点</Badge>
}
```

后面加：

```jsx
/* ---------- NodeStackBadge ---------- */
// 组合节点的入口可达地址来自首跳，出口 IPv6 转发能力来自尾跳（与
// RegenerateRule 里 exitIsIPv6 校验最后一跳 relay_host_v6 的语义一致）；
// 单点节点入口出口就是它自己，两者天然相等。
export function nodeStack(node) {
  const entryV4 = node.node_type === 'composite' ? !!node.entry_relay_host : !!node.relay_host
  const entryV6 = node.node_type === 'composite' ? !!node.entry_relay_host_v6 : !!node.relay_host_v6
  const exitV6 = node.node_type === 'composite' ? !!node.exit_relay_host_v6 : !!node.relay_host_v6
  return { entryV4, entryV6, exitV6 }
}

export function NodeStackBadge({ node }) {
  const { entryV4, entryV6, exitV6 } = nodeStack(node)
  const entryParts = [entryV4 && 'v4', entryV6 && 'v6'].filter(Boolean)
  if (entryParts.length === 0 && exitV6 === entryV6) return null
  return (
    <span className="inline-flex items-center gap-1">
      {entryParts.length > 0 && <Badge color="gray">{entryParts.join('+')}</Badge>}
      {exitV6 !== entryV6 && <Badge color={exitV6 ? 'blue' : 'amber'}>出口{exitV6 ? '支持' : '不支持'} v6</Badge>}
    </span>
  )
}
```

- [ ] **Step 2: 构建确认无语法错误**

Run: `cd web && npm run build`
Expected: 构建成功，无报错

- [ ] **Step 3: Commit**

```bash
git add web/src/components/ui.jsx
git commit -m "feat: add node protocol-stack badge component"
```

---

### Task 7: 节点列表展示协议栈标签

**Files:**
- Modify: `web/src/pages/nodes/List.jsx:7`（import）、`web/src/pages/nodes/List.jsx:222`

**Interfaces:**
- Consumes: `NodeStackBadge`（Task 6 产出）

- [ ] **Step 1: import 组件**

`web/src/pages/nodes/List.jsx:7` 现状：

```jsx
import { Loading, Empty, Badge, Modal, Confirm, NodeTypeBadge, useConfirm, Select } from '../../components/ui'
```

改为：

```jsx
import { Loading, Empty, Badge, Modal, Confirm, NodeTypeBadge, NodeStackBadge, useConfirm, Select } from '../../components/ui'
```

- [ ] **Step 2: 在「类型」列旁展示标签**

`web/src/pages/nodes/List.jsx:222` 现状：

```jsx
                  <td><NodeTypeBadge type={n.node_type} /></td>
```

改为：

```jsx
                  <td>
                    <div className="flex items-center gap-1.5 flex-wrap">
                      <NodeTypeBadge type={n.node_type} />
                      <NodeStackBadge node={n} />
                    </div>
                  </td>
```

- [ ] **Step 3: 构建确认无语法错误**

Run: `cd web && npm run build`
Expected: 构建成功

- [ ] **Step 4: 手动验证**

Run: `cd web && npm run dev`，打开节点列表页，确认：
- 只有 `relay_host` 的单点节点显示 `v4` 标签
- 同时有 `relay_host`/`relay_host_v6` 的节点显示 `v4+v6` 标签
- 组合节点入口出口 v6 能力不同时，额外显示「出口支持/不支持 v6」标签

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/nodes/List.jsx
git commit -m "feat: show protocol-stack tag in node list"
```

---

### Task 8: 节点详情展示协议栈标签

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx:6`（import）、`web/src/pages/nodes/Detail.jsx:150`

**Interfaces:**
- Consumes: `NodeStackBadge`（Task 6 产出）

- [ ] **Step 1: import 组件**

`web/src/pages/nodes/Detail.jsx:6` 现状：

```jsx
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, NodeTypeBadge, useConfirm, Select } from '../../components/ui'
```

改为：

```jsx
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, NodeTypeBadge, NodeStackBadge, useConfirm, Select } from '../../components/ui'
```

- [ ] **Step 2: 在标题区展示标签**

`web/src/pages/nodes/Detail.jsx:148-150` 现状：

```jsx
              <div className="flex items-center gap-2.5 flex-wrap">
                <h1 className="m-0 text-[22px] font-bold tracking-[-0.01em]">{node.name}</h1>
                <NodeTypeBadge type={node.node_type} />
```

改为：

```jsx
              <div className="flex items-center gap-2.5 flex-wrap">
                <h1 className="m-0 text-[22px] font-bold tracking-[-0.01em]">{node.name}</h1>
                <NodeTypeBadge type={node.node_type} />
                <NodeStackBadge node={node} />
```

- [ ] **Step 3: 构建确认无语法错误**

Run: `cd web && npm run build`
Expected: 构建成功

- [ ] **Step 4: 手动验证**

Run: `cd web && npm run dev`，打开一个单点节点和一个组合节点的详情页，确认标题区正确显示协议栈标签，与列表页展示逻辑一致。

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/nodes/Detail.jsx
git commit -m "feat: show protocol-stack tag in node detail header"
```

---

### Task 9: 规则表单入口节点下拉框展示协议栈前缀

**Files:**
- Modify: `web/src/components/RuleFormModal.jsx:2`（import）、`web/src/components/RuleFormModal.jsx:65-73`

**Interfaces:**
- Consumes: `nodeStack`（Task 6 产出）

- [ ] **Step 1: import `nodeStack`**

`web/src/components/RuleFormModal.jsx:2` 现状：

```jsx
import { Modal, Select, ProbeButton } from './ui'
```

改为：

```jsx
import { Modal, Select, ProbeButton, nodeStack } from './ui'
```

- [ ] **Step 2: 在 label 前拼协议栈前缀**

`web/src/components/RuleFormModal.jsx:65-73` 现状：

```jsx
  const fmtRate = (n) => {
    if (showRate === false) return n.name
    const r = n.rate_multiplier ?? 1
    return r !== 1 ? `${n.name} (×${r})` : n.name
  }
  const groups = [
    { label: '单点', options: nodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
    { label: '组合', options: nodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
  ]
```

改为：

```jsx
  // Select 的 label 必须是纯字符串（既要参与搜索过滤的 .toLowerCase()，
  // 也不支持渲染 JSX），沿用 landingOptions 那种文本前缀写法标协议栈。
  const fmtStack = (n) => {
    const { entryV4, entryV6, exitV6 } = nodeStack(n)
    const parts = [entryV4 && 'v4', entryV6 && 'v6'].filter(Boolean)
    let tag = parts.join('+')
    if (exitV6 !== entryV6) tag += exitV6 ? ' 出口支持v6' : ' 出口不支持v6'
    return tag ? `[${tag}] ` : ''
  }
  const fmtRate = (n) => {
    const stack = fmtStack(n)
    if (showRate === false) return `${stack}${n.name}`
    const r = n.rate_multiplier ?? 1
    return r !== 1 ? `${stack}${n.name} (×${r})` : `${stack}${n.name}`
  }
  const groups = [
    { label: '单点', options: nodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
    { label: '组合', options: nodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
  ]
```

- [ ] **Step 3: 构建确认无语法错误**

Run: `cd web && npm run build`
Expected: 构建成功

- [ ] **Step 4: 手动验证**

Run: `cd web && npm run dev`，打开规则创建表单（管理员 `/rules` 与用户 `/my/rules` 两处都要看），确认「入口节点」下拉框里每个节点名前有 `[v4]`/`[v4+v6]`/`[v4 出口支持v6]` 之类的前缀，且搜索框按名称过滤仍然正常工作（前缀不干扰按节点名搜索，因为 `String(o.label).toLowerCase().includes(q)` 是子串匹配）。

- [ ] **Step 5: Commit**

```bash
git add web/src/components/RuleFormModal.jsx
git commit -m "feat: prefix rule entry-node dropdown options with protocol-stack tag"
```

---

## 计划自查

**Spec coverage：**
- A（parseExit 提示修复）→ Task 1 ✅
- B（自动探测与上报，含 connectIP/自探按地址族分工）→ Task 2（daemon 探测 + Hello 字段）、Task 3（hub.go 家族感知填充）✅
- C（协议栈标签数据模型 + 三处 UI）→ Task 4（Node 字段 + 解析函数）、Task 5（4 处调用点接入）、Task 6（NodeStackBadge 组件）、Task 7（列表）、Task 8（详情）、Task 9（规则表单下拉框）✅

**占位符扫描：** 每个 Step 都给了完整代码/命令，无 TBD。

**类型一致性：** `wsproto.Hello.ProbedV4/ProbedV6`（Task 2 定义）→ Task 3 `fillNodeRelayHosts` 直接消费 `hello.ProbedV4`/`hello.ProbedV6`，命名一致。`db.Node.EntryRelayHost/EntryRelayHostV6/ExitRelayHostV6`（Task 4 定义）→ Task 5 api.go 拷贝字段名一致，Task 6 `nodeStack` 读取的 JSON 字段名（`entry_relay_host` 等）与 Task 4 的 json tag 一致。`resolveCompositeRelayStack`（Task 4 未导出纯函数）只在 Task 4 自己的测试里直接调用，`ResolveCompositeRelayStack`（导出包装）在 Task 5 使用 —— 两者签名和调用方式在各自任务里没有混用错误。
