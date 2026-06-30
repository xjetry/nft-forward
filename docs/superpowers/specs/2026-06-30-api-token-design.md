# API Token — 用户自助查询接口

## 概述

允许用户创建 API Token，通过 Token 认证的只读端点查询自己的账户信息。适用于外部脚本/监控工具、第三方面板集成、Telegram Bot 等场景。

## 数据模型

新建 `api_tokens` 表（migration `0021_api_tokens.sql`）：

```sql
CREATE TABLE IF NOT EXISTS api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token        TEXT    NOT NULL UNIQUE,
    disabled     BOOLEAN NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
```

约束说明：

- `user_id UNIQUE` — 每用户最多一个 token
- `ON DELETE CASCADE` — 删除用户时自动清理
- `token UNIQUE` — 用于快速查找

Token 格式：`db.RandToken(32)` 生成 64 字符 hex 字符串。

## API 端点

### Token 管理（Session 认证，`/api/my/token`）

挂在现有 user routes 下，使用 `requireAPIAuth` + `requireRole("user")` 中间件。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/my/token` | 获取 token 状态（有/无、是否停用、创建时间、最近使用时间）。不返回明文，仅返回前缀供辨识 |
| POST | `/api/my/token` | 创建 token，返回明文（仅此一次） |
| DELETE | `/api/my/token` | 删除 token |
| POST | `/api/my/token/refresh` | 刷新：生成新 token 值，旧的立即失效，返回新明文 |
| POST | `/api/my/token/toggle` | 切换停用/启用状态 |

### 公开查询端点（Token 认证，`/api/v1/info`）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/info` | 返回完整用户信息 JSON |

认证方式（优先级从高到低）：

1. `Authorization: Bearer <token>`
2. `?token=<token>`

### `/api/v1/info` 响应结构

```json
{
  "username": "alice",
  "traffic_used": 1073741824,
  "traffic_quota": 10737418240,
  "traffic_reset_days": 30,
  "last_traffic_reset_at": 1719676800,
  "expires_at": 1735689600,
  "rule_count": 5,
  "max_forwards": 10,
  "nodes": [
    {
      "name": "HK-01",
      "rule_count": 2,
      "rate_multiplier": 1.5,
      "unidirectional": false,
      "traffic_used": 536870912,
      "traffic_quota": 5368709120
    }
  ]
}
```

- 流量字段统一用 bytes（整数），调用方自行格式化
- `expires_at` 为 `null` 表示永不过期
- `nodes` 只包含已授权节点

## 认证中间件

新建 `requireTokenAuth` 中间件，独立于现有 `requireAPIAuth`：

1. 读 `Authorization: Bearer <token>` header
2. 若无，读 `?token=` 查询参数
3. 都无 → 401
4. 查 `api_tokens` 表 → 不存在 → 401
5. `token.disabled` → 403 "Token 已停用"
6. 查 `users` 表 → `user.disabled` → 403 "账号已被禁用"
7. 更新 `last_used_at`
8. 将 user 注入 context（复用 `userKey`），继续

## 前端

在用户 Dashboard 页面新增 Token 管理卡片，与流量/规则等信息并列。

### 无 Token 状态

显示「创建 API Token」按钮。

### 有 Token 状态

- Token 前缀（`a3f8b2...`）、创建时间、最近使用时间
- 状态标签：启用（绿）/ 停用（灰）
- 操作按钮：停用/启用、刷新、删除

### 创建/刷新后

Modal 显示完整 token 明文 + 一键复制按钮，提示「请立即保存，关闭后无法再次查看」。

## 不在范围内

- **限流** — 项目当前无限流机制，不在此引入
- **Token 过期时间** — 用户手动管理（停用/删除/刷新），保持简单
- **管理员管理用户 Token** — 用户自助，管理员删用户时 CASCADE 清理即可
