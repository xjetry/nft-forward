# Per-Node 流量配额设计

## 概述

将用户流量配额从"全局共享一个总量"扩展为"可按节点单独配置"，同时支持节点流量倍率和自动周期重置。

## 数据模型

### nodes 表新增

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `traffic_multiplier` | REAL | 1.0 | 流量计费倍率，作用于全局配额累计 |

### user_nodes 表新增

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `traffic_quota_bytes` | INTEGER | 0 | per-node 专有配额，0 = 无专有配额 |
| `traffic_used_bytes` | INTEGER | 0 | per-node 已用流量（原始字节） |

### users 表新增

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `traffic_reset_days` | INTEGER | 0 | 自动重置周期天数，0 = 永不重置 |
| `last_traffic_reset_at` | INTEGER | 0 | 上次自动重置的时间戳，用于判断是否已进入新周期 |

### Go struct 变更

```go
// Node 新增
TrafficMultiplier float64 `json:"traffic_multiplier"`

// UserNode 新增
TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
TrafficUsedBytes  int64 `json:"traffic_used_bytes"`
```

nodeCols / scanNode / grants.go inline scan 三处对齐更新。

## 流量计量

### 配额模型：默认回退

- per-node 配额优先（`user_nodes.traffic_quota_bytes > 0` 时启用）
- 未配置 per-node 配额的节点不做 per-node 跟踪，流量仅通过倍率影响全局配额

### 三层独立扣除

以组合节点 X(A→B→C) 为例，实际流量 1GB：

| 层级 | 计量方式 | 扣除量 |
|------|---------|--------|
| 全局配额 | Σ(字节 × 节点倍率) | 1GB×A倍率 + 1GB×B倍率 + 1GB×C倍率 |
| 组合节点 X 专有配额（如配置） | 原始字节，1倍 | +1GB |
| 组成节点 A/B/C 各自专有配额（如配置） | 原始字节，1倍 | 各 +1GB |

### applyCounters 改造

- 移除 `EntryRuleHopIDs` 调用和 entry-hop 过滤
- 预加载 `nodeMultipliers map[int64]float64`
- 对每个 sample：
  - per-node：`AddUserNodeTraffic(db, userID, nodeID, bytesDelta)` — 原始字节
  - 全局：`AddUserTraffic(db, userID, int64(math.Round(float64(bytesDelta) * multiplier)))` — 乘以倍率
- `OnTrafficUpdate` 回调传入 `(userID, nodeID)` 以触发两层检查

## 超额执行

### Per-node 超额

当某节点 N 的 per-node 配额耗尽：

- 仅影响该用户在节点 N 上的规则，以及任何 hop 链包含节点 N 的规则
- 不禁用用户，不影响其他未受限节点
- 实现方式：在 `ActiveRuleHopsForPush` 查询中增加排除条件

### 全局超额

行为不变：禁用整个用户，re-dispatch 所有受影响节点。

### ActiveRuleHopsForPush 查询改造

新增排除条件——规则的任一 hop 所在节点对该用户的 per-node 配额已耗尽：

```sql
EXISTS (
  SELECT 1 FROM rule_hops rh2
  JOIN user_nodes un ON un.user_id = r.owner_id AND un.node_id = rh2.node_id
  WHERE rh2.rule_id = r.id
    AND un.traffic_quota_bytes > 0
    AND un.traffic_used_bytes >= un.traffic_quota_bytes
)
```

### Re-dispatch 范围

节点 N 超额 → 找出所有经过 N 的规则 → 这些规则涉及的所有节点都需要 re-dispatch。

### 恢复

管理员重置 per-node 用量或调大配额后，re-dispatch 受影响节点使规则重新生效。

## 自动周期重置

### 逻辑

- 基于 `users.created_at` 和 `traffic_reset_days` 计算周期
- `cycle_start = created_at + N * reset_days * 86400`（N 为使 cycle_start ≤ now 的最大整数）
- `traffic_reset_days = 0` 时永不自动重置
- 检查时机：`applyCounters` 累加流量前，发现跨周期则先重置再累加
- 重置范围：全局 `traffic_used_bytes` + 所有 per-node `traffic_used_bytes`
- 重置后如用户因流量超额被禁用，自动解除禁用并 re-dispatch

### 跨多周期

用户长期无流量跨了多个周期时，直接重置到当前周期，不逐周期回放。

## API 接口

### 新增

| 方法 | 路径 | 用途 |
|------|------|------|
| POST | `/nodes/{id}/multiplier` | 设置节点流量倍率 |
| POST | `/users/{id}/nodes/{nodeId}/quota` | 设置 per-node 专有配额 |
| POST | `/users/{id}/nodes/{nodeId}/reset-traffic` | 重置 per-node 用量 |
| POST | `/users/{id}/reset-days` | 设置重置周期天数 |

### 修改

| 接口 | 变更 |
|------|------|
| `POST /users/{id}/reset-traffic` | 同时清零全局 + 所有 per-node 用量，超额禁用时自动解禁并 re-dispatch |
| `POST /users/{id}/grant-nodes` | 增加可选 `traffic_quota_bytes` 参数（默认 0） |
| GET 用户详情/full-view | 返回 per-node 配额、用量、重置周期 |
| GET 节点列表/详情 | 返回 `traffic_multiplier` |

### 数据格式

```json
// 设置 per-node 配额
{"traffic_quota_bytes": 10737418240}

// 设置节点倍率
{"traffic_multiplier": 0.5}

// 设置重置周期
{"traffic_reset_days": 30}

// Grant 节点时可选带配额
{"node_ids": [1,2], "max_forwards": 10, "traffic_quota_bytes": 5368709120}
```

## 迁移

```sql
ALTER TABLE nodes ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0;
ALTER TABLE user_nodes ADD COLUMN traffic_quota_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_nodes ADD COLUMN traffic_used_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN traffic_reset_days INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN last_traffic_reset_at INTEGER NOT NULL DEFAULT 0;
```

### 向后兼容

- `traffic_multiplier` 默认 1.0：现有节点行为不变。中继节点升级后倍率为 1.0，如需维持"中继不计费"，管理员需手动设为 0
- `user_nodes` 新字段默认 0：无专有配额，无已用量，行为不变
- `traffic_reset_days` 默认 0：永不自动重置，保持现有手动重置行为

## 边界情况

- **revoke 节点授权**：per-node 数据随 `user_nodes` 行删除
- **节点被删除**：`ON DELETE CASCADE` 自动清除
- **倍率修改**：立即生效于后续流量，已累计的全局用量不追溯调整
- **重置周期修改**：基于 `created_at` 重新计算，下次流量到来时按新周期判断
- **管理员 bypass**：节点 owner 自动 bypass grant 检查，无 per-node 配额限制
