# 中继链路编排（Relay Chain Orchestration）

> 本文的「链路 / chain」指**跨多个受管节点的中继转发链**（gomami → nnc-hk → nnc-tw → seednet），
> 与 `2026-05-23-daemon-forward-chain-design.md` 里的 netfilter **FORWARD chain** 是两个完全不同的概念，勿混淆。

## 背景

一条转发（`forwards` 表的一行）= 单台节点上的一跳：`监听端口 → 目标IP:端口`（`internal/db/migrations/0001_init.sql`）。
要搭一条多节点中继链，运营者必须在每台机上**分别**建一条 forward，并**手动对齐中间端口**
（上一跳的 `target_port` 必须等于下一跳的 `listen_port`），还要**手填每台下一跳的公网 IP**。

用户的真实生产场景（见 memory `vless-chain-topology-findings`）：
client → `gomami`(HK 入口) → `nnc-hk` → `nnc-tw` → `seednet`(vless reality 出口) 的 relay 链。
痛点是「要操作 3 台机器的 TUI / 配 3 条转发，容易填错端口、配置错误」。
同一调查还确认：**内核态多跳会让客户端崩到 ~2MB/s，userspace（split-TCP/PEP）才是多跳的解**
（v0.5.0 起 forward 支持 `mode=userspace`，`internal/db/migrations/0005_forward_mode.sql`）。

现有架构关键约束：
- daemon 是 **owner 分段** ruleset：`tui` 段（节点本地 TUI 规则）/ `panel` 段（面板下发）。
  跨段端口冲突在 apply 时被 daemon **拒绝（409）**（README「架构」）。
- 面板侧 `forwards` 表只装 panel 段；节点本地 tui 段通过 agent 上报、存在 `node_tui_snapshot`
  （`internal/server/hub.go:240` `UpsertTuiSnapshot`）。
- **租户的转发必须挂在已授权的通道（`tunnels`）里**——端口段 / CIDR 白名单 / 带宽 / 配额都靠 tunnel 约束
  （`internal/server/handlers_my.go:tenantCreateForward`）。租户无法在节点上建「自由转发」。
- 自动分配端口现有实现 `db.UsedPortsOnNode`（`internal/db/queries.go:409`）**只查 `forwards` 表**，
  **没算节点本地 tui 段**——这是一个现存隐患：面板看着空闲的端口可能撞上 tui 段，导致下发被 daemon 拒绝。

## 目标

在 webui 里把「中继链路」做成**一等持久对象**，由系统自动生成并对齐每一跳的 forward：

1. 运营者只需：选**有序的一串跳** + 填**出口**（任意 `host:port`），系统**自动生成入口**（可复制的 `ip:port`）。
2. **角色二元性**（由架构强制）：
   - **管理员**链路：积木是**节点**，端口在高位段自由分配，不计量。
   - **租户**链路：积木是**自己已授权的通道（tunnel）**（每个 tunnel 隐含一个节点），端口在该 tunnel 端口段内分配，
     沿用 tunnel 的 CIDR / 带宽 / 配额 / max_forwards 约束。
3. **逐跳** mode（kernel / userspace）可选；proto（tcp/udp）整条统一；udp 跳强制 kernel。
4. **完整的节点级端口占用检查**（`forwards` 表 ∪ 该节点 tui 快照）作为地基，手动建转发与链路自动分配共用；
   顺带修掉 `UsedPortsOnNode` 漏看 tui 段的现存隐患。
5. 改顺序 / 增删跳 → **重算并整条重下发**；离线节点容忍（记录落库，agent 重连补下发）；按节点显示同步状态。

## 非目标

- **不**把链路入口暴露成新的租户通道（admin 链路入口直接用；tenant 链路复用其已授权通道）。可后续扩展。
- **不**做逐跳混合 proto（proto 整条统一；只有 mode 逐跳）。
- **不**为出口做复用规范化表（出口就是每条链路自带的 `host:port`）。
- **不**保证 userspace 跳在节点 OS 级别绝对无端口冲突（面板看不到节点本机 socket，由 daemon 兜底报错）。
- 仅 IPv4（沿用现有限制）；面板单实例（无水平扩展）。

## 设计

### 名词与角色二元性

| 角色 | 链路积木 | 端口分配范围 | 约束 | 计量 |
|---|---|---|---|---|
| 管理员 | 受管**节点** | 高位段 `[20000,60000]` | 无 | 不计量（`tenant_id=NULL`） |
| 租户 | 自己已授权的**通道(tunnel)** | 该 tunnel 的 `port_start..port_end` | proto_mask / CIDR(仅出口) / 带宽 / max_forwards | 每跳独立计量 |

这不是一个可选项，而是被现有架构强制的：管理员可建自由 forward → 链路积木是节点；
租户只能在 granted tunnel 内建 forward → 链路积木是 tunnel。两种链路共用同一套**生成 + 对齐**逻辑，
仅「约束层 + 端口范围」按角色不同。

### 数据模型

`internal/db/migrations/0006_relay_chains.sql`：

```sql
-- 节点数据面可达地址：当该节点处于中继链路里时，上一跳 DNAT/relay 打过去的目标。
-- 空 = 从未进过链路；进链路前由 handler 校验必填。
-- 与 nodes.address(控制面，agent 反向拨入，无可靠数据面 host) 区分。
ALTER TABLE nodes ADD COLUMN relay_host TEXT NOT NULL DEFAULT '';

-- 一条中继链路 = 从自动分配的入口端点、经 N 个受管节点、到自由填写的出口的有序转发链。
-- tenant_id NULL => 管理员链路（不计量、自由分配端口）；非 NULL => 租户链路（跳落在其 granted tunnel 内）。
CREATE TABLE chains (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id         INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  proto             TEXT NOT NULL CHECK(proto IN ('tcp','udp')),
  exit_host         TEXT NOT NULL,
  exit_port         INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_node_id     INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_chains_tenant ON chains(tenant_id);

-- 每跳一行，按 position 排序（0 = 入口跳）。tunnel_id：admin 链路为 NULL，
-- 租户链路为该跳取端口/约束所依据的 granted tunnel。listen_port 为在 node_id 上分配的端口；
-- mode 为该跳的数据面选择。
CREATE TABLE chain_hops (
  chain_id    INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  tunnel_id   INTEGER REFERENCES tunnels(id) ON DELETE CASCADE,
  listen_port INTEGER NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  PRIMARY KEY (chain_id, position)
);
CREATE INDEX idx_chain_hops_node ON chain_hops(node_id);

-- 给自动生成的 forward 打标记，使链路能按 chain_id 整条重算/删除。一跳拥有恰好一条 forward。
ALTER TABLE forwards ADD COLUMN chain_id INTEGER REFERENCES chains(id) ON DELETE CASCADE;
```

说明：
- 生成的 forward 复用现有 `forwards` 表与下发/计量/计数器管线。租户链路的 forward 同时带 `tenant_id` + `tunnel_id`
  （现有 `buildRules` 已据 tunnel 盖带宽、计数器已按 tenant 累加），admin 链路两者为 NULL。
- `chains.entry_node_id / entry_listen_port` 是派生值，但**落库**以保证入口端点在改链路时稳定可复制。
- 删除链路：删 `chains` 行 → 级联删 `chain_hops` + 标记 `chain_id` 的 forwards；handler 删后对受影响节点下发以清内核规则。
- 删租户：`chains.tenant_id` 级联删链路 + 跳 + forwards；其 forward 带 `tenant_id`，现有
  `DistinctTenantNodes` 仍能在删除前收集到受影响节点做 fanout（`handlers_admin.go:deleteAdminTenant`）。

### 端口占用与自动分配（地基）

新增 `db.OccupiedPortsOnNode(nodeID, proto, excludeChainID)`：返回该 (node, proto) 上**完整**占用端口集 =

```
S = { listen_port : forwards WHERE node_id=? AND proto=? AND (chain_id IS NULL OR chain_id != excludeChainID) }
T = { f.listen_port : f ∈ parse(node_tui_snapshot[node].forwards_json), f.proto == proto }   -- 尽力而为
return S ∪ T
```

- `excludeChainID`：重算某条链路时排除它**自己**当前的 forward，使未变的跳能保留原端口（否则会把自己看成占用）。
  普通建转发传 0（不排除）。
- `T`（tui 快照）尽力而为：可能过时/缺失；daemon 的 409 仍是最终裁判。

复用现有 `pickFreePort(start, end, used)`（`handlers_my.go:269`，随机偏移避免并发同撞）。

**改造既有手动建转发**（顺带修隐患）：
- `tenantCreateForward`：把 `UsedPortsOnNode` 换成 `OccupiedPortsOnNode`（自动分配与显式填端口都先过完整占用检查，
  端口被占报「端口 X 已被占用（本地 TUI / 其他转发）」而非落库后下发神秘失败）。
- `createForward`（管理员手动）：新增同样的完整占用预检。

`UsedPortsOnNode` 由 `OccupiedPortsOnNode` 取代（保留旧函数名做薄封装或直接替换调用点，二选一在实现计划定）。

### 生成与对齐算法

`internal/server/chains.go`（新文件）核心：`func regenerateChain(tx, chain) (entry string, affectedNodes []int64, err error)`

```
输入：chain（含 proto、exit_host/port、tenant_id）、有序 hops[0..k-1]（node_id、tunnel_id?、mode）
前置校验：
  - k >= 1；同一 node 不得在一条链路出现两次（自我中继无意义、且会同机自撞端口）。
  - 每个 hop.node.relay_host != ""，否则 err「节点 <name> 未设置中继地址」。
  - proto=udp 的链路：所有 hop.mode 强制视为 kernel（userspace 仅 TCP）。
  - 租户链路：每个 hop.tunnel_id 必须是该租户在 hop.node 上的 granted tunnel；
    tunnel.proto_mask 必须允许 proto；mode=userspace 要求 proto=tcp。

端口分配（稳定，按 node 复用而非 position）：
  prevPort = { f.node_id -> f.listen_port : f ∈ forwards WHERE chain_id=chain.id }   // 改动前本链路各节点端口（删旧前先读出）
  chosen   = {}                                                                       // 本次已为各 node 选定的端口（防同链路内自撞）
  for i in 0..k-1:
    range = tenant ? [tunnel_i.port_start, tunnel_i.port_end] : [20000, 60000]
    occ   = OccupiedPortsOnNode(node_i, proto, chain.id) ∪ values(chosen)
    if prevPort[node_i] ∈ range 且 ∉ occ:  p = prevPort[node_i]      // 节点仍在链路里 → 保留原端口，入口/规则不漂移
    else:                                   p = pickFreePort(range, occ)
                                            if p==0 err「节点 <name> 端口段无可用端口」
    hops[i].listen_port = p; chosen[node_i] = p

事务内顺序：先读 prevPort → 算各跳端口/目标 → 删本链路旧 hops+forwards → 插新 hops+forwards。
（OccupiedPortsOnNode 带 excludeChainID=chain.id，占用集已忽略本链路自身旧 forward，故重算不会把自己看成占用。）

目标对齐 + 落 forward：
  for i in 0..k-1:
    target = (i < k-1) ? (node_{i+1}.relay_host, hops[i+1].listen_port)   // 中间跳：打到下一跳节点
                       : (exit_host, exit_port)                            // 末跳：打到出口
    insert forward{ chain_id=chain.id, node=node_i, proto, listen=hops[i].listen_port,
                    target_ip=target.host, target_port=target.port, mode=hops[i].mode,
                    tenant_id=chain.tenant_id, tunnel_id=hops[i].tunnel_id }

入口：chain.entry_node_id = node_0；chain.entry_listen_port = hops[0].listen_port
      entry = node_0.relay_host + ":" + hops[0].listen_port
affectedNodes = distinct(node_i for all i) ∪ (重算前旧 forward 涉及但现已移除的节点)
```

**约束层细节**：
- **中间跳目标**（下一跳 relay_host）由系统从所选节点推导、**自动可信**，**不过租户 CIDR 校验**——
  租户只能选自己有 tunnel 的节点，注不进任意中间 IP，无安全漏洞。
- **仅出口**是用户填的任意目标：租户链路里它要过**末跳 tunnel** 的
  `validateAgainstTunnel`（proto_mask + CIDR）；管理员链路无限制。
- **配额**：提交租户链路前预检 `CountForwardsForTenant + 新增跳数 <= MaxForwards`，
  且每个所用 tunnel 的 `CountForwardsForTenantTunnel + 该 tunnel 上新增跳数 <= grant.MaxForwards`。
- **带宽/计量**：每跳继承其 tunnel 的 `bandwidth_mbps`；同一份字节流过 k 跳 = **每跳独立计量**
  （≈k× 配额消耗），符合「确实占了 k 台中继带宽」，且零特殊处理（沿用现有按 forward 的计数器）。
  有效吞吐 = 各跳带宽最小值。

### mode 与 proto

- proto ∈ {tcp, udp}，整条统一。mode ∈ {kernel, userspace} **逐跳**存于 `chain_hops.mode`。
- userspace 仅 TCP：proto=udp 时所有跳落库即 kernel；UI 对 udp 链路禁用 userspace 选项。
- 默认值：UI 默认把每跳 mode 预选 **userspace**（对症用户的多跳客户端崩溃问题），运营者可逐跳改回 kernel。

### 编辑 / 重算 / 下发

- 新建 / 改顺序 / 增删跳 / 改 mode / 改出口：在**一个事务**里 `regenerateChain`（按 `chain_id` 删旧插新），
  提交后对 `affectedNodes` 调 `dispatchAfterFanout`（`server.go:184`，并发下发 + 单节点失败聚合进 flash）。
- 离线节点：forward 已落库，agent 重连时按 `ActiveForwardsForPush` 自动补下发（现有行为）。
- daemon 报错（跨段 409 / userspace 绑定失败）：经 `MarkNodeDispatchError` 落到该节点 `last_error`，
  flash 提示「链路已保存，但节点 <name> 下发失败：<err>」。
- **一键重新分配**：链路详情页对「下发失败」的跳提供按钮，强制对该跳重选端口（跳过当前冲突端口）后重下发。

### relay_host 维护

- 节点详情页（`node_detail.html`）「基本信息」卡新增「中继地址」可编辑字段 + `POST /nodes/{id}/relay-host`；仅管理员。
- 链路构建器里，未设 relay_host 的节点在下拉中标注「（未设中继地址）」且不可选；提交时二次校验。

### UI 与路由

新增路由（`server.go:Router`）：

```
管理员（adminOnly 组）:
  GET  /chains                列表
  GET  /chains/new            构建器
  POST /chains                创建
  GET  /chains/{id}           详情（含可复制入口、各跳状态）
  POST /chains/{id}           保存（改顺序/跳/mode/出口）
  POST /chains/{id}/delete    删除
  POST /chains/{id}/hops/{pos}/reallocate   单跳重分配端口并重下发
  POST /nodes/{id}/relay-host 设节点中继地址

租户（tenantOnly 组）:
  GET  /my/chains             列表
  GET  /my/chains/new         构建器（积木 = 我的 granted tunnels）
  POST /my/chains             创建
  GET  /my/chains/{id}        详情
  POST /my/chains/{id}        保存
  POST /my/chains/{id}/delete 删除
```

模板：`chains.html`、`chain_detail.html`、`chain_form.html`（admin）；`my_chains.html`、`my_chain_form.html`（tenant，
可与 admin 模板共享 partial）。`_layout.html` 导航加「链路」（admin）、`my_dashboard` 加「我的链路」（tenant）。

构建器交互：有序跳列表，每行 = 节点(admin)/通道(tenant) 选择 + mode 选择 + 上移/下移按钮；
顶部填出口 `host:port`；保存后高亮显示**可复制入口 `ip:port`**。路径以
`gomami → nnc-hk → nnc-tw → seednet:8443` 形式展示。

## 错误处理矩阵

| 场景 | 行为 |
|---|---|
| 链路中某节点未设 `relay_host` | 提交即拒，提示「节点 <name> 未设置中继地址」，构建器中该节点不可选 |
| 同一节点在链路出现两次 | 提交即拒，提示「同一节点不能在链路中重复」 |
| 端口段内无空闲端口 | 提交即拒，提示「节点 <name> 端口段(<a>-<b>)无可用端口」 |
| 手动建转发撞已占端口（panel 或 tui 段） | 提交即拒，提示「端口 X 已被占用（本地 TUI / 其他转发）」 |
| daemon 跨段 409（快照过时未挡住） | 落库成功；flash「节点 <name> 下发失败：端口冲突」；提供一键重分配 |
| userspace 跳 OS 级绑定失败（被他进程占） | 落库成功；flash 报该节点 err；一键重分配换端口重试 |
| 离线节点 | 落库成功；待 agent 重连自动补下发；节点状态显示「待同步」 |
| 租户超 max_forwards / 通道 max_forwards | 提交即拒，提示具体上限 |
| 租户出口不在末跳通道 CIDR 内 | 提交即拒，沿用 `validateAgainstTunnel` 文案 |
| 租户选了非自己 granted 的通道 / proto 不被 proto_mask 允许 | 提交即拒 |
| proto=udp 且选了 userspace | UI 禁用；后端兜底强制 kernel |

## 测试

### 单元测试（`internal/db`、`internal/server`）
- `OccupiedPortsOnNode`：仅 panel forwards、仅 tui 快照、两者并集、excludeChainID 正确排除本链路、proto 隔离。
- `regenerateChain`：
  - 3 跳 admin 链路目标对齐正确（中间跳打下一跳 relay_host:port，末跳打 exit）；入口端点正确。
  - 改顺序后重算：未变跳端口保留、变动跳重连；旧节点被移除并进 affectedNodes。
  - 同节点重复 / 缺 relay_host / 端口段耗尽 各自报错。
  - 租户链路：端口落在 tunnel 段内、tunnel_id 正确、出口过 CIDR、中间跳跳过 CIDR、配额预检。
  - udp 链路强制 kernel。
- 改造后的 `tenantCreateForward` / `createForward` 占用预检（含 tui 段命中）。

### 集成测试（docker fixture，扩展现有 `docker/`）
- 起 1 server + 2 agent（host net，互可达），admin 建 2 跳链路 → 验证两节点 forward 入库、双向连通、入口端点可达。
- 改顺序 → 验证重连 + 旧规则清除。
- 停一个 agent 后建/改链路 → 验证落库 + 节点「待同步」+ 重连补下发。
- tui 段先占端口，再建经该节点的链路 → 验证自动分配避开 tui 端口（不触发 409）。

### 手动验证
- 复刻用户场景 gomami→nnc-hk→nnc-tw→seednet（userspace 各跳），webui 一键建链，复制入口给 vless 客户端跑通。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| tui 快照过时/缺失致漏挡冲突 | 完整占用检查消灭绝大多数；daemon 409 兜底 + 一键重分配；快照随 agent `tui_segment_changed` 刷新 |
| userspace 跳被节点 OS 端口占用 | 高位段降低概率；daemon 绑定失败回传 + 一键重分配；admin 应给 tunnel 选不易冲突的端口段 |
| 租户 N 跳 ≈N× 配额，可能意外 | 设计明确「每跳计量」并在 UI 链路详情标注「本链路消耗 N 份配额」；如需「只计一跳」是后续可选项 |
| 部分节点下发失败致链路半生效 | 落库为准 + 节点级状态/错误可见 + resync/一键重分配；不做跨节点强一致回滚（与现有单条 forward 行为一致） |
| 误把本功能与 netfilter FORWARD chain 混淆 | 文档/命名统一用「中继链路 / relay chain」；代码包名 `chains` 限定在 server 层编排 |

## 实施顺序提示

1. migration `0006_relay_chains.sql` + `nodes.relay_host` 读写。
2. `OccupiedPortsOnNode` + 改造 `tenantCreateForward` / `createForward` 占用预检（独立可测、先修隐患）。
3. `chains` / `chain_hops` 的 db 层 + `regenerateChain` 生成/分配核心（充分单测，不碰 UI）。
4. 管理员 `/chains` 路由 + 模板 + 节点 relay_host 字段。
5. 租户 `/my/chains`（复用核心，仅约束层 + 积木来源不同）。
6. dispatch 接线（fanout / 一键重分配）+ 集成测试。
