# TUI 编辑/删除链式规则 — 设计

## 背景与目标

server 下发的链式（chain）转发现在在 TUI 中只读：链式行 `editable=false`，端口/目标/模式/备注都改不了，删除也只能在 server webui 做。本设计让 TUI 放开链式行的**受限编辑**与**删除整条链路**，并把操作同步回 server 落库、由 server 重新下发给链路涉及的所有 agent。

目标（用户确认）：

- 链式行放开编辑：**本地端口、模式（kernel/userspace）、备注**。
- **目标地址、目标端口、协议锁定只读**（防断链）。
- 改本地端口时，server 同步更新链路上下游（上游那一跳的目标端口、入口端点）。
- 删除链式行 → TUI 二次确认 → server 删除**整条链路** → 重新下发受影响节点。
- server 校验失败（端口越界/被占用）通过**结果回执**反馈给发起用户。

## 现状与约束

1. **链式行是 server 编排的骨架。** `RegenerateChain`（`internal/db/chains.go:292-424`）逐跳编排：`hop[i].target = hop[i+1].relay_host:listen_port`，末跳指向 `chain.ExitHost:ExitPort`，并把入口跳端口写回 `chains.entry_listen_port`。改任一跳的端口/目标都牵动相邻跳，故端口/目标必须由 server 全局重算，而非单 agent 本地改。

2. **快照机制表达不了链式端口编辑。** 现有非链式 panel 编辑走 `PanelSegmentEdit` 整段快照，server 按 `(node_id, proto, listen_port)` 定位行（`internal/server/hub.go` `applyPanelEdits`、`internal/db/queries.go` `UpdateForward`）。改 `listen_port` 会让定位键本身变化，server 查不到原行——这正是既有编辑边界锁 `listen_port` 的根因。链式端口编辑必须改用携带 `chain_id` 的命令。

3. **`(chain_id, node_id)` 唯一定位一跳。** `RegenerateChain` 拒绝同一节点在链路中重复（`chains.go:308-310`）。因此发起编辑的 agent 只需上报 `chain_id`，server 凭 WebSocket 连接的 node 身份即可精确定位那一跳，无需携带 position 等额外元信息。

4. **`RegenerateChain` 会覆盖单跳的自定义字段。** 它从 `HopInput`（node + mode + tunnel）重建，端口自己分配（保持范围内的现有端口、否则随机），`comment` 硬编码为 `链路 X · 第N跳`（`chains.go:404`）。所以"让用户指定的端口/备注存活"必须成为 `RegenerateChain` 读取或接受的状态，否则任何一次重算（含改端口自己触发的重算）都会冲掉它。

5. **node→server 帧目前都是 fire-and-forget。** dialer 进入 serve loop 后，`tui_segment_changed`/`panel_segment_edit`/`counters` 发出后都不等 ack（`internal/daemon/dialer.go:298-311`）。但 `hello`/`register_local` 是"发帧→同步等 ack"的先例（`dialer.go:199,221`）。同步回执需要在 serve loop 的 `readCh` switch 上加 ack 分支 + 按 `Envelope.ID` 配对的 pending 机制。

6. **server 已有"算出受影响节点 → 批量下发"机制。** webui 改链路走 `RegenerateChain` → `dispatchAfterFanout(affected)`（`internal/server/chains.go:163,253,267`）。本设计直接复用。

## 核心决策

**① 链式操作走命令式路径，与非链式的快照式并存。** 链式行的端口/目标是 server 编排的骨架，命令式（server 权威重算）比快照式（落库即权威）更贴合。发起编辑的 agent 本地**不直接改**，只把意图上报 server；server 重算后下发给链路涉及的所有 agent。

**② `(chain_id, 连接node)` 定位 + 命令帧不携带骨架字段。** `ChainHopEdit` 帧只带 `{ChainID, ListenPort, Mode, Comment}`——不带 target/proto。server 用现有 hops 重算得出 target、用 `chain.proto` 作协议，**协议层面就锁死了目标/协议**，比事后兜底校验更强。

**③ 用户自定义端口/备注成为 `RegenerateChain` 的输入与保持状态。** `HopInput` 加 `DesiredPort` 与 `Comment`，`chain_hops` 加 `comment` 列。优先级保证 TUI 改动在后续任何重算（含 webui 保存）后存活：
- 端口：`DesiredPort>0` → 采纳并校验范围+占用（非法报错）；`==0` → 沿用现有 `prevPort` 保持/随机逻辑。
- 备注：`Comment` 非空 → 用它；空 → 从现有 `chain_hops.comment` 保持；仍空 → 回退默认 `链路X·第N跳`。

现有调用方（webui `createChain`/`saveChain` 经 `adminHopInputs`）传零值 `DesiredPort=0`/`Comment=""`，行为不变；webui 重存链路不带 comment 时保持 TUI 改过的备注，不冲掉。

**④ 同步结果回执。** daemon 发命令帧后阻塞等 server 回执（带超时回退），把成功/失败原因作为 TUI 提交的同步结果，使端口冲突等失败对用户可见，而非静默。

## 数据流

**编辑：**
```
TUI 改链式行（端口/模式/备注；proto/target 锁定）
  → daemonclient.ChainEdit(chain_id, port, mode, comment)   [unix socket: POST /chain/edit]
  → daemon handler → dialer 发 ChainHopEdit{ChainID,ListenPort,Mode,Comment}，阻塞等回执
  → server hub: 凭连接 node + chain_id 定位跳
       → 校验端口（范围/占用）
       → 读完整 hops，改目标跳（DesiredPort/Mode/Comment）
       → RegenerateChain（联动上游 target_port + entry_listen_port）
       → dispatchAfterFanout(受影响节点)
       → 回 ChainCmdAck{OK:true, Entry}
  → daemon handler 收回执 → HTTP 200 → TUI 显示成功；各 agent 收到新 ruleset
  → TUI 刷新看到新端口
（校验失败：server 回 {OK:false, Error}，不下发，链路保持原样；TUI 显示失败原因）
```

**删除：**
```
TUI 链式行按 d → 二次确认（链路名 + 影响 N 个节点） → 确认
  → daemonclient.ChainDelete(chain_id)   [unix socket: POST /chain/delete]
  → daemon handler → dialer 发 ChainDelete{ChainID}，阻塞等回执
  → server hub: DeleteChain(chain_id) → dispatchAfterFanout(受影响节点) → 回 ChainCmdAck{OK:true}
  → 各 agent 收到新 ruleset（该链路行消失）
  → TUI 看到链路消失
```

## 协议层（`internal/wsproto`）

| 帧常量 | 方向 | payload |
|---|---|---|
| `TypeChainHopEdit` | node→server | `ChainHopEdit{ChainID int64, ListenPort int, Mode string, Comment string}` |
| `TypeChainDelete` | node→server | `ChainDelete{ChainID int64}` |
| `TypeChainCmdAck` | server→node | `ChainCmdAck{OK bool, Error string, Entry string}`；请求 ID 经 `Envelope.ID` 回带配对 |

## server 端

- **migration**：新增迁移给 `chain_hops` 加 `comment TEXT NOT NULL DEFAULT ''`。
- **`RegenerateChain` 扩展**（`internal/db/chains.go`）：
  - `HopInput` 加 `DesiredPort int` 与 `Comment string`。
  - 端口分配：`DesiredPort>0` 时校验 `[rangeLo,rangeHi]` 且未占用，违则返回明确错误；否则走现有保持/随机逻辑。
  - 写 `chain_hops` 时带 `comment`；`forwards.comment` 与之一致。comment 取值按决策③优先级。
- **hub 处理**（`internal/server/hub.go`）：
  - `readerLoop` 加 `TypeChainHopEdit`/`TypeChainDelete` 分支。
  - 编辑：`(连接 node, chain_id)` 定位跳 → 读 `ListChainHops` 组装 `HopInput`、改目标跳字段 → 在事务内 `RegenerateChain` → `dispatchAfterFanout` → 回 ack。
  - 删除：`DeleteChain` → `dispatchAfterFanout` → 回 ack。
  - 任一校验/DB 失败：回 `{OK:false, Error}`，不改库不下发。
- **不变量**：链式编辑的 target/proto 不可经此路径改动——命令帧不携带它们，server 始终用重算结果与 `chain.proto`。

## daemon 端

- **socket endpoint**（`internal/daemon/handlers.go`）：`POST /chain/edit`、`POST /chain/delete`。handler 调 dialer 同步命令、阻塞等 ack；超时回退提示"已提交，请刷新查看"；把结果作为 HTTP 响应。
- **dialer**（`internal/daemon/dialer.go`）：
  - 加 `cmdCh`（handler→serve loop 传待发帧）+ `pending map[string]chan chainAck` + mutex。
  - 生成请求 ID（复用现有 `time` 基的 ID 生成方式，避免随机源）。
  - serve loop `readCh` 加 `TypeChainCmdAck` 分支：按 `Envelope.ID` 唤醒对应 pending channel。
  - `runOnce` 返回（连接断）时清理 pending，让等待者拿到"连接断开"错误而非干等超时。
- **`daemonclient`**：加 `ChainEdit(chainID, port, mode, comment)` / `ChainDelete(chainID)`，同步返回 `{ok, error}`。

## TUI 端 —— 字段锁定矩阵

| 行类型 | 协议 | 本地端口 | 目标 | 模式 | 备注 | 删除 |
|---|---|---|---|---|---|---|
| tui 本地 | 可改 | 可改 | 可改 | 可改 | 可改 | 本地段删除 |
| panel 非链式 | 锁 | 锁 | 可改 | 可改 | 可改 | 不可（沿用现状） |
| panel 链式 | 锁 | **可改** | 锁 | **可改** | **可改** | **二次确认 → 删整链** |

- 链式行 `editable` 从"完全只读"放开为"受限可编辑"。编辑表单按行类型应用锁定：链式行锁 proto/target、放开 port/mode/comment（与非链式 panel 行锁 proto/port 的现有做法对称，方向相反）。
- 链式行编辑提交走 `daemonclient.ChainEdit`（命令，非 `PostRuleset` 快照）。成功后靠 server 下发刷新；失败显示 server 回执原因。提交后本地不乐观改写链式行（真相在 server）。
- 链式行按 `d` → 二次确认框，显示`链路名 + 影响 N 个节点`，确认后 `daemonclient.ChainDelete`。
- **入口跳端口一视同仁允许改**：position 0 的本地端口即链路对外入口，server 重算时同步更新 `entry_listen_port`；外部客户端需自行跟随。不特殊禁止（否则单跳链路完全不能改端口）。

## 编辑边界（server 兜底）

| 字段 | 链式行 | 机制 |
|---|---|---|
| 协议、目标 IP/端口 | 只读 | 命令帧不携带，server 用重算结果/`chain.proto`（协议层即锁） |
| 本地端口 | 可改 | `DesiredPort` 校验范围+占用，非法回执报错 |
| 模式、备注 | 可改 | 落 `chain_hops`/`forwards`，经决策③在重算后保持 |

## 测试策略（TDD，每步失败测试先行）

- **db**：`RegenerateChain` 的 `DesiredPort` 采纳/越界拒绝/占用拒绝；`Comment` 显式/保持/回退默认三态；入口跳改端口后 `entry_listen_port` 同步。
- **wsproto**：三个新帧 roundtrip + 常量。
- **server hub**：链式编辑联动上游 `target_port`；删除整链 + 受影响节点；校验失败不改库；回执内容。
- **daemon dialer**：命令帧发送 + ack 按 ID 配对唤醒；连接断清理 pending。
- **TUI**：字段锁定矩阵（链式行可进编辑但锁 proto/target）；链式删除二次确认走 `ChainDelete`；回执成功/失败显示。
- **回归**：`go test ./...` 全绿；`RegenerateChain` 扩展不破坏既有 webui 创建/保存链路路径。

## 非目标（YAGNI）

- 不处理并发竞态（串行假设，沿用既有设计）。
- 不放开非链式 panel 行的删除（不在本需求）。
- 不放开链式行的协议/目标编辑（防断链；改链路拓扑仍走 server webui）。
- 不处理跨编辑入口（TUI 与 webui 同时改同一链路）的一致性，串行下"落库即权威"。

## 关键文件索引

| 关注点 | 位置 |
|---|---|
| 链式端口/备注编排 | `internal/db/chains.go:292-424`（`RegenerateChain`）、`:404`（comment 硬编码） |
| 链路删除 | `internal/db/chains.go:228-250`（`DeleteChain`） |
| chain_hops schema | `internal/db/migrations/0006_relay_chains.sql:29-37` |
| webui 重算/下发范式 | `internal/server/chains.go:149,239,260` + `dispatchAfterFanout` |
| 非链式 panel 编辑（对照） | `internal/server/hub.go` `applyPanelEdits`、`internal/db/queries.go` `UpdateForward` |
| dialer ack 先例 / serve loop | `internal/daemon/dialer.go:199,221,255-314` |
| node→server fire-and-forget 范式 | `internal/daemon/dialer.go:298-311` |
| 协议帧 | `internal/wsproto/messages.go` |
| TUI 渲染/编辑/字段锁定 | `internal/tui/tui.go`（`rowAt`/`enterEditMode`/`submitEdit`/`updateConfirmDelete`/`viewForm`） |
