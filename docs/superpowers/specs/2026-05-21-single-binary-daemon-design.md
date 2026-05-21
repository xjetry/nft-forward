# 单二进制 + Host Daemon 架构

## 背景

`nft-forward` 当前有三个独立二进制：

- `nft-forward` (TUI) — 直接 `nft.Apply` 写本机 nftables；state 在 `/var/lib/nft-forward/rules.json`
- `nft-agent` — HTTP :7878 接收 server 推送，直接 `nft.Apply` 写本机；state 在 `/var/lib/nft-forward/agent-state.json`
- `nft-server` — Web UI :8080；通过 `local://` sentinel scheme **进程内嵌** agent 操作本机；state 在 `/var/lib/nft-forward/embedded-agent-state.json`

**核心痛点**：三个角色都假设自己**独占**本机 nftables，且各自维护一份"完整 ruleset"的 state 文件。这导致：

1. 同一台主机无法**同时**运行 TUI 与 server / agent —— 谁后跑谁的 `nft.Apply` 会把对方的 ruleset 完全覆盖
2. 用户场景"先装 TUI，后扩展成 server / node，TUI 仍可用"无法实现
3. 三份 state 文件存的是同一份事实的不同视图，互不感知

## 目标

**单二进制 `nft-forward` + subcommand 切换角色 + 本机 host daemon 作为 nftables 的唯一控制器**：

- 一份发布产物
- 默认 `nft-forward` 进 TUI（保留现有卖点）
- `nft-forward daemon` 作为唯一操作 nftables 的进程（systemd 跑）
- `nft-forward server` / `daemon --listen :7878` 作为 daemon 的"非 TUI 客户端"，分别处理 Web UI 和远程 panel 推送
- TUI、server-side 业务、远程 panel push 三种规则来源**共存**于本机 nftables，互不覆盖

## 非目标

- **不引入 gRPC 或自定义 codec**。daemon IPC 沿用项目已有的 HTTP handler 风格
- **不做容器化**。daemon 必须 host 运行，把 server/agent 装容器收益已被本架构吸收（部署的"重"被 daemon 替代）；现有 `docker/` 留作 dev fixture
- **不做远程协议 v2**。server → 远程 agent 仍走现有 HTTP push；远程节点上 daemon HTTP-enable 后扮演旧 agent 角色
- **不改业务模型**。租户、配额、限速、通道等 server-side 业务概念维持原样
- **不引入新外部依赖**。除现已使用的 charmbracelet、SQLite 等不增包

## 设计

### 角色与 subcommand 表面

```
nft-forward                          → 默认进 TUI client
nft-forward daemon                   → 守护进程（systemd 跑）。监听 unix socket，操作本机 nftables，
                                       是唯一调用 nft.Apply 的进程
nft-forward daemon --listen ADDR --token-file PATH
                                     → 同上 + 额外监听 HTTP（接收远程 panel 推送）。
                                       这是当前 nft-agent 角色的等价表达
nft-forward server [--addr :8080] [--db PATH] ...
                                     → Web UI / 业务逻辑层。规则下发通过 daemon socket（本机）或
                                       HTTP（远程节点）— 与本架构前的 nft-server 表面 flag 一致
nft-forward apply                    → 兼容当前 systemd unit 调用入口；从 rules.json 读规则并通过
                                       daemon socket 提交后退出
nft-forward --install-service        → 安装 nft-forward-daemon.service（替代旧的 nft-forward.service）
nft-forward --uninstall-service      → 卸载
```

**等价替换**：
- 旧 `nft-agent --listen :7878 --token-file ...` ⇔ 新 `nft-forward daemon --listen :7878 --token-file ...`
- 旧 `nft-server --addr :8080 ...` ⇔ 新 `nft-forward server --addr :8080 ...`
- 旧 `nft-forward --apply` ⇔ 新 `nft-forward apply`

### Daemon 数据模型：owner-segmented ruleset

Daemon 内部不再维护一份扁平 `[]Rule`，而是按 owner 分段：

```go
// nft-forward/internal/daemon/state.go
type State struct {
    // owner → rules. 已知 owner: "tui"、"panel"、其他保留给未来扩展（如 "ansible"、"terraform"）。
    Owners map[string][]nft.Rule
}
```

**Owner 命名**：
- `tui` — 本机用户通过 TUI 提交的规则
- `panel` — server 推送的规则（无论 server 在本机还是远程；远程时通过 daemon HTTP-enable 接口推入）

**Apply 时**：daemon 把所有 owner 的规则**合并**成一份 ruleset 提交给 `nft.Apply`。Counter 仍按 listen-port + proto 在 nftables 里 keyed，不分 owner。

**冲突检测**：daemon 在 Apply 前检测：
- 同一 owner 内端口冲突 → daemon 拒绝该 client 的提交请求（HTTP 400）
- 跨 owner 端口冲突 → daemon 拒绝**后提交**的请求，并在错误信息里告知该端口被哪个 owner 占用（"tcp/8080 已被 owner=tui 占用"）

### Daemon IPC 协议

**Transport**：HTTP over Unix Socket，路径 `/var/run/nft-forward.sock`，文件权限 `0660`、属主 `root:nft-forward`（install.sh 创建 group，root 默认在 group 里；普通用户加入 group 后可跑 TUI 不带 sudo）。

**Endpoints**（与现有 `internal/agent` 的 HTTP 接口表面统一）：

| Method | Path | 用途 |
|---|---|---|
| `GET /v1/health` | — | 探活，返回 `{"ok":true}` |
| `GET /v1/ruleset` | — | 返回当前完整 ruleset（按 owner 分段） |
| `POST /v1/ruleset/{owner}` | body: `{"rules":[...]}` | 全量替换该 owner 的 segment，触发 daemon merge + apply |
| `GET /v1/counters` | — | 返回每条 rule 的字节/包计数（沿用现状） |
| `POST /v1/resolve` | body: `{"rules":[...]}` | DNS 域名预解析（沿用 nft.ResolveHosts） |

**Authn**：
- Unix socket 连接：peer credential（SO_PEERCRED）—— group 检查，无需额外 token
- HTTP enabled 模式（远程接入）：Bearer token（与现有 agent 一致），token 从 `--token-file` 读

### State 文件

唯一文件：`/var/lib/nft-forward/state.json`，由 daemon 独占写入。schema：

```json
{
  "version": 1,
  "owners": {
    "tui":   [{"id": "...", "proto": "tcp", "src_port": 8080, ...}],
    "panel": [{"id": "...", "proto": "tcp", "src_port": 22, ...}]
  }
}
```

**启动时自动迁移**：daemon 启动时检测以下旧文件，存在则导入并删除：

| 旧文件 | 导入到 |
|---|---|
| `/var/lib/nft-forward/rules.json` | `state.owners.tui` |
| `/var/lib/nft-forward/agent-state.json` | `state.owners.panel` |
| `/var/lib/nft-forward/embedded-agent-state.json` | `state.owners.panel` |

迁移完后立即写入 `state.json` 并删除旧文件。若三份旧文件同时存在（不可能但保险）则 `panel` segment 以 `embedded-agent-state.json` 为准（最新内嵌 agent 视为权威）。

### 客户端：TUI

`internal/tui` 改造：

- 删除 `store.Load` / `store.Save` 直接调用
- 删除 `nft.Apply` 直接调用
- 改为通过 `internal/daemonclient`（新包）调 daemon HTTP API
- TUI 启动时若 `GET /v1/health` 失败 → 报错并提示 `sudo systemctl start nft-forward-daemon.service`，**不 fallback** 到直接管 nftables（保持单一控制路径）

### 客户端：server

`internal/server` 改造：

- 删除 `local://` sentinel 特殊路径
- 删除进程内嵌 agent (`embeddedAgent` / `Bootstrap` 等)
- 本机节点 = 一个普通节点，address 为 `unix:///var/run/nft-forward.sock`
- 远程节点 = HTTP URL，与现状一致
- Pusher 调用统一走 `daemonclient`，本机 vs 远程仅 transport 不同（URL scheme `unix://` vs `http(s)://`）
- Poller 同上 — `GET /v1/counters` 走 socket 或 HTTP

### Daemon HTTP-enable 模式（替代 nft-agent）

当 `nft-forward daemon --listen :7878 --token-file ...` 时，daemon 在 unix socket 之外**额外**起一个 HTTP server：

- 端点表面与上节相同（`/v1/ruleset/panel` 等）
- 认证用 Bearer token
- 远程 server 推规则 → daemon HTTP 接口 → 内部走与本地 client 完全一致的 apply 流程
- 即：远程节点的 daemon 本质上是"socket + HTTP 双接入的 daemon"，并非另一种进程

### cmd 目录归并

| 现状 | 重构后 |
|---|---|
| `cmd/nft-forward/` | **保留**，扩展为 dispatch：tui / daemon / server / apply / install-service |
| `cmd/nft-agent/` | **删除**。功能由 `nft-forward daemon --listen ...` 取代 |
| `cmd/nft-server/` | **删除**。功能由 `nft-forward server ...` 取代 |

`install.sh` 升级时检测到旧 `/usr/local/sbin/nft-agent` 或 `nft-server` 存在 → 提示用户它们已被 `nft-forward` 子命令替代 → 自动清理。

### Daemon systemd 单元

`/etc/systemd/system/nft-forward-daemon.service`（由 install.sh 写入）：

```ini
[Unit]
Description=nft-forward host daemon (nftables controller)
After=network-online.target nftables.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/sbin/nft-forward daemon
Restart=on-failure
RuntimeDirectory=nft-forward
RuntimeDirectoryMode=0750
StateDirectory=nft-forward
StateDirectoryMode=0750

[Install]
WantedBy=multi-user.target
```

`nft-forward-server.service`（仅 server 角色时存在）：

```ini
[Unit]
Description=nft-forward web panel
Requires=nft-forward-daemon.service
After=nft-forward-daemon.service

[Service]
ExecStart=/usr/local/sbin/nft-forward server --addr :8080
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

agent role 不再有独立 service —— 通过修改 `nft-forward-daemon.service` 的 `ExecStart` 为 `nft-forward daemon --listen :7878 --token-file ...` 即可。install.sh 提供 `install.sh agent --token ...` 操作来 patch daemon unit。

### install.sh 流程

| 子命令 | 行为 |
|---|---|
| `install.sh` (无 mode) / `install.sh tui` | 下载二进制 + 写 `nft-forward-daemon.service` + `enable --now` daemon + 提示用户运行 `nft-forward` |
| `install.sh server [--addr :8080] [--password ...]` | 在 daemon 已装的基础上写 `nft-forward-server.service` + `enable --now` |
| `install.sh agent --token <token>` | 在 daemon 已装的基础上，把 daemon unit 的 ExecStart 改为带 `--listen :7878 --token-file ...`，写 token 文件，`daemon-reload && restart` |
| `install.sh uninstall server` | `disable --now nft-forward-server.service` + 删 unit。daemon 保留 |
| `install.sh uninstall agent` | 把 daemon unit ExecStart 还原为不带 `--listen` 的形态 + 删 token 文件 + `restart` |
| `install.sh uninstall daemon` | 卸载 daemon（卸载前要求 server / agent 都已先卸载，避免悬空依赖）。`state.json` 默认保留 |

### Pre-check 与权限

- daemon 启动时执行现有 `preflight`（root 检查、nftables 可用、ip_forward enable、apt 兜底）
- TUI / server 等 client 启动时不再做 preflight —— 只检测 daemon socket 是否可连
- daemon 创建 `nft-forward` system group（若不存在），socket 文件 group 即此 group
- TUI 用户需在该 group 里才能不带 sudo 跑（install.sh 文档说明）

## 实现侧改动清单

| # | 类别 | 路径 | 操作 |
|---|---|---|---|
| 1 | 新增 | `internal/daemon/` | 新包：state、ipc handler、merge & apply、迁移逻辑 |
| 2 | 新增 | `internal/daemonclient/` | 新包：HTTP-over-unix-socket / HTTP client 抽象 |
| 3 | 改 | `internal/agent/` | 缩成 thin wrapper（HTTP listener 委托给 daemon）；或合并入 `internal/daemon/`，按落地难度选 |
| 4 | 改 | `internal/server/` | 删 `local://` 特殊路径与嵌入式 agent；pusher / poller 走 daemonclient |
| 5 | 改 | `internal/tui/` | 删 store/nft 直接调用；改 daemonclient |
| 6 | 改 | `internal/store/` | 仅用于 daemon 内部 state 持久化；或合并入 `internal/daemon/` |
| 7 | 改 | `cmd/nft-forward/` | dispatch：tui / daemon / server / apply / install-service |
| 8 | 删 | `cmd/nft-agent/` | 整目录删除 |
| 9 | 删 | `cmd/nft-server/` | 整目录删除 |
| 10 | 改 | `install.sh` | 单二进制 + daemon-first + 角色叠加 |
| 11 | 改 | `internal/systemd/` | 写两个 service 文件而不是一个 |
| 12 | 改 | `README.md` | 重组三种运行模式描述，强调"装一次，按需扩展" |
| 13 | 新增 | `docs/architecture.md`（可选） | 一张架构图 + 协议表 |

## 测试与验证

### 单元 / 集成测试

- daemon ruleset merge：多 owner 输入 → 验证合并后无冲突、有冲突时正确报错
- daemon state migration：构造三种旧文件场景 → 验证导入 + 删除
- daemon HTTP enable：测 token auth + 远程推送等价于本地 socket 推送
- TUI daemonclient：mock socket server → 验证 TUI add/edit/delete 走 socket
- server pusher：本机 unix:// + 远程 http:// 两种 transport 都跑通

### End-to-end manual

- **场景 A：纯 TUI 升级到带 server**
  1. 干净 host 装 `install.sh` → 跑 TUI 加 3 条规则
  2. 跑 `install.sh server` → 浏览器登面板
  3. 在面板加一条规则
  4. **同时**在 TUI 里看到原 3 条 + 面板加的 1 条；TUI 里改 / 删自己加的规则 → 面板不感知（owner segment 隔离）
  5. `nft list ruleset` 看到 4 条合并的规则
- **场景 B：纯 TUI 升级到带 agent role**
  1. 装 `install.sh` + 跑 TUI 加规则
  2. 跑 `install.sh agent --token ...`
  3. 远程 panel 上注册该节点、推规则
  4. TUI 自己原规则仍在；panel 推的规则与之共存
- **场景 C：迁移旧 nft-server**
  1. 装旧版（裸金属或 docker） + 用一段时间，DB 有数据
  2. 装新版（覆盖二进制 + 跑 install.sh server）
  3. daemon 启动时把 `embedded-agent-state.json` 迁入 `state.panel`
  4. server 启动时直接复用 panel.db，业务数据保留；本机节点 address 由 `local://` 改为 `unix:///var/run/nft-forward.sock`（DB migration）
- **场景 D：daemon 重启后状态恢复**
  1. 各 owner 各加几条规则
  2. `systemctl restart nft-forward-daemon` 
  3. daemon 从 state.json 重建 ruleset，验证 `nft list ruleset` 完整恢复

### 现有 `docker/test.sh` 端到端

适配新架构 fixture（容器内跑 daemon + agent role），验证 1 server + 3 agent role 节点的多租户、配额、限速、计数等用例不回归。

## 实现规模与迁移路径

- 代码改动估计：~60% 现有代码受影响（agent、server、tui、cmd 都要改；nft、tc、sysdeps、systemd、store 等基础包大多不变）
- 落地建议**分阶段单 PR / 单 spec 推进**，每阶段产物可独立 ship：
  - **A. daemon 骨架** — 新增 `internal/daemon` + socket listener + 简化的 owner-less ruleset apply。`nft-forward daemon` 子命令可独立运行
  - **B. owner segment + 迁移** — 引入 owner 模型 + 旧 state 文件迁移逻辑
  - **C. TUI client 化** — TUI 改走 daemonclient（最直接的用户可见验证）
  - **D. server / agent client 化** — 删 `cmd/nft-agent` `cmd/nft-server` + 重构 server 内部
  - **E. install.sh 整改 + README 重写**

每阶段一份 plan，串行执行。本 spec 是 A-E 全景；首份 plan 仅展开阶段 A，避免一次过大。

## Out-of-scope

- **TLS / mTLS** — daemon HTTP enable 仍只支持 Bearer token；TLS 由用户在前置 reverse proxy 处理（与现状一致）
- **多架构镜像** — 不在本架构范围
- **Helm / k8s manifest** — 同上
- **rules.json 增量同步协议** — 仍是全量 replace（每个 owner segment 替换）
- **Daemon multi-tenancy（多个 daemon 实例同机跑）** — 仅一个 daemon 实例独占 nftables 表 `ip nft_forward`

## Open questions

无 — 通过多轮澄清已锁定 D1-D8 所有关键决策点。

后续若发现新边界（如 client 同 owner 并发提交的并发控制、远程 daemon 升级时的协议版本协商），开新 spec 处理。
