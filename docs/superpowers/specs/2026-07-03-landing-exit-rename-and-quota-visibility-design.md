# 落地出口改名 + 用户侧配额可见性修复 + 重置按钮样式 — 设计

## 背景

落地出口账本（`user_landing_exits`）已经承载每用户每出口的配额与已用量，
admin 在用户详情"解析出的节点"表格里设置限额，用户侧"落地节点"页展示
"已用/总量"。本次解决三个遗留问题：

1. **用户侧配额列显示"—"**：用户在概览页粘贴了与 admin 分配节点相同
   host:port 的本地 URI 时，`mergeLanding` 本地优先，配额数据（挂在
   server 节点对象上）随之丢失，配额列退化为"—"。配额按 host:port 计量
   并强制执行，与哪条 URI 赢得合并无关，展示不应受合并结果影响。
2. **节点名称不可编辑**：解析出的节点名来自订阅/URI 解析，
   `SyncUserLandingExits` 每次同步用解析结果覆盖 `name`，admin 无法给
   节点起持久的名字。
3. **重置按钮样式**：解析出的节点表格里"重置"是文字链接，与同表格
   "用途"列的 pill 标签风格不统一。

## A. 用户侧配额列修复

`web/src/pages/my/LandingNodes.jsx`：

- 用 serverNodes 构建 `host:port → {quota_bytes, used_bytes, exceeded}`
  查询表（复用 `lib/landing.js` 的 host:port 拼接约定，含 IPv6 方括号）。
- "已用/总量"单元格不再按 `n.source === 'local'` 判断，改为按 host:port
  查表：查到账本就显示配额/已用/超额标记，查不到才显示"—"。
- 来源列、URI 复制行为不变（本地 URI 仍然赢得合并）。

不改通用 `mergeLanding`：它有多个调用点，字段合并语义只有这一页需要。

## B. 节点名称可编辑（含 URI 名称重写）

**不变量**：改名必须在订阅刷新后存活；原始名称不可丢（清除改名要能恢
复）；角色（用途）按 `protocol:host:port` 匹配，改名不得影响角色。

### 数据层

- 迁移：`user_landing_exits` 加 `name_override TEXT NOT NULL DEFAULT ''`。
  空串 = 未改名，显示解析出的 `name`。
- `LandingExit` 加 `NameOverride` 字段（`json:"name_override"`），
  `landingExitCols` 与 `scanLandingExit` 同步更新。
- 新函数 `SetUserLandingExitName(d, userID, host, port, name)`：写
  override，空串清除；行不存在返回 not-found。
- `SyncUserLandingExits` 不动：它只覆盖 `name` 列，override 天然存活。

### 服务端

- `internal/landing` 新增 `RewriteName(uri, name) (string, error)`，与
  `RewriteEndpoint` 对称：
  - authority 类协议（vless/trojan/hysteria2/socks 等）与 ss：替换/追加
    URL fragment（名称做 percent-encoding）；
  - vmess：base64 解码 JSON、改 `ps`、重编码；
  - snell：替换 `=` 前的名称前缀。
- 新 admin 路由 `POST /users/{id}/landing-exits/rename`
  `{host, port, name}`：校验与 quota/reset 同款（host 非空、port 合法、
  账本行存在否则 404）；`name` 为空串表示清除改名。
- `apiListUserLandingExits`：响应自然携带 `name_override`（结构体序列化）。
- `apiMyLandingNodes`：账本有 override 时，视图的 `name` 用 override，
  `uri` 用 `RewriteName` 重写；重写失败回退原 URI（复制仍可用）。
  stale 快照回退路径同样应用 override。

### 管理端 UI

`web/src/pages/users/Detail.jsx` 解析出的节点表格：

- 名称单元格显示生效名（`override || 解析名`），有账本行
  （`exitByAddr` 命中）时可点击进入行内编辑：input + 保存/取消，回车
  保存，清空保存 = 恢复原名。改过名的行给出轻量视觉提示（如原名
  tooltip），便于对照订阅源。
- residual（已不在来源）行同样可改名。
- 无账本行（尚未同步）的节点名称不可编辑。

## C. 重置按钮样式

`Detail.jsx` 两处"重置"（在册行与 residual 行）由文字链接改为与
`ADMIN_ROLE_OPTS` pill 一致的样式：
`px-2 py-0.5 text-[11px] font-semibold rounded-md border`，蓝色系配色
（`bg-blue-50 text-blue-700 border-blue-200` + dark 变体）。

## 测试

- Go：
  - rename API：设置、清除、404（无账本行）、参数校验；
  - `RewriteName`：authority/ss/vmess/snell 各协议 + 已有 fragment 替换
    + 畸形 URI 报错；
  - override 在 `SyncUserLandingExits` 后存活；
  - `/my/landing-nodes`：override 生效于 name 与重写后的 uri，stale
    回退路径同样生效。
- 前端：`vite build` 通过；手动核对三处 UI。

## 非目标

- 概览页/创建规则选择器的配额展示（本次只修"—"）。
- 改名写回订阅源或手动 URI 文本。
- per-node 授权限额（`user_nodes`）不涉及。
