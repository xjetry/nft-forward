# Per-Node 配额 UI + 倍率迁移 + 端口范围

## A. 倍率迁移（nodes → node_hops）

倍率是组合节点跳序的属性，同一物理节点在不同组合链中可有不同倍率。

### DB 迁移 0014

```sql
ALTER TABLE node_hops ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0;
```

`nodes.traffic_multiplier` 列保留（SQLite 兼容），代码中移除所有引用。

### Go struct 变更

- `Node` 移除 `TrafficMultiplier`；`nodeCols`/`scanNode`/`grants.go` inline scan 同步移除
- `NodeHop` 加 `TrafficMultiplier float64`
- `scanNodeHop` 和 `ListNodeHops`/`ListAllNodeHops` 的 SELECT 同步追加

### applyCounters 改造

```go
hopMultipliers := db.HopMultipliers(h.DB) // map[compositeNodeID]map[physicalNodeID]float64

mult := 1.0
if r.NodeID != nodeID {
    if m, ok := hopMultipliers[r.NodeID][nodeID]; ok {
        mult = m
    }
}
weighted := int64(math.Round(float64(s.BytesDelta) * mult))
```

直接规则（r.NodeID == nodeID）倍率固定 1.0。

### 删除 NodeMultipliers

旧函数 `NodeMultipliers(d)` 删除，替换为 `HopMultipliers(d)`。

### API

现有 `POST /nodes/{id}/hops` 已接受跳序更新。扩展请求体，每跳加 `traffic_multiplier` 字段。

### UI

组合节点详情页 → 跳序表格（已有 mode 下拉框），每跳追加倍率输入框，保存时一起提交。

## B. 端口范围（添加节点）

### 现状

`nodes.port_range` 已有字段（默认 `10001-20000`），创建节点时后端设默认值。

### 前端变更

添加节点对话框中新增两个可选输入框：
- 起始端口（默认 10000）
- 结束端口（默认 19999）

创建节点时传 `port_range: "10000-19999"` 到后端。

### 安装脚本

节点详情页生成的安装命令中，如果 `port_range` 不是默认值，拼入 `--port-range <start>-<end>` 参数。

## C. 用户详情页 — per-node 配额

### 授权节点表格扩展

在现有 `max_forwards` 列之后追加三列：

| 列 | 内容 |
|----|------|
| 流量配额 | `PerNodeQuotaForm` 内联表单，输入 GB，POST `/users/{id}/nodes/{nodeId}/quota` |
| 已用流量 | `fmtTrafficGB(used, quota)`，quota=0 显示 "∞" |
| 操作 | 重置按钮，confirm 后 POST `/users/{id}/nodes/{nodeId}/reset-traffic` |

### ResetDaysForm

在现有 `ExpiryForm` / `MaxForwardsForm` / `QuotaForm` 行中追加 `ResetDaysForm`：
- 输入天数，0=永不重置
- POST `/users/{id}/reset-days`

### 流量显示增强

用户信息区的流量行增加重置周期显示：`12.3 / 50.0 GB (每30天重置)` 或 `(不自动重置)`。

## D. 节点详情页

不新增倍率编辑（已移到组合节点跳序页）。无变更。
