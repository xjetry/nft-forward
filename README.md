# nft-forward

基于 nftables 的轻量多节点端口转发平台。**单一二进制，零外部依赖**——同一份产物在同一台主机上可以同时充当本机 TUI、多租户 Web 面板和远程受控节点，三种角色共用同一个 host daemon，规则互不覆盖。

---

## 设计思路

市面上有 realm、gost、哆啦A梦面板等方案，但各有不足：

| 维度 | realm | gost | 哆啦A梦面板 | **nft-forward** |
|------|-------|------|-----------|-----------------|
| **数据面** | 用户态 relay（单进程） | 用户态 relay + 丰富协议栈 | 调用 iptables/realm 做后端 | nftables 内核态 DNAT **或** 用户态 split-TCP，逐跳可选 |
| **多节点管理** | 无，每台机器独立配置 | 无，配置文件驱动 | 有面板，但面板与节点是独立进程 | 单二进制内置面板 + agent 反向连接，节点零端口暴露 |
| **多跳链路** | 需手动逐节点配置 | 转发链配置复杂 | 支持，但需手动对齐端口 | 面板自动编排：选跳 + 填出口，端口全自动分配对齐 |
| **多租户** | 无 | 无 | 有，但权限粗粒度 | 通道级授权（端口段 / CIDR / 带宽 / 配额），租户自助建链路 |
| **预连接优化** | 无 | 无 | 无 | userspace 模式内置 TCP 预连接池，多跳链路省掉每一跳的握手 RTT |
| **TUI** | 无 | 无 | 无 | 内置终端交互 TUI，单机场景免开浏览器 |

### 核心差异

**1. 双数据面——内核态与用户态按需混合**

realm/gost 全走用户态 relay，吞吐受限于用户态拷贝开销。nft-forward 的默认模式是 **nftables DNAT**（内核态零拷贝），吞吐量接近线速；在需要拆分 TCP 连接的多跳场景，可逐跳切换为 **userspace split-TCP** 模式——同一条链路的不同跳可以混用两种模式。

**2. TCP 预连接池（userspace 模式）**

借鉴 [TCP-preconnection-relay](https://github.com/Xeloan/TCP-preconnection-relay) 的思路：每个 userspace 转发端口预先与下一跳维持一个连接池（默认 4 条），客户端连入时直接取用已建好的连接，跳过 TCP 三次握手。在 3 跳跨洋链路（每跳 RTT 100ms+）中，可省去 300ms+ 的首字节延迟。池空时自动 fallback 为按需 dial，延迟不会比无池更差。通过环境变量 `NFT_FORWARD_POOL_SIZE` 控制池大小，设为 0 关闭。

**3. 单二进制全角色 + agent 反向连接**

哆啦A梦面板需要在每个节点上部署独立的 agent 进程并开放端口。nft-forward 的 agent 是 daemon 的一种运行模式（`--connect wss://...`），主动反向连入面板，节点无需暴露任何端口，NAT 后的机器也能纳管。

**4. 面板自动编排多跳链路**

选有序的跳 + 填出口 `host:port`，系统自动分配并对齐各跳的监听端口，生成可复制的入口地址。修改跳序时端口保持稳定，入口不变。组合通道功能允许管理员把常用多跳序列打包（如 `po0 → rfc-jp-t1` 打包为 `jp-direct`），租户直接选组合即可。

---

## 一键安装

```bash
# 交互式
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh -o /tmp/nft-forward-install.sh && sudo bash /tmp/nft-forward-install.sh

# 直接指定角色
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s -- tui
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s -- server
curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | sudo bash -s -- agent --panel-url https://panel.example.com --token <64位hex>
```

> 仅支持 amd64 Linux；需 root。升级用 `sudo nft-forward-upgrade` 或 `sudo bash install.sh update`。

---

## 架构

```
┌──────────────────┐         ┌────────────────────┐
│  浏览器 (admin/  │  HTTPS  │  反向代理 (可选)    │
│  tenant)         │ ──────► │  Caddy / Nginx      │
└──────────────────┘         └─────────┬──────────┘
                                       │ HTTP :8080
                              ┌────────▼─────────┐
                              │  nft-forward      │
                              │  server           │
                              │  ┌─────────────┐  │
                              │  │ SQLite WAL   │  │
                              │  │ Agent Hub    │  │
                              │  │ Dispatcher   │  │
                              │  └─────────────┘  │
                              └────────┬─────────┘
                                       │ Unix socket / WSS
                   ┌───────────────────┼───────────────────┐
              ┌────▼────┐        ┌─────▼─────┐       ┌─────▼─────┐
              │ daemon  │        │ daemon    │       │ daemon    │
              │ (本机)   │        │ (节点 A)  │       │ (节点 B)  │
              │         │        │ --connect │       │ --connect │
              │ nftables│        │ wss://…   │       │ wss://…   │
              │ + tc    │        │ nftables  │       │ nftables  │
              │ + pool  │        │ + pool    │       │ + pool    │
              └─────────┘        └───────────┘       └───────────┘
```

**设计约束：**

- **daemon 是唯一的内核接口**——TUI 和 server 都不直接调用 `nft`，全部通过 daemon 的 Unix socket API 提交规则
- **owner-segmented ruleset**——`tui` 段和 `panel` 段互不干扰，跨段端口冲突时拒绝后提交方
- **原子 apply**——每次规则变更是 nftables 的 add → delete → recreate 三步原子操作
- **专用表 `ip nft_forward`**——不影响主机已有防火墙
- **Docker/ufw 自动兼容**——自动同步 `DOCKER-USER` / `ufw-user-forward` 放行规则

---

## 数据面

### 内核态（默认）

nftables DNAT，零拷贝，吞吐接近线速。适合单跳或对延迟不敏感的场景。

### 用户态（split-TCP）

TCP 用户态 relay，每一跳独立建立 TCP 连接。避免 TCP-over-TCP 的拥塞折叠问题，多跳链路推荐。

内置 **TCP 预连接池**：每个转发端口预先向目标建立连接并放入池中待用。新请求进来直接取用，跳过握手延迟。

| 配置 | 说明 |
|------|------|
| `NFT_FORWARD_POOL_SIZE=4` | 每端口预连接数（默认 4） |
| `NFT_FORWARD_POOL_SIZE=0` | 关闭预连接，退化为按需 dial |

池的行为：
- 后台 goroutine 持续补充到设定数量
- `Get()` 取出时做零字节探测，死连接自动丢弃
- 池空时 fallback 为同步 dial，不阻塞等待
- 目标地址热更新时旧池关闭、新池自动建立

---

## 多租户

| 概念 | 说明 |
|------|------|
| **租户** | 一组配额约束（最大转发数、流量配额、过期时间） |
| **通道** | 节点上的一个端口段（如 `rfc-jp-t1: 10001-20000/tcp+udp`），管理员创建 |
| **授权** | 管理员将通道授权给租户，附带 per-通道上限 |
| **组合通道** | 管理员预定义的多跳序列（如 `po0 → rfc-jp-t1` → 命名 `jp-direct`） |

租户自助操作：
- **转发**：选通道 → 填目标 → 端口自动分配（或手指定），受通道的端口段/CIDR/带宽约束
- **链路**：选通道或组合通道 → 填出口 → 系统自动展开跳、分配端口、生成入口地址

---

## 中继链路

面板自动编排多跳中继：选有序的跳 + 填出口 `host:port`，系统自动分配各跳端口。

- 每跳可独立选内核态或用户态
- 端口在编辑时保持稳定，入口不变
- 组合通道选择后自动展开为多跳
- 出口支持域名（daemon 侧 DNS 解析 + 定期刷新）
- 拓扑变化自愈：节点地址变更时自动重算受影响链路

---

## 命令

```
nft-forward                    TUI 交互（需 daemon 运行中）
nft-forward daemon             前台启动 daemon
nft-forward daemon --connect wss://panel/v1/agents \
    --panel-token-file /etc/nft-forward/panel.token
                               agent 模式：反向连入面板
nft-forward server [--addr :8080] [--db PATH]
                               Web 面板
```

首次启动 server 会创建 admin 账号并打印随机密码。忘记密码可用 `--reset-admin-password` 重置。

---

## 文件路径

| 路径 | 用途 |
|------|------|
| `/var/run/nft-forward.sock` | daemon Unix socket |
| `/var/lib/nft-forward/state.json` | daemon 规则持久化（owner-segmented） |
| `/var/lib/nft-forward/panel.db` | 面板 SQLite 数据库 |
| `/etc/nft-forward/panel.token` | agent token |

## 环境变量

| 变量 | 说明 | 默认 |
|------|------|------|
| `NFT_FORWARD_DNS_INTERVAL` | DDNS 重解析周期 | `60s` |
| `NFT_FORWARD_POOL_SIZE` | userspace 预连接池大小 | `4` |

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
go test ./...
go vet ./...

# 交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o build/nft-forward ./cmd/nft-forward
```

依赖：Go ≥ 1.24，无 CGO。运行时需 nftables + iproute2（仅 Linux）。

```
nft-forward/
├── cmd/nft-forward/        入口（tui / daemon / server）
├── internal/
│   ├── daemon/             owner-segmented ruleset、状态持久化
│   ├── forward/            数据面（nftables kernel + userspace relay + connpool）
│   ├── nft/                nftables 渲染 / apply / counter
│   ├── tc/                 tc HTB 限速
│   ├── tui/                bubbletea TUI
│   ├── db/                 SQLite schema + 查询
│   ├── wsproto/            agent ↔ panel WebSocket 帧
│   ├── resolver/           DNS 解析
│   └── server/             Web 面板（路由、认证、hub、dispatcher、模板）
└── install.sh              一键安装脚本
```

---

## 已知限制

- 仅 IPv4
- 限速仅 egress 方向
- 面板单实例（SQLite）
- WSS 入口不内置 TLS，需反代做 TLS 终结
