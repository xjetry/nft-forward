# 节点 IPv6 出口校验修复 + 协议栈标签

## 概述

创建规则时提交 IPv6 出口地址会被 `parseExit` 以「出口需为 host:port 形式」这个不准确的错误拦下，根本原因是不带方括号的裸 IPv6 字面量（如 `2001:db8::1:1080`）在 `net.SplitHostPort` 看来是歧义地址，解析直接失败——请求还没走到后面。而 `RegenerateRule` 里其实已经有正确的节点能力校验（`exitIsIPv6` 检查链路最后一跳的 `relay_host_v6`，报错是「节点 X 未设置 IPv6 中继地址」），只是从未被触发到。

在修复格式提示的同时，补上节点 v4/v6 协议栈的自动探测（agent 端主动探测出口地址并上报）与协议栈标签展示（节点列表 / 详情 / 规则入口下拉框）。

## 范围

**A.** `parseExit` 错误提示修复
**B.** 节点 `relay_host` / `relay_host_v6` 自动探测与上报
**C.** 协议栈标签：数据推导 + 三处 UI

**非目标**：不新增 nodes 表字段；不校验节点是否真的开启了 IPv6 forwarding（`nft.IPv6ForwardEnabled` 之类是既有机制，不在本次范围）；不改变「清空字段会在下次连接被重新填充」这一既有语义（v4 现状如此，v6 保持一致）。

---

## A. Bug 修复：`parseExit` 格式提示

文件：`internal/server/shared.go`

`net.SplitHostPort` 对带方括号的 IPv6（`[2001:db8::1]:1080`）解析完全正常，只有不带方括号的裸 v6 字面量会因为「too many colons」失败。当解析失败时，增加一个启发式判断：若原始输入不以 `[` 开头、且冒号数 ≥ 2，视为「大概率是漏加方括号的 v6 地址」，给出针对性提示；否则维持原有的通用格式错误。

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
	...
}

func looksLikeBareIPv6(raw string) bool {
	return !strings.HasPrefix(raw, "[") && strings.Count(raw, ":") >= 2
}
```

节点能力校验（「该节点不支持 IPv6」语义）完全复用 `internal/db/rules.go` 里 `RegenerateRule` 已有的 `exitIsIPv6` 检查，不改动。格式一旦能正确解析出 bracketed v6 地址，请求就会走到这段校验，得到准确的「节点 X 未设置 IPv6 中继地址，不能转发 IPv6 目标」提示。

---

## B. 节点 relay_host / relay_host_v6 自动探测与上报

### 现状

`internal/server/hub.go` 的 `ServeWS` 里，agent 连 WS 时面板从 HTTP 连接提取对方地址（`extractIP`，感知反代 XFF），若 `node.RelayHost` 为空就写入。这个机制只能覆盖单一地址族（取决于这次连接实际用的是 v4 还是 v6），且从未处理过 `relay_host_v6`。

`connectIP` 是「服务端外部观测到的、经过 NAT 之后」的地址，比本机自行判断出口地址更准（已用测试验证：本机走代理/VPN 环境下，本地路由探测拿到的是错误地址；`connectIP` 不受这个干扰）。

### 设计

**daemon 侧**（`internal/daemon/dialer.go`）：新增一个探测函数，用 UDP-dial 技巧向固定公网地址发起「连接」（UDP 不会真正发包，只是触发内核路由表选路），读出被选中的本地源地址：

```go
const (
	probeV4Target = "8.8.8.8:80"
	probeV6Target = "[2001:4860:4860::8888]:80"
)

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
```

在 `runOnce` 构造 Hello 前调用，v4/v6 各探测一次，每次连接都重新探测（不缓存）。v6 探测失败（拨号出错，说明这台机器没有 v6 出口路由）就留空字段。

**`wsproto.Hello`** 新增两个 `omitempty` 字段（旧 agent 不带，服务端要能优雅回退）：

```go
ProbedV4 string `json:"probed_v4,omitempty"`
ProbedV6 string `json:"probed_v6,omitempty"`
```

**服务端**（`hub.go`）：`connectIP` 对它自己观测到的那个地址族保持权威，agent 自探只用来补 `connectIP` 没覆盖到的那个地址族。两者都遵循「仅在字段当前为空时才写入」的门控，不会覆盖管理员手动设置的值：

```go
connectIP := extractIP(r)
connectIsV6 := false
if ip := net.ParseIP(connectIP); ip != nil {
	connectIsV6 = ip.To4() == nil
}

if node.RelayHost == "" {
	if !connectIsV6 && connectIP != "" {
		_ = db.UpdateNodeRelayHost(h.DB, node.ID, connectIP)
	} else if hello.ProbedV4 != "" {
		_ = db.UpdateNodeRelayHost(h.DB, node.ID, hello.ProbedV4)
	}
}
if node.RelayHostV6 == "" {
	if connectIsV6 && connectIP != "" {
		_ = db.UpdateNodeRelayHostV6(h.DB, node.ID, connectIP)
	} else if hello.ProbedV6 != "" {
		_ = db.UpdateNodeRelayHostV6(h.DB, node.ID, hello.ProbedV6)
	}
}
```

这样 v4 的既有准确性（NAT-aware 的外部观测）不退化，v6 靠自探补齐；双 IP 实例（如内网/国内地址 vs 探测出的出口/海外地址）如果管理员已手动填过，永远不会被自动探测覆盖。

---

## C. 协议栈标签

### 数据来源

不新增 nodes 表字段。单点/self 节点直接由既有 `relay_host` / `relay_host_v6` 是否非空推导：非空即视为支持该协议族。

组合节点（composite）自己的这两列通常是空的——链路的入口可达地址来自首跳，出口 v6 能力来自尾跳（`RegenerateRule` 里 `exitIsIPv6` 校验用的正是最后一跳的 `relay_host_v6`）。为组合节点新增三个只读、非持久化的 JSON 字段（`internal/db/queries.go` 的 `Node` 结构体，不进 `nodeCols`/`scanNode`，纯内存计算）：

```go
EntryRelayHost   string `json:"entry_relay_host,omitempty"`   // 首跳 relay_host
EntryRelayHostV6 string `json:"entry_relay_host_v6,omitempty"` // 首跳 relay_host_v6
ExitRelayHostV6  string `json:"exit_relay_host_v6,omitempty"`  // 尾跳 relay_host_v6
```

新增解析函数，与既有 `ResolveCompositeOnline` / `ResolveCompositeRateMultiplier`（`internal/db/queries.go`）同样的模式：批量查 `node_hops`，只对 `node_type == "composite"` 的行填充上述三个字段，单点节点保持空值（前端退回读自己的 `relay_host`/`relay_host_v6`）。

```go
func ResolveCompositeRelayStack(d *sql.DB, nodes []*Node) {
	hops, err := ListAllNodeHops(d)
	if err != nil {
		return
	}
	byNode := make(map[int64][]*NodeHop)
	for _, h := range hops {
		byNode[h.NodeID] = append(byNode[h.NodeID], h)
	}
	byID := make(map[int64]*Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, n := range nodes {
		if n.NodeType != "composite" {
			continue
		}
		chain := byNode[n.ID]
		if len(chain) == 0 {
			continue
		}
		// node_hops.position 已保证有序（PRIMARY KEY (node_id, position) 且查询按 position 排列）
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

**调用位置**（照抄 `ResolveCompositeRateMultiplier` 现有的调用点风格，逐一加上，避免漏加导致某个页面标签静默缺失）：

| 端点 | 文件:行 | 对应 UI |
|---|---|---|
| `apiListNodes` | `internal/server/api.go:281-282` | 节点列表 |
| `apiGetNode`（composite 分支） | `internal/server/api.go:453-482` | 节点详情 |
| `apiListRules`（admin） | `internal/server/api.go:~1129` | 规则表单入口下拉框（管理员侧） |
| `apiMyListRules`（user） | `internal/server/api.go:~2054` | 规则表单入口下拉框（用户侧） |

### 前端推导逻辑

```js
const entryV4 = node.node_type === 'composite' ? !!node.entry_relay_host : !!node.relay_host
const entryV6 = node.node_type === 'composite' ? !!node.entry_relay_host_v6 : !!node.relay_host_v6
const exitV6  = node.node_type === 'composite' ? !!node.exit_relay_host_v6  : !!node.relay_host_v6
```

单点节点 `entryV6 === exitV6`，标签只需展示一组；组合节点两者可能不同，标签需要分入口/出口两段展示。

### UI 三处

**1. 节点列表**（`web/src/pages/nodes/List.jsx`，「类型」列，紧邻 `NodeTypeBadge`）

新增 `NodeStackBadge` 组件（`web/src/components/ui.jsx`，复用现有 `Badge` 原语）：单点节点显示一组徽标（`v4`/`v6`，都支持则两个都显示，都不支持则不显示徽标）；组合节点入口出口不同时，拆成「入 v4」「出 v4 v6」两组短标签。

**2. 节点详情**（`web/src/pages/nodes/Detail.jsx`）

同一个 `NodeStackBadge`，放在节点信息展示区（与 relay_host / relay_host_v6 字段展示相邻）。

**3. 规则表单入口节点下拉框**（`web/src/components/RuleFormModal.jsx`）

`Select` 组件的 `label` 必须是纯字符串（既要参与搜索过滤的 `.toLowerCase()`，也不支持 JSX），沿用现有 `landingOptions` 里 `` `[${protocol}] ${name}` `` 的文本前缀写法：

- 单点节点：`[v4]`、`[v6]`、`[v4+v6]` 前缀
- 组合节点入口出口一致：同上
- 组合节点入口出口不同：`[入v4→出v4+v6]` 这种更明确的写法

对应 `groups` 构造那段（现有 `fmtRate` 函数旁边）扩展前缀拼接逻辑。

---

## 测试计划

- `parseExit`：裸 v6（无方括号）→ 方括号提示；bracketed v6 且节点不支持 v6 → 「节点 X 未设置 IPv6 中继地址」；bracketed v6 且节点支持 → 通过；真正的格式错误（如 `"not-an-address"`）→ 维持原提示。
- 自动探测：mock/单测覆盖 `probeOutboundIP` 对拨号失败的处理（v6 不可用时返回空）；`hub.go` 侧针对 connectIP 为 v4/v6 两种场景各测一遍「己方地址族权威、另一侧交给自探」的分支。
- `ResolveCompositeRelayStack`：单测覆盖单跳/多跳组合节点、子节点缺失 relay_host_v6 等边界。
- 前端：三处标签手动过一遍（单点 v4-only / v4+v6 节点，组合节点入口出口不同的场景）。
