# daemon 显式声明 relay_host（v4/v6）设计

## 背景 / 根因

`relay_host`/`relay_host_v6` 是节点的数据面地址：中继链路用它作为"上一跳"打向本节点的目标地址（`internal/server/shared.go` 的 `buildRuleView` 直接拿它拼 entry 地址）。目前这两个字段的自动识别逻辑（`internal/server/hub.go` 的 `fillNodeRelayHosts`）只在字段为空时，用这次 WebSocket 握手观测到的连接源 IP（`extractIP`）做**一次性**填充。

这套逻辑隐含的假设是"daemon 拨号连 server 时，操作系统按默认路由选中的出口 = 其他节点应该访问这个节点的入口"。单出口机器上这个假设成立；但在国内入口 + 海外出口双 IP 的中转机（如 hep-ix）上不成立：

- 拓扑：本地访问 → 前置 → **ix(入口)** —— **ix(出口)** → 落地
- 前置只跟 ix 的入口 IP 通信（这是链路要求的数据面地址）
- ix 主动拨号连 server（控制通道）时，源 IP 由操作系统默认路由决定，实际选中的是出口 IP
- 结果：`relay_host` 首次握手时被自动填成了出口 IP，而不是前置真正连接的入口 IP

现有系统里已经有手动覆盖机制（`POST /api/nodes/{id}/relay-host`，节点详情页"中继地址（数据面）"输入框），字段一旦非空就不会再被自动逻辑覆盖，可以作为临时止血手段。但每台双出口中转机都要装完之后手动去 UI 改一次，这次设计要解决的是把这一步收敛到装机参数里。

## 目标 / 非目标

**目标**

- daemon 可以在启动参数里显式声明 `relay_host`（v4/域名）和/或 `relay_host_v6`，随 Hello 消息上报给 server
- server 收到声明值后，把它当作权威来源：每次握手都会（在值变化时）同步写入 DB，即便字段之前已经有值，声明值也能覆盖——保证配置漂移能自愈，而不是只在字段为空时生效一次
- 一旦某个字段的值来自声明，节点详情页对应的输入框禁止编辑，后端 API 也拒绝手动覆盖，避免管理员在 UI 改了之后又被下一次握手悄悄冲掉，产生"改了但没生效"的困惑
- 装机脚本 `install.sh` 支持在 `agent` 模式下透传这两个新参数

**非目标**

- 不做 daemon 拨号时的本地出口 IP/网卡绑定（`LocalAddr`）。国内入口 IP 未必具备出站到 server 的路由能力，强行绑定可能直接导致连不上 server；且这条路本质上还是在依赖"连接观测"这套本就不该用来做地址声明的机制
- 不改变没有配置声明值的节点的现有行为（自动识别 / 手动 UI 覆盖都保持原样）
- 不提供"强制覆盖声明值"的后门。要变更只能改 daemon 配置并重启——这是有意的取舍，保持单一权威来源，避免两条写入路径互相打架

## 数据库变更

新增迁移 `internal/db/migrations/0024_node_relay_host_declared.sql`：

```sql
ALTER TABLE nodes ADD COLUMN relay_host_declared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN relay_host_v6_declared INTEGER NOT NULL DEFAULT 0;
```

跟 `disabled`/`hidden`/`unidirectional` 一样用 0/1 表示布尔值。`Node` struct（`internal/db/queries.go`）新增两个 `bool` 字段：`RelayHostDeclared`、`RelayHostV6Declared`。

> 加列必须三处对齐：`nodeCols` 常量、`scanNode`、`grants.go` 的 `ListNodesForUser` 内联 scan——漏掉第三处会在通过用户授权路径读节点时静默丢字段（zero value），历史上已经因为这个坑出过问题，实现时要逐一检查。

## 协议变更（internal/wsproto/messages.go）

`Hello` 新增两个可选字段，与现有 `ProbedV4`/`ProbedV6` 并列：

```go
// DeclaredRelayHost/DeclaredRelayHostV6 are explicit operator-provided
// addresses (set via daemon --relay-host/--relay-host-v6), distinct from
// ProbedV4/ProbedV6 which are the agent's own outbound-route guesses. When
// present they are authoritative and override whatever is in the DB,
// unlike ProbedV4/ProbedV6 which only ever seed an empty field.
DeclaredRelayHost   string `json:"declared_relay_host,omitempty"`
DeclaredRelayHostV6 string `json:"declared_relay_host_v6,omitempty"`
```

字段是新增且 `omitempty`，老 daemon 不发送、老 server 不认识都不会破坏协议兼容性。

## server 端变更（internal/server/hub.go）

`ServeWS` 在调用 `fillNodeRelayHosts` 之前，先跑一个新函数 `applyDeclaredRelayHosts`：

```go
func applyDeclaredRelayHosts(d *sql.DB, node *db.Node, declaredV4, declaredV6 string) {
    if declaredV4 != "" {
        if isValidRelayHost(declaredV4) {
            if node.RelayHost != declaredV4 || !node.RelayHostDeclared {
                _ = db.UpdateNodeRelayHost(d, node.ID, declaredV4)
                _ = db.SetNodeRelayHostDeclared(d, node.ID, true)
                node.RelayHost, node.RelayHostDeclared = declaredV4, true
            }
        } else {
            log.Printf("hub: node %d declared invalid relay_host %q, ignoring", node.ID, declaredV4)
        }
    } else if node.RelayHostDeclared {
        // daemon 配置移除了声明：解锁字段，但保留现有值，避免链路瞬间失联
        _ = db.SetNodeRelayHostDeclared(d, node.ID, false)
        node.RelayHostDeclared = false
    }
    // v6 分支结构相同，用 isValidRelayHostV6 校验后调用 UpdateNodeRelayHostV6 / SetNodeRelayHostV6Declared
}
```

`apiSetNodeRelayHostV6`（`internal/server/api.go`）目前把"必须是合法 IPv6 字面量、且不能是 v4-mapped"这条校验内联写在 handler 里，没有独立函数。本次改动把它提成一个包级函数 `isValidRelayHostV6(host string) bool`（跟 `isValidRelayHost` 并列），API handler 和 `applyDeclaredRelayHosts` 都调用它，避免同一段校验逻辑写两份。

`fillNodeRelayHosts` 本身不需要改：声明生效后字段非空，它内部各个 `if node.RelayHost == ""` 分支自然不会再介入；执行顺序是先 `applyDeclaredRelayHosts` 再 `fillNodeRelayHosts`。

校验失败（声明值不合法）时只记日志、不中断握手——跟现有 `fillNodeRelayHosts` 对 `UpdateNodeRelayHost` 错误的处理方式一致（忽略错误，不影响连接建立）。这条路径是自动化的按连接同步，不是管理员的显式操作，不写 `WriteAudit`，跟 `fillNodeRelayHosts` 现状保持一致。

`db.SetNodeRelayHostDeclared(d *sql.DB, id int64, v4 bool)` / 对应 v6 版本是新增的两个简单 UPDATE helper，仿照 `UpdateNodeRelayHost` 的写法。

## API 变更（internal/server/api.go）

`apiSetNodeRelayHost` / `apiSetNodeRelayHostV6` 在读到 node 之后，各自检查对应的 `RelayHostDeclared`/`RelayHostV6Declared`：

```go
if node.RelayHostDeclared {
    jsonErr(w, http.StatusConflict, "该字段由节点 daemon 的 --relay-host 参数管理，如需修改请更新节点配置后重启 daemon")
    return
}
```

## 前端变更（web/src/pages/nodes/Detail.jsx）

`load()` 读取 `node.relay_host_declared`/`node.relay_host_v6_declared` 存入 state。对应 `ConfigField` 里的 `input`/保存按钮在 declared 为真时加 `disabled`，hint 文案追加"（由 daemon 启动参数管理，UI 不可修改）"。

## install.sh 变更

`agent` 安装模式新增可选参数 `--relay-host`/`--relay-host-v6`，仿照现有 `port_range` 的拼接方式：

```sh
[[ -n "$relay_host" ]] && relay_arg+=" --relay-host $relay_host"
[[ -n "$relay_host_v6" ]] && relay_arg+=" --relay-host-v6 $relay_host_v6"
write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token${range_arg}${relay_arg}"
```

## daemon 端变更

- `cmd/nft-agent/main.go`：新增 `--relay-host`/`--relay-host-v6` 两个 flag，跟 `--port-range` 同一模式，赋给 `daemon.Config`
- `internal/daemon/daemon.go`：`Config` 新增 `DeclaredRelayHost`/`DeclaredRelayHostV6`；`New()` 透传给 `Daemon` 的私有字段（跟 `portRange` 一样落在 `handlers.go` 定义的 `Daemon` struct 里）
- `internal/daemon/dialer.go`：`DialerConfig` 新增两个字段；`runOnce` 组装 `wsproto.Hello` 时带上

## 测试计划

- `internal/server/hub_test.go`：新增用例覆盖——声明值覆盖已有非空值；声明值校验失败被忽略且不中断握手；声明值清空后 `*_declared` 标记复位但字段值保留
- `internal/server/api.go` 对应测试：`RelayHostDeclared=true` 时 `apiSetNodeRelayHost`/`V6` 返回 409
- `internal/daemon/dialer_test.go`：验证配置了声明值时 Hello payload 带上对应字段，未配置时字段为空（不影响老协议）
- 手动验证：hep-ix 用 `--relay-host <国内入口IP>` 重装/重启 daemon，确认 server 端 `relay_host` 立即变为该值，节点详情页对应输入框变为禁用态

## 文件变更清单

| 文件 | 变更 |
|------|------|
| `internal/db/migrations/0024_node_relay_host_declared.sql` | 新增迁移 |
| `internal/db/queries.go` | `Node` 加两个字段；`nodeCols`/`scanNode` 对齐；新增 `SetNodeRelayHostDeclared`/`SetNodeRelayHostV6Declared` |
| `internal/db/grants.go` | `ListNodesForUser` 内联 scan 对齐 |
| `internal/wsproto/messages.go` | `Hello` 新增 `DeclaredRelayHost`/`DeclaredRelayHostV6` |
| `internal/server/hub.go` | 新增 `applyDeclaredRelayHosts`，`ServeWS` 里在 `fillNodeRelayHosts` 之前调用 |
| `internal/server/api.go` | `apiSetNodeRelayHost`/`apiSetNodeRelayHostV6` 加 declared 拦截；提取 `isValidRelayHostV6` 供 hub.go 复用 |
| `internal/daemon/daemon.go` | `Config` 新增两字段并透传 |
| `internal/daemon/handlers.go` | `Daemon` struct 新增两个私有字段 |
| `internal/daemon/dialer.go` | `DialerConfig` 新增两字段；`runOnce` 组装 Hello 时带上 |
| `cmd/nft-agent/main.go` | 新增 `--relay-host`/`--relay-host-v6` flag |
| `install.sh` | agent 模式新增装机参数并写入 systemd unit |
| `web/src/pages/nodes/Detail.jsx` | declared 字段禁用对应输入框，hint 文案更新 |
