# /api/v1 Phase 3 设计：admin 铸 token + 声明式写 + usage 聚合 + OpenAPI

## 背景与目标

`feat/api-v1` 已落地 phase 1（只读观测）与 phase 2（`my/*` 用户自助规则写）。
Phase 3 把「管理端能力」暴露到 token 化的 `/api/v1` 稳定契约上，让程序化 / AI agent
能自助开户、配额运营、观测账单，并给出机器可读的接口自描述。

本轮范围（明确边界）：
1. admin 铸 token（bootstrap 拿自己的 token）+ scope 强制。
2. admin 自动开户闭环（建用户并一并发 token）。
3. admin 声明式 SET 写（配额 / 续期 / 授权 / 限速 / resync / set-enabled）。
4. `GET /api/v1/usage` 账单聚合。
5. 手写 OpenAPI 并托管。

**明确不在本轮**（留后续 phase）：破坏性端点（删用户 / 删节点 / upgrade-all /
reset-password）、列表分页、audit 增补 `source` / `token_id` 列。

## 设计不变量（沿用 phase 1/2，改动必守）

- `/api` = session cookie 的 SPA 私有面（随前端演进）；`/api/v1` = Bearer token
  公共稳定契约。两者用**独立 DTO**（`v1Node` / `v1User` / `v1Rule` 等），禁止把
  `/api` handler 直接重挂到 `/api/v1`。
- envelope：成功 `{"data":...}`，失败 `{"error":{"code","message"}}`
  （`v1OK` / `v1Err` + `code*` 常量）。
- token 带 scope（`read` / `readwrite`）；scope 默认 `read`，既有 token 绝不被写端点
  静默提权。
- 三层中间件：`requireTokenAuth`（限流 + `last_used` 节流）→ `v1RequireRole` →
  `requireScope`。admin 写组统一叠 `requireScope(readwrite)`。
- **枚举 oracle 不变量**：普通用户按 node_name 解析失败必须与「存在但未授权」同返
  403，避免泄漏未授权节点名。**admin 可枚举全部节点**——故 admin 端 `batch-apply`
  的 `skipped_nodes`（名字解析不到就跳过并回报）语义保留，不适用 oracle 屏蔽。
- **共享核心等价**：任何被 `/api/v1` 复用的 admin 写逻辑，必须抽成 SPA 与 v1 同调的
  共享函数，行为（含再派发等副作用）严格等价。绝不在 v1 侧另写一份漂移的实现。

## Token 模型（不改 schema）

保持「每用户唯一 token」（`api_tokens.user_id UNIQUE`）。据此定语义：

- admin 开户 = 建用户 + 发其唯一 token。
- 为**已有 token** 的用户「再铸」= **轮换**（旧值失效，返回新明文一次性）。这与「重置
  某用户 API 凭据」的直觉一致。
- 不引入多 token / label / 独立 scope 的复杂度（合『接口层要薄、不过度设计』）。

## 工作流 1：admin 铸 token（bootstrap）+ scope 强制

**问题**：token 管理路由 `/my/token*` 当前挂在 SPA `/api` 面的
`requireRole("user")` 组内，admin role 被挡，一个 token 都拿不到——于是 admin 无
Bearer 去调 `/api/v1` 的 admin 端点。

**改法**（SPA `/api` 面，最小）：把 `/my/token`(GET)、`POST /my/token`、
`DELETE /my/token`、`POST /my/token/refresh`、`POST /my/token/toggle` 从
`requireRole("user")` 组移到仅 `requireAPIAuth` 的组（session 已认证即可）。
handler 不改，仍按 `u.ID` 键自己的单 token；admin session → admin 自己的 token。

scope 强制已由 `requireScope` 就位，下述所有 admin 写端点叠 `requireScope(readwrite)`。

## 工作流 2：自动开户闭环（`/api/v1` admin + readwrite）

抽 `apiCreateUser` 的共享核心 `s.provisionUser(adminID, params) (*db.User, *adminError)`
（DB 写 + 审计），SPA `apiCreateUser` 改为薄封装同调之。新增 `/api/v1` 端点：

### `POST /api/v1/users`
建用户，可选一并铸 token（闭环）。

请求体：
```json
{
  "username": "svc-bot",
  "password": "optional-omit-to-autogen",
  "role": "user",
  "max_forwards": 100,
  "traffic_quota_bytes": 0,
  "expires_at": 0,
  "issue_token": true,
  "token_scope": "readwrite"
}
```
- `password` 缺省 → 服务端生成随机（纯 API 消费者无需登录面；生成值不回显，除非产品后续
  需要，本轮不回显密码）。
- `issue_token` 默认 `true`；为 `true` 时在同一响应内建 token 并返回**一次性明文**。
- `expires_at`：unix 秒，`0` 表示不过期。

响应：
```json
{ "data": { "user": <v1User>, "token": "<plaintext-once>", "token_scope": "readwrite" } }
```
- 用户名冲突 → `409 conflict`。

### `POST /api/v1/users/{id}/token`
铸 / 轮换目标用户单 token。请求体 `{ "scope": "readwrite" }`（缺省 `read`）。

- 单 token 模型下，若用户已有 token 则**轮换**（旧值失效）。
- 需新增 DB helper `IssueUserToken(userID, scope) (plaintext string, rotated bool, err error)`：
  有则 rotate（等价 `RefreshAPIToken` + 覆写 scope），无则 create。

响应：`{ "data": { "token": "<plaintext>", "scope": "...", "token_prefix": "...", "rotated": true } }`

### `GET /api/v1/users/{id}/token`
读目标用户 token 元数据（`token_prefix` / `scope` / `disabled` / `created_at` /
`last_used_at`），**永不返明文**。用户无 token → `{ "data": { "has_token": false } }`。
此端点仅需 `read` scope（观测）。

### `DELETE /api/v1/users/{id}/token`
吊销目标用户 token。`{ "data": { "deleted": true } }`。

## 工作流 3：admin 声明式 SET 写（`/api/v1` admin + readwrite）

统一采用**声明式 SET（PUT/DELETE 绝对值）**语义，重试天然幂等，不引 `Idempotency-Key`。
从 SPA handler 抽共享核心（含再派发副作用），SPA 与 v1 同调，保证等价。

核心函数（拟置于 `internal/server/admin_core.go` 或 `shared.go`），签名统一返回
`*adminError`（`{status int; code string; msg string}`），并有 `writeAdminErrV1` 落 envelope：

| 核心 | 抽自 | 副作用 |
|---|---|---|
| `setUserQuota(adminID,userID,bytes)` | `apiSetUserQuota` | 审计 |
| `setUserMaxForwards(adminID,userID,n)` | `apiSetMaxForwards` | 审计 |
| `setUserExpiry(adminID,userID,unix)` | `apiSetUserExpiry` | 审计 + **再派发**用户所有节点 |
| `setUserEnabled(adminID,userID,enabled)` | `apiToggleUser` | 审计 + 解禁 / 再派发（声明式取代 toggle） |
| `grantUserNode(adminID,userID,nodeID,mf,bytes)` | `apiGrantNode` | 审计（ensure-granted / upsert） |
| `revokeUserNode(adminID,userID,nodeID)` | `apiRevokeNode` | 删该 grant 下规则 + fanout + 审计 |
| `setPerNodeQuota(adminID,userID,nodeID,bytes)` | `apiSetPerNodeQuota` | 审计（+ 现有再派发口径） |
| `setPerNodeRateLimit(adminID,userID,nodeID,mbytes)` | `apiSetPerNodeRateLimit` | 审计 + 现有再派发口径 |
| `batchApplyGrants(adminID,userIDs,grants)` | `apiBatchApplyGrants` | 授权 + rate 覆写 + 受影响节点 fanout |
| `resyncNode(nodeID)` / `resyncAllNodes()` | `apiResyncNode` / `apiResyncAllNodes` | 派发 |

v1 端点（全部 `v1RequireRole("admin") + requireScope(readwrite)`）：

- `PUT  /api/v1/users/{id}/quota`               `{ "traffic_quota_bytes": N }`
- `PUT  /api/v1/users/{id}/max-forwards`        `{ "max_forwards": N }`
- `PUT  /api/v1/users/{id}/expiry`              `{ "expires_at": unix|0 }`
- `PUT  /api/v1/users/{id}/enabled`             `{ "enabled": bool }`
- `PUT  /api/v1/users/{id}/grants/{nodeId}`     `{ "max_forwards": N, "traffic_quota_bytes": N }`（ensure-granted）
- `DELETE /api/v1/users/{id}/grants/{nodeId}`   （ensure-absent）
- `PUT  /api/v1/users/{id}/nodes/{nodeId}/quota`      `{ "traffic_quota_bytes": N }`
- `PUT  /api/v1/users/{id}/nodes/{nodeId}/rate-limit` `{ "rate_limit_mbytes": N }`
- `POST /api/v1/grants/batch-apply`             `{ "user_ids":[...], "grants":[{ "node_name","max_forwards","traffic_quota_bytes","rate_limit_mbytes" }] }`
- `POST /api/v1/nodes/{id}/resync`
- `POST /api/v1/nodes/resync-all`

说明：
- `expiry` v1 用 **unix 秒**（程序化更干净），与 `v1User.expires_at` 输出口径一致；
  SPA 仍收 `YYYY-MM-DD`，核心内部统一到 unix。
- `grants` 的 PUT 用路径参数 `{nodeId}`（单节点声明式）；批量沿用 `batch-apply`。
- `batch-apply` / `resync` 用 POST（批量动作 / 触发型动作），效果幂等。

## 工作流 4：`GET /api/v1/usage`（admin + read）

一次账单快照（区别于 `/users` 明细与 `/dashboard` 概览）：

```json
{
  "data": {
    "generated_at": 1751932800,
    "totals": { "users": 12, "nodes_total": 8, "nodes_online": 7, "rules": 40, "traffic_bytes": 123456789 },
    "users": [
      { "user_id": 3, "username": "svc-bot", "role": "user", "disabled": false,
        "traffic_used_bytes": 111, "traffic_quota_bytes": 0, "rule_count": 4, "expires_at": null }
    ],
    "nodes": [
      { "node_id": 5, "name": "hk-1", "node_type": "entry", "rate_multiplier": 1.0,
        "traffic_bytes": 222, "rule_count": 6 }
    ]
  }
}
```
- 用户维复用 `db.ListUsers` + `db.FillUserRuleCounts`（同 `v1ListUsers`）。
- 节点维需 per-node 流量聚合：优先复用现有 admin 聚合口径（原始流量统一视图，见近期
  `聚合视图统一显示原始流量`）；若无直接可复用的按节点求和，补一个薄
  `db.TrafficBytesByNode() map[int64]int64`（按规则计费口径求和到节点）。
- 仅需 `read` scope。

## 工作流 5：OpenAPI

- 手写 **OpenAPI 3.1** 规格，覆盖 `/api/v1` 全端点、bearer 鉴权 + scope、`v1*` DTO、
  错误 envelope 与 `code*` 枚举。以 `openapi.yaml` 为**唯一人工真源**入库。
- `go:embed` + **`GET /api/v1/openapi.json` 免鉴权托管**：挂在 `requireTokenAuth`
  **之外**的独立组（`registerV1Routes` 用 `r.Use(requireTokenAuth)` 覆盖全组，故 openapi
  端点须在该组外单独注册，例如在 `Router()` 里 `r.Get("/api/v1/openapi.json", ...)`
  先于 `r.Route("/api/v1", ...)`，或在 route 内用独立子组不 `Use` 鉴权）。
- yaml→json 落地：go.mod **无 yaml 依赖**。倾向零新运行时依赖，实现计划在两方案里定：
  (a) 提交 `openapi.yaml`（源）+ 由 `go:generate` 生成并提交 `openapi.json`（embed + 托管）；
  (b) 直接手写 `openapi.json` 为托管产物、`openapi.yaml` 作可读文档。默认倾向 (a)，
  避免手工双写漂移。

## 路由落点小结（`registerV1Routes`）

```
/api/v1 (requireTokenAuth)
  read（任意 token）:      /info /probe /probe-chain
  user read / readwrite:   /my/nodes /my/rules*（已有）
  admin read:              /nodes /users /dashboard /usage /users/{id}/token(GET)
  admin readwrite:         POST /users; POST/DELETE /users/{id}/token;
                           PUT quota|max-forwards|expiry|enabled;
                           PUT/DELETE grants/{nodeId}; PUT nodes/{nodeId}/quota|rate-limit;
                           POST grants/batch-apply; POST nodes/{id}/resync; POST nodes/resync-all
/api/v1/openapi.json（无鉴权，独立注册）
```

## 测试要点

- bootstrap：admin session 能创建 / 读 / 刷新 / 停用自己的 token。
- 闭环：`POST /api/v1/users {issue_token,token_scope}` 返回可用 token，随即用该 token
  过 `requireScope(readwrite)` 调 admin 写端点。
- scope 强制：`read` token 调任一写端点 → 403 `scope_required`；既有默认 `read`
  token 不能写。
- 声明式幂等：同一 PUT 重复两次结果一致、无副作用叠加；`revoke` 二次为 no-op 成功。
- 共享核心等价：v1 改配额 / 续期 / 授权后，SPA 读到同样状态；expiry / revoke 的再派发
  确实触发（与 SPA 路径一致）。
- 枚举 oracle：普通 token 仍 403 屏蔽未授权节点名；admin `batch-apply` 正常回报
  `skipped_nodes`。
- `/usage`：totals 与 users/nodes 明细自洽；仅 admin 可达。
- OpenAPI：`GET /api/v1/openapi.json` 无 token 可取，返回合法 JSON 且端点齐全。
