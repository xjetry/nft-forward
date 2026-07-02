# 规则错误与节点状态解耦（rule-error ⇏ node-error）

日期：2026-07-02

## 背景与根因

面板上节点显示"错误"徽章的**唯一来源**是 `nodes.last_error` 非空
（`web/src/pages/nodes/List.jsx:394`、`web/src/pages/nodes/Detail.jsx:433`）。

`last_error` 的写入链路：

```
dispatchToNode (server.go:185)
  → Dispatcher.Dispatch(nodeID, rules, rev)
      · 远程节点：Hub.SendApplyRuleset → agent OnApply=SetPanelRuleset
      · self-node：unix socket /v1/apply → handleApplyRuleset → 同样 SetPanelRuleset
  → SetPanelRuleset → reconcileOwners
      · handlers.go:116  resolveFn(ResolveHosts) 聚合 DNS 失败 → 整份 apply 失败
      · handlers.go:119  requireResolvedHosts → 任一 DestHost 未解析 → 整份 apply 失败
  → agent 返回 ApplyAck{OK:false, Error:"dns: 4212: lookup 4212: no such host"}
  → SendApplyRuleset 返回 "apply rejected: ..." (hub.go:333)
  → MarkNodeDispatchError → nodes.last_error 被写上
  → 面板显示"错误"
```

即：**一条规则的目标解析不了，会把整个节点拖成"错误"，且该节点上其它正常规则也一并不下发。**

触发时机（"过一会自动变错误"）：节点初次连接/心跳不碰规则，先显示正常；等规则被推送、或 agent 每 60s
（`internal/daemon/dns.go:66`）的 DNS 重解析刷新触发一次事件驱动 dispatch 时，才失败并染红。

"4212" 之所以被当成域名：`resolver.IsHostname("4212")` 返回 `true` —— `net.ParseIP("4212")` 为 nil（非合法 IP），
且全为数字字符逐字符校验通过，于是被误判为域名去做 DNS 解析。纯数字串永远不可能是合法 DNS 名（TLD 不能全数字）。

已有事实：**bootstrap 路径本就是容错的**（`daemon.go:167-198`：记录 partial DNS failure、只下发能解析的、从不报错）。
本设计让面板下发路径与之对齐。

## 目标 / 非目标

**目标**

1. 单条规则的目标无法解析，**不再**把节点状态染成"错误"；节点上能解析的规则照常下发。
2. 坏规则以**节点级琥珀警告**呈现（非错误），避免静默失败。
3. **创建/编辑时校验**：明显写错的出口地址（如纯数字 `4212`）在源头被拒绝并给出清晰提示。

**非目标**

- 不做规则级（per-rule）的错误徽章 / DB 字段 / UI（超出本次范围，属过度设计）。
- 不改动转发数据面（nftables 渲染、relay）。
- 不追求 warning 的实时性（见"已接受的限制"）。

## 设计总览

三部分，彼此正交：

### A. reconcileOwners 统一容错（best-effort apply）

将解析失败从"整份拒绝"改为"尽力而为"：把解析结果切分为
`applyable`（字面 IP，或已解析出 IP 的域名）与 `unresolved`（有 DestHost 但 DestIP 为空），
**只下发 applyable**，把 `unresolved` 作为返回值上交调用方。

- 移除 reconcile 流水线里的硬失败：`handlers.go:116` 不再把 ResolveHosts 的聚合 DNS 错误当致命错误
  （ResolveHosts 在 lookup 失败时会保留该条 `out[i].DestIP` 原值，因此**之前解析成功的规则不会被拆除**，
  只有从未解析成功的 DestIP=="" 会被跳过——与 bootstrap 同义）；`handlers.go:119` 的 `requireResolvedHosts` 硬失败整段移除。
- `partitionResolved(rules) (applyable, unresolved []nft.Rule)` 抽成一个 helper，bootstrap（daemon.go）复用，消除重复。
- `requireResolvedHosts` 变为死代码 → 删除（连带其调用点与断言它的测试改写，见"测试"）。

**为什么统一容错、而不加 strict/tolerant 开关**：DNS 解析是"尽力而为"这一语义应对**所有**路径成立。
交互式创建的"别让我把地址写错"诉求，由下面 C 的**语法校验**在入口拦截，而不是靠 reconcile 的 DNS 硬失败——
后者会误伤"语法合法但此刻 DNS 暂时抖动"的正常域名规则（今天就有这个毛病）。切开这两个关注点后 reconcile 更简单、行为更一致。

reconcileOwners 全部调用者及其新语义：

| 调用者 | 路径 | 变化 |
|---|---|---|
| `SetPanelRuleset` (daemon.go:340) | 远程 WS + self-node 全量下发 | 容错；把 unresolved 汇成 warning 上交（见 B） |
| `refreshOnce` (dns.go:44) | 每 60s DNS 刷新 | 容错；agent 本地，warning 不回传（见限制） |
| `clearTuiSegment` (daemon.go:367) | 迁移后清理 tui 段 | 容错（删段不该被无关未解析规则阻塞） |
| `handleCreateRule` tui 分支 (handlers.go:333) | 独立模式本地建规则 | 容错；语法校验前置到入口（C） |
| `handleUpdateRule` tui 分支 (handlers.go:384) | 独立模式本地改规则 | 容错；语法校验前置到入口（C） |
| `handleDeleteRule` tui 分支 (handlers.go:452) | 独立模式本地删规则 | 容错 |

### B. 节点级琥珀警告（last_warning）

新增 `nodes.last_warning`（`TEXT NOT NULL DEFAULT ''`）。它是"最近一次成功下发时被跳过的规则"的快照。

warning 沿下发链回传：

1. **wsproto**：`ApplyAck` 增加 `Warning string \`json:"warning,omitempty"\``。
2. **agent**：`SetPanelRuleset` 从 `unresolved` 生成可读摘要（列监听端口 + 目标 host，截断上限），
   返回 `(warning string, err error)`。
3. **dialer**：`OnApply` 签名改为 `func(ctx, rev, rules) (warning string, err error)`；
   构造 `ApplyAck{OK: err==nil, Error: errMsg, Warning: warning}`（dialer.go:408）。
4. **self-node HTTP 对等**：`handleApplyRuleset` 响应体带 `warning`；`daemonclient.ApplyRuleset`
   改为返回 `(warning string, err error)`；`Dispatcher.Dispatch` 与 `sendLocalDefault` 改为返回 `(string, error)`。
5. **server**：`Hub.SendApplyRuleset` 改为返回 `(warning string, err error)`（ack.OK 时返回 `ack.Warning`）；
   `dispatchToNode` 捕获 warning 交给 `MarkNodeApplied(id, warning)`。
6. **前端**：`Detail.jsx` / `List.jsx` 在 `last_warning` 非空且无 `last_error` 时显示琥珀提示（非红），
   节点状态保持正常；`Detail.jsx` 基本信息区加一条琥珀说明行（列出无法解析的规则摘要）。

### C. 创建/编辑时校验（语法层）

新增 `resolver.PlausibleHostname(s string) bool`：`IsHostname(s)` 为真，且**去掉可能的尾点后最后一个标签不是全数字**。
它把 `4212`、`1.2.3.999` 这类"语法即错"的目标判为非法，同时放行正常域名。

入口校验（拒绝既非合法 IP、又非 plausible 域名的出口地址）：

- **面板**：`parseExit`（`shared.go:119`）。覆盖全部四个出口地址入口
  （`api.go:1205/1399/2142/2285`，即 admin/my 的 create/edit）。
  加：`if net.ParseIP(host) == nil && !resolver.PlausibleHostname(host) { return err("出口地址非法：… 不是合法 IP 或域名") }`。
- **独立模式本地**：`handleCreateRule` / `handleUpdateRule` 的 tui 分支，在 reconcile 前用同一 `PlausibleHostname` 校验
  `req.ExitHost`（仅当非合法 IP 时），不合法即 400 返回。

`buildRules`（server.go:303）里的 `IsHostname` **不改**：入口拦截后新数据不会有纯数字 host；
存量坏行（如现网这条 `4212`）由 A 的容错跳过 + B 的警告兜底。

## 状态语义（last_error / last_warning 交互）

- **UI 优先级**：`已禁用` > `错误`(红, last_error) > `警告`(琥珀, last_warning) > `在线·已同步/待同步` > `离线`。
  与 `Detail.jsx:431-439` 现有 disabled→error→online 顺序一致，警告插在 error 与 online 之间。
- `MarkNodeApplied(id, warning)`：stamp `last_apply_at`、清 `last_error`、置 `last_warning=warning`
  （warning 为空即清除警告）。→ 干净成功清两者；成功但有跳过则清 error、置 warning。
- `MarkNodeDispatchError(id, msg)`：置 `last_error=msg`，**并清 `last_warning`**（一次更新的失败尝试没有产生新的
  "已应用/已跳过"状态，旧 warning 不再可信；红色本就优先，避免红琥珀并存）。

## 已接受的限制

- **warning 时效性**：warning 是**下发时刻**的快照。若某条规则因**短暂** DNS 抖动被跳过，之后 60s 刷新循环
  重解析成功并本地下发，agent **不会**回传 server，`last_warning` 会滞留到下一次事件驱动 dispatch 才刷新。
  对永久性坏规则（如 `4212`）这是正确行为；对短暂抖动它会多显示一会儿。鉴于本项目"薄转发层、不过度设计"的取向，
  **接受并记录**，不为此加 agent→server 的额外回传通道。
- 独立（无面板）模式下被跳过的规则只在 agent 日志可见（无面板承载警告）；语法错误仍在入口被拒。

## 测试

- **改写** `internal/daemon/daemon_test.go:322/326/366`：原断言 `reconcileOwners` 在 `bad.example.com` 时返回
  `dns: … no such host`。改为断言容错契约：**应用可解析子集、返回 unresolved 列表、err==nil**。
- **新增** reconcile 测试：混合规则（1 可解析 + 1 不可解析）→ 只有可解析的进入数据面，unresolved 含另一条。
- **新增** `resolver.PlausibleHostname` 表驱动测试：`4212`/`1.2.3.999`/`""` → false；
  `a.com`/`x-1.example.org`/`sub.example.com.`（尾点）→ true；`1.2.3.4`（IP）→ false。
- **新增** `parseExit` 测试：`4212:80` → 错误；`1.2.3.4:80` / `ex.com:80` → OK。
- **新增** server dispatch 测试：agent 返回 ApplyAck{OK:true, Warning:"…"} → `dispatchToNode` 不写 last_error、
  写 last_warning；OK 无 warning → 两者皆清。

## 实现清单（含易漏点）

- [ ] `resolver.go`：`PlausibleHostname` + 测试。
- [ ] `shared.go` `parseExit`：出口地址语法校验。
- [ ] daemon：`partitionResolved` helper；`reconcileOwners` 统一容错并返回 `unresolved`；更新全部 6 个调用者签名；
      bootstrap 复用 helper；删除 `requireResolvedHosts`。
- [ ] `SetPanelRuleset` 生成 warning；`handleApplyRuleset` 响应带 warning；`daemonclient.ApplyRuleset` 返回 warning。
- [ ] dialer `OnApply` 签名 + `ApplyAck.Warning`（wsproto）。
- [ ] `Hub.SendApplyRuleset` / `Dispatcher.Dispatch` / `sendLocalDefault` 返回 `(warning, err)`。
- [ ] `dispatchToNode`：捕获 warning；`MarkNodeApplied(id, warning)`；`dispatchAfterMutation` 成功但有 warning 时写信息性 flash。
- [ ] **db 加列三处对齐（[[nodes-column-scan-lockstep]]）**：`Node.LastWarning string` 字段 +
      `nodeCols`（queries.go:255，紧跟 `last_error`）+ `scanNode`（queries.go:274 附近，紧跟 `&n.LastError`）+
      **`grants.go` 内联 scan（第三处，插在 node 列段内 `&n.LastError` 之后、grant 列之前，切勿放末尾）**。
- [ ] `MarkNodeApplied` 增 warning 参数；`MarkNodeDispatchError` 清 last_warning。
- [ ] 迁移 `0023_node_last_warning.sql`：`ALTER TABLE nodes ADD COLUMN last_warning TEXT NOT NULL DEFAULT ''`。
- [ ] 前端 `Detail.jsx`（HeaderStatus 增琥珀分支 + 基本信息琥珀说明行）、`List.jsx`（琥珀徽章）。
- [ ] 全量测试 + `go build`。
