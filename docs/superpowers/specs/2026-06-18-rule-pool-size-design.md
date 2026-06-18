# 每条规则的预热连接数(web 可控)

## 背景

userspace TCP 转发用连接池预拨上游连接（"预握手"），池大小当前是节点级环境变量
`NFT_FORWARD_POOL_SIZE`（默认 4，启动时读一次，节点上所有 userspace 规则共用）。
排查偶发断连时需要按规则调小/关闭预热，但目前只能改环境变量并重启 daemon，不可在
面板控制、也无法按规则区分。本设计把池大小做成**每条规则**的配置项，可在 web 管理端
设置，随现有规则下发链路生效。

## 目标与非目标

- 目标：管理端可为每条转发规则设置预热连接数；改动随 `apply_ruleset` 下发到节点并即时
  生效（无需重启 daemon）；存量规则行为不变。
- 非目标：用户端 `/my/rules` 不暴露此项（普通用户规则默认 kernel 模式，无连接池）；
  不改 kernel 模式数据面；不引入新的 server→daemon 通道。

## 数据模型与语义

`rules` 表新增可空列 `pool_size INTEGER`，`db.Rule` 增 `PoolSize sql.NullInt64`。三态：

- **NULL（未设置）** → 用节点默认 `NFT_FORWARD_POOL_SIZE`（默认 4）。存量规则即此态，
  零行为变化。
- **0** → 关闭预热：每个客户端即时新拨上游，无预握手。
- **N（1–64）** → 预热 N 条。

校验范围 `0 ≤ N ≤ 64`，越界返回 400。池仅对 userspace 模式的跳生效；kernel 跳忽略该值。

## 下发链路（复用 apply_ruleset，零新通道）

- `nft.Rule` 增 `PoolSize *int json:"pool_size,omitempty"`。用指针区分 未设置/0/N：
  - 向后兼容：旧面板不发该字段 → nil → daemon 用节点默认；旧 daemon 忽略未知字段。
- `buildRules` 逐 hop 构建 `nft.Rule` 时，从 `ruleMap[rh.RuleID].PoolSize` 取值，套到该
  规则每个 hop 的 `nft.Rule.PoolSize`（per-rule 值应用到该规则的所有 userspace 跳）。
- `computeRev` 对 `nft.Rule`（仅清 RuleID/RuleName/OwnerName）做哈希，`PoolSize` 自动进
  rev；改池数 → rev 变 → 节点自动重下发。此为必须保持的不变量：池大小是数据面状态，
  必须参与 rev 计算。

## 数据面（daemon）

- 池大小从"节点级单值"改为"逐监听端口/逐规则"：`openListener` 与 `Reconcile` 计算
  `effective := *r.PoolSize（非 nil 时）否则 envPoolSize()`，按该值建/不建池。
- `envPoolSize()` 保留为节点级默认（NULL 规则与未携带该字段的旧 push 都回退到它）。
- 热更新：`Reconcile` 当前仅在上游 addr 变化时重建池；需扩展为"addr 或池大小变化"都
  重建，使面板改动即时生效，无需重启。

## Web UI（管理端）

- 管理端规则**创建弹窗 + 编辑表单**增加"预热连接数"数字输入：可空，placeholder 显示
  默认值（4），空=默认、0=关闭。附说明：仅对 userspace 模式的跳生效。
- create/update 规则 API body 增 `pool_size *int`（可空）；校验后持久化。
- 规则详情页展示当前值（未设置时显示"默认"）。
- 用户端 `/my/rules` 不改。

## 影响面

db 迁移 → `db.Rule` 与规则 create/update 查询 → 管理端 api（create/update body + 校验）
→ `nft.Rule` → `buildRules` → forward（`openListener`/`Reconcile`）→ web 两处表单 + 详情。

## 测试

- forward 单测：`PoolSize=0` → 不建池、走即时拨号；`=N` → 建 N；热更新改池数 → 旧池
  Close、新池按新值建。
- server 单测：create/update 持久化 `pool_size`；`buildRules` 把规则池数套到各 hop；
  `computeRev` 随池数变化而变化；越界值被拒。
- 实际预拨行为依赖上游连通，不在单测范围。

## 风险与回滚

- 风险点：池大小若未进 rev 哈希，面板改动不会下发——已在不变量中固定。
- 回滚：NULL 语义保证旧行为；移除字段即回到环境变量单值模式。
