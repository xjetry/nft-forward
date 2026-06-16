# TUI-Server 规则同步与统一管理

## 概述

将 TUI 从本地独立模式升级为 server 感知的统一客户端。连接 server 时所有规则操作走 server API（server 权威），断开时本地管理。支持升级迁移、降级保留、端口自动分配。同步更新 WebUI 节点编辑和规则展示。

## 1. 数据模型变更

### nodes 表增加 owner_id

```sql
ALTER TABLE nodes ADD COLUMN owner_id INTEGER REFERENCES users(id);
```

- 节点操作者，admin 创建/配置节点时设置
- `EnsureSelfNode()` 自动设 self-node 的 owner_id 为 admin 用户
- TUI 创建规则时 server 用 `node.owner_id` 作为 `rule.owner_id`
- owner 自动拥有对该节点的 grant（无需单独授权）

### nft.Rule 增加 HopCount

```go
HopCount int `json:"hop_count,omitempty"`
```

- metadata，不参与 rev hash
- server 在 `buildRules()` 时填充
- TUI 字段锁定判断：`HopCount > 1` 锁定 proto/dest

### 端口范围

本地模式自动分配使用 `[ChainPortMin=10001, ChainPortMax=20000]`，与 server 一致。

## 2. WebSocket 协议扩展

### 新增消息

| 消息 | 方向 | 用途 |
|------|------|------|
| `rule_create` → `rule_cmd_ack` | node→server | TUI 创建单跳规则 |
| `rule_update` → `rule_cmd_ack` | node→server | TUI 编辑单跳规则全字段 |
| `migrate_rules` → `rule_cmd_ack` | node→server | 升级时批量迁移本地规则 |

### rule_create

```go
type RuleCreate struct {
    Proto      string
    ExitHost   string
    ExitPort   int
    ListenPort int    // 0=自动分配
    Mode       string
    Comment    string
    Name       string
}
```

Server: 查 node.owner_id → quota 检查 → CreateRule + RegenerateRule(1-hop) → dispatch → ack(entry)

### rule_update

```go
type RuleUpdate struct {
    RuleID     int64
    Proto      string
    ExitHost   string
    ExitPort   int
    ListenPort int    // 0=保持原值
    Mode       string
    Comment    string
    Name       string
}
```

Server: 验证 owner + hop_count==1 → UpdateRuleHeader + RegenerateRule → dispatch → ack(entry)

### migrate_rules

```go
type MigrateRules struct {
    Rules []nft.Rule
}
```

Server: 查 owner_id → 批量 CreateRule + RegenerateRule → 任一失败全部回滚 → dispatch → ack

### 移除 panel_segment_edit

不再需要 fire-and-forget 快照机制，所有操作走精确命令。

## 3. Daemon 架构

### HTTP API

```
GET  /v1/rules          → []nft.Rule (扁平列表)
POST /v1/rules          → CreateRuleReq → {entry, listen_port}
PUT  /v1/rules/{id}     → UpdateRuleReq → ok
DELETE /v1/rules/{id}   → ok
GET  /v1/status         → {connected, node_name, node_id}
GET  /v1/health         → {ok: true} (保留)
```

移除 `GET /v1/ruleset`, `POST /v1/ruleset/{owner}`, `POST /v1/rule/edit`, `POST /v1/rule/delete`。

### 连接模式路由

`d.Dialer() != nil` 判断路由：

| 操作 | 已连接 | 未连接 |
|------|--------|--------|
| List | 返回 "panel" segment | 返回 "tui" segment |
| Create | WS rule_create | 加入 "tui" + auto-assign port + apply |
| Edit(单跳) | WS rule_update | 更新 "tui" 中对应规则 + apply |
| Edit(组合) | WS rule_hop_edit | 不可能（本地无组合规则） |
| Delete | WS rule_delete | 从 "tui" 移除 + apply |

### 升级迁移

触发：dialer hello_ack 成功后，检查 "tui" segment 非空。

1. 发送 `migrate_rules`（全部 tui 规则）
2. ack OK → 清空 tui segment → 保存 state.json
3. server dispatch apply_ruleset → 存入 panel segment
4. ack error → 不清空，tui 规则继续本地生效

### 降级保留

触发：daemon 启动时无 `--connect` 但 panel segment 非空。

1. 遍历 panel 规则，清除元数据（RuleID=0, RuleName="", HopCount=0）
2. 合并入 tui segment（跳过端口冲突的）
3. 清空 panel + agent_meta
4. 保存 state.json → apply kernel

### 本地端口自动分配

`PickLocalFreePort(proto)`: 收集所有 segment 已占用端口 → `PickFreePort(10001, 20000, occupied)`

## 4. TUI 变更

### daemonClient 接口

```go
type daemonClient interface {
    Status() (StatusResp, error)
    ListRules() ([]nft.Rule, error)
    CreateRule(CreateRuleReq) (CreateRuleResp, error)
    UpdateRule(id string, UpdateRuleReq) error
    DeleteRule(id string) error
}
```

### 字段锁定矩阵

| 字段 | 本地(RuleID=0) | 单跳server(HopCount=1) | 组合(HopCount>1) |
|------|----------------|------------------------|------------------|
| Proto | 可编辑 | 可编辑 | 锁定 |
| SrcPort | 可编辑 | 可编辑 | 可编辑 |
| DestIP | 可编辑 | 可编辑 | 锁定 |
| DestPort | 可编辑 | 可编辑 | 锁定 |
| Mode | 可编辑 | 可编辑 | 可编辑 |
| Comment | 可编辑 | 可编辑 | 可编辑 |

### SrcPort 允许为空

输入为空时 listen_port=0（自动分配）。placeholder 显示 "自动"。

### 状态栏

连接时：`已连接 {node_name}`。未连接时：`本地模式`。

### 每次操作后刷新

所有 CRUD 操作完成后 `ListRules()` 重新加载。

## 5. Server Hub 变更

### handleRuleCreate

查 node.owner_id → quota → CreateRule + RegenerateRule(1-hop) → dispatch → ack

### handleRuleUpdate

验证 owner + hop_count==1 → UpdateRuleHeader + RegenerateRule → dispatch → ack

### handleMigrateRules

批量 CreateRule + RegenerateRule → 任一失败全部回滚 → dispatch → ack

### buildRules() 填充 HopCount

批量查询 `RuleHopCounts(ruleIDs)` → 填入每条 rule 的 HopCount。

### EnsureSelfNode() 设置 owner_id

查找 admin 用户 → `UPDATE nodes SET owner_id = ? WHERE node_type = 'self' AND owner_id IS NULL`

### grant bypass

`node.owner_id == userID` 时自动拥有节点权限，无需查 user_nodes。

## 6. WebUI 变更

### 节点详情页

- 新增禁用/启用切换按钮（`POST /api/nodes/{id}/toggle`）
- 新增节点创建时的 owner 选择器（`POST /api/nodes` 增加 owner_id 字段）
- 现有 name/relay_host 编辑保持不变

### 规则展示简化

- 规则列表、详情、表单中仅显示节点名称，不显示 IP/端口/ID/类型 badge
- 节点选择器只列名称

### 规则创建端口策略

- 移除创建表单中的 entry_port 字段（admin 和 user 均自动分配）
- 规则详情页保留 hop 端口 reallocate 表单（已有）
- admin 规则列表中 entry 列可点击内联编辑端口
