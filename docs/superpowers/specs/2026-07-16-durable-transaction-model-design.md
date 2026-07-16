# 统一 Durable Transaction Model 设计（密封操作 + 权威根）

## 背景与问题

托管规则跳（SB 集成）功能把 daemon 从"无状态执行器"推成了"有恢复语义的控制面"，但控制面的持久化模型是逐条路径生长出来的，没有统一的事务模型。审视既有实现（`feat/sb-managed-rule-hop-leases` 分支）暴露出十类系统性缺口：

1. **状态源不唯一**：owner 原始意图、冻结的 transition 规则、counter manifest、内存 AuthorizationBook、内核 active mark 集——五种"当前配置"表示并存，无权威聚合。不同路径各选来源（恢复读 applied 快照 + open operations，renew 读 ACTIVE manifests + 授权快照），mark、DNS、授权与实际规则互相漂移。
2. **持久化意图不完整**：operation 主要持久化状态名与少量标识。renew 只保存 desired_grants blob 与 deadline，不保存具体规则、mark、TTL；恢复时只能从当前 ACTIVE 状态重推导。
3. **状态机与事务不一致**：逻辑上的单次状态转换拆成约 8–10 个独立 commit（Prepare 与 fence stamp 分离、授权 Commit 内存化后 epoch 与 CAS 分两事务、stale 路径 finalize 与 journal 分两事务）。崩溃可停在数据库合法、业务语义不完整的中间态。
4. **恢复不是确定性重放**：full reconcile 的恢复接近 resume，但 renew 恢复从当前 ACTIVE 与授权快照重推导；时间流逝、boot 变化或并发更新后，恢复结果不等于原请求。
5. **cutover 缺权威旧态输入**：pre-fence 的 mark 集取自新 plan（`knownMarksFromPlan`），而非持久化的"必须封掉的旧 ACTIVE 集"；退役旧代实际依赖 InstallStaged 的整表原子替换，active 非空时的二次托管 cutover 无法通过 active-empty 断言。
6. **身份密封不完整**：plan hash 排除 mark/generation 且不含 shaping；rule payload hash 不含 mark；owner raw rules 只有独立 payload_hash，无跨表交叉锚定。单表自洽不代表控制图自洽。
7. **计量终态不闭合**：stale 路径的 FinalizeCounterSets 与 AppendFinalRecord 分属两个事务，中间崩溃产生"已 FINAL 却无 final_record 引用"的用量——durable 但永不可导出，重跑也不补写。
8. **boot 边界没有进入状态机**：AuthorizationBook 是 boot 作用域内存状态；boot 错配只把 WAITING_DNS 请求标 STALE（且作为 LoadAppliedSnapshot 的副作用散落执行），其余旧 operation 缺统一收口。
9. **ACK 不是持久化结果的一部分**：恢复可到达 SUCCEEDED，但原请求应收到的完整 ACK（result 全文）无法重新生成。
10. **审计只审表**：审计器重算 plan hash、身份 hash 与两条链，但不横跨 owner intent、transition、manifest、lane、final payload、数据面身份的全链路一致性；测试替身弱于生产合同（fake fence 忽略 marks 清空全部 active、采样测试预写 totals、renew 恢复只覆盖单 mark 同进程）。

根因是缺一个统一的 durable transaction model：

```
精确请求意图 → 权威旧态 → 原子副作用边界 → 完整身份与计量证明 → 可确定重放的终态结果
```

本设计把这个闭环实体化为两个核心抽象——**权威根（Authoritative Root）**与**密封操作（SealedOperation）**——并统一 daemon 侧与 panel 侧托管域的全部写路径。

## 目标

- daemon 侧规则/授权/计量/数据面状态收敛到单一权威聚合，全部写路径统一为密封操作。
- 任何操作在任意崩溃点恢复后产出同一终态、同一 ACK 字节：恢复是密封面的确定性重放，不是从 live 状态重推导。
- 计量终态单入口：封闸、尾样、计价、FINAL、journal、fence 一个事务，任何 lane 终结路径共用。
- 计数身份与准入身份解耦：所有规则（普通 + 托管）的样本携带密封 lane 身份，废除 `(proto, listen_port)` 反推。
- panel 侧托管集成域操作化，panel↔daemon 下发闭环（DISPATCH）持久化 fence 与结果。
- NF↔SB 契约按新模型重定义（直接替换，不做旧 wire 兼容；SB 侧未来按新交互模型重构连接器）。
- 全图审计器横跨六域交叉锚定；测试合同消灭替身与生产的语义分歧。

## 非目标

- 不改 NF 普通业务（用户/规则 CRUD/grant/配额/限速/周期重置）的 panel 计费表与计费链路；普通规则只并轨到统一身份传输（DELTA 级账本），账本语义不变。
- 不做 event sourcing：不追求任意历史点重建，只保证当前权威状态 + 未完成操作的确定性完成。
- 不做旧数据迁移：功能未发布、无生产数据，daemon store 与 panel 托管域 DDL 一次成型，从 main 新开分支实现。
- 不在本设计内实现 SB 侧连接器改造（联动数据模型在此定义，SB 按其重构）。

## 核心闭环

| 闭环环节 | 实体化为 |
|---|---|
| 精确请求意图 | 创建事务密封的 intent 全文（canonical blob + hash） |
| 权威旧态 | 创建事务密封的 precondition：base_config_epoch + 待退役旧 ACTIVE 集 |
| 原子副作用边界 | 恰好两个必经原子事务（seal / final）+ 幂等副作用 + readback 证明 |
| 完整身份与计量证明 | 六域 Merkle 根 + 全覆盖 effect_plan_hash + FINAL 单入口 + 两条 append-only 链 |
| 可确定重放的终态结果 | 终态事务密封的 result ACK 全文，重试原样重发字节 |

---

## A. 权威状态模型：单根 + 六域

daemon store 维持单 SQLite 文件、单写者、WAL、`synchronous=FULL`、`MaxOpenConns(1)` 的既有纪律。新增单行聚合根：

```
authoritative_root {
  config_epoch      单调递增，只在操作终态事务推进
  root_hash         H(prev_root_hash, op_result_hash, 六域 hash)
  prev_root_hash    上一终态的 root_hash（root 自身构成 hash 链，给审计器全序）
  last_operation_id
}
```

### 六域

| 域 | 内容 | 收敛掉的旧状态源 |
|---|---|---|
| owners | applied 意图集 + owner heads（raw blob + payload_hash） | owner_intents 多状态漂移面 |
| dataplane | 已安装数据面身份 {generation, effect_plan_hash} | 内核 marker 单方真相 |
| authorization | 持久化授权行 {lease key, mark, deadline, boot_id, authorization_epoch} | 纯内存 AuthorizationBook |
| metering | 计量图结构（metering/counter manifests、lane 定义、active index） | counter manifest 孤岛 |
| journal | final_records 头 {journal_base_seq/hash, next_final_seq, acked_final_seq, stream_epoch} | store_meta 游标散置 |
| binding | {panel_instance_epoch, node_id, store_uuid} 三元密封 | 无（新增；panel DB 重建即绑定失配，fail-closed） |

### 结构性规则

1. **root 覆盖结构，不覆盖流量。** 高频累计值（lane totals、priced units、billable）不进 root，否则每个样本都要重写根。累计值完整性由两条 append-only hash 链（pricing_events、final_records）加"只增不减"不变量护航。root 管"图"，链管"流"。
2. **权威状态 = root + 至多一个 open operation 的已执行前缀。** 操作中途的 durable 变更（如 FINAL 事务产物）挂在该 operation 名下；root 只在操作终态事务推进。配合 one-open-operation 部分唯一索引（沿用），任何中间态都可被指认为"哪个操作执行到哪一步"，"数据库合法但业务语义不完整"的匿名中间态不复存在。
3. **决策读锚 epoch，观测读锚身份。** planner/admission/恢复/快照导出等决策读必须携带读取时的 config_epoch，提交时 CAS 校验未变（乐观并发）；counter 采样等观测热路径只锚定 lane 身份——lane 身份本身被 manifest 密封锚到 root，间接受根保护，热路径不做全图校验。

### 域 hash 的双算法

每域一个 domain_hash 行。事务内**增量维护**（只重算被触碰域 + 顶层组合）；审计器从该域规范化全量投影**全量重算**并对账。两种算法互为对方的正确性测试。规范化编码沿用 canonical 域前缀 + 长度前缀 LE 二进制 + 确定性排序的既有形态。

### 授权域持久化与时间基准

AuthorizationBook 降级为纯投影：授权域行落库后，内存 book 只是 lease_loop 定时器的派生缓存，永不作为恢复或决策的输入源。

租约时间基准 = **进程内单调钟 + 进程 boot_id**。授权行持久化 boot_id 与单调 deadline；跨进程（boot_id 不匹配）一律视为已过期，由 BOOT_FENCE 操作统一收尾。不引入 wall-clock 双轨——SB 租约本就是 10–30s 高频续租，进程重启后由新租约重建授权，fail-closed 且语义自洽。

---

## B. 密封操作模型：一次密封、两个原子边界

所有写路径统一为 SealedOperation。种类：

- daemon 侧：APPLY（panel 下发 / DNS 刷新 / 本地重投影）、RENEW（租约续期）、EXPIRE（租约到期 finalize）、BOOT_FENCE（跨 boot 残余收口）、SHUTDOWN_FINAL（有序停机封账）。
- panel 侧（见 E 节）：CLAIM、RELEASE、BARRIER、LEASE、DISPATCH。

每个操作以两个**必经边界事务**（seal / final）为闭合端点；边界之间允许存在挂在该操作名下的中间事务（如 C 节的 TX-FINAL、TX-commit）与幂等副作用，它们构成 A 节规则 2 所说的"已执行前缀"。两个边界事务：

### 创建事务（seal）

一个事务密封四件东西并置 state=SEALED：

- **intent**：请求全文，canonical 编码 blob + intent_hash。同 request_id 重试：同 hash → 返回/等待原操作；异 hash → 幂等冲突，永不静默改写。
- **precondition**：`base_config_epoch` + **待退役旧态集**——从权威根 authorization/metering 域读出的旧 ACTIVE lane epochs、旧 marks、旧数据面身份，密封为副本。pre-fence 从此只用密封旧态集，永不从新 plan 推导。
- **effect_plan**：具体副作用全文——
  - 解析后的具体规则（DNS 结果是密封输入；密封后与外部世界解耦，永不重解析。DNS 刷新 = 新的 APPLY 操作）；
  - 本事务内分配的 marks 与 generation（分配从执行期提前到密封期；STALE 操作浪费的编号无害——编号本就单调永不复用）；
  - 要创建的新 lane 集与要终结的旧 lane 集；
  - TTL 与单调 deadline；
  - shaping 参数（限速/分组）。
  - `effect_plan_hash` **全覆盖**以上所有字段（转发元组 + marks + generation + shaping + deadline + lane epochs + 计价引用）。数据面身份改为 {generation, effect_plan_hash}。
- **promised ACK**：request_id、operation_id、stream_epoch 等创建期即可知的应答承诺字段。drain seq 等执行期才产生的字段在 FINAL 事务盖上（ACK 分两段密封）。

### 终态事务（final）

一个事务完成：

- result ACK 全文（result_payload blob）——崩溃后重试**原样重发字节**；
- owner heads / intent 状态推进、applied generation 推进；
- `config_epoch++`、六域增量 hash 重算、root_hash 链推进；
- 状态 CAS 到终态（SUCCEEDED / STALE / FAILED）。

RENEW 从此密封具体的 {mark, lane, metering_epoch, deadline} 集合；EXPIRE / BOOT_FENCE / SHUTDOWN_FINAL 是同一模型的退化形态——都有密封旧态集与密封结果，没有"轻量旁路"。

---

## C. 执行与恢复：状态机 resume，输入只准来自密封面

nft/userspace 副作用不可事务化，执行器保留"CAS 状态机 + 幂等副作用 + readback 证明"的形态，外加一条铁律：**每个 state 的输入只允许来自 sealed plan；禁止读全局 live 状态。** readback 是执行与恢复期间唯一允许的 live 读，且只回答"副作用 X 是否已落地"。

### APPLY 时序（4 个 durable 事务）

```
TX-seal    密封 intent/precondition/effect_plan（含 mark、generation 分配）/promised ACK
                                                            → SEALED
S1   FenceAdmission(密封旧态集的 marks) + readback 证明        → FENCED
S2   采集旧 lane 内核尾样（只读内核）
TX-FINAL   尾样折入 lane totals + 计价推进 + FINAL checkpoint
           + DRAIN_PREVIOUS journal 追加 + 旧 lane lifecycle→FINAL
           + 摘出 active index + 新 lane 建 STAGED
           + fence(stream_epoch, drain_through_seq) 盖到操作    → DRAINED
S3   InstallStaged：原子替换 ruleset + 写新身份 marker + 空 active set
                                                            → STAGED
TX-commit  新 lane STAGED→ACTIVE + 授权域行落库（mark/deadline/boot_id）
                                                            → COMMITTED
S4   Activate/SyncManagedGrants：marks 进 active set 并带 TTL   → SYNCED
TX-final   result ACK 全文 + heads/generation 推进 + config_epoch++ + root 推进
                                                            → SUCCEEDED
```

失败分支：S3 之前失败 → STALE 分支（密封好的 finalize 路径：旧代保持封闸，新 lane ABORTED，走 FINAL 单入口封账，终态事务密封失败 ACK——失败 ACK 同样携带已产生的 fence）；S3 之后只有向前恢复或 STALE finalize，永无回滚到旧数据面。

### 恢复协议

- **恢复 = resume 密封面**：读 open operation 的 sealed plan，从持久 state 继续执行。同一 store 恢复任意多次 → 同一终态、同一 ACK 字节。RENEW 恢复不再从当前 ACTIVE 重推导。
- **precondition 校验**：resume 时密封 base_config_epoch 与当前 root 不符（说明中途有其他操作终态化——正常情况下 one-open-op 阻止这一点，防御性校验）→ 确定性走 STALE 分支。
- **boot 边界进状态机**：启动流程固定为①resume 所有 open operation 至终态（密封 deadline 是单调钟语义，跨进程一律已过期 → 确定性 STALE/EXPIRE finalize）；②扫描授权域残余的跨 boot 行，创建 BOOT_FENCE 操作走统一 FINAL 协议收尾；③全量审计通过后才接受新请求。boot 错配处理不再是任何读路径的副作用。
- **store 丢失/损坏**：不属于自动恢复路径。内核存在托管状态而 store 缺失 → 清空 active set、进入恢复必需状态、拒绝托管 apply/renew（fail-closed 铁律沿用）。

---

## D. 计量闭环：FINAL 单入口 + 统一身份、分级账本

### FINAL 单入口

任何 lane 生命终结——cutover drain、stale、expire、shutdown、boot fence、barrier——必须经过**同一个函数、同一个事务**（即 C 节 TX-FINAL）：

```
{ 前置：封闸已发生（readback 证明）
  尾样折入 lane totals（绝对值游标差额）
  计价推进（priced_units += Directional(delta)，禁止用累计重算）
  FINAL checkpoint（每 metering_epoch）
  final_record 追加（hash 链）
  lane lifecycle → FINAL，摘出 active index
  fence（stream_epoch, drain_through_seq）盖到操作 }
```

"finalize 成功但 journal 缺记录"的分裂在结构上不可能：二者是同一事务的两行写。

### 计数身份与准入身份解耦

- **计数身份**：每条规则（普通 + 托管）的每个 concrete-protocol lane 在 effect_plan 中密封 lane_id（store 内单调分配、全局唯一、永不复用，同 mark ordinal 纪律）；内核侧落为**命名 nft counter 对象**（对象名由 lane_id 派生），采样按对象名读回绝对累计。`(proto, listen_port)` 反推彻底废除——端口复用污染与 l4proto set 表达式解析失败毒化样本两类问题从机制上消失。userspace 转发按同一 lane_id 上报。
- **准入身份**：mark 只负责托管规则的放行/封闸。known_managed_marks（guard 精确集）+ active_managed_marks（timeout 准入集）双集合语义原样保留；mark 单调分配、永不复用。

### 分级账本

wire 上所有样本统一为 `{lane_id, 绝对累计}`；panel 按 lane 等级分流：

- **EXACT（托管 lane）**：journal/FINAL/ack/GC 全闭环，exactly-once 导出到 SB 账本。双层累计沿用：counter lane 只负责 raw 累计，同 hop 的多个 lane（如 tcp+udp）把计价增量汇入同一 metering_epoch。
- **DELTA（普通 lane）**：按 lane 折差额进既有 user_traffic/landing 计费表，语义不变（容忍崩溃窗口丢增量），panel 计费链路（配额/限速/周期重置）不动。

### 计价不可变

metering_manifest 不可变 + metering_epoch 即计价身份的既有形态保留：倍率/单双向/计费开关变化 = 新 metering_epoch = 新 lane，旧 lane 走 FINAL。计价引用密封进 effect_plan 后，改价影响在途 lane 依旧不可能；renew 密封 metering_epoch，改价后纯续租无法延长旧计价实例。

---

## E. panel 侧操作化与 NF↔SB 联动模型

### 托管集成域聚合根

panel DB 承载大量非托管业务，不做全库六域化；聚合根只建在托管集成域，per-controller 单行：

```
controller_root { integration_epoch 单调, domain_hash, last_operation_id }
```

`integration_epoch` 是原 usage_generation 的语义强化：账本折叠、revision 生命周期、claim/release/barrier/lease、拓扑变化——**所有托管域写**都在同一事务推进它。"拓扑变化必须伴随 generation 推进"从约定升级为结构。

### panel 侧 SealedOperation

统一表 `integration_operations`，五种：CLAIM、RELEASE、BARRIER、LEASE、DISPATCH。与 daemon 侧同款两个原子边界：

- 创建事务密封 {request_id, intent 全文, precondition(integration_epoch + link_epoch + topology_generation), effect plan}；
- 终态事务密封 {result_payload ACK 全文, integration_epoch++, domain_hash 推进}。

幂等 tombstone 是该表终态行的自然形态（同 request_id 永久返回同一 result_payload 字节；被 release 终结的旧 claim 请求永久返回 RELEASED）。

### DISPATCH：跨系统闭环的铰链

panel→daemon 的每次下发是一个 DISPATCH 操作：

```
panel: DISPATCH seal（密封 payload hash + apply_request_id + 目标节点）
daemon: APPLY seal（绑定同一 apply_request_id）
        同 ID 同 payload → resume 或重放原终态；同 ID 异 payload → 幂等冲突
daemon: 终态 ACK { result_payload + fence(stream_epoch, drain_through_seq) }
panel:  DISPATCH 终态事务：持久化 fence + result + integration_epoch++
```

barrier/release 的 readiness 判定只看持久化状态：相关 DISPATCH 均已终态、fence 已覆盖、对应 EXACT lane 的 FINAL 已折入 ledger。panel 重启后 readiness 从表中确定性重算，不依赖内存重建。self-node 与远端 daemon 使用同一 DISPATCH/APPLY/ACK/fence 语义。

### NF↔SB 契约（直接替换，无旧 wire 兼容）

外形与既有冻结契约同构（端点集合、认领/释放/快照/ack/屏障/租约/绑定），SB 连接器重构量可控；语义地基替换为：

1. **统一写信封**：所有写端点请求 `{panel_instance_epoch, request_id, intent...}`，响应 `{operation_id, status, result...}`；幂等由 integration_operations 终态行保证。epoch 不匹配 → 整批拒绝。
2. **快照**：单读事务导出 `{panel_instance_epoch, integration_epoch, users[{link_epoch, topology_generation, rules[{rule_id, rule_revision, active, landing, hops[...]}]}]}`。对 SB 仍按 hop 行导出——lane 是 NF 内部概念，同 hop 多 lane 经 metering_epoch 聚合后成行；`raw_up/raw_down/billable_bytes` 整 revision 生命周期只增不减。
3. **ack/GC**：按 integration_epoch 确认；水位单调不回退；GC 谓词 `finalized_integration_epoch <= acked 水位` 保留。
4. **租约**：per-user 整批原子替换 + topology_generation CAS + TTL 钳制保留；lease key 含 metering_epoch；NF 内部执行为 LEASE 操作 → 相关节点 DISPATCH/RENEW 的密封链。

JSON 类型纪律沿用：NF 数值 ID 一律 int64 number；epoch/UUID 一律 string。openapi yaml+json 双份手写同步。跨仓契约 fixture 逐字节钉死。

---

## F. 全图审计与测试策略

### 审计器三层

1. **域内**：从规范化全量投影重算每域 domain_hash，与增量维护值对账。
2. **跨域交叉锚定**：owner intent payload_hash ↔ sealed intent_hash；effect_plan ↔ counter manifests ↔ active index ↔ 内核 readback（known/active marks、命名 counter 对象、身份 marker）；journal 链连续性 + `acked_final_seq >= journal_base_seq` 等水位不变量；授权域行 ↔ 内核 active set。
3. **跨组件**：panel DISPATCH 密封 payload hash ↔ daemon APPLY sealed intent hash（同 apply_request_id 必同 hash）；panel ledger 折叠序列 ↔ daemon FINAL 序列。

时机：操作终态后审触碰域；启动全量；测试中每步全量。审计失败 = 拒绝服务托管路径（fail-closed），不是记日志继续跑。

### 测试策略

1. **Dataplane 合同测试**：同一套行为测试跑 fake 与真实 netns 两个实现。fake 必须实现生产合同——FenceAdmission 只摘指定 marks、Sync 容忍非空 active、Install 原子替换语义。跑不过合同套件的替身不允许存在。
2. **kill 矩阵**：从状态机定义枚举全部事务边界与副作用边界为 kill 点，逐点注入崩溃 → 恢复 → 全量审计 + 终态断言（同一 ACK 字节、零丢账、零重复计费）。全遍历即"可确定重放"的机器证明。
3. **采样真值**：计量测试输入只能来自内核读数（netns 实测或录制回放），禁止预写 totals 直接注入折叠结果。
4. **双进程恢复**：daemon 真实重启（同一 store 文件），覆盖 renew 跨 boot 过期、BOOT_FENCE、store 与内核不一致等路径。
5. **跨仓 golden fixture**：新契约在 NF 与 SB 两仓各存一份、golden test 逐字节钉死。

## G. 工程分解

从 main 新开分支，九个可独立验收的阶段：

| # | 阶段 | 内容 | 验收 |
|---|---|---|---|
| 1 | 密封基座 | canonical 编码扩展（intent/effect_plan/result 三类 blob）、SealedOperation 类型、daemon store DDL 一次成型 | 编解码 round-trip + hash 向量测试 |
| 2 | 根与审计 | 六域 domain_hash + authoritative_root + 增量维护 + 全量审计器 | 双算法对账测试；审计器先于一切写路径存在 |
| 3 | 执行器 + FINAL | 4 事务状态机、readback 合同、FINAL 单入口、journal hash 链移植 | 单操作 kill 矩阵全绿 |
| 4 | 计量身份 | 命名 nft counter 对象、lane_id 采样、userspace 同身份、分级账本 wire | netns 采样真值测试 |
| 5 | daemon 操作全集 | APPLY/RENEW/EXPIRE/BOOT_FENCE/SHUTDOWN_FINAL、lease_loop 对接 | 全操作 kill 矩阵 + 双进程恢复 |
| 6 | panel 操作化 | integration_operations、controller_root、DISPATCH 闭环、fence 持久化 | panel 重启 readiness 重算测试 |
| 7 | 新 HTTP 契约 | 端点 + openapi 双份 + 跨仓 fixture | 契约 golden test |
| 8 | 测试收口 | kill 矩阵全遍历、双进程、netns 合同测试成体系 | 全套件绿 |
| 9 | 普通规则并轨 | DELTA lane 折入既有计费表，删除 (proto,port) 反推路径 | 既有计费测试语义不变 |

依赖关系：1→2→3 是地基串行；4 依赖 3；5 依赖 3、4；6 可与 5 并行（wire 以 fixture 先行冻结）；7 依赖 6；8 贯穿；9 依赖 4，可最后独立落地。

## 强制不变量清单

1. config_epoch / integration_epoch / generation / mark ordinal / journal seq / lane raw / priced_units / billable 只增不减（回收仅允许在 ack 水位之下）。
2. 至多一个非终态 daemon operation；root 只在操作终态事务推进。
3. 每个操作以 seal/final 两个边界事务闭合；中间事务与副作用只允许出现在两者之间、挂在该操作名下，副作用必须幂等、可 readback。
4. 执行与恢复的输入只准来自密封面；readback 是唯一 live 读且只判"已落地与否"。
5. pre-fence 只使用密封旧态集；数据面替换只经 InstallStaged 原子路径。
6. 任何 lane 终结必经 FINAL 单入口事务；final_record 与 lifecycle 翻转同事务。
7. 同 request_id 同 payload → 同 result_payload 字节；异 payload → 幂等冲突。
8. 跨 boot 授权一律过期，BOOT_FENCE 收口；store 丢失/损坏 fail-closed，禁止静默重建计费流。
9. effect_plan_hash 全覆盖副作用字段；数据面身份 = {generation, effect_plan_hash}。
10. 审计失败即断路托管路径；测试替身必须通过与生产实现相同的合同套件。

## 已考虑的替代方案

- **在既有分支上逐点补洞**（renew 密封、stale 原子化、boot fence 操作化、ACK result_payload）：工程量最小，但五个状态源并存的根因保留，审计器仍只能局部审计，后续每加一条路径都可能重新漂移。放弃。
- **完整 event sourcing**（外部输入全部事件化、状态由 fold 派生、恢复=纯重放）：与非事务内核交互阻抗失配——readback 事件的真值来自现实世界，重放时必须信任历史读数，反而引入新的伪造面；且本系统不需要任意历史点重建。放弃。
- **wall-clock 双轨 deadline**（跨 boot 恢复 renew 时以 wall-clock 上界续命）：引入时钟回拨/跳变的新故障面，收益仅是省去一次重新续租；SB 侧本就 10–30s 高频续租。放弃，跨 boot 一律 fail-closed 过期。
- **root 覆盖高频累计值**：每样本重写根，写放大不可接受；累计完整性已有 hash 链 + 单调不变量。放弃。
