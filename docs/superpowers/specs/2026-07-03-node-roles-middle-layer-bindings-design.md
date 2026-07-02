# 设计：节点角色与中间层绑定（规则级联组链）

## 背景与问题

同一地区往往有多个「入口」节点（单点或组合，如 `dmit-hk-pro`、`火山广州前置-hepta深港IX`）。要为这些入口提供一种中间层链路（如 akari 内网、misaka 互联、akari-hk→akari-tw），现状必须为「入口 × 中间层」的每一个组合各物化一个组合节点：

- 创建与维护成本是 O(N×M)：每新增一种中间层，要为所有入口重建一遍组合节点；
- 用户的节点选择器被成品组合刷屏，重复信息多；
- 但又不能把自由组合开放给用户，否则会出现非常规链路（如港-美-欧再回港落地）。

## 目标

- 新增一种中间层 = 建 1 个节点 + 绑定若干上游，即对所有绑定的入口可用，不再物化 N×M 个组合节点。
- 用户只能沿管理员定义的绑定图选择链路，组合权保持在管理员手中。
- 计费模型同步简化：通用倍率只由入口决定；专属配额按原始字节在各授权节点上扣。

## 非目标

- 不引入地区/标签元数据，绑定为显式手动多选。
- TUI 不改动（物化后的 rule_hops 对 TUI 仍是普通链式规则）。
- 存量手工创建的「入口+层」成品组合节点不自动迁移，继续可用，由管理员逐步清理。

## 概念模型

- **角色**：节点新增位掩码角色，入口（bit0）与中间层（bit1）可兼有。单点与组合节点均可承担任一角色（akari-hk::akari-tw 作为组合节点即可当中间层整体复用）。
- **绑定**：有向边「downstream 中间层可挂在 upstream 之后」。upstream 可为入口或中间层（支持层接层级联），downstream 必须具有中间层角色。
- **规则路径**：规则除入口（`rules.node_id`，语义不变）外记录所选中间层的有序列表；物化链 = 入口段展开 ++ 各中间层段展开。

## 数据模型

1. `nodes.roles INTEGER NOT NULL DEFAULT 1`：bit0=入口，bit1=中间层。迁移默认所有存量节点为入口。
   加列必须同步 `nodeCols`（`internal/db/queries.go`）、`scanNode`、`internal/db/grants.go` 中 `ListNodesForUser` 的 inline SELECT 三处，漏任何一处会静默清空数据。
2. `node_bindings(upstream_node_id, downstream_node_id, mode)`：
   - 主键 `(upstream_node_id, downstream_node_id)`，两列均 `REFERENCES nodes(id) ON DELETE CASCADE`；
   - `mode TEXT NOT NULL DEFAULT 'userspace' CHECK(mode IN ('kernel','userspace'))`——该边被使用时衔接段（upstream 段尾跳 → downstream 段首跳）的转发模式；
   - downstream 须具有中间层角色（应用层校验）。
   绑定/解绑/改边模式只影响此后新建或重推的规则，存量规则保持物化快照——与 `node_hops` 编辑的既有语义一致。
3. `rules.via_node_ids TEXT NOT NULL DEFAULT '[]'`：有序 JSON 数组，所选中间层节点 ID。链的推导来源必须持久化在规则上：凡带 `node_id` 的编辑路径会从节点配置重推链（`internal/server/api.go` 的 `hopsForNode` 消费方），若路径只存在于 rule_hops 快照中，一次编辑就会把中间层静默剥掉。
4. `rule_hops.via_node_id INTEGER NOT NULL`：溯源列，该物理跳所属逻辑段的节点 ID（入口段 = `rules.node_id` 的值，各层段 = 层节点 ID）。存量行 backfill 为所属规则的 `node_id`。配额压制、via 授权记账、grant 限速分组、链路展示都依赖它。
5. `node_hops.traffic_multiplier` 废除读写。迁移时先将每个组合节点的 `nodes.rate_multiplier` 置为其各跳倍率之和（等值于现状的有效计费值与 `×N` 显示值，存量组合规则计费金额不变），此后该列保留为休眠列、代码不再读写，不做物理删除。

## 链的推导与物化

- 链 = `expand(入口) ++ expand(via1) ++ expand(via2) …`；`expand` 复用 `hopsForNode` 的展开逻辑（组合按其 `node_hops` 顺序展开，单点即自身）。注入点在 `hopsForNode` 一层，create / edit / my 三条路径共享，保证行为一致。
- 段内跳的 mode 沿用该组合段的 `node_hops.mode`。
- 段尾跳的 mode：链尾 = 规则 `exit_mode`（既有不变量：出口段模式归规则所有，`node_hops` 末跳 mode 保持 dormant）；非链尾 = 所经绑定边的 `mode`。
- UDP 规则全链强制 kernel（`RegenerateRule` 既有行为，userspace relay 仅支持 TCP）。
- 端口分配、按节点保持端口稳定、每机切片下发、rev 幂等计算均无改动。
- 从现有 rule_hops 复制的路径（header-only 编辑、换端口、relay 变更 rewire）天然保留中间层；从节点配置重推的路径一律经 via 拼接。

## 服务端校验

规则创建/编辑时对 via 路径逐项校验（UI 只是便利层，服务端是权威）：

- 相邻两段 `(prev, next)` 在 `node_bindings` 中存在边；
- 每个 via 节点具有中间层角色；
- my 路径下用户对入口与每个 via 节点均有授权（`user_nodes`）；
- 链上物理节点不重复（`RegenerateRule` 既有校验）：组合入口与某层共享物理机时返回 409，表单侧只做逻辑级去重过滤，物理级冲突由服务端报错并透出可读信息；
- 编辑请求未携带 `via_node_ids` 字段时保留现值，防止旧客户端静默降级（与 `entry_family` 的 normalize 处理同理）。

## 计费模型

| 账本 | 现状 | 新口径 |
|---|---|---|
| 用户全局用量 `users.traffic_used_bytes` | 每物理跳累加 `billedDelta × 跳倍率 × billingRate` | 仅入口跳（position 0）计一次：`billedDelta × 入口节点 rate_multiplier × billingRate` |
| 每跳倍率 `node_hops.traffic_multiplier` | 每跳独立配置，组合有效倍率 = 求和 | 废除；组合节点倍率使用自身 `rate_multiplier` |
| 专属配额 `user_nodes.traffic_used_bytes` | 按物理跳累加加权值 | 按原始字节（`billedDelta`：尊重节点 unidirectional，不乘倍率与 billingRate）扣在各逻辑段的授权上：每段首跳 → 该段节点的 grant；物理子节点不再单独计 |
| hop totals `rule_hops.total_bytes` | 每跳存加权值 | 入口跳存计费值（`FillRuleTraffic` 显示逻辑零改动），其余跳存原始字节；`last_*` 不变 |
| 落地出口账本 | 末跳原始字节（up+down，双向） | 不变 |
| 配额压制 | 任一物理跳授权或 `rules.node_id` 授权超额 → 整链撤下 | 匹配条件扩展到 `via_node_id`：via 授权超额同样压整链（`ActiveRuleHopsForPush`、`RulesAffectedByNode`） |

- via 授权的 `rate_limit_mbytes` 沿用 grant shaping 机制，shaping 组作用在该层段首个物理跳（由溯源列标注归属）。
- 规则数配额 `max_forwards` 仅对入口授权计数；via 授权上的该字段对经过规则不生效，避免一条规则在多处授权同时占名额造成难排查的 409。via 的容量控制交给配额与限速。
- 用户全局 `max_forwards` 仍按物理跳数计：选择中间层的规则占用更多名额，表单需提示。

## UI

管理端：

- 节点列表/详情：角色徽章与角色编辑（多选框）；
- 具有中间层角色的节点详情新增「上游绑定」卡片：多选上游节点（入口/中间层，含全选），每条边附衔接模式选择（默认 userspace）；
- `CompositeNodeModal` / `CompositeHopsCard` 移除每跳倍率列；组合节点详情增加 `rate_multiplier` 编辑。

规则表单（admin 与用户共用 `RuleFormModal`）：

- 入口选择器只列具入口角色的节点（my 路径再交授权集合）；
- 级联「下级线路」下拉：候选 = 当前节点绑定下级 ∩ 用户授权（my） ∩ 中间层角色 ∩ 未在链上；默认「直接转发」；过滤后候选为空则不渲染下一级；上游选择变更时下游级联重置；
- 表单下方实时链路预览：`入口 → 层1 → … → 目标`；
- 协议栈标签与 v6 出口校验随链尾动态重算（my API 需为授权节点附带出入口栈信息，复用 `ResolveCompositeRelayStack` 机制）。

用户端展示：

- 规则列表/详情渲染 via 链路（via 均为用户被授权节点，名字可见，无信息泄露）；
- Dashboard 自动出现中间层授权行（配额/用量可见），用户可以看到自己所选链路的消耗。

## 迁移与兼容

- `nodes.roles` 默认全部为入口，管理员手工为 akari 等节点打中间层角色；
- `rule_hops.via_node_id` backfill 为所属规则的 `node_id`；
- 组合节点 `rate_multiplier := Σ(node_hops.traffic_multiplier)`，保证存量计费连续；
- 专属配额从加权值切换为原始字节：存量已用量数字不回算，此后按新口径累加，倍率≠1 的授权会出现一次性口径变化；
- 不带 `via_node_ids` 的旧请求保留现值；不带 `roles` 的节点创建默认入口角色。

## 测试要点

- 链推导：via 拼接顺序、段尾 mode 取边配置、链尾取 `exit_mode`、UDP 强制 kernel、重复物理节点 409、绑定边缺失与 via 无授权 / 无角色被拒；
- 计费：全局用量仅入口跳计一次、组合倍率取自身 `rate_multiplier`、via 授权按原始字节扣、压制条件覆盖 via 授权；
- 编辑路径：带 `node_id` 的编辑保留 via、header-only 与 rewire 不丢中间层、不带字段的请求不降级；
- 迁移：倍率聚合与现状有效值等值、溯源列 backfill、`nodeCols`/`scanNode`/inline SELECT 三处对齐。

## 设计取舍与理由

- **通用倍率只在入口计一次**：同一份字节流经每一跳，逐跳累加使价格随链长增长，与「中间层是入口的增值链路」的定价模型不符；组合入口迁移为倍率求和使存量价格不变。
- **专属配额按原始字节**：配额表达「该授权节点为用户转发了多少流量」，属于资源占用口径，与定价（倍率）解耦。
- **衔接模式挂在绑定边上**：衔接段的两端由边唯一确定，是该配置的自然归属；默认 userspace 与组合跳缺省一致，对多跳 TCP（split-TCP）场景友好。
- **中间层照走授权体系**：级联候选 = 绑定 ∩ 授权，管理员既控制拓扑（绑定图）也控制人（授权），并复用配额/限速能力；代价是授权管理量增加，由既有批量授权工具缓解。
- **角色用位掩码**：同一节点可以既作为入口出售、又作为中间层被挂载，避免为双重身份复制节点。
