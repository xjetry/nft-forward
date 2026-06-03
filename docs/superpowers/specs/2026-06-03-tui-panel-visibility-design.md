# TUI 可见并受限编辑 panel 段、同步回 server — 设计

## 背景与目标

daemon 将转发规则按 owner 分段:`tui`(节点本地 TUI 管理)与 `panel`(server 下发,含链式转发)。TUI 当前只读写 `tui` 段,因此 server 下发的链式转发在 TUI 中不可见、不可管理。

目标:让 TUI 能**看到**所有 `panel` 段规则、**受限编辑**它们,并把编辑**同步回 server 落库**,使 server 成为编辑后的权威来源。

约束(用户确认):
- 按**串行**处理,不考虑 server 下发与 TUI 编辑的并发竞态。
- 编辑边界:**链式规则只能改安全字段(模式/带宽/备注),非链式规则全字段可改**。

## 现状与约束

1. **owner 分段**:`setOwnerRuleset(ctx, owner, rules, rev)` 是统一写路径(`internal/daemon/handlers.go:192`);写入后仅 `owner=="tui"` 触发 `tuiHook` 上报(`handlers.go:239`)。TUI 渲染只读 `owners["tui"]`(`internal/tui/tui.go:122`、`:518`)。

2. **panel 段覆盖机制**:server 每次 dispatch 从 `forwards` 表整段重建并下发(`ActiveForwardsForPush`,`internal/db/queries.go:403`),daemon 侧 `setOwnerRuleset("panel",...)` 整段替换 `owners["panel"]`。触发点:admin 对 forward/tunnel/chain 的 CRUD、租户配额、resync。**推论:只要编辑同步回 server 并写入 `forwards` 表,后续 dispatch 自然包含该编辑,不会覆盖。**

3. **链式端口耦合**:`RegenerateChain`(`internal/db/chains.go:390-410`)逐跳编排:`hop[i].target_port = hop[i+1].listen_port`,`hop[i].target_ip = hop[i+1].relay_host`,末跳指向 `chain.ExitHost:ExitPort`。**改任一跳的监听端口/目标都会破坏相邻跳的指向,故链式规则的端口/目标是只读骨架。**

4. **上报现状**:`tui` 段上报链路完整(`tui.go` → `setOwnerRuleset` → `tuiHook` → `dialer.NotifyTuiChanged` → `TypeTuiSegmentChanged` 帧 → `hub` → `UpsertTuiSnapshot`),但落库仅为显示快照,不回流 `forwards` 表。**不存在 node→server 的 panel 段编辑上报帧。**

5. **数据结构**:`nft.Rule`(`internal/nft/nft.go:25`)与 `wsproto.Forward`(`internal/wsproto/messages.go:46`)均不携带 chain 信息。

## 整体架构 · 三个关键决策

**① panel 规则携带 chain 元信息。** 给 `nft.Rule` 增加两个 `omitempty` 字段 `ChainID int64` / `ChainName string`,纯元信息,不参与数据平面(DNAT 渲染、userspace 转发、`MergedRuleset` 去重、DNS 解析均不读取)。server 在 dispatch 的 `forward → nft.Rule` 转换处填充(数据源 `forwards.chain_id` join `chains.name`)。TUI 据此区分链式/非链式、显示所属链路、决定编辑边界。

**② 新增 node→server 上报帧 `TypePanelSegmentEdit`。** daemon 写 `panel` 段后触发新的 `panelHook`(与 `tuiHook` 对称)→ `dialer.NotifyPanelEdited` → 发帧 → server `hub` 接收。

**③ 落库即权威。** server 收到编辑帧 → 校验 → 更新 `forwards` 表(链式额外更新 `chain_hops.mode`)。因 dispatch 本就从 `forwards` 表重建,落库后续下发自然带上编辑。

## 数据流

**可见:**
```
server dispatch (forward→nft.Rule 填充 ChainID/ChainName)
  → ApplyRuleset.Rules
  → daemon owners["panel"]
  → TUI GetRuleset() 读 tui ∪ panel
  → 渲染:本地段 / server 托管段(显示链路名);链式行端口/目标只读
```

**编辑同步:**
```
TUI 编辑(按 chain_id 限制可改字段)
  → daemonclient.PostRuleset("panel", rules)
  → setOwnerRuleset("panel", rules) 本地应用 + 持久化
  → panelHook → dialer.NotifyPanelEdited(forwards)
  → TypePanelSegmentEdit 帧
  → hub 接收 → 校验(链式仅安全字段) → 更新 forwards (+ chain_hops.mode)
  → 落库成权威
```

## 编辑边界(server 兜底校验,不只靠 TUI 前端)

| 规则类型 | 可改字段 | 只读字段 |
|---|---|---|
| 链式(chain_id≠空) | `mode`、带宽、备注 | 监听端口、目标 IP、目标端口 |
| 非链式(chain_id 空) | 全字段 | — |

server 收到编辑后,对链式 forward 必须**拒绝**端口/目标变化(即使 TUI 前端已限制,server 仍兜底校验,防止协议被绕过破坏链路)。

## 分阶段实现

> 实现约定:代码注释只解释 WHY 与不变量,不出现阶段编号等过程信息。

### 阶段一:可见

让 TUI 看到 panel 段并标注来源/链路。最小风险,直接解决"看不到"。

改动:
- `internal/nft/nft.go`:`Rule` 增 `ChainID`/`ChainName`(omitempty)。
- server dispatch 路径(`internal/server/server.go:149` 一带 + `ActiveForwardsForPush`):`forward → nft.Rule` 转换处填充 chain 元信息;查询 join `chains` 取名称。
- `internal/tui/tui.go`:`loadInitialRules`(:122)、刷新(:518)读 `tui` ∪ `panel`;列表渲染区分 owner(本地/server 托管)并显示 `ChainName`;链式行标记端口/目标只读。

验证:`nft.Rule` 新字段不改变 apply/merge/render 行为的测试;dispatch 填充 chain 元信息的测试;TUI 合并显示的渲染测试。

### 阶段二:非链式编辑同步

打通 node→server 的 panel 编辑上报,只处理 `chain_id` 空的 forwards。

改动:
- `internal/wsproto/messages.go`:加 `TypePanelSegmentEdit` 常量 + payload `PanelSegmentEdit{Forwards []Forward}`(与 `TuiSegmentChanged` 一致的整段快照,复用 `wsproto.Forward`)。server 按 `(node_id, proto, listen_port)` 定位 forwards 行、从 DB 读 `chain_id` 判定链式与否,帧上无需携带 chain_id。
- `internal/daemon/handlers.go`:`setOwnerRuleset` 对 `owner=="panel"` 触发 `panelHook`(与 tuiHook 对称,锁外调用)。
- `internal/daemon/daemon.go`:接线 `panelHook` → `dialer.NotifyPanelEdited`。
- `internal/daemon/dialer.go`:`NotifyPanelEdited` + 发 `TypePanelSegmentEdit`(参照 `NotifyTuiChanged`/`TypeTuiSegmentChanged`)。
- `internal/tui/tui.go`:非链式 panel 规则编辑 UI → `PostRuleset("panel", ...)`。
- `internal/server/hub.go`:`readerLoop` 加 `TypePanelSegmentEdit` 分支 → 逐条按键 reconcile;本阶段只接受 `chain_id` 空的行更新,链式行忽略(留待下一阶段放开)。
- `internal/db`:`UpdateForward`(按 `node_id`+`proto`+`listen_port` 定位行)。

验证:端到端编辑同步(daemon PostRuleset panel → 帧 → hub → forwards 行更新);hub 对非链式编辑的落库测试。

### 阶段三:链式安全字段

在阶段二基础上放开链式规则的安全字段编辑。

改动:
- `internal/tui/tui.go`:链式规则编辑限定 `mode`/带宽/备注。
- `internal/server/hub.go`:校验扩展——链式编辑只接受安全字段,拒端口/目标变化。
- `internal/db`:更新 `chain_hops.mode` 及对应 forward 行;确认 `RegenerateChain` 从 `chain_hops.mode` 读取,使改动在重算后保留。

验证:链式编辑校验(拒端口、收 mode);`chain_hops.mode` 落库后 `RegenerateChain` 保留改动的测试。

## 测试策略

每阶段均以失败测试先行(TDD):
- 单元:数据结构不变性、db 更新函数、hub 校验分支。
- 集成:daemon↔server 经 WS 帧的编辑同步闭环;`httptest` + 真实 `s.tmpl` 渲染验证 TUI/webui 显示。
- 回归:`go test ./...` 全绿;`nft.Rule` 扩展不破坏既有 apply/merge/state 持久化测试。

## 非目标(YAGNI)

- 不处理并发竞态(串行假设)。
- 不支持 TUI 改链式规则的端口/目标(防断链;改链路仍走 server webui)。
- 不实现 node 重连时的 rev 短路自动补发(既有未实现项,与本需求无关)。
- 不改动既有 `tui` 段上报路径与语义。

## 关键文件索引

| 关注点 | 位置 |
|---|---|
| owner 写路径 / hook | `internal/daemon/handlers.go:192,239` |
| panel 段重建下发 | `internal/db/queries.go:403`、`internal/server/server.go:149` |
| 链式端口编排 | `internal/db/chains.go:390-410` |
| tui 上报范式(参照) | `internal/daemon/dialer.go`(NotifyTuiChanged)、`internal/server/hub.go:233` |
| 数据结构 | `internal/nft/nft.go:25`、`internal/wsproto/messages.go:46` |
| TUI 渲染/编辑 | `internal/tui/tui.go:122,518,531` |
