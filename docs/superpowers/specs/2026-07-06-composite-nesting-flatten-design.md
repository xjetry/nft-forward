# 组合节点嵌套展平设计

## 背景与现状

组合节点(`nodes.node_type='composite'`)是一个逻辑虚拟节点:它自己没有 agent,成员链存在 `node_hops` 表(`node_id`=组合, `hop_node_id`=物理子节点, `position`, `mode`)。它的唯一作用是让一组物理单点能被当作一个整体在规则里引用。

当前组合是**扁平单层**的:`validateCompositeChildren`(`internal/server/api.go`)硬性拒绝"成员是组合",理由是嵌套会把组合 id 落进 `rule_hops.node_id`,而那里没有 agent 可服务。因此:

- `expandSegment` 对组合只展开一层,不递归。
- 没有环检测(结构上不可能成环)。
- 展示层 `ResolveCompositeHops`/`ResolveCompositeOnline`/`ResolveCompositeRelayStack` 都是单层聚合。

已确认的现有语义(嵌套设计必须保持):

- **物化时机**:组合的展平发生在**保存规则时**——`expandSegment`/`hopsForChain` 把组合摊成物理单跳固化进 `rule_hops`,组合 id 永不入数据面。`rule_hops` 是**快照**,反映建规则那一刻的组合定义;之后改组合定义不会自动传播到已存规则。
- **计费**:整条规则的计费倍率只取入口逻辑节点的 `nodes.rate_multiplier`,中间层与组合子跳不叠加自己的倍率。
- **via 归属**:`rule_hops.via_node_id` 是逻辑段归属键,既是计费段分组键(段首收一次),也是 grant/配额影响判断键(`RulesAffectedByNode`)。用户对组合有授权即可,不要求对每个物理子节点有授权。
- **mode**:组合内部各段用各自 `node_hops.mode`;末跳 mode 休眠——作规则出口时被规则 `exit_mode` 覆盖,作中间层时保留自己配置。组合/多跳链只认 `exit_mode`,忽略传统 `mode` 别名。
- **角色**:`hopsForChain` 对入口逻辑节点校验 entry role、对每个 via 逻辑节点校验 via role(`EffectiveNodeRoles`)——校验作用于逻辑节点,不是展平后的物理点。
- **出口禁令**:`exitHopForbidsDirect` 检查整条链最终物理末跳是否 `no_direct_exit`。

## 目标

把组合从"扁平单层"放开成"可嵌套":组合的成员可以是另一个组合,任意深度。组合仍是**纯语法糖**——最终数据面只有物理单点,组合作为一个概念在 `rule_hops` 里不存在。展平从单层改成递归到物理单点,并新增环检测与规模上限。

## 非目标

- 不改物化时机:仍是保存规则时快照,改组合定义不自动传播,需手动重建规则。
- 不改计费模型:整条规则倍率仍只认入口顶层节点。
- 不引入新表或 schema 迁移。

## 核心不变量

递归展平**不改变入口、出口、最终末跳这三个边界的语义**。规则直接引用的顶层逻辑节点仍是入口/via,整条物理链的最终末跳仍是出口。因此所有挂在这些边界上的现有校验——角色校验、出口禁令、`exit_mode` 覆盖、计费段首、via 归属——都无需改动即可继续正确工作。这是"最小改动"的根据,也是评审递归实现是否正确的判据。

`node_hops` 与 `rule_hops` 表结构、CHECK 约束**保持不变**:`hop_node_id` 本就是节点引用,过去只是被应用层挡住不许指组合。改动全在应用层。

## 设计

### 递归展平 (`expandSegment`)

`expandSegment` 从单层改为递归展开到物理单点:

- 遇物理点 → 产出一跳 `HopInput{NodeID, Mode: node_hops.mode, ViaNodeID: 顶层}`。
- 遇组合 → 递归展开其 `node_hops`,产出的所有物理跳的 `ViaNodeID` **仍记规则直接引用的顶层逻辑节点**(内层纯糖透明:授权/配额/计费只认顶层,内层组合是定义者的实现细节)。
- **mode**:递归进内层时,每个物理跳的 mode 取它在**最内层**组合的 `node_hops.mode`;外层那行指向组合的 mode 字段自然不参与(内层黑盒)。

`expandSegment` 只负责产出物理跳序列。整条物理链最终末跳的 `exit_mode` 覆盖(作出口)/保留自己(作中间层)由 `hopsForChain` 在顶层对序列的最后一跳做,与现在完全一致——`hopsForChain` 按逻辑段调用 `expandSegment`,每段返回序列的最后一跳就是该段末跳,段边界逻辑不变。`RegenerateRule`/`rule_hops` 写入路径不变。

### 环检测

取代原 `validateCompositeChildren` 的"禁止嵌套"校验。在保存组合成员的两个写入路径(创建组合、改跳序)上,对组合引用图做 fail-fast 环检测:从被编辑的组合出发 DFS,若沿 `hop_node_id`(仅组合边)能回到自己(含直接自引用),拒绝保存并在错误里点明环路径。环在写入时就被挡住,保证展平必然终止。

### 规模上限

`expandSegment` 递归时累计物理跳数,超过常量 `maxFlattenedHops`(默认 32)即报错中止。理由:递归展平存在指数放大(`C2=[C1,C1]`、`C3=[C2,C2]`… 展平后 `2^n` 跳),而一条转发链物理跳过多本身无意义(每跳一个中转)。环检测保证 DFS 终止,跳数上限保证展平规模有界,二者合起来完备。展平深度被跳数上限隐含(每层至少 +1 跳),不单独设深度上限。

### 展示层递归

`ResolveCompositeHops`/`ResolveCompositeOnline`/`ResolveCompositeRelayStack` 全部改为递归到物理单点:

- **Hops**:UI 摊开显示递归展平后的物理链。
- **Online**:所有(递归展平后)物理子点都可达,组合才在线。
- **RelayStack**:入口 relay 取递归展平首个物理跳、出口取最后物理跳。

这三处在内存里对 `node_hops` 图递归展开。编辑时已挡环,展示时理论上无环;仍防御性地带 visited 集合与深度上限,避免脏数据导致死循环。

### 删除:软警告 + 二次确认

删一个被其他组合引用的节点(物理或组合)时,后端先返回受影响的组合列表,前端弹窗二次确认后再删。删除本身仍靠 `node_hops` 的 `ON DELETE CASCADE` 移除引用行。需新增查询"节点被哪些组合直接引用"(如 `db.CompositeReferrers`),`apiDeleteNode` 接入:确认前把受影响组合返回给前端。

理由:嵌套让"组合被组合引用"成为常见关系,静默 cascade 会悄悄改变上层组合定义;软警告让用户看到影响,又不硬阻断(与"糖"理念一致——组合可自由增删,只需知情)。

### 前端

- 组合编辑器(`CompositeNodeModal`、`CompositeHopsCard`):子节点下拉放开组合(去掉 `node_type !== 'composite'` 过滤),但排除组合自己与会成环的候选(前端即时提示,后端权威校验)。选中的成员是组合时,隐藏该行的 mode 选择器(内层黑盒,mode 无意义)。
- `RuleFormModal` 的链路预览 `flattenNode` 改为递归展平,显示到物理单点。
- 删节点走软警告弹窗:展示"将影响组合 X、Y",确认后再发删除请求。

## 角色与出口禁令(嵌套下的行为)

无需改动,由核心不变量保证:

- **角色校验**:`hopsForChain` 对入口/各 via 的**逻辑节点**校验 entry/via role。组合作为逻辑节点整体被判角色,内层组合与物理子点不单独校验——与"内层纯糖透明"一致。
- **出口禁令**:`exitHopForbidsDirect` 检查整条物理链的最终末跳。递归展平后末跳位置语义不变,自动覆盖(嵌套组合的最终物理末跳若是 `no_direct_exit` 节点仍被拒)。

## 测试

- **环检测**:直接自引用、A↔B 双向环、更长环链都在写入时被拒。
- **递归展平正确性**:`CO=[CI,X]`、`CI=[A,B]` 展平为 `[A,B,X]`,每跳 `via_node_id` 均记顶层,mode 取最内层配置。
- **规模上限**:指数放大构造(`Cn=[Cn-1,Cn-1]`)在超 `maxFlattenedHops` 时被拒。
- **mode 递归**:内层各段 mode 保留,整条链最终末跳被规则 `exit_mode` 覆盖(作出口)/保留自己(作中间层)。
- **Online / RelayStack**:递归聚合正确;某个深层物理子点离线使整个嵌套组合离线。
- **角色 / 出口禁令**:嵌套组合作入口/via 时校验顶层角色;嵌套组合最终物理末跳为 `no_direct_exit` 时被拒。
- **删除软警告**:删被引用节点返回受影响组合列表。
- 原 `TestCompositeCannotNestComposite` 替换为"嵌套允许 + 环被拒 + 超限被拒"。

## 受影响文件

- `internal/server/api.go`:`expandSegment` 递归;环检测取代 `validateCompositeChildren`;`apiDeleteNode` 接入软警告。
- `internal/db/queries.go`:`resolveCompositeHops`/`resolveCompositeOnline`/`resolveCompositeRelayStack` 递归。
- `internal/db/grants.go`:新增 `CompositeReferrers`(节点被哪些组合直接引用)。
- `web/src/pages/nodes/List.jsx`、`web/src/pages/nodes/Detail.jsx`:组合编辑器放开组合成员、按成员类型隐藏 mode、成环即时提示。
- `web/src/components/RuleFormModal.jsx`:`flattenNode` 递归展平预览。
- 删节点软警告弹窗(相应前端页面)。
- **无 migration**。
