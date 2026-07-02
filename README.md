# nft-forward

基于 nftables 的轻量多节点端口转发平台。**两个二进制（`nft-server` + `nft-agent`），零外部依赖**——面板管理多节点推送规则，节点 agent 反向连入面板，零端口暴露。支持内核态 DNAT 与用户态 split-TCP 逐跳混用，多租户配额管理，组合节点自动编排多跳链路。

---

## 与同类方案对比

| 维度 | realm | gost | 哆啦A梦面板 | **nft-forward** |
|------|-------|------|-----------|-----------------|
| **数据面** | 用户态 relay | 用户态 relay + 协议栈 | 调用 iptables/realm | nftables 内核态 DNAT **或** 用户态 split-TCP，逐跳可选 |
| **多节点管理** | 无 | 无 | 有，节点需开端口 | agent 反向 WebSocket 连入面板，节点零端口暴露 |
| **多跳链路** | 手动逐节点配置 | 转发链配置复杂 | 需手动对齐端口 | 面板自动编排：选跳 + 填出口，端口全自动分配对齐 |
| **多租户** | 无 | 无 | 有，粗粒度 | 节点级授权 + 规则配额 + 流量配额（全局 / per-node）+ 到期 + 周期重置 |
| **预连接** | 无 | 无 | 无 | userspace 模式内置 TCP 预连接池，多跳省握手 RTT |
| **TUI** | 无 | 无 | 无 | 内置终端 TUI，单机免浏览器 |

---

## 一键安装

交互式（自动选模式）：

```bash
sudo bash <(curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh)
```

直接指定角色（管道模式）：

```bash
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s server
curl -fsSL ... | sudo bash -s server --addr 127.0.0.1:7788
curl -fsSL ... | sudo bash -s agent --panel-url https://panel.example.com --token <hex>
curl -fsSL ... | sudo bash -s tui
```

> 仅支持 amd64 Linux；需 root + nftables。

### install.sh 模式

| 模式 | 说明 |
|------|------|
| `tui` | 单机 TUI（装 nft-agent，自动启动 daemon） |
| `server` | 控制面板（装 nft-server + nft-agent，含 daemon） |
| `agent` | 远程节点（daemon 反向 dial 面板 WebSocket） |
| `update` | 拉 latest 二进制原子替换 + restart + 失败自动回滚 |
| `update-script` | 更新本地 install.sh（不动二进制） |
| `uninstall <角色>` | 卸载指定角色（`server` / `agent` / `daemon` / `all`） |
| `reset-password` | 重置面板 admin 密码 |

升级：`sudo nft-forward-upgrade`（安装时自动生成的脚本）。

---

## 架构

```
┌──────────────┐        ┌─────────────────┐
│  浏览器       │ HTTPS  │  反向代理 (可选)  │
│  admin/user  │ ─────► │  Caddy / Nginx   │
└──────────────┘        └────────┬────────┘
                                 │ HTTP
                        ┌────────▼────────┐
                        │   nft-server    │
                        │  Web 面板 + API  │
                        │  Agent Hub      │
                        │  SQLite WAL     │
                        └────────┬────────┘
                                 │ Unix socket / WSS
                 ┌───────────────┼───────────────┐
            ┌────▼────┐    ┌─────▼─────┐    ┌────▼─────┐
            │nft-agent│    │ nft-agent │    │nft-agent │
            │ (本机)   │    │ (节点 A)  │    │(节点 B)  │
            │ daemon  │    │ --connect │    │--connect │
            │nftables │    │ wss://…   │    │wss://…   │
            │ + tc    │    │ nftables  │    │nftables  │
            │ + pool  │    │ + pool    │    │+ pool    │
            └─────────┘    └───────────┘    └──────────┘
```

**`nft-server`**（面板）：Web UI（React SPA，embed 进二进制）、JSON API、agent hub（WebSocket 长连接管理）、SQLite 存储。

**`nft-agent`**（节点）：nftables 内核操作、tc 限速、userspace relay + 连接池、DNS 解析。无面板/sqlite/web 依赖，体积小（UPX 压缩后 ~2.5 MB）。不带参数运行进入 TUI，`daemon` 子命令启动后台服务。

---

## 数据面

### 内核态（默认）

nftables DNAT，零拷贝，吞吐接近线速。支持 TCP / UDP / TCP+UDP。

### 用户态（split-TCP）

每一跳独立建 TCP 连接，避免 TCP-over-TCP 拥塞折叠。多跳链路推荐。

内置 **TCP 预连接池**：每个转发端口预先向目标建立连接放入池中，新请求直接取用，跳过握手延迟。

| 环境变量 | 说明 | 默认 |
|---------|------|------|
| `NFT_FORWARD_POOL_SIZE` | 每端口预连接数（0 = 关闭） | `4` |
| `NFT_FORWARD_DNS_INTERVAL` | DDNS 重解析周期 | `60s` |

---

## 多租户

管理员创建用户并授权节点，用户自助在授权范围内创建转发规则。

### 用户配额

- **规则配额**：最大转发规则数
- **流量配额**：全局流量上限（GB），可设周期自动重置（如每 30 天）
- **到期时间**：支持快捷延期（+1天/+7天/+30天/+1年）
- **per-node 流量配额**：单节点独立限额
- **管理备注**：仅管理员可见的注释

### 节点授权

管理员将节点授权给用户，附带 per-node 规则上限。支持批量授权/撤销。

---

## 组合节点与多跳链路

管理员将多个单点节点编排为**组合节点**（如 `上海 → 日本`），指定跳序、节点间各段的模式（内核态/用户态）和流量倍率。用户在组合节点上创建规则，系统自动展开多跳、分配端口、对齐上下游。

- 拖拽调整跳序，端口保持稳定
- 节点间各段独立选内核态或用户态；出口段（最后一跳 → 目标）的模式由创建规则的用户选择
- 出口支持域名（daemon 侧 DNS 定期刷新）
- 连通性探测：每跳独立测延迟，结果逐跳显示

---

## 落地节点 / 代理订阅

管理员为用户配置**落地节点来源**：

- 支持订阅地址（Remnawave 等面板的订阅链接）
- 支持手动 URI（vless://、trojan:// 等）
- 订阅与手动 URI 可组合
- 解析出的节点可标记为「落地」「直连」或「未配置」
- 用户创建规则时可选落地节点作为出口

---

## Web 面板功能

### 管理员

- 节点管理：创建/编辑/禁用/删除/隐藏节点，设中继地址（IPv4 / IPv6）、端口范围、倍率
- 组合节点：创建多跳序列，拖拽排序，节点间各段模式与倍率（出口段模式归规则）
- 用户管理：创建/编辑/禁用/删除用户，统一表单设置配额/到期/备注
- 规则管理：查看/编辑/删除所有规则
- 节点推送升级：一键推送 agent 二进制到远程节点并重启
- 连通性探测：逐跳 TCP 探测延迟
- 安装命令生成：含 token 的一键安装命令，支持 gh-proxy

### 用户

- 自助创建/编辑/删除转发规则
- 查看流量使用、配额、到期状态
- 连通性探测
- 复制入口地址（URI / YAML 格式切换）
- 代理节点管理（本地浏览器存储）

### 前端

- React SPA + Tailwind CSS，embed 进 nft-server 二进制
- 响应式设计，移动端完整可用
- 深色/浅色主题切换
- 敏感信息脱敏模式

---

## 命令

```
nft-agent                      TUI 交互（需 daemon 运行中）
nft-agent daemon               前台启动 daemon
nft-agent daemon --connect wss://panel/v1/agents \
    --panel-token-file /etc/nft-forward/panel.token
                               agent 模式：反向连入面板

nft-server [--addr :8080] [--db PATH]
                               Web 面板
nft-server --reset-admin-password <newpw>
                               重置 admin 密码
```

首次启动 server 会创建 admin 账号并打印随机密码。

---

## 文件路径

| 路径 | 用途 |
|------|------|
| `/usr/local/sbin/nft-server` | 面板二进制 |
| `/usr/local/sbin/nft-agent` | 节点二进制（daemon + TUI） |
| `/var/run/nft-forward.sock` | daemon Unix socket |
| `/var/lib/nft-forward/state.json` | daemon 规则持久化 |
| `/var/lib/nft-forward/panel.db` | 面板 SQLite 数据库 |
| `/etc/nft-forward/panel.token` | agent token |
| `/usr/local/sbin/nft-forward-upgrade` | 自动生成的升级脚本 |

---

## 升级

```bash
sudo nft-forward-upgrade
# 或
sudo bash install.sh update
```

拉 latest → sha256 校验 → 备份 → 原子替换 → 重启 → health-check → 失败自动回滚。

---

## 开发

```bash
git clone <repo> && cd nft-forward

# 测试
go test ./...
go vet ./...

# 编译面板（含前端）
cd web && npm run build && cd ..
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o build/nft-server ./cmd/nft-server

# 编译节点（可复现构建，版本标签独立于二进制）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags="-s -w" -o build/nft-agent ./cmd/nft-agent
upx -9 --lzma build/nft-agent
```

依赖：Go ≥ 1.24，Node.js（前端构建），无 CGO。运行时需 nftables + iproute2（仅 Linux）。

```
nft-forward/
├── cmd/
│   ├── nft-server/         面板入口
│   └── nft-agent/          节点入口（daemon + TUI）
├── internal/
│   ├── daemon/             owner-segmented ruleset、状态持久化、WebSocket dial
│   ├── daemonclient/       TUI → daemon Unix socket 客户端
│   ├── forward/            数据面（nftables kernel + userspace relay + connpool）
│   ├── nft/                nftables 渲染 / apply / counter
│   ├── tc/                 tc HTB 限速
│   ├── tui/                bubbletea TUI
│   ├── db/                 SQLite schema + 查询
│   ├── server/             Web 面板（路由、认证、hub、dispatcher、probe、landing）
│   ├── wsproto/            agent ↔ panel WebSocket 协议帧
│   ├── resolver/           DNS 解析 + 定期刷新
│   ├── landing/            落地节点 URI 解析
│   ├── portutil/           端口范围解析
│   └── sysdeps/            系统依赖检查
├── web/                    React + Tailwind 前端（vite 构建，embed 进 server）
└── install.sh              一键安装/升级/卸载脚本
```

---

## 已知限制

- 限速仅 egress 方向
- 面板单实例（SQLite）
- WSS 不内置 TLS，需反代做 TLS 终结
