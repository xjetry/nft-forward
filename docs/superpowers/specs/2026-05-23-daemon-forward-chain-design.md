# Daemon 自管 FORWARD chain 兼容性

## 背景

`nft-forward` daemon 当前只维护自己的 `ip nft_forward` 表（`prerouting` nat dnat + `postrouting` nat masquerade）。在干净 Debian/Ubuntu 主机上 `filter` 表的 FORWARD chain 默认 policy=ACCEPT，DNAT 后的包能正常 forward。

但只要主机上装了**任何修改 FORWARD chain default policy 为 drop 的工具**——Docker / ufw / firewalld 这一类——nft-forward 的 DNAT 流量就会在 FORWARD hook 处被静默丢弃，表现为"client SYN 重传，packet 进了 sg-cn2 但 DNAT 后没出去"。这在生产 Docker 主机上是常态：

```
Chain FORWARD (policy DROP)
 pkts bytes target            source           destination
  451 27044 DOCKER-USER       0.0.0.0/0        0.0.0.0/0
  451 27044 DOCKER-FORWARD    0.0.0.0/0        0.0.0.0/0
```

非 docker 流量跳完 DOCKER-USER（空 chain，return）+ DOCKER-FORWARD（不匹配任何容器规则，return）后，回到 FORWARD chain 没匹配任何 rule → policy DROP 接管 → 包丢。

用户的实际症状：装 nft-forward + 配 DNAT → 转发不通。手动 `iptables -I DOCKER-USER ... -j ACCEPT` 后立刻通。

## 目标

让 daemon 自带 FORWARD 兼容层，**在用户不感知的情况下**完成跨越 FORWARD policy=drop 障碍的工作。目标场景：

- 干净系统（FORWARD policy=ACCEPT）：daemon 不做任何额外动作（不引入冗余规则）。
- Docker 主机（FORWARD policy=DROP + DOCKER-USER chain 存在）：daemon 自动往 DOCKER-USER 注入对应放行规则。
- ufw 主机（FORWARD policy=DROP + ufw-user-forward chain 存在）：daemon 自动往 ufw-user-forward 注入。
- 未来扩展（firewalld 等）：通过同一接口扩展。

## 非目标

- **不主动改 FORWARD chain default policy**。daemon 不会把 policy 从 drop 改成 accept——这会破坏 Docker / ufw / firewalld 的安全 invariant，且很难在 daemon 卸载时还原（中间被其他工具改过就丢上下文）。
- **不监听 docker / firewall daemon 启停事件**。这些工具重启可能短暂影响 user-extension chain，但 chain 内容一般不会被它们清空。等下一次 daemon Apply 自动 re-sync 即可。
- **不引入新的运行时依赖**。沿用现有 `nft` 命令行工具，不引入 libmnl / libnftnl 等。
- **不发布 firewalld shim**。本次仅 docker-user + ufw 两个；留接口便于未来加。
- **不改 daemon 的 ownership 模型**。`ip nft_forward` 表仍由 daemon 完全拥有；shim 只在跨表 chain 里**插入并跟踪带 owner comment 的 rule**，不创建、不删除、不修改这些 chain 本身。

## 设计

### 架构

daemon 现有数据流：

```
HTTP POST /v1/ruleset/<owner>
    ↓ daemon merge owner-segmented state
    ↓
applier.Apply(allRules, iface)
    ├─ nft.Apply(allRules)        // 重建 nft_forward 表
    └─ tc.Apply(allRules, iface)  // 重建 tc HTB
```

修改后：

```
applier.Apply(allRules, iface)
    ├─ nft.Apply(allRules)               // 既有：重建 nft_forward 表
    ├─ shim.SyncAll(allRules)            // 新增：同步所有激活 shim
    └─ tc.Apply(allRules, iface)         // 既有
```

`shim.SyncAll` 遍历内置 shim registry，对每个 shim：

1. 跑 `Detect()`：检测兼容目标是否存在
2. 不存在 → 跳过（不报错）
3. 存在 → 跑 `Sync(rules)`：先按 owner comment 删除该 shim 之前注入的 rule，再插入与当前 rules 匹配的新 rule

### 文件结构

```
internal/nft/shim/
├── shim.go              ForwardShim interface + Registry + SyncAll/CleanupAll 入口
├── docker_user.go       DockerUserShim — DOCKER-USER chain (table ip filter)
├── ufw.go               UfwShim — ufw-user-forward chain (table ip filter)
├── render.go            纯函数：渲染 nft 脚本片段 + 解析 nft -a list 输出找 handle
├── render_test.go       纯函数单元测试（不需 root）
├── docker_user_test.go  DockerUserShim 单元测试
├── ufw_test.go          UfwShim 单元测试
└── registry_test.go     Registry 调度逻辑测试

internal/daemon/
└── applier.go           Apply() 在 nft.Apply 之后调用 shim.SyncAll

internal/nft/
└── nft.go               不动；shim 包是独立单元
```

### 接口定义

```go
// ForwardShim 描述一个 firewall 工具的兼容接入。每个 shim 负责一个具体的
// user-extension chain (e.g. DOCKER-USER, ufw-user-forward)。
type ForwardShim interface {
    // Name returns the shim's identifier, used in logs.
    Name() string

    // Detect returns true when this shim's target chain currently exists
    // on the system. Detection is cheap (single `nft list chain` call)
    // and called on every Sync.
    Detect() bool

    // Sync makes the target chain match `rules`: removes any
    // owner-comment-tagged rule that doesn't belong, inserts missing
    // ones. Idempotent. Returns nil when chain doesn't exist.
    Sync(rules []nft.Rule) error

    // Cleanup removes every owner-comment-tagged rule from the target
    // chain. Idempotent. Returns nil when chain doesn't exist.
    Cleanup() error
}

// Registry holds all built-in shims.
type Registry struct {
    shims []ForwardShim
}

func DefaultRegistry() *Registry  // built-in: docker-user, ufw

func (r *Registry) SyncAll(rules []nft.Rule) error
func (r *Registry) CleanupAll() error
```

`SyncAll` / `CleanupAll` 行为：遍历所有 shim 调对应方法，任一失败**不**短路（继续遍历其他 shim），最后聚合错误返回。Daemon 上层把聚合错误 log 出来但**不**当作 fatal——core nft_forward 表已经 apply 成功。

### Rule 内容

`Sync(rules)` 渲染到目标 chain 里的 nft 规则集：

每条 DNAT 规则 → 一条 forward accept：

```
ip daddr <dest_ip> <proto-dport-match> counter accept comment "nft-forward managed"
```

`<proto-dport-match>` 跟 `internal/nft/nft.go::protoPostMatch` 一致（tcp / udp / tcp+udp 都支持）。

通用回包放行（只插入一次，不随 rule 增加而重复）：

```
ct state established,related counter accept comment "nft-forward managed"
```

为什么也要 `ct state established,related`：DNAT 改写的是正向包，回包不被 conntrack DNAT-revert 重写 dst（dst 是 client 真实 IP）。回包 forward 时不会匹配上面 per-DNAT 的 `ip daddr` 规则，所以需要这条通用 ct state 兜底。

### Owner 标识

所有 shim 注入的 rule 共用 comment 字串 `nft-forward managed`。cleanup 时按这个字串 grep handle：

```bash
nft -a list chain ip filter DOCKER-USER \
  | awk '/comment "nft-forward managed"/ { ... extract handle ... }'
```

不在 comment 里嵌 rule_id：每次 Sync 都"删旧加新"，不需要 per-rule update。

### Sync 流程（伪代码）

```go
func (s *DockerUserShim) Sync(rules []nft.Rule) error {
    if !s.Detect() {
        return nil
    }
    handles, err := parseDockerUserHandles(runNft("-a", "list", "chain", "ip", "filter", "DOCKER-USER"))
    if err != nil {
        return err
    }
    script := renderShimScript("ip", "filter", "DOCKER-USER", rules, handles)
    return runNftScript(script)
}
```

`renderShimScript` 生成单个 `nft -f -` 脚本：

```
delete rule ip filter DOCKER-USER handle 12
delete rule ip filter DOCKER-USER handle 17
delete rule ip filter DOCKER-USER handle 23
add rule ip filter DOCKER-USER ct state established,related counter accept comment "nft-forward managed"
add rule ip filter DOCKER-USER ip daddr 10.20.1.20 tcp dport 8443 counter accept comment "nft-forward managed"
add rule ip filter DOCKER-USER ip daddr 10.20.1.21 udp dport 53 counter accept comment "nft-forward managed"
```

单个 transaction 保证 all-or-nothing。删除旧 handle + 添加新 rule 原子完成，中间不存在 "deleted but not yet re-added" 的窗口。

### 错误处理

| 失败类型 | shim 内行为 | applier 上层行为 |
|---|---|---|
| `Detect()` 探测失败（nft 命令报错） | 返回 false，被当作"chain 不存在"跳过 | 不影响 Apply |
| `Sync()` 写 nft 脚本失败 | 返回 error 给上层 | log warning，**不**让 Apply 整体失败 |
| `Cleanup()` 删除失败（daemon Stop 时） | 返回 error | log warning，daemon 仍正常退出 |

核心不变量：**shim 失败不阻塞核心 DNAT 路径**。Daemon 的本职是维护 `nft_forward` 表（DNAT + MASQUERADE 已经 apply 成功），shim 是 best-effort 补丁层。

### Cleanup 时机

```
daemon lifecycle:
├─ Start
│  └─ 启动重放 state.json → applier.Apply → shim.SyncAll
├─ Apply (HTTP /v1/ruleset/<owner> 触发)
│  └─ applier.Apply → shim.SyncAll
└─ Stop (SIGTERM / SIGINT)
   └─ shim.CleanupAll
```

`install.sh uninstall daemon --purge` 通过 `systemctl disable --now nft-forward-daemon.service` 触发 daemon Stop → CleanupAll 自动执行。

**残留兜底**：daemon 崩溃没走 Stop 路径时，残留留在 DOCKER-USER / ufw-user-forward 里。下次 daemon Start 时 Apply 会再跑 SyncAll，**"删旧加新"语义同样清掉过期残留**。永远不会无限累积。

### 干净系统行为

无 Docker、无 ufw 的纯净 Debian/Ubuntu 主机：

- `DockerUserShim.Detect()` → false（DOCKER-USER 不存在）
- `UfwShim.Detect()` → false
- shim.SyncAll 遍历空——什么都不做
- 主机原有 FORWARD policy=ACCEPT，转发本来就通

唯一可观察的行为差异：daemon 启动日志多一行 `shim docker-user: target chain not found, skipping`（每个 shim 一行；只在 Start 时记，避免每次 Apply spam）。

### 未知 firewall 检测

daemon 启动时如果检测到 **FORWARD chain default policy = drop** 但所有 shim 都 `Detect() == false`，记一行 WARN：

```
WARN: FORWARD chain has drop policy but no known firewall shim detected.
      forwarded traffic may be blocked. supported shims: docker-user, ufw.
      manual remediation may be required (see docs/firewall-integration.md).
```

不主动改 policy；交给用户。

## 测试

### 单元测试（不需 root）

`internal/nft/shim/render_test.go`：

- `parseHandles`: 从 `nft -a list chain` 输出 fixture 中提取 comment 含 `nft-forward managed` 的 rule 的 handle
- `renderShimScript`: 输入 (table, chain, rules, staleHandles)，golden file 比对 nft 脚本

`internal/nft/shim/docker_user_test.go` / `ufw_test.go`：

- 每个 shim 测 Detect / Sync / Cleanup 的脚本生成（mock 掉 `runNft`），不实际跑 nft

`internal/nft/shim/registry_test.go`：

- Registry.SyncAll: 注册 2 个 shim，一个 detect 成功一个失败 → 只对成功的调 Sync
- 一个 Sync 失败 + 一个成功 → 上层收到聚合错误但成功的 shim 已经写入
- CleanupAll 同样验证遍历不短路

### 集成测试（root，docker fixture）

`docker/test.sh` 追加新 step：

```bash
note "N. shim 注入 / 同步 / 清理"
docker compose exec daemon bash -c '
  set -e
  # 容器里手工建 DOCKER-USER chain 模拟 Docker
  nft add table ip filter
  nft add chain ip filter DOCKER-USER
  # 提交一条 DNAT，触发 daemon SyncAll
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[{\"id\":\"a\",\"proto\":\"tcp\",\"src_port\":58443,\"dest_ip\":\"10.20.1.20\",\"dest_port\":8443}]}"
  # 验证 DOCKER-USER 里出现 nft-forward managed rule
  nft list chain ip filter DOCKER-USER | grep -q "nft-forward managed" \
    || { echo "shim 未注入"; exit 1; }
  # 提交空 ruleset，触发删除
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[]}"
  # 验证 DOCKER-USER 里 nft-forward managed 全部消失（ct state 兜底也走）
  ! nft list chain ip filter DOCKER-USER | grep -q "nft-forward managed" \
    || { echo "shim 未清理"; exit 1; }
'
green "  shim 注入与清理验证通过"
```

ufw shim 类似（手工建 `ufw-user-forward` chain 验证）。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| Docker 重启时清空 DOCKER-USER 内容 | 实测 Docker 重启不动 DOCKER-USER 自定义内容；万一被清，下次 daemon Apply（rule 变化）自动 re-sync。用户也可手动 `systemctl restart nft-forward-daemon.service` 强制 re-sync |
| daemon Stop 没走完 cleanup（崩溃 / kill -9） | 下次 Start 时 SyncAll 的"删旧加新"语义自动收口，残留不累积 |
| shim 与未来 daemon 版本不一致（升级时 comment 字串若改） | comment 字串 `nft-forward managed` 锁定为 stable string，未来版本不改；如必须改，提供 migration（先按旧 comment 清，再按新 comment 加） |
| nft 命令在某些发行版可能不支持 `-a` 标志 | `nft -a` 是 nftables 0.9+ 通用功能。Debian 12 / Ubuntu 22.04+ 默认满足。daemon 已有依赖检查；shim 沿用同一依赖 |
| user-extension chain 名在某些发行版叫法不同（ufw 在 RHEL 系不存在） | 每个 shim Detect 失败就 skip。RHEL/CentOS 系（firewalld 主导）由未来 firewalld shim cover |
| shim 注入的 rule 数量随 DNAT 规则数线性增长 | nft 表能撑数千条 rule。100 条 DNAT 时 DOCKER-USER 多 100 条 rule，远低于性能拐点 |
