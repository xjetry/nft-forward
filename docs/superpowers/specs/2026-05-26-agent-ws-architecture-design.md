# Agent 反向 WebSocket 架构

## 背景

`nft-forward` 当前的 server / agent 通信是 **server 主动 push**：

- `server` 进程内的 `pusher.go` 通过 HTTP POST 拨向远程 agent 的 `daemon --listen :PORT`，下发 panel 段 ruleset。
- `poller.go` 通过 HTTP GET 周期性轮询 agent 的 counters。
- agent 节点必须在宿主机上监听一个 HTTP 端口（默认 7878），bearer token 鉴权。

这套模型有几个痛点：

1. **agent 节点宿主机必须暴露端口**。当用户想统一用 docker bridge 网络 + caddy 反代时，daemon 必须 host network（操作宿主机的 nftables / tc），导致 `--listen` 端口也必然在宿主机监听，bridge 隔离失效。
2. **NAT / 家宽 agent 无法被 push**——server 拨不进去。
3. **server 启动两个 goroutine（pusher + poller）维持节点视图**，反复 dial / poll 模式重；agent 视角下没有"在线状态"概念，只能靠 server 周期 probe 推断。

参考 Komari 探针的设计：**agent 主动 dial 出 WebSocket 长连接**到 server，所有 ruleset 下发 + counters 上报 + TUI 段同步走同一个长连接。这彻底翻转方向，让 agent 节点不再需要监听任何端口。

## 目标

- agent 节点：daemon 通过 `--connect wss://panel/v1/agents` 主动建立长连接，**不在宿主机监听任何端口**（unix socket 不算）。
- server 节点：所有 HTTP 服务（Web UI + WS endpoint）整合在单一端口（如 `:8080`），可以放进 docker bridge，宿主机暴露与否由部署者决定。
- admin 在面板上对 agent 节点拥有与 TUI 等同的全权操作能力；普通 user 仍受 tenant/tunnel 配额约束（沿用现有 `db.Tunnel` 模型）。
- 注册 agent 到 server 时，agent 本地已有的 tui 段规则**安全迁移**到 server（"ACK 后才清"），server 不可达时 tui 段永不丢。
- 模式互转 tui ↔ agent / tui ↔ server / agent ↔ server 在 install.sh 已有的框架内继续支持，对称地处理 tui 段 / panel 段。
- 本机 TUI 在 agent / server 模式下仍可用，但本地 tui 段变动通过 WS 同步到 panel，admin 可见且可一键 import。
- Docker compose 模板调整：server 进 bridge，端口映射注释化，不绑定具体反代工具。

## 非目标

- **不引入新的网络层隧道**（WireGuard / GRE / VXLAN）。"Tunnel" 一词在本项目里继续指 `db.Tunnel`（租户配额包）。
- **不为旧 agent (--listen) 提供兼容**：破坏性升级，所有 agent 节点同步升级。
- **不在 server 端提供 caddy / nginx 反代具体配置**：compose template 只给可启动骨架，反代方案由部署者自定。
- **不引入新协议技术栈**（gRPC、protobuf 等）：JSON over WebSocket，最小依赖。
- **不改 daemon 的 owner-segmented state 模型**：tui / panel 段语义沿用，state.json schema 仅扩展 `agent_meta` 块。
- **不引入自动双向同步**：tui 段 → panel 段的导入需要 admin 手动点。

## 设计

### 架构总览

```
┌─ panel 节点 ────────────────────────────────────────────────┐
│                                                              │
│  ┌──────────────┐         unix socket          ┌──────────┐  │
│  │              │ ───────────────────────────► │          │  │
│  │              │ /v1/ruleset/panel            │  daemon  │  │
│  │   server     │                              │ (本机)   │  │
│  │   (HTTP+WS)  │  ┌── self-node ──┐           │          │  │
│  │              │  │ 内置虚拟 node │           └──────────┘  │
│  │              │  │ 不走 WS       │                         │
│  └──────┬───────┘  └───────────────┘                         │
│         │                                                    │
│         │ WS hub (/v1/agents) [bearer = nodes.secret]        │
└─────────┼────────────────────────────────────────────────────┘
          │
          │ wss:// (agent 主动 dial)
          │
┌─────────▼──── remote agent 节点 ────┐
│                                      │
│  ┌─────────────────────────────┐    │
│  │  daemon  (host network)     │    │
│  │   ├─ unix socket: TUI / 本机│    │
│  │   ├─ ws dialer  : panel hub │    │
│  │   └─ state.json: tui/panel  │    │
│  └─────────────────────────────┘    │
└──────────────────────────────────────┘
```

### 组件职责

| 组件 | 职责 |
|---|---|
| `internal/daemon/dialer.go`（新） | agent 专属 WS 客户端：dial、hello、register_local、apply_ruleset 接收、counters/ping/tui_segment_changed 发送、断线指数退避重连 |
| `internal/server/hub.go`（新） | WS 服务端：`/v1/agents` upgrade、token 校验、per-conn reader/writer goroutine、`SendApplyRuleset` 入口 |
| `internal/server/selfnode.go`（新） | server 启动时确保本机 daemon 在 `nodes` 表里有特殊 `node_kind='self'` 行；下发时分叉走 unix socket |
| `internal/daemon/handlers.go`（改） | unix socket `POST /v1/ruleset/tui` 写入后 hook → 触发 dialer 推送 `tui_segment_changed`；新增 `POST /v1/admin/demote-to-tui` |
| `internal/server/pusher.go`（删） | HTTP push 路径不复存在 |
| `internal/server/poller.go`（删） | counters 由 agent 主动 push |
| `cmd/nft-forward/main.go` `runDaemon`（改） | 去掉 `--listen` / `--token-file`；新增 `--connect` / `--panel-token-file` |
| `install.sh`（改） | agent 模式写 `--connect wss://...`；`switch_role_cleanup` / `do_uninstall agent` 适配新参数 |

### 数据流（建链 + 注册 + 段迁移）

```
agent.dialer: WS dial wss://panel.example.com/v1/agents
            : send {type:"hello", node_token:"<64hex>", agent_version, os, arch, last_applied_rev}
server.hub  : 验证 token → SELECT id FROM nodes WHERE secret = ?
            : reply {type:"hello_ack", node_id:42, name:"edge-1"}
            : UPDATE nodes SET online=1, last_seen=NOW(), agent_version=...
agent       : if state.agent_meta.migrated_at == 0 AND state.owners["tui"] not empty:
              send {type:"register_local", id:"r1", payload:{forwards:[...tui_segment...]}}
server.hub  : if nodes.local_migrated_at IS NULL:
                BEGIN; INSERT forwards (...) ...; UPDATE nodes SET local_migrated_at=NOW(); COMMIT
              else:
                imported = []   // 幂等，已迁移过
              reply {type:"register_local_ack", id:"r1", payload:{imported:[...]}}
agent       : on ack: SetAgentMeta(migrated_at=NOW()), clear owners["tui"], SaveState
server.hub  : (常规通道) send {type:"apply_ruleset", id:"a1", payload:{rev:N, rules:[...panel...]}}
              // 若 hello 时 agent 上报的 last_applied_rev == 当前 rev，跳过本次下发
agent       : daemon.SetOwnerRuleset("panel", rules) → applier.Apply
            : SetAgentMeta(last_applied_rev=N), SaveState
            : reply {type:"apply_ack", id:"a1", payload:{rev:N, ok:true}}
```

### JSON 协议

**信封**：

```json
{
  "type": "string",
  "id":   "string",          // 仅 req/resp 类带；notification 不带
  "payload": { ... }
}
```

**消息族**：

| 方向 | type | payload 字段 | ack? |
|---|---|---|---|
| → | `hello` | `node_token`, `agent_version`, `os`, `arch`, `last_applied_rev` | ✓ `hello_ack` |
| ← | `hello_ack` | `node_id`, `name` 或 `error` | — |
| → | `register_local` | `forwards: [Forward...]` | ✓ `register_local_ack` |
| ← | `register_local_ack` | `imported: [{listen_port, target, rule_id}]` 或 `error` | — |
| ← | `apply_ruleset` | `rev`, `rules: [Rule...]` | ✓ `apply_ack` |
| → | `apply_ack` | `rev`, `ok`, `error?` | — |
| → | `counters` | `samples: [{listen_port, proto, bytes_delta}]` | ✗ |
| → | `tui_segment_changed` | `forwards: [Forward...]` | ✗ |
| → | `ping` | `ts` | ✓ `pong` |
| ← | `pong` | `ts` | — |
| ← | `error` | `code`, `message` | — |

**类型复用**：

- `Rule` 直接是 `internal/nft.Rule`。
- `Forward`（register_local / tui_segment_changed 用）：`{proto, listen_port, target_ip, target_port, comment, bandwidth_mbps}`。server 端入库时映射到 `forwards` 表。

**关键设计点**：

1. **apply_ruleset 是全量替换**，不是增量 diff。每次 server 推 panel 段全量；agent 收到后 `SetOwnerRuleset("panel", rules)` 走现有 owner-segmented merge。
2. **rev 反向同步**：`rev` 是 server 端给当前 panel 段的版本标识；语义是 "agent 拿这个 rev 就等于已经应用了这一版"。每次 admin 改 forwards 影响某 node 时 server 重新计算。agent 持久化 `agent_meta.last_applied_rev`；`hello` 时上报；server 比对：相同则跳过下发，不同则全量推送。具体实现可选 INTEGER 计数器或 panel-segment hash，留给实施阶段决定。
3. **register_local 只在 `migrated_at==0` 时发**。迁移完成后，即使 tui 段再次有内容也走 `tui_segment_changed`（通知性质）。
4. **counters 用 delta 模型**：agent 重启 nftables counter 归零，绝对值会让 server 误判流量降到 0。server 端只做加法；rule 索引 by `(node_id, listen_port, proto)`。
5. **req/resp 关联**：agent 端维护 `map[id]chan response`；server 端 hub 同理。outstanding 通常 ≤ 5，自己实现轻于引第三方 RPC 框架。

### state.json schema v3

```json
{
  "version": 3,
  "owners": { "tui": [...], "panel": [...] },
  "agent_meta": {
    "migrated_at":      "2026-05-26T10:00:00Z",
    "last_applied_rev": 17,
    "panel_url":        "wss://panel.example.com/v1/agents"
  }
}
```

`stateSchemaVersion = 3`。`LoadState` v2 → v3 升级填零值。`AgentMeta` 是 daemon 内部字段，不通过 unix socket API 暴露给 TUI。

### daemon 启动序列（agent 模式）

```
daemon main:
  1. Bootstrap()                              ← 现有：load state.json → MergedRuleset → Apply
  2. ListenSocket() + serve unix socket
  3. if cfg.Connect != "":
       start dialer goroutine
       start refreshLoop                      ← 现有 DNS 重解析
  4. block on signals

dialer goroutine:
  for {
    1. dial wss://...                         ← 失败 → backoff
    2. send hello                             ← hello_ack NACK (token 错) → 永久 abort，daemon 继续 apply 现有 state
    3. recv hello_ack
    4. if state.AgentMeta.MigratedAt == 0 && state.HasTuiSegment():
         send register_local
         recv register_local_ack
         if ok:
           daemon.OnLocalMigrated(imported)   ← 清 tui 段 + 写 MigratedAt + Save
    5. enter loop:
         counters ticker (30s)  → send counters
         ping ticker     (10s)  → send ping, expect pong
         tui dirty signal       → send tui_segment_changed (dedup last-write-wins)
         recv apply_ruleset     → SetOwnerRuleset("panel", rules) → send apply_ack
         recv pong              → reset read deadline (30s)
         read deadline exceed   → break, reconnect
  }
```

dialer 是 daemon 内部 goroutine，与 unix socket handler 共享 `*Daemon` 实例。`SetOwnerRuleset(owner, rules)` 是 daemon 已有的内部入口（现已在 handlers 中暴露），dialer 直接调用，不绕 HTTP。

### 段迁移竞态分析

| 故障点 | 保护 | 结果 |
|---|---|---|
| dial 永远失败 | dialer 退避不阻塞 daemon | tui 段保留，daemon 继续 apply。registration pending |
| `hello` 之后断 | hello_ack 未收到 → 重连 | 同上 |
| server INSERT 失败 | 回 `error` 字段，agent 不更新 MigratedAt | 下次重连重试 register_local |
| server INSERT 成功但 ack 丢失 | 重连时 server 看 `nodes.local_migrated_at`，已 set → 回 empty `imported` ack（幂等）| agent 仍清 tui 段，最终一致 |
| ack 收到、tui 段已清、panel 段下发未到 | nft 表里 tui 段刚 apply 的规则继续生效 | 重连后 server 推 panel 段无缝接管 |
| 迁移中 TUI 同时写 tui 段 | daemon 加锁：`MigratedAt == 0` 时 register goroutine 持锁，TUI 写阻塞到 register_local_ack | 串行化避免半迁移 |

**server 端幂等性 SQL**：

```sql
SELECT local_migrated_at FROM nodes WHERE id = ?;
-- 若已 set，直接回 ack with imported=[]
-- 若未 set，BEGIN; INSERT forwards (...); UPDATE local_migrated_at = NOW(); COMMIT
```

### 重连退避

- 初始间隔 `1s`，每次失败 ×2，封顶 `60s`，加 ±20% jitter。
- 任何成功 `hello_ack` 重置回 `1s`。
- 永久失败（hello_ack 返回 token 错）：不再重试，log 错误，daemon 继续运行。

### server 端 hub 结构

```go
package server

type Hub struct {
    db    *sql.DB
    mu    sync.RWMutex
    conns map[int64]*agentConn
}

type agentConn struct {
    nodeID  int64
    ws      *websocket.Conn
    writeCh chan []byte
    closed  chan struct{}
    pending map[string]chan json.RawMessage
    pendMu  sync.Mutex
}

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev int) error
func (h *Hub) Dispatch(nodeID int64, rules []nft.Rule, rev int) error  // self-node 分叉
```

每个 `agentConn` 起 reader + writer goroutine。reader 解 JSON 分发；writer 串行化写避免并发。`SendApplyRuleset` 生成 `id`、注册 `pending[id]`、推 `writeCh`、等 `apply_ack` 或 30s 超时。

同一 nodeID 重连：旧连接被新连接替换（"last writer wins"），旧 conn 关闭。

### self-node 注入

`server` 启动时 `EnsureSelfNode(db)`：

```sql
INSERT INTO nodes (name, address, secret, node_kind, online, last_seen)
VALUES ('self', 'unix:///var/run/nft-forward.sock', '', 'self', 1, NOW())
ON CONFLICT(name) WHERE node_kind='self' DO UPDATE SET last_seen=NOW();
```

下发路径分叉：

```go
func (h *Hub) Dispatch(nodeID int64, rules []nft.Rule, rev int) error {
    n, _ := db.GetNode(h.db, nodeID)
    if n.NodeKind == "self" {
        c, _ := daemonclient.New(daemon.DefaultSocketPath)
        return c.PostRuleset("panel", rules)
    }
    return h.SendApplyRuleset(nodeID, rules, rev)
}
```

self-node 始终 `online=1`、无 token、UI 不显示 "agent 安装命令"。卸载 server 角色时 self-node 行随 panel.db 删除（沿用 `do_uninstall server --purge`）。

### DB schema 变更

```sql
ALTER TABLE nodes ADD COLUMN local_migrated_at TIMESTAMP;
ALTER TABLE nodes ADD COLUMN last_seen         TIMESTAMP;
ALTER TABLE nodes ADD COLUMN online             INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN agent_version      TEXT;
ALTER TABLE nodes ADD COLUMN node_kind          TEXT NOT NULL DEFAULT 'remote';
-- 删除：dirty 列（pusher 不复存在）
-- 现有 address：remote node 仅诊断展示（agent 自己 dial 时 panel URL 写在 unit 里，不读 DB）
--               self node 存 'unix:///var/run/nft-forward.sock' 表意

CREATE TABLE node_tui_snapshot (
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  forwards_json TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
```

### install.sh 变更

**daemon unit 模板**：

```bash
# tui   : write_daemon_unit ""
# server: write_daemon_unit ""
# agent : write_daemon_unit " --connect wss://panel/v1/agents --panel-token-file /etc/nft-forward/panel.token"
```

**命令形态**：

```
sudo bash install.sh agent --panel-url https://panel.example.com --token <64hex>
```

`https → wss`、`http → ws` 自动转换，append `/v1/agents` 路径。token 写入 `/etc/nft-forward/panel.token` (mode 0600)。

**detect_existing_roles**：`grep '--connect'` 替代 `grep '--listen'`，其余矩阵不变。

**do_uninstall agent**（非 `--purge`）：调 daemon unix socket `POST /v1/admin/demote-to-tui`，再 `write_daemon_unit ""` + restart。

**新增 daemon API**：`POST /v1/admin/demote-to-tui`：

- 原子地把 state.json 里 panel 段合并到 tui 段（按 `listen_port` 去重，冲突时 panel 段优先），
- 重置 `agent_meta.migrated_at = 0`、`last_applied_rev = 0`、`panel_url = ""`，
- SaveState。

**update 路径**：`do_update` 不动。新二进制启动后 dialer 自动按 unit 里的 `--connect` 重连。

### TUI 同步钩子

`internal/daemon/handlers.go` 处理 `POST /v1/ruleset/<owner>`：

```go
func (d *Daemon) postRuleset(owner string, rules []nft.Rule) error {
    // ... existing apply + save ...
    if owner == "tui" && d.dialer != nil {
        d.dialer.NotifyTuiChanged(rules)
    }
    return nil
}
```

`NotifyTuiChanged` 在 dialer 端 dedup（last-write-wins）：

```go
func (dl *Dialer) NotifyTuiChanged(rules []nft.Rule) {
    select {
    case dl.tuiCh <- rules:
    default:
        dl.pendingTui.Store(&rules)
    }
}
```

pure-tui 模式或 server 模式本机 `d.dialer == nil`，整个 hook 直接 no-op。

### Panel UI 行为

节点详情页加 section：

```
本地 TUI 规则 (N 条)              [一键导入到 panel]
  - tcp 80   → 10.0.0.1:80
  - udp 53   → 10.0.0.2:53
最后上报: 2 分钟前
```

数据来自 `node_tui_snapshot.forwards_json`。"一键导入"：INSERT 到 `forwards` 表（owner=admin），随后给 agent 发 `apply_ruleset`。Admin 可二次操作"通知 agent 清 tui 段"。

**不自动 import**：避免双向同步竞态。

### Docker compose 模板

```yaml
services:
  daemon:
    # 不动：host network 是硬约束
    build:
      context: ..
      dockerfile: docker/Dockerfile
    image: nft-forward:dev
    container_name: nftf-daemon
    cap_add: ["NET_ADMIN", "NET_RAW", "SYS_MODULE"]
    network_mode: host
    volumes:
      - daemon-state:/var/lib/nft-forward
      - daemon-run:/var/run
    command: ["/usr/local/sbin/nft-forward", "daemon"]

  server:
    image: nft-forward:dev
    depends_on: [daemon]
    container_name: nftf-server
    networks: [nftf]
    # 默认不映射端口；部署者自行决定如何对外暴露：
    # 临时本机访问可取消注释 ports；
    # 生产用反代，另加 caddy/nginx service 加入 nftf 网络反代 server:8080。
    # ports:
    #   - "127.0.0.1:8080:8080"
    volumes:
      - daemon-run:/var/run
      - server-data:/var/lib/nft-forward
    command: ["/usr/local/sbin/nft-forward", "server", "--addr", ":8080"]

networks:
  nftf:
    driver: bridge

volumes:
  daemon-state:
  daemon-run:
  server-data:
```

README 加一节说明默认 compose 不暴露端口，列出三种典型对外暴露方式（直接 ports、加 caddy/nginx service、host network 直跑）。

`docker/test.sh` 同步改成通过 service name `server:8080` 访问。

## 错误处理矩阵

| 失败类型 | dialer 行为 | server 行为 |
|---|---|---|
| dial 失败 / TCP RST | 退避重连 | — |
| hello_ack `error` token 不匹配 | 永久 abort，log，daemon 仍 apply 本地 state | 关闭 WS |
| register_local 失败 | 不更新 MigratedAt，下次重连重试 | 事务 rollback |
| register_local ack 丢 | 重连后再发；server 看 local_migrated_at 已 set → empty imported ack | 幂等 |
| apply_ruleset agent apply 失败 | send `apply_ack {ok:false, error}` | log，更新 `nodes.last_error` |
| 心跳超时（30s 无 pong） | 主动关闭 WS，重连 | 同样：read deadline 触发，hub 移除 conn |
| 同 nodeID 重连 | — | 关闭旧 conn，新 conn 替换 |
| server 重启 | 所有 agent 重连风暴，jitter 退避缓冲 | self-node 行被 EnsureSelfNode 重新写入 |

核心不变量：**tui 段在 server 未 ACK 之前永不清空**；**panel 段在卸载 agent 角色（非 --purge）时永不丢，转为 tui 段保留**。

## 测试

### 单元测试

- `internal/daemon/dialer_test.go`：序列化、退避算法（fake clock）、段迁移状态机、register_local 幂等、tui 通知 dedup
- `internal/server/hub_test.go`：hello 鉴权失败、同 nodeID 重连替换、SendApplyRuleset 超时、counters delta 累加、register_local 幂等
- `internal/daemon/state_test.go`：v2 → v3 升级、demote-to-tui 合并语义（panel 覆盖 tui）
- `internal/server/selfnode_test.go`：EnsureSelfNode 幂等、Dispatch 在 self-node 时走 unix socket

### 集成测试

`docker/test.sh` 追加：

```bash
note "X. WebSocket agent dialer 注册"
# server 容器（bridge）+ agent 容器（host net）；agent unit 用 --connect ws://server:8080/v1/agents
# 验证 hello_ack、online=1、双向 apply、断开重连

note "Y. tui→agent 段迁移 + ACK"
# pure-tui 容器先 POST tui 段，restart 加 --connect
# 验证 forwards 表入库、state.json tui 段清空 + migrated_at 非零、ack 丢失场景幂等

note "Z. agent→tui 降级"
# install.sh uninstall agent（不 --purge）
# 验证 panel 段合并到 tui 段、daemon unit 无 --connect、nft 表内容不变
```

### 手动验证

- pure-tui 节点装新版无可观察变化
- server 启动后 nodes 表里有 self 行；在 self 行编辑 forward 立即生效
- 远程 NAT 后 agent 能反向 dial 成功
- `systemctl restart nft-forward-server.service` → agent 60s 内重连
- server 不可达时 daemon log 每分钟不超过 1 行（退避后期）

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| WS 长连接经 caddy/nginx 反代时 idle timeout 导致频繁断连 | ping 10s + read deadline 30s 保活；部署文档建议反代层 idle ≥ 60s |
| 同一 nodeID 短时间多次重连刷爆 hub | 重连退避 + jitter；hub 端 last writer wins 替换旧连接 |
| register_local 上报数据被中间人截获 | WSS（TLS）部署；token 走 bearer，不在 URL query |
| schema v2 → v3 升级失败 | LoadState v2 路径明确填零值；带迁移单元测试覆盖 |
| TUI 高频写引发 tui_segment_changed 风暴 | dialer 端 dedup（last-write-wins）+ 30s 上报间隔上限 |
| 旧 agent 节点未升级 → 新 server 不识别 | 破坏性升级公告；install.sh update 路径覆盖所有节点 |
| 隧道（db.Tunnel）配额绕过 | admin 等同 TUI 是有意为之；普通 user 路径仍走 forwards.tenant_id + tenant_tunnels grants 校验 |

## 实施顺序提示

实施计划由后续 writing-plans 流程产出。粗略顺序参考：

1. state.json schema v3 + AgentMeta
2. daemon 内部 `SetOwnerRuleset` 重构（dialer 复用）+ `POST /v1/admin/demote-to-tui` 路由
3. DB schema 变更 + EnsureSelfNode + Dispatch 分叉
4. WS message types + hub.go + dialer.go
5. cmd/nft-forward main 参数变更
6. install.sh 重做（detect / switch / uninstall / 命令形态）
7. panel UI 节点详情页 tui 段 section + 一键 import
8. 删除 pusher.go / poller.go 及其引用
9. docker-compose.yml + docker/test.sh 调整
10. README 更新（部署、反代说明、升级注意）
