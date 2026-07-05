# 节点用途双栏改造设计

## 背景与目标

节点详情页的「节点角色」卡片当前把入口、中间层做成两个可切换的 pill 按钮
（bitmask：entry=1、via=2），勾选中间层才展开「上游绑定」编辑器，而「已绑定的
下游」是只读展示——因为绑定边 `{upstream, downstream, mode}` 的所有权归下游节点，
只能在下游节点自己的详情页编辑。

本次把它重构为左右两栏、各自独立开关的「节点用途」卡片，并把下游关系从只读升级
为可在上游侧直接编辑：

- 左栏「入口」：开关 + 已绑定下游（可多选增删、每条边配转发模式）。
- 右栏「中间层」：开关 + 已绑定上游（沿用现有能力）。

## 非目标

- 不改绑定边的数据结构与 `rule_hops` 展开语义。
- 不改上游绑定编辑器的既有交互，仅将其归入右栏。
- 不引入「纯中间层节点在独立入口之外管理下游」的通道——下游编辑随入口栏（见语义简化）。

## 数据模型与迁移

- roles bitmask 不变：entry=1、via=2，至少保留一位。
- 绑定边 `node_bindings{upstream_node_id, downstream_node_id, mode}` 不变。mode 是
  衔接段（上游段尾跳 → 下游段首跳）的转发模式，从上游侧或下游侧看都是同一条边的
  同一个值。
- 迁移：新增一条迁移把存量节点设为 `roles=3`（同时具备入口与中间层），但排除
  `node_type='self'` 的面板内置节点——self 不作链路中间层，赋予 via 只会让它误入
  绑定候选。
- 新建节点默认由 `1` 改为 `3`（`CreateNode` 只建真实 agent 节点），保持「所有节点
  默认两用途」的一致性；`UpsertSelfNode` 的 self 节点不改。同步更新
  `internal/db/queries_test.go` 中默认角色为 entry 的断言。

## 后端 API

绑定边由「下游拥有」扩展为「上下游任一侧均可编辑其相关子集」。新增对称的 upstream
端读写，与现有 downstream 端并存：

- db 层新增：
  - `ListBindingsForUpstream(id)`：列出 `upstream_node_id=id` 的全部边。
  - `ReplaceBindingsForUpstream(id, edges)`：`DELETE WHERE upstream_node_id=id` 后
    重新插入。只作用于「以 id 为上游」的边子集，不触碰下游节点的其它上游边。与
    `ReplaceBindingsForDownstream` 对称。
- 路由新增：
  - `GET  /nodes/{id}/downstream-bindings`：返回以 id 为上游的边。
  - `POST /nodes/{id}/downstream-bindings`：整体替换以 id 为上游的边集。
- POST 校验：
  - id 必须存在，且具备 entry 或 via 角色（能作为上游被接入）。
  - body 中每个 downstream 必须存在且具备 via 角色（能作为中间层接在本节点之后），
    否则 400。前端已过滤候选，后端仍强校验。
  - 禁止自绑；downstream 去重；mode ∈ {kernel, userspace}，空值默认 userspace
    （与 downstream 端一致，避免落入 kernel 的全局回退）。
  - 写审计。

一条边 `(down=B, up=A)` 同时属于「A 的下游集」和「B 的上游集」。在 A 的入口栏移除
B，即删除该边，B 的中间层栏也随之少一个上游 A——这是同一条边的两个视图，行为一致。

## 前端

`RolesCard` 重构为左右两栏的「节点用途」卡片，窄屏下堆叠。

- 左栏「入口」：
  - 顶部一个入口开关，切换 entry 位。
  - 开启时展开「已绑定的下游」编辑器：
    - 多选下拉，候选 = 已开启中间层（via）角色的其它节点，附「全选」。
    - 每条下游行：节点名 + per-edge 转发模式选择器（kernel/userspace）+ 删除。
  - 关闭时仅显示开关。
  - 取代原来独立的只读「已绑定的下游」区块。
- 右栏「中间层」：
  - 顶部一个中间层开关，切换 via 位。
  - 开启时展开「已绑定的上游」编辑器，沿用现有实现（多选上游 + per-edge 模式 +
    删除 + 全选）。上游候选不按角色过滤（上游可为入口或中间层，任一即合格），维持
    现状；这与下游候选须过滤 via 的非对称是刻意的——下游必须能作中间层接在后面。
- 两个开关至少保留一个；后端已校验，前端保存前给出提示。
- 保存：整卡一个统一「保存」按钮（不是每栏各一个），因为 roles 是整块 bitmask，一次
  写最安全。dirty 分三部分追踪（roles / 上游边 / 下游边），串行提交：
  `/roles`（若 dirty）→ `/bindings`（上游，若 dirty）→ `/downstream-bindings`（下游，
  若 dirty）。任一失败仍触发静默刷新，使 dirty 基线与实际持久化状态对齐（沿用现有
  容错：多个 POST 非事务，刷新避免保存按钮与服务端状态不一致）。
- 下游边懒加载：入口开启时首次拉取 `GET /nodes/{id}/downstream-bindings`；加载失败
  保持 rows 为 null（不覆盖）并提供重试，避免一次网络抖动把已存绑定清空——对称现有
  上游边的处理。

## 语义说明（体现在 UI 文案）

- 入口栏管下游（谁接在本节点后面）；中间层栏管上游（本节点接在谁后面）。
- 下游必须具备中间层角色才能级联在本节点之后（候选已过滤，后端强校验）。
- 转发模式是衔接段模式，边的 mode 从上游侧与下游侧看是同一个值，改一处即改这条边。
- 语义简化：物理上纯中间层节点也可拥有下游，但本设计把下游编辑挂在入口栏、随入口
  开关展开。迁移后两栏默认都开，可同时管理上下游；仅当手动关闭入口时下游编辑器隐藏。

## 测试

- db：
  - `ReplaceBindingsForUpstream` / `ListBindingsForUpstream` 往返。
  - upstream 侧替换只影响 `upstream=id` 的边，不误删下游节点的其它上游边。
- server：
  - `POST /nodes/{id}/downstream-bindings` 校验——下游缺 via 拒绝、上游缺角色拒绝、
    自绑拒绝、重复拒绝、mode 非法拒绝、空 mode 落 userspace。
- 迁移：存量节点 roles 迁移为 3；新建节点默认 3。
