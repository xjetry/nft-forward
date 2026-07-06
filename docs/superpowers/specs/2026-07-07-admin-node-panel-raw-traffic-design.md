# 管理员聚合视图统一显示原始流量

## 背景与问题

管理员的节点列表页和 Dashboard 目前展示一列「流量」,数据来自 `NodeTrafficSums`
(`SELECT node_id, SUM(traffic_used_bytes) FROM user_nodes GROUP BY node_id`)——
即该节点上所有用户授权的当期用量之和。

这个累加值没有物理意义:它把**不同用户、不同计费口径**的数字混加在一起——
有的用户走单向计费(只计上行)、有的走双向,且每个用户按各自的流量周期独立重置
清零。跨这些口径求和,得到的既不是节点真实转发量,也不对应任何单一计费口径。

`node_raw_traffic.raw_bytes` 才是干净的节点指标:节点真实转发字节(上行+下行),
不乘倍率、不做单向裁剪、不随任何用户周期重置。这才是管理员想看的"这个节点转发
了多少"。

## 目标

管理员的**聚合视图**(节点列表页 + Dashboard)统一只显示原始流量,移除计费口径的
跨用户累加列。

单个用户视角的流量、以及 per-规则的流量不在此列(口径单一、有意义),保持不变。

## 设计

### 组合节点的原始流量

原始流量按**物理节点**记账(`AddNodeRawTraffic` 以上报 counter 的物理 node_id 写入)。
组合节点是逻辑编排单元,自身没有物理转发量,因此在 `node_raw_traffic` 中没有记录。

组合节点显示其**入口物理子节点的原始流量**,作为该组合链吞吐的代表值。入口节点通过
`ResolveCompositeHops` 展平组合(含嵌套)后取首个物理 hop 的节点。

已知近似:入口节点的 raw 是该物理节点的全部转发量,包含它上面其他规则/组合的流量,
并非该组合专属。这是可接受的展示层近似——真实精确的分解需要看物理节点本身。

### 后端

1. `apiListNodes`(`internal/server/api.go`):在返回的 `node_raw_traffic` map 中,
   为每个组合节点补入其入口物理子节点的 raw。前端因此无需感知组合展平,直接读
   `node_raw_traffic[组合id]` 即可。
2. `apiDashboard`(`internal/server/api.go`):响应新增 `node_raw_traffic` 字段,
   组合入口填充逻辑与 `apiListNodes` 一致(抽成共用 helper,避免两处口径漂移)。
3. 若 `NodeTrafficSums` / `node_traffic` 在这两个 endpoint 移除列后不再被读取,
   一并移除该查询,省去每次加载的 `SUM … GROUP BY`。

### 前端

4. 节点列表 `web/src/pages/nodes/List.jsx`:
   - 删除「流量」列(`node_traffic`)。
   - 保留的原始流量列标题为「原始流量」,并在**单点 tab 和组合 tab 都显示**
     (移除现有的 `tab !== 'composite'` 限制)。
   - 排序按原始流量。
   - 移动端卡片同步:去掉「流量」,保留原始流量。
5. Dashboard `web/src/pages/Dashboard.jsx`:两处流量数字由 `node_traffic[n.id]`
   改为 `node_raw_traffic[n.id]`(桌面表格 + 移动卡片)。

## 不变量

- 节点详情页「经过该节点的规则」表的「流量」列不动:它是 per-规则的
  `rule_hops.total_bytes`(单规则、单入口用户,口径一致,有意义)。
- 原始流量记账逻辑(`hub.go` 的 `applyCounters` / `rawAdd`)不动。
- 组合入口节点始终取展平后的首个**物理**节点。
- 若入口物理节点无 raw 记录(从未转发),显示 0。

## 不做

- 不给组合节点新增独立的 raw 计数(逻辑节点无物理转发量,不值得为展示引入记账)。
- 不改动单用户视角的流量展示。
- 不改动 per-规则流量口径。
