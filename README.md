# nft-forward

基于 nftables 的轻量端口转发工具。**单一二进制，角色按需叠加**：一份发布产物在同一台主机上可以同时充当本机转发 TUI、多租户 Web 控制面板以及远程受控节点，三种功能共用同一个 host daemon，规则互不覆盖。整套运行时依赖只有 nftables 与 iproute2；无 gRPC、无额外守护进程。

---

## 一键安装

```bash
# 交互式（依次可选 tui / server / agent / update / uninstall）
sudo bash <(curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh)

# 或直接指定角色（可管道，无需 TTY）—— tui 单机 / server 面板 / agent 受控节点
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s -- tui
# wget 版
wget -qO- https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s -- tui
```

> 仅支持 amd64 Linux；脚本下载单一二进制并按角色配置 systemd 服务，需 root。升级用 `update`，卸载用 `uninstall <角色>`。

---

## 目录

- [一键安装](#一键安装)
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

# 远程受控节点（agent 反向连向 panel，宿主机不暴露端口）
sudo bash install.sh agent --panel-url https://panel.example.com --token <64位hex>

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

### agent 节点（远程纳管）

agent daemon 通过 WebSocket 反向连向 panel，不在宿主机暴露任何端口。

```bash
sudo bash install.sh agent --panel-url https://panel.example.com --token <64位hex>
```

Token 在面板节点详情页生成。Panel URL 必须是 agent 能到达的地址（如反代后的公网域名）。
http(s):// 和 ws(s):// 自动归一为 wss/ws，并 append `/v1/agents` 路径。

要从 panel 卸载 agent 角色（保留本地 forward）：

```bash
sudo bash install.sh uninstall agent
```

这会把 panel 推过的规则合并回本地 tui 段，重启 daemon 进入纯 TUI 模式；
所有 forward 继续生效，只是改由本地 TUI 管理。`--purge` 则把所有 panel
推过的规则一并清空。

### Docker 部署

`docker/docker-compose.yml` 提供基线模板：daemon 跑在 host network（nftables
需要），server 跑在 `nftf` bridge 网络，默认不映射端口。

- 本机临时访问：取消 compose 里 `server.ports` 的注释。
- 生产用反代：自行加 caddy / nginx / traefik service 进 `nftf` 网络，
  反代 `nftf-server:8080`。

agent 节点装机后，daemon 需要能从本地网络到达 panel 的 WSS 入口。
反代必须允许 WebSocket Upgrade、且 idle timeout ≥ 60s（dialer 每 10s
ping，避开默认 60s 反代 timeout）。

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
                                        │  Agent Hub (WSS endpoint)  │
                                        │  Dispatcher                │
                                        └────────────┬───────────────┘
                                                     │ HTTP over unix socket
                                        ┌────────────▼───────────────┐
                                        │   nft-forward daemon       │
                                        │  /var/run/nft-forward.sock │
                                        │  owner-segmented ruleset   │
                                        │  nftables + tc HTB         │
                                        └────────────▲───────────────┘
                                                     │ WebSocket（wss）
                                                     │ Bearer token / JSON 帧
                                                     │ apply_ruleset 下行
                                                     │ counters / register_local 上行
                                 ┌───────────────────┴───────┐
                             ┌───┴────────┐   ┌──────────────┴─┐
                             │  远程节点 1 │   │  远程节点 2     │
                             │ nft-forward│   │ nft-forward    │
                             │ daemon     │   │ daemon         │
                             │ --connect  │   │ --connect      │
                             │ wss://…    │   │ wss://…        │
                             └────────────┘   └────────────────┘
```

**关键设计约束**：

- `nft-forward daemon` 是主机上**唯一**直接操作 nftables 和 tc 的进程。TUI 和 server 都不再直接调用 `nft`，全部通过 daemon 的 Unix socket HTTP API 提交规则。
- Daemon 内部维护 **owner-segmented ruleset**：每个 owner（`tui`、`panel`）独占自己的规则段。Server 下发的 panel 段不影响用户在 TUI 里添加的 tui 段，反之亦然。跨段端口冲突时，daemon 拒绝**后提交**的请求并说明被哪个 owner 占用。
- Server（Web 面板）通过 Unix socket 操作本机 daemon；远程节点由 daemon 主动反向连接 server 的 `/v1/agents` WebSocket。本机节点与远程节点共用同一套 hub/dispatcher 逻辑，仅 transport 不同（unix socket vs WSS）。
- Agent daemon 模式（`--connect wss://panel/v1/agents --panel-token-file ...`）让 daemon 主动 dial panel，承载 `apply_ruleset` 下行 + `counters` / `register_local` 上行。宿主机不暴露任何端口，bridge 网络 + 反代部署天然可行。
- Nftables 使用专用表 `ip nft_forward`，不影响主机已有的防火墙规则；每次 apply 是原子的三步（add → delete → recreate）。

---

## 命令表面

```
nft-forward                                     默认进 TUI（要求 daemon 已运行）
nft-forward daemon                              前台启动 daemon（systemd 通常负责）
nft-forward daemon --connect wss://panel/v1/agents \
    --panel-token-file /etc/nft-forward/panel.token  daemon 反向连接 panel，充当远程受控节点
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
| `/etc/nft-forward/panel.token` | Agent 反向连接 panel 时使用的 bearer token（mode `0600`） |
| `/etc/sysctl.d/99-nft-forward.conf` | ip_forward 持久化 |

环境变量：

| 变量 | 作用 |
|---|---|
| `NFT_FORWARD_DNS_INTERVAL` | Agent / TUI 后台重解析 DDNS 周期（如 `30s`、`2m`），缺省 60s |

**DDNS 目标**：转发目标支持域名（如 `home.example.ddns.net`）。解析只在 daemon 侧进行，按 `NFT_FORWARD_DNS_INTERVAL` 周期重解析；底层 IP 变化时自动重建 nftables 规则。解析失败时保留上次成功的 IP，不撕掉已生效的转发。多租户场景下，设置了 `target_cidr_allow` 的通道只允许直接填 IPv4，不允许域名，避免通过 DNS 绕过 CIDR 限制。

**流量配额与限速**：每个 agent daemon 在 WSS 上周期性 push `counters` 帧（默认 5s 一次），server 的 hub 入库累加到 `tenants.traffic_used_bytes`；超额时自动禁用租户并清空其规则。限速通过 tc HTB 实现：daemon 在数据面网卡上建 HTB 树，按规则的监听端口打 nfmark 后路由到对应 class。

---

## 升级与迁移

> **2026-05 升级注意**：本版本翻转了 agent ↔ panel 通信方向。旧版的
> `daemon --listen :PORT` 路径已删除；旧 agent 必须重装：
>
> ```bash
> sudo bash install.sh agent --panel-url https://panel.example.com --token <旧 token>
> ```
>
> install.sh 会自动 detect 已有的 `--listen` 残留 unit、改写为 `--connect`
> 形态，旧 token 仍然有效（token 模型沿用 nodes.secret 不变）。

### 日常升级

新版发布后用 `install.sh update` 升级现有部署：

```bash
sudo bash install.sh update
```

行为：拉 GitHub latest 二进制 → sha256 校验 → ELF x86-64 架构校验 → 备份旧二进制到 `/usr/local/sbin/nft-forward.bak` → 原子替换 → `systemctl restart nft-forward-daemon.service`（+ `nft-forward-server.service` 如有）→ 10 秒 health-check。失败自动回滚到旧二进制并重启。

约束：

- `update` 总是拉 `latest`；要锁版本/降级请用 `install.sh tui --release v1.2.3`（等价于重装）
- `update` 不动 systemd unit 文件，`--connect` / `--addr` / token 等角色配置完全保留
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
2. 删除旧的独立二进制 `/usr/local/sbin/nft-server` 和 `/usr/local/sbin/nft-agent`（这两个名字在新架构中已不存在，功能分别由 `nft-forward server` 和 `nft-forward daemon --connect ...` 承担）；
3. 安装新的单一二进制并写入 `nft-forward-daemon.service`（和 server 角色的 `nft-forward-server.service`）。

**旧 state 文件迁移**：daemon 首次启动时（`state.json` 尚不存在），自动检测并导入旧格式文件：

| 旧文件 | 导入到 |
|---|---|
| `/etc/nft-forward/rules.json` | `state.json` → `tui` segment |
| `/var/lib/nft-forward/agent-state.json` | `state.json` → `panel` segment |
| `/var/lib/nft-forward/embedded-agent-state.json` | `state.json` → `panel` segment（优先级高于上一条） |

每个被处理的文件重命名为 `<原路径>.migrated`（不删除，留人工备份）。后续 daemon 重启不重复迁移：只要 `state.json` 已存在，迁移跳过。

**面板数据库迁移**：旧版 `nft-server` 的 `panel.db` 直接复用，新版 `nft-forward server` 首次启动会执行 schema migration，把本机节点的地址由旧的 `local://` 更新为 `unix:///var/run/nft-forward.sock`，业务数据（节点、通道、租户、转发记录）保持不变。

### 防火墙兼容

daemon 启动 / 每次 apply 时会自动同步 `DOCKER-USER`（如果装了 Docker）和 `ufw-user-forward`（如果装了 ufw）chain 里的放行规则，让 nft-forward 的 DNAT 流量穿透 Docker / ufw 把 FORWARD policy 设成 drop 的环境。daemon 卸载或停止时这些规则自动清除。

无 Docker / 无 ufw 的纯净系统：daemon 不动任何 chain，启动日志没有 shim 相关输出。

如果你装了别的 firewall 工具（firewalld 等）让 FORWARD policy=drop 但 daemon 不能自动处理，启动日志会有一行 `WARN: FORWARD chain has drop policy but no known firewall shim detected`——这时需要手动在你的 firewall 里放行 nft-forward DNAT 后的流量。

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
│   ├── wsproto/            agent ↔ panel WebSocket 帧定义（apply_ruleset / counters / register_local）
│   └── server/             Web 面板：路由、认证、handler、agent hub、dispatcher、HTML 模板
└── docker/                 dev fixture（Dockerfile + docker-compose.yml）
```

Socket 权限：daemon 创建 `nft-forward` system group（若不存在），socket 文件属于该 group（`0660`）。希望不带 `sudo` 运行 TUI 的用户加入该 group 即可；`install.sh` 文档中有说明。

---

## 协议参考

Daemon 暴露的本地 HTTP API（仅 Unix socket）：

| Method | Path | 用途 |
|---|---|---|
| `GET` | `/v1/health` | 探活，返回 `{"ok":true}` |
| `GET` | `/v1/ruleset` | 返回当前完整 ruleset（按 owner 分段）`{"owners":{"tui":[...],"panel":[...]}}` |
| `POST` | `/v1/ruleset/{owner}` | 全量替换该 owner 的 segment，body `{"rules":[...]}` |
| `GET` | `/v1/counters` | 每条规则的字节 / 包计数（按 proto + src_port keyed） |

`POST /v1/ruleset/{owner}` 的冲突语义：同 owner 内端口重复返回 `400`；跨 owner 端口冲突（后提交抢占已占端口）返回 `409`，响应体说明被哪个 owner 占用。`GET /v1/ruleset`（不带 `{owner}`）返回只读视图，`POST /v1/ruleset`（扁平，无 owner 路径段）返回 `410 Gone`，不再接受。

Server 与远程 agent daemon 之间的 WebSocket 协议（path `/v1/agents`，Bearer
token 鉴权，token 沿用 `nodes.secret`）下行 `apply_ruleset`、上行 `counters` /
`register_local` / `tui_segment_changed` 等 JSON 帧。详细帧结构见 `internal/wsproto/`
与设计文档：

> `docs/superpowers/specs/2026-05-26-agent-ws-architecture-design.md`

---

## 已知限制

- 仅 IPv4。IPv6 架构上加 `ip6 nft_forward` 表即可，暂未实现
- 限速只在 egress 方向。双向限速需 ifb，未实现
- 面板单实例（SQLite，无水平扩展）
- panel 的 `/v1/agents` WSS 入口不内置 TLS；agent 反向连接要走可信反代（Caddy / Nginx / Traefik）做 TLS 终结，并允许 WebSocket Upgrade + 长连接（idle ≥ 60s）。agent token 须通过安全信道分发
- 所有自动化测试覆盖 `internal/daemon` 与 `internal/daemonclient` 单元层；端到端 systemd / `install.sh` 流程仍需手动验证
