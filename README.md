# nft-forward

基于 nftables 的轻量端口转发工具。一份代码、一个仓库提供三种运行形态：

| 二进制 | 角色 | 体积 |
|---|---|---:|
| `nft-forward` | 单机 TUI（直接管本机 nftables） | ~3.7 MB |
| `nft-agent` | 受控节点（被 panel 推送规则） | ~5.9 MB |
| `nft-server` | 控制面板（Web UI + SQLite + 多租户 + 配额 + 限速） | ~13 MB |

设计取舍详见 [架构](#架构)；功能完整度对照 flux-panel 的 Spring Boot + MySQL + GOST + React 体系，**整套依赖只剩下 nftables 与 iproute2**。

---

## 目录

- [快速构建](#快速构建)
- [运行模式 1：单机 TUI](#运行模式-1单机-tui)
- [运行模式 2：中心面板 + 远程节点](#运行模式-2中心面板--远程节点)
  - [启动 server](#启动-server)
  - [启动 agent](#启动-agent)
  - [Server 主动连接 Agent](#server-主动连接-agent)
- [多租户使用](#多租户使用)
- [流量配额与带宽限速](#流量配额与带宽限速)
- [常用配置项](#常用配置项)
- [忘记 admin 密码 / 故障恢复](#忘记-admin-密码--故障恢复)
- [架构](#架构)

---

## 快速构建

依赖：Go ≥ 1.21（推荐 1.22+）。无 CGO，跨平台编译。

```bash
git clone <repo> nft-forward && cd nft-forward
mkdir -p build
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-forward ./cmd/nft-forward
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-agent   ./cmd/nft-agent
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-server  ./cmd/nft-server
```

arm64 把 `GOARCH=amd64` 换成 `GOARCH=arm64`。三个二进制完全独立，按需分发。

### 目标机系统要求

- Linux 内核 ≥ 5.10，支持 nftables（Debian 12 自带 6.1，满足）
- Debian / Ubuntu 系（用 apt-get）—— 启动时若缺 `nftables` / `iproute2` 会**非交互自动 apt-get 安装**
- 其他发行版（RHEL/Arch/Alpine）：自己装好 `nftables` + `iproute2` 即可，二进制不会自动调 apt

### 运行时依赖一览

| 二进制 | 必装 | 启动时自动兜底 |
|---|---|---|
| `nft-forward`（TUI） | `nftables` | ✅ 启动若缺会自动 `apt-get install -y nftables` + `modprobe nf_tables` |
| `nft-agent` | `nftables` + `iproute2` | ✅ 同上，二者皆自动装 |
| `nft-server` | `nftables` + `iproute2` | ✅ 同上 |

自动兜底的行为：

- 用 `DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends <missing>` 安装
- 全程不弹任何 y/N 提示
- 非 root 启动 / 没 apt-get → 立即报错给清晰说明，不死撑

预先一次装好也可以（适合 image baking）：

```bash
sudo apt-get install -y nftables iproute2
```

---

## 运行模式 1：单机 TUI

适用：自己一台机器，没有面板需求。直接编辑本机 nftables。

```bash
sudo ./nft-forward
```

首次启动会：

1. 检查 root；
2. 若未装 nftables → 询问是否 `apt-get install`，拒绝则退出；
3. 若 `net.ipv4.ip_forward=0` → 自动启用，并写入 `/etc/sysctl.d/99-nft-forward.conf` 让重启后保留；
4. 询问是否启用开机持久化（推荐 Y）。同意后：
   - 复制自身到 `/usr/local/sbin/nft-forward`
   - 注册 `/etc/systemd/system/nft-forward.service`
   - `systemctl enable` 它，开机自动用 `nft-forward --apply` 重放 `/etc/nft-forward/rules.json`

TUI 键位：

| 键 | 作用 |
|---|---|
| ↑/↓ 或 j/k | 选中规则 |
| a / n / + | 新增转发 |
| d | 删除当前选中 |
| c | 清空全部 |
| r | 从磁盘重新加载 |
| q | 退出 |

新增表单字段：协议（tcp/udp）、监听端口、目标 IPv4、目标端口、备注。`Tab` 切换字段，`Enter` 保存，`Esc` 取消。

其他命令行参数：

```text
--apply              加载 rules.json 并应用到内核后退出（开机由 systemd 调用）
--install-service    安装 systemd 持久化单元后退出
--uninstall-service  卸载 systemd 单元（rules.json 与内核状态保留）
```

---

## 运行模式 2：中心面板 + 远程节点

适用：一台 panel + N 台分布在各处的 VPS 节点。Panel 不直接转发流量，只做控制面；节点跑 agent 接收推送。

### 启动 server

Server **必须以 root 运行**：它在启动时自动把自己注册成名为 `localhost` 的节点，进程内拉起 agent 处理本机的 nftables / tc。所以 server 所在的 host 也成了一个受控节点，admin 可以直接在面板上给它绑通道、推转发。首次启动若缺 `nftables` / `iproute2` 会**非交互自动 apt 安装**。

```bash
# 最简
sudo ./nft-server --addr :8080

# 生产推荐：固定 admin 密码、自定义 DB 路径
sudo ./nft-server \
  --addr :8080 \
  --db /var/lib/nft-forward/panel.db \
  --bootstrap-admin-password 'YourStrongPassword!' \
  --agent-iface eth0
```

首次启动会创建 `admin` 账号。若没传 `--bootstrap-admin-password`，会**随机生成密码并打印到 stdout**：

```
================================================
 首次启动 - 已创建管理员账号
 用户名: admin
 密  码: <随机 16 位 hex>
 请妥善保存。可通过 --bootstrap-admin-password 自定义。
================================================
```

打开 `http://<server-ip>:8080`，用 admin / 上面那个密码登录。**强烈建议**先点右上「修改密码」改成自己的。

Server 启动时会：

1. 检查 root + nftables + tc，自动启用 `ip_forward`
2. 在 DB 中创建 / 复用名为 `localhost` 的节点，地址为 `local://`（特殊 sentinel scheme，不开端口、不走网络）
3. 进程内启动 agent 实例并 `Bootstrap`（从 `/var/lib/nft-forward/embedded-agent-state.json` 恢复上次 ruleset）
4. 启动两个后台 goroutine：
   - **Pusher**：当节点/通道/转发任何一处变更时，把新 ruleset 推到对应节点。远程节点走 HTTP，本机节点直接 Go 方法调用（零网络开销，不暴露任何端口）。节点离线时标记 dirty，30s 周期自动 reconcile。
   - **Poller**：每 5s 拉一次每个节点的计数；远程节点拉 `/v1/counters`，本机节点直接调内嵌 agent 的方法。把 nft 计数差量累加到 `tenants.traffic_used_bytes`；超额或过期则自动禁用租户并清空规则。

Server flag：

| flag | 默认 | 说明 |
|---|---|---|
| `--addr` | `:8080` | 面板 HTTP 监听地址 |
| `--db` | `/var/lib/nft-forward/panel.db` | SQLite 数据库路径（WAL 模式） |
| `--bootstrap-admin-password` | _空_ | 首次启动给 admin 设的密码；空则随机 16 hex 字符并打印 |
| `--agent-iface` | 自动检测默认路由 | 内嵌 agent 的数据面网卡（tc HTB 作用对象） |
| `--reset-admin-password` | _空_ | 非空时把指定 admin 账号密码重置为该值后退出（见[故障恢复](#忘记-admin-密码--故障恢复)） |
| `--reset-admin-username` | `admin` | 配合 `--reset-admin-password` 指定账号名 |

生产部署建议在 server 前面放 Caddy/Nginx 做 TLS 终结。Server 本身只跑 HTTP（远程 agent 的 token 在调用时走 Bearer 头；面板登录必须由 TLS 反代保护）。

### 启动 agent

Agent **必须 root**（因为要操作 nftables 和 tc）。每个节点：

```bash
# 1. 把 token 写到节点上（Server 添加节点时会显示）
sudo install -d /etc/nft-forward /var/lib/nft-forward
echo '<server 给的 64 位 hex token>' | sudo tee /etc/nft-forward/agent.token >/dev/null
sudo chmod 600 /etc/nft-forward/agent.token

# 2. 启 agent；首次启动若缺 nftables / iproute2 会自动 apt 安装
sudo ./nft-agent \
  --listen :7878 \
  --token-file /etc/nft-forward/agent.token
```

Agent flag：

| flag | 默认 | 说明 |
|---|---|---|
| `--listen` | `:7878` | HTTP 监听地址；`:7878` 监听全部 IP，`192.168.1.10:7878` 只监听指定 NIC IP |
| `--iface` | 自动检测（解析默认路由）| tc HTB 作用的数据面网卡。识别失败回落到 `eth0`，可手动指定如 `ens3` |
| `--token-file` | `/etc/nft-forward/agent.token` | bearer token 文件 |
| `--token` | _空_ | 调试用，直接传 token 字符串 |
| `--state` | `/var/lib/nft-forward/agent-state.json` | 本地缓存的最近 ruleset；agent 重启时用它先恢复内核状态，不必等 panel 重连 |
| `--skip-nft-check` | false | 跳过 nft / ip_forward 启动检查 |

生产环境部署成 systemd unit（agent 不自己装，避免和 panel 控制范围冲突，自己写或用以下模板）：

```ini
# /etc/systemd/system/nft-agent.service
[Unit]
Description=nft-forward agent
After=network-online.target nftables.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/sbin/nft-agent --listen :7878 --token-file /etc/nft-forward/agent.token
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now nft-agent
```

### Server 主动连接 Agent

**方向**：panel → agent（panel 是 HTTP client，agent 是 server）。Agent 端必须能被 panel 的出口访问到（节点 7878 端口对 panel 开放）。

流程：

1. 在 panel 上 **节点 → 添加节点**：
   - 名称：随意，比如 `hk-1`
   - Agent 地址：`http://10.0.0.1:7878` 或 `https://node-1.example.com:7878`
   - Token：留空 = 由 panel 随机生成 64 位 hex；也可以填一个自己定的串
2. 提交后跳到节点详情页，会显示**完整的一行 systemd 安装命令**（含 token），直接复制到目标机器执行即可
3. 节点上 agent 启动后，下一次 panel 任何变更触发 push 都会自动验证连通性；详情页 30s 内会从「待同步」变成「已同步」

注意事项：

- agent 不主动连 panel，因此 agent **不需要出口公网**。Panel 必须能反向访问 agent 的 7878。如果节点在 NAT 后，需要把 panel 放到节点能访问的位置（VPN / Tailscale / 反向隧道）或在节点上开放端口
- Token 全程经过 `Authorization: Bearer` 头传递。建议在 panel 前置 TLS（Caddy / Nginx），并在 agent 前置 TLS 或绑定到内网 IP（`--listen 10.0.0.1:7878`）以避免明文传输 token
- 如果只想让 panel 这台机器本身作为节点（不部署任何远程 agent），不用额外做什么——server 启动时已自动注册自身为 `localhost` 节点

---

## 多租户使用

完整 admin 操作流（典型一次新租户开通）：

1. **节点 → 添加节点**：登记一台或多台节点
2. **通道 → 新建通道**：在某个节点上划定一个端口段 + 协议 + 目标 CIDR 白名单 + 可选带宽
   - 例：`name=t-hk1, node=hk-1, proto=tcp+udp, port_start=20000, port_end=20999, target_cidr_allow=10.0.0.0/24, bandwidth_mbps=10`
3. **租户 → 新建租户**：填名称、最大转发数（默认 100）、流量配额 MB（0 = 不限）
4. **租户详情 → 授权通道**：把刚建好的通道授权给这个租户，设置在该通道内的最大转发数
5. **租户详情 → 添加账号**：填用户名 / 密码 → 把这两个字段交给租户

租户拿到账号后：

- 登录 panel → 自动跳到「我的面板」（只能看自己的配额和被授权的通道，看不到节点/其他租户等信息）
- 「我的转发」→ 新增：监听端口必须落在通道允许的端口段内、目标 IP 必须在 CIDR 白名单内，否则被拒
- 可随时删除自己的转发；管理员侧 audit_logs 会记录

Admin 对租户的额外操作（租户详情页）：

| 操作 | 行为 |
|---|---|
| 禁用 / 启用 | 立即推空规则给所有节点 / 重新下发 |
| 重置流量并启用 | 清零已用流量，撤销「超额自动禁用」 |
| 自定义 quota 字节 | 测试用，精度到字节（UI 上 MB 粒度不够时） |
| 删除租户 | 连带删除其全部转发，释放占用的端口 |

Admin 对用户的操作（用户列表 / 租户详情）：

- **禁用账号**：账号 = tenant 时**同步禁用租户**（转发立即失效）；admin 账号只禁用登录
- **重置密码**：生成 16 位 hex 随机密码，**一次性**通过 flash 提示框下发，刷新即丢
- **删除账号**：如果是某租户的最后一个账号 → 连带删除租户和它的全部转发；否则只删账号

任何登录用户都可以右上角「修改密码」自助改密。

---

## 流量配额与带宽限速

**配额（A）**：每条转发的 nft 规则都带 `counter`。Poller 每 5s 把 (proto, listen_port) 对应的字节数累到 `tenants.traffic_used_bytes`：

- `traffic_quota_bytes == 0`：不限
- `traffic_used_bytes >= traffic_quota_bytes`：自动 `disabled=1`，立即清空该租户在所有节点的规则；audit_logs 留痕
- 也会因 `expires_at` 到期触发

**限速（B）**：在通道上设置 `bandwidth_mbps > 0`，agent 会：

1. 在 nft 规则前加 `meta mark set <listen_port>`，给匹配的包打标
2. 在数据面网卡上建立 HTB 树：
   - `qdisc 1: htb default 1`，class `1:1` 为兜底（100Gbit，等于不限速）
   - 每条限速规则 `class 1:<port-hex> rate <mbps>mbit ceil <mbps>mbit`
   - filter `fw handle 0x<port-hex> classid 1:<port-hex>` 路由打标的包到对应 class

只生效在**egress 方向**（数据出本机网卡时）。需要双向限速可以下一轮加 ifb，目前未实现。

校验命令：

```bash
sudo nft list table ip nft_forward     # 找到带 meta mark 的规则
sudo tc qdisc show dev eth0            # 应有 qdisc htb 1: root
sudo tc class show dev eth0            # 应有 class htb 1:<port-hex>
sudo tc filter show dev eth0           # 应有 fw filter -> classid
```

---

## 常用配置项

环境变量 / 文件位置：

| 路径 | 用途 |
|---|---|
| `/var/lib/nft-forward/panel.db` | server SQLite |
| `/var/lib/nft-forward/agent-state.json` | 远程 agent 最近一次 ruleset 缓存 |
| `/var/lib/nft-forward/embedded-agent-state.json` | server 内嵌 agent 缓存 |
| `/etc/nft-forward/agent.token` | agent 的 bearer token |
| `/etc/nft-forward/rules.json` | TUI 模式的真相源 |
| `/etc/sysctl.d/99-nft-forward.conf` | ip_forward 持久化 |
| `/etc/systemd/system/nft-forward.service` | TUI 模式的开机持久化 unit |

环境变量：

| 变量 | 作用 |
|---|---|
| `NFT_FORWARD_CONFIG` | 覆盖 TUI 模式默认的 `rules.json` 路径 |

---

## 忘记 admin 密码 / 故障恢复

### 方法 1（推荐）：用 server 自带的重置命令

停掉正在跑的 server（systemctl / docker / Ctrl-C 任意一种），然后用同一个二进制带上 `--reset-admin-password` 跑一次：

```bash
sudo /usr/local/sbin/nft-server \
  --db /var/lib/nft-forward/panel.db \
  --reset-admin-password 'MyNewStrongPw!'
```

输出 `已重置 admin 的密码（同时清空其所有活跃会话、解除禁用状态）` 后进程退出。然后正常 `systemctl start nft-server`（或其他启动方式）即可用新密码登录。

行为：

- 只改密码，**其他数据全部保留**（节点、用户、转发、计数）
- 顺手清掉该账号的 `disabled` 标志，万一你之前把自己禁用了也能救回来
- 清空该账号的所有 `sessions` 行——意外泄露的 cookie 立刻失效
- 写入 `audit_logs`（`action=admin.reset_password_cli`）
- 不启动 HTTP server，单次执行后退出

想要随机密码：

```bash
NEW_PW=$(openssl rand -hex 8)
sudo nft-server --db /var/lib/nft-forward/panel.db --reset-admin-password "$NEW_PW"
echo "新密码: $NEW_PW"  # 自行保管
```

只能重置 `role=admin` 的账号；要重置租户用户的密码请走面板 `/users/{id}/reset-password`（管理员登录后操作）。

### 方法 2（兜底）：直接改 SQLite

如果手里的 server 还没更新到带 `--reset-admin-password` 的版本，可用 sqlite3 工具 + 生成 bcrypt hash：

```bash
sudo apt-get install -y sqlite3 python3-bcrypt
HASH=$(python3 -c 'import bcrypt; print(bcrypt.hashpw(b"MyNewStrongPw!", bcrypt.gensalt()).decode())')

sudo systemctl stop nft-server
sudo sqlite3 /var/lib/nft-forward/panel.db \
  "UPDATE users SET pw_hash='$HASH', disabled=0 WHERE username='admin';
   DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username='admin');"
sudo systemctl start nft-server
```

### 方法 3（核选项）：admin 账号都被误删了

如果连 admin 用户记录都没了：

```bash
sudo systemctl stop nft-server
sudo sqlite3 /var/lib/nft-forward/panel.db "DELETE FROM users WHERE role='admin';"
sudo nft-server --db /var/lib/nft-forward/panel.db --bootstrap-admin-password 'MyNewPw!'
```

`bootstrap` 逻辑检测到没有用户后会自动重建 admin。业务数据（节点 / 通道 / 用户 / 转发）保持不变。

### 防呆建议

- 用密码管理器保存初始 / 重置后的密码，少靠记忆
- 定期备份 `panel.db`：`cp /var/lib/nft-forward/panel.db /backup/panel.db.$(date +%F)`
- 多创建一个 admin 账号互为备份（目前需要直接写 SQL；后续会加多 admin 管理 UI）

---

## 架构

```
┌────────────────────────┐         ┌────────────────────────┐
│      Browser           │  HTTPS  │  Caddy / Nginx (TLS)   │
│  (admin / tenant)      │ ──────► │  reverse proxy         │
└────────────────────────┘         └───────────┬────────────┘
                                               │ HTTP :8080
                                   ┌───────────▼────────────────┐
                                   │      nft-server (root)      │
                                   │  ┌──────────────────────┐   │
                                   │  │ chi router + UI      │   │
                                   │  │ SQLite (WAL)         │   │
                                   │  │ Pusher goroutine     │   │
                                   │  │ Poller goroutine     │   │
                                   │  │ 内嵌 agent (in-proc) │   │
                                   │  │  ↳ 进程内方法调用    │   │
                                   │  │  ↳ 节点名 localhost  │   │
                                   │  │  ↳ 地址 local://     │   │
                                   │  └────────┬─────────────┘   │
                                   └───────────┼─────────────────┘
                                               │ Bearer token, JSON
                                               │ POST /v1/apply
                                               │ GET  /v1/counters
                                               │ GET  /v1/status
                                  ┌────────────┴─────────┐
                                  │                      │
                          ┌───────▼──────┐      ┌────────▼─────┐
                          │  nft-agent   │      │  nft-agent   │
                          │  (hk-1)      │      │  (us-1)      │
                          │              │      │              │
                          │  • nft       │      │  • nft       │
                          │  • tc HTB    │      │  • tc HTB    │
                          └──────────────┘      └──────────────┘
```

**核心约束**：

- panel 是 HTTP client，远程 agent 是 server。**节点不主动连 panel**，因此节点不需要出口；但 panel 必须能反向访问节点 7878
- panel 启动时强制把自己也作为节点纳入：进程内嵌 agent，地址 `local://`，pusher / poller 走 Go 方法调用，不开任何端口
- nftables 用专用表 `ip nft_forward`，不污染用户已有规则；每次 apply 是「add table + delete table + recreate」的原子三步
- 远程 agent 本地缓存最近一次 ruleset；agent 单独重启时不依赖 panel 也能恢复（内嵌 agent 缓存到 `embedded-agent-state.json`）
- panel 把 SQLite 当唯一真相源；任何节点上的差异都会在下一次 push / 30s reconcile 中被覆盖
- 所有写操作 → `audit_logs` 表

代码组织（Go 3700+ 行 / HTML 模板 660+ 行）：

```
nft-forward/
├── cmd/
│   ├── nft-forward/  TUI 入口
│   ├── nft-agent/    Agent 入口
│   └── nft-server/   Server 入口（启动时强制注册自身为 localhost 节点）
├── internal/
│   ├── nft/          nftables 渲染 + apply + counter 解析
│   ├── tc/           HTB qdisc/class/filter 重建
│   ├── store/        TUI 的 rules.json 持久化
│   ├── systemd/      开机持久化 unit
│   ├── tui/          bubbletea 实现
│   ├── db/           SQLite schema + migrations + 查询
│   ├── agent/        Agent HTTP API（/v1/apply、/v1/counters、/v1/status）
│   └── server/       Panel：路由、Auth、handlers、Pusher、Poller、HTML 模板
└── docker/           e2e 测试（1 server + 3 agents）
```

---

## 已知限制

- 仅 IPv4。IPv6 没做（架构上加一份 `ip6 nft_forward` 表即可）
- 限速只在 egress 方向。双向限速需 ifb，未实现
- panel 单实例。SQLite + 单文件，没做水平扩展（也不打算做）
- 没做 mTLS，节点 token 走 Bearer。需要更强保护时前置反代 + 客户端证书
- 没做计费 / 套餐 / 自助注册 / 邮件 / 多语言。请用其他工具的话用 flux-panel
