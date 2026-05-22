# nft-forward

基于 nftables 的轻量端口转发工具。**单一二进制，角色按需叠加**：一份发布产物在同一台主机上可以同时充当本机转发 TUI、多租户 Web 控制面板以及远程受控节点，三种功能共用同一个 host daemon，规则互不覆盖。整套运行时依赖只有 nftables 与 iproute2；无 gRPC、无额外守护进程。

---

## 目录

- [介绍](#介绍)
- [快速开始](#快速开始)
- [架构](#架构)
- [命令表面](#命令表面)
- [配置与持久化](#配置与持久化)
- [升级与迁移](#升级与迁移)
- [开发](#开发)
- [协议参考](#协议参考)

---

## 介绍

`nft-forward` 让一台普通 Linux 主机变成可管理的端口转发节点，同时解决了"单机 TUI 用户想扩展成面板托管节点"时过去需要重装三个不同二进制的痛点。

核心设计是一个 **host daemon**（`nft-forward daemon`）：它是主机上**唯一**调用 nftables 和 tc 的进程，监听 Unix socket `/var/run/nft-forward.sock`，其他所有角色（TUI、Web 面板、远程推送接入）都是这个 daemon 的 HTTP client。daemon 在内部把规则按 *owner* 分段管理（`tui` 段、`panel` 段），每次有一方提交新规则时，daemon 合并所有段后原子地刷新内核 nftables 表，跨段端口冲突在合并时被拒绝并报告给调用方。

这一设计意味着你可以在同一台机器上**同时**运行 TUI（本地交互）、Web 面板（多租户管理）和远程节点接收（panel 的 HTTP push），彼此不干扰。规则持久化由 daemon 独立完成，进程重启后自动从 `state.json` 恢复，不依赖外部"开机重放"机制。

---

## 快速开始

所有安装路径都从同一个 `install.sh` 开始，下载单一 `nft-forward` 二进制并按角色配置 systemd 单元。

```bash
# TUI 单机（自动安装 daemon systemd 服务）
sudo bash install.sh tui
# 安装完成后运行 TUI：
sudo nft-forward

# Web 控制面板（daemon + server 两个 unit 都会启用）
sudo bash install.sh server

# 指定面板端口
sudo bash install.sh server --addr :9000

# 远程受控节点（daemon 额外监听 HTTP，接受远程 panel 推送；token 从面板"添加节点"页拷贝）
sudo bash install.sh agent --token <64位hex>

# 指定 agent 监听端口
sudo bash install.sh agent --port 7900 --token <64位hex>

# 交互式（无参数时脚本会询问模式）
sudo bash install.sh
```

脚本会自动完成：

1. 从 [GitHub releases](https://github.com/xjetry/nft-forward/releases/latest) 下载对应架构的 `nft-forward` 二进制并 sha256 校验；
2. 安装到 `/usr/local/sbin/nft-forward`；
3. 写入并启用 `nft-forward-daemon.service`（以及 server 角色时的 `nft-forward-server.service`）；
4. 检测并清理旧版遗留的 `nft-forward.service` / `nft-server.service` / `nft-agent.service` unit 和旧二进制（详见[升级与迁移](#升级与迁移)）；
5. 打印访问地址、systemctl 命令和日志位置。

### 系统要求

- Linux 内核 ≥ 5.10，支持 nftables（Debian 12 自带 6.1，满足）
- Debian / Ubuntu 系：若 `nftables` / `iproute2` 未安装，daemon 启动时会非交互自动 `apt-get install`
- 其他发行版（RHEL / Arch / Alpine）：预先 `apt-get install -y nftables iproute2`（或对应包管理器）即可；daemon 不会调用非 apt 系包管理器
- 必须以 root 运行 daemon（操作 nftables 和 tc 需要）

---

## 架构

```
┌─────────────────────────────┐         ┌──────────────────────────┐
│      浏览器（admin/tenant）  │  HTTPS  │  Caddy / Nginx（TLS 终结）│
│                             │ ──────► │  反向代理（可选）          │
└─────────────────────────────┘         └────────────┬─────────────┘
                                                     │ HTTP :8080
                                        ┌────────────▼───────────────┐
                                        │    nft-forward server      │
                                        │  chi router + HTML 模板    │
                                        │  SQLite WAL (panel.db)     │
                                        │  Pusher goroutine          │
                                        │  Poller goroutine          │
                                        └────────────┬───────────────┘
                                                     │ HTTP over unix socket
                                        ┌────────────▼───────────────┐
                                        │   nft-forward daemon       │
                                        │  /var/run/nft-forward.sock │
                                        │  owner-segmented ruleset   │
                                        │  nftables + tc HTB         │
                                        └────────────────────────────┘
                                          │ Bearer token, HTTP
                                          │ POST /v1/ruleset/panel
                                 ┌────────┴──────────┐
                             ┌───▼────────┐   ┌──────▼─────────┐
                             │  远程节点 1 │   │  远程节点 2     │
                             │ nft-forward│   │ nft-forward    │
                             │ daemon     │   │ daemon         │
                             │ --listen   │   │ --listen :7878 │
                             │ :7878      │   │                │
                             └────────────┘   └────────────────┘
```

**关键设计约束**：

- `nft-forward daemon` 是主机上**唯一**直接操作 nftables 和 tc 的进程。TUI 和 server 都不再直接调用 `nft`，全部通过 daemon 的 Unix socket HTTP API 提交规则。
- Daemon 内部维护 **owner-segmented ruleset**：每个 owner（`tui`、`panel`）独占自己的规则段。Pusher 可以只替换 `panel` 段，不影响用户在 TUI 里添加的 `tui` 段规则；反过来也是。跨段端口冲突时，daemon 拒绝**后提交**的请求并说明被哪个 owner 占用。
- Server（Web 面板）把本机 daemon 视为一个普通节点，地址为 `unix:///var/run/nft-forward.sock`；远程节点地址为 `http(s)://host:7878`。Pusher / Poller 对两种 transport 使用同一套逻辑，仅 URL scheme 不同。
- Daemon HTTP-enable 模式（`--listen :7878 --token-file ...`）让 daemon 在 Unix socket 之外额外监听 HTTP，接受远程 panel 的 Bearer token 认证推送。远程节点本质上是"socket + HTTP 双接入的 daemon"，不是另一种进程。
- Nftables 使用专用表 `ip nft_forward`，不影响主机已有的防火墙规则；每次 apply 是原子的三步（add → delete → recreate）。

---

## 命令表面

```
nft-forward                                     默认进 TUI（要求 daemon 已运行）
nft-forward daemon                              前台启动 daemon（systemd 通常负责）
nft-forward daemon --listen :7878 \
    --token-file /etc/nft-forward/daemon.token  daemon 额外监听 HTTP，充当远程受控节点角色
nft-forward server [--addr :8080] [--db PATH]   启动 Web 面板
```

TUI 启动时通过 `GET /v1/health` 探活 daemon。若 daemon 未运行，TUI 会打印错误并提示 `sudo systemctl start nft-forward-daemon.service`；不会 fallback 到直接操作 nftables（保持单一控制路径）。

`nft-forward server` 首次启动会创建 `admin` 账号并打印随机密码到 stdout。传 `--bootstrap-admin-password` 可预设密码。若忘记密码，停服后用同一二进制带 `--reset-admin-password` 执行一次即可重置，不影响其他数据。

TUI 键位：

| 键 | 作用 |
|---|---|
| ↑/↓ 或 j/k | 选中规则 |
| a / n / + | 新增转发 |
| d | 删除当前选中 |
| c | 清空全部 |
| r | 重新加载 |
| q | 退出 |

---

## 配置与持久化

| 路径 | 用途 |
|---|---|
| `/var/run/nft-forward.sock` | Daemon Unix socket（group `nft-forward`，mode `0660`） |
| `/var/lib/nft-forward/state.json` | Daemon 状态文件，owner-segmented，daemon 独占写 |
| `/var/lib/nft-forward/panel.db` | Server（面板）SQLite 数据库（WAL 模式） |
| `/etc/nft-forward/daemon.token` | Agent 角色的 bearer token（mode `0600`） |
| `/etc/sysctl.d/99-nft-forward.conf` | ip_forward 持久化 |

环境变量：

| 变量 | 作用 |
|---|---|
| `NFT_FORWARD_DNS_INTERVAL` | Agent / TUI 后台重解析 DDNS 周期（如 `30s`、`2m`），缺省 60s |

**DDNS 目标**：转发目标支持域名（如 `home.example.ddns.net`）。解析只在 daemon 侧进行，按 `NFT_FORWARD_DNS_INTERVAL` 周期重解析；底层 IP 变化时自动重建 nftables 规则。解析失败时保留上次成功的 IP，不撕掉已生效的转发。多租户场景下，设置了 `target_cidr_allow` 的通道只允许直接填 IPv4，不允许域名，避免通过 DNS 绕过 CIDR 限制。

**流量配额与限速**：Server 的 Poller 每 5s 读取 daemon 的 `/v1/counters` 并累加到 `tenants.traffic_used_bytes`；超额时自动禁用租户并清空其规则。限速通过 tc HTB 实现：daemon 在数据面网卡上建 HTB 树，按规则的监听端口打 nfmark 后路由到对应 class。

---

## 升级与迁移

### 日常升级

新版发布后用 `install.sh update` 升级现有部署：

```bash
sudo bash install.sh update
```

行为：拉 GitHub latest 二进制 → sha256 校验 → ELF x86-64 架构校验 → 备份旧二进制到 `/usr/local/sbin/nft-forward.bak` → 原子替换 → `systemctl restart nft-forward-daemon.service`（+ `nft-forward-server.service` 如有）→ 10 秒 health-check。失败自动回滚到旧二进制并重启。

约束：

- `update` 总是拉 `latest`；要锁版本/降级请用 `install.sh tui --release v1.2.3`（等价于重装）
- `update` 不动 systemd unit 文件，`--listen` / `--addr` / token 等角色配置完全保留
- daemon 短暂重启期间 nftables 规则从 state.json 重放，规则不丢

### 角色切换

支持互切：

```bash
sudo bash install.sh tui      # 从 server/agent 降回 tui
sudo bash install.sh server   # 从 tui/agent 切到 server
sudo bash install.sh agent    # 从 tui/server 切到 agent
```

脚本会自动 detect 当前角色并清理冲突的旧 unit / token。**但 state.json 中之前 panel 段的规则会保留并继续生效**——从 server/agent 降回 tui 不会自动撤掉旧的远程推送规则。需要彻底清的话先跑：

```bash
sudo bash install.sh uninstall server --purge   # 卸 server 同时清 panel.db + daemon panel 段
sudo bash install.sh uninstall agent  --purge   # 卸 agent 同时清 /etc/nft-forward + daemon panel 段
sudo bash install.sh uninstall daemon --purge   # 卸 daemon 同时清 state / sysctl / nftables 表 / 系统组
```

`--purge` 仅在 `uninstall` 模式下有效；不带 `--purge` 时数据完全保留（向后兼容现有行为）。

**从旧版（三二进制布局）升级**，运行新版 `install.sh` 时脚本会自动：

1. 检测并 `systemctl disable --now` 旧的 `nft-forward.service`、`nft-server.service`、`nft-agent.service`，删除对应 unit 文件；
2. 删除旧的独立二进制 `/usr/local/sbin/nft-server` 和 `/usr/local/sbin/nft-agent`（这两个名字在新架构中已不存在，功能分别由 `nft-forward server` 和 `nft-forward daemon --listen` 承担）；
3. 安装新的单一二进制并写入 `nft-forward-daemon.service`（和 server 角色的 `nft-forward-server.service`）。

**旧 state 文件迁移**：daemon 首次启动时（`state.json` 尚不存在），自动检测并导入旧格式文件：

| 旧文件 | 导入到 |
|---|---|
| `/etc/nft-forward/rules.json` | `state.json` → `tui` segment |
| `/var/lib/nft-forward/agent-state.json` | `state.json` → `panel` segment |
| `/var/lib/nft-forward/embedded-agent-state.json` | `state.json` → `panel` segment（优先级高于上一条） |

每个被处理的文件重命名为 `<原路径>.migrated`（不删除，留人工备份）。后续 daemon 重启不重复迁移：只要 `state.json` 已存在，迁移跳过。

**面板数据库迁移**：旧版 `nft-server` 的 `panel.db` 直接复用，新版 `nft-forward server` 首次启动会执行 schema migration，把本机节点的地址由旧的 `local://` 更新为 `unix:///var/run/nft-forward.sock`，业务数据（节点、通道、租户、转发记录）保持不变。

---

## 开发

依赖：Go ≥ 1.22，无 CGO，跨平台编译。运行时依赖 `nftables` + `iproute2`（仅 Linux）。

```bash
# 构建
git clone <repo> nft-forward && cd nft-forward
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o build/nft-forward ./cmd/nft-forward

# arm64
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
    -o build/nft-forward-arm64 ./cmd/nft-forward

# 跑测试
go test ./...
go vet ./...

# docker dev fixture（1 个 daemon 容器 + 1 个 server 容器）
docker compose --file docker/docker-compose.yml up
```

代码组织（Go 3700+ 行 / HTML 模板 660+ 行）：

```
nft-forward/
├── cmd/nft-forward/        TUI / daemon / server subcommand 入口
├── internal/
│   ├── daemon/             owner-segmented ruleset、Unix socket HTTP handler、state 持久化与迁移
│   ├── daemonclient/       HTTP-over-unix-socket / HTTP client 抽象（TUI 和 server 共用）
│   ├── nft/                nftables 渲染 + apply + counter 解析
│   ├── tc/                 tc HTB qdisc/class/filter 重建
│   ├── tui/                bubbletea TUI 实现（daemonclient 调用 daemon）
│   ├── db/                 SQLite schema + migrations + 查询
│   └── server/             Web 面板：路由、认证、handler、Pusher、Poller、HTML 模板
└── docker/                 dev fixture（Dockerfile + docker-compose.yml）
```

Socket 权限：daemon 创建 `nft-forward` system group（若不存在），socket 文件属于该 group（`0660`）。希望不带 `sudo` 运行 TUI 的用户加入该 group 即可；`install.sh` 文档中有说明。

---

## 协议参考

Daemon 暴露的 HTTP API（Unix socket 和可选的 TCP HTTP-enable 模式共用同一套端点）：

| Method | Path | 用途 |
|---|---|---|
| `GET` | `/v1/health` | 探活，返回 `{"ok":true}` |
| `GET` | `/v1/ruleset` | 返回当前完整 ruleset（按 owner 分段）`{"owners":{"tui":[...],"panel":[...]}}` |
| `POST` | `/v1/ruleset/{owner}` | 全量替换该 owner 的 segment，body `{"rules":[...]}` |
| `GET` | `/v1/counters` | 每条规则的字节 / 包计数（按 proto + src_port keyed） |

`POST /v1/ruleset/{owner}` 的冲突语义：同 owner 内端口重复返回 `400`；跨 owner 端口冲突（后提交抢占已占端口）返回 `409`，响应体说明被哪个 owner 占用。`GET /v1/ruleset`（不带 `{owner}`）返回只读视图，`POST /v1/ruleset`（扁平，无 owner 路径段）返回 `410 Gone`，不再接受。

远程节点的 HTTP-enable 接口与 Unix socket 端点完全等价，额外要求 `Authorization: Bearer <token>` 头（token 从 `--token-file` 读取）。详细设计与数据类型见：

> `docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md`

---

## 已知限制

- 仅 IPv4。IPv6 架构上加 `ip6 nft_forward` 表即可，暂未实现
- 限速只在 egress 方向。双向限速需 ifb，未实现
- 面板单实例（SQLite，无水平扩展）
- daemon HTTP-enable 模式不内置 TLS；建议在前置反代（Caddy / Nginx）处做 TLS 终结，并把 agent token 通过安全信道分发
- 所有自动化测试覆盖 `internal/daemon` 与 `internal/daemonclient` 单元层；端到端 systemd / `install.sh` 流程仍需手动验证
