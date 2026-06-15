# 节点管理 JSON API 设计

## 背景与目标

当前节点管理（增删改查、resync）只有 admin 登录后通过 Web 表单操作的 HTML handler：
请求是 form-encoded、认证靠 `nft_session` cookie、响应是重定向 + flash cookie。
这套机制无法在脚本 / CI 中可靠驱动。

目标：提供一组 JSON HTTP API，使节点的完整 CRUD 可被程序化调用，与现有 HTML 后台
行为一致、互不干扰。

非目标：forwards / chains / tenants 等其它资源的 API（本期只做 nodes）；图形化的
API 令牌管理界面（本期用静态注入）。

## 认证

- 独立的 admin 级静态密钥，不绑定具体 user。
- 配置来源（优先级从高到低）：
  - server 子命令新增 flag `--api-token`；
  - 环境变量 `NFT_FORWARD_API_TOKEN`。
- `server.New` 增加可选配置入口，把 token 存入 `Server.apiToken`。
- 新增中间件 `requireAPIToken`：
  - 从 `Authorization: Bearer <token>` 取值，用 `crypto/subtle.ConstantTimeCompare`
    与 `s.apiToken` 比对；
  - `s.apiToken == ""`（未配置）→ 所有 `/api/v1/*` 请求返回
    `401 {"error":"API access disabled"}`，即默认关闭，必须显式注入才启用；
  - 缺失 / 不匹配 → `401 {"error":"unauthorized"}`。
- 审计：`audit_logs.user_id` 无外键约束，API 操作以哨兵 `user_id = 0` 写审计，
  action 沿用现有动词（`node.create` / `node.delete` / `node.rename` / `node.set_relay_host`），
  便于与 HTML 路径在审计流中统一查询。

## 路由与语义（`/api/v1/nodes`）

挂在独立的路由分组下，仅经过 `requireAPIToken`（不经过 `requireAuth` / session）。

| 方法 | 路径 | 作用 | 成功响应 |
|------|------|------|----------|
| `GET` | `/api/v1/nodes` | 列出所有节点 | `200 {"nodes":[<node>...]}` |
| `POST` | `/api/v1/nodes` | 创建节点，body `{"name":"...", "secret":"(可选)"}` | `201 <node-with-install>` |
| `GET` | `/api/v1/nodes/{id}` | 节点详情 | `200 <node>` |
| `PATCH` | `/api/v1/nodes/{id}` | 修改 `{"name":"(可选)", "relay_host":"(可选)"}` | `200 <node>` |
| `DELETE` | `/api/v1/nodes/{id}` | 删除节点并重连受影响链路 | `204`（无 body） |
| `POST` | `/api/v1/nodes/{id}/resync` | 触发对该节点重新下发 | `200 {"ok":true}`，失败 `502 {"error":...}` |

### `<node>` JSON 形状

序列化自 `db.Node`，只暴露稳定字段：

```json
{
  "id": 7,
  "name": "rfc-jp-t1",
  "relay_host": "203.0.113.9",
  "online": true,
  "agent_version": "0.5.0",
  "last_seen": 1718000000,
  "node_kind": "remote",
  "created_at": 1717900000
}
```

不暴露 `secret`（仅在 create 响应里回显一次）、不暴露 legacy push-era 字段。

### 创建响应（含注册凭据）

`POST` 的 201 响应在 `<node>` 基础上追加：

```json
{
  "...": "(node 字段)",
  "secret": "<agent token>",
  "install_command": "curl -fsSL .../install.sh | sudo bash -s -- agent --panel-url <panel> --token <secret>"
}
```

`install_command` 中的 panel 地址复用 `showNode` 现有推断逻辑：优先 `panel_url`
setting，未配置时回退到请求的 `Host`。

### 校验与错误

- `POST` name 为空 → `400 {"error":"name 不能为空"}`。
- `PATCH` name 显式传空串 → `400`；`relay_host` 传空串表示"清空"（与 HTML 一致），
  传非空但非 IPv4 / 非域名 → `400`（复用现有 `net.ParseIP` + `resolver.IsHostname` 校验）。
  字段缺省（JSON 中不出现该 key）表示不改动该字段。
- `{id}` 非法或节点不存在 → `404 {"error":"节点不存在"}`。
- body 非法 JSON → `400 {"error":"invalid json"}`。

## 重构（共享核心，避免 HTML/JSON 逻辑重复）

现有几个 mutation 辅助函数把"下发副作用"和"写 flash cookie"耦合在一起，
JSON 路径不能写 flash。提取无 HTTP 的核心，HTML/JSON 各包一层：

- `dispatchAfterFanout(w, nodeIDs, action)` → 提取
  `fanoutDispatch(nodeIDs) []failure`（返回每节点失败明细）。HTML 封装把失败拼成
  flash；JSON 路径目前不需要把 fanout 失败回吐到响应（删除是 `204`），但失败仍按现有
  方式落到各节点 `last_error` + 日志。
- `rewireChainsAfterNodeChange(w, chainIDs, action)` → 提取核心，返回受影响节点
  与错误；HTML/JSON 复用。
- `dispatchAfterMutation(w, nodeID, action)` 的核心就是已解耦的 `dispatchToNode`，
  JSON create/patch 直接调用它并在失败时通过响应字段或日志体现。

JSON 工具：新增 `writeJSON(w, status, v)` 与 `writeJSONError(w, status, msg)`。

create/delete/rename/relay_host 全部复用现有 `db.CreateNode`、`db.DeleteNode`、
`db.RenameNode`、`db.UpdateNodeRelayHost`、`db.ChainsReferencingNode`，保证与 HTML
路径行为一致。

## 文件改动

- `cmd/nft-forward/main.go`：server 子命令新增 `--api-token` flag + env 读取，传入
  `server.New`。
- `internal/server/server.go`：`Server` 增 `apiToken` 字段；`New` 接受配置；
  `Router()` 注册 `/api/v1` 分组。
- `internal/server/auth.go`（或新文件 `api_auth.go`）：`requireAPIToken` 中间件。
- 新文件 `internal/server/api_nodes.go`：6 个 JSON handler + node 序列化。
- `internal/server/helpers.go`：`writeJSON` / `writeJSONError`。
- `internal/server/server.go` / `chains.go`：上述 mutation 辅助函数重构。
- 新文件 `internal/server/api_nodes_test.go`：测试。

## 测试

`internal/server/api_nodes_test.go`，沿用 `handlers_admin_test.go` 的搭建模式：

- 认证：未配置 token（401 disabled）、配置后无 Authorization（401）、错 token（401）、
  对 token（放行）。
- create：成功返回 201 + secret + install_command；空 name → 400；重复 / db 错误路径。
- list / get：内容正确；get 不存在 → 404。
- patch：改名生效；空 name → 400；非法 relay_host → 400。
- delete：节点消失、引用该节点的链路被重连（验证副作用），返回 204。
- 序列化：响应不含 `secret`（除 create）与 legacy 字段。

## 向后兼容

纯增量：不动现有 HTML 路由与行为。未设置 `--api-token` / `NFT_FORWARD_API_TOKEN`
时，`/api/v1` 全部 401，等同于"未启用"，不影响既有部署。
