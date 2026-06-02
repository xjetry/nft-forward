# 每转发可选「内核态零拷贝 / 用户态 TCP 中继」设计

状态:草案(待评审) · 日期:2026-06-02

## 1. 背景与目标

nft-forward 当前只有一种数据平面:nftables prerouting DNAT + postrouting
masquerade(L3/L4,内核零拷贝,转发对端到端透明)。一条客户端 TCP 连接在
经过多级转发时,始终是「同一条端到端 TCP 流」——每一跳只改 IP 头,拥塞控制
(cwnd、丢包恢复)端到端共享。在多跳链路里,任一跳的抖动/丢包都会拖垮整条
流的 cwnd,客户端(尤其 macOS/Windows)在 ≥2 跳时容易雪崩。

本特性为**每条转发**新增一个可选项:转发模式(mode)。

- `kernel`(默认,现有行为):nftables DNAT/masquerade,零拷贝,透明。
- `userspace`(新增,仅 TCP):daemon 内嵌的 TCP 分段中继(split-TCP /
  Performance-Enhancing-Proxy)。在本跳**终结并重新发起** TCP:客户端只看到
  一条干净的本地连接,本跳与下一跳各自拥有独立的 cwnd / 丢包恢复 / 缓冲。

实现必须**内嵌在 Go daemon 内**,保持项目「单二进制、仅依赖 nftables +
iproute2、无额外进程/守护」的身份——不引入 gost 等外部转发器。

设计取向(用户明确要求)——项目核心价值:**轻量转发 + TUI + Web 面板
(agent/server)**。落到本特性:

- **转发层(功能层)轻量简单、不过度设计**:用户态 relay 取 realm/gost 之精华
  ——薄的连接级双向拷贝 + 半关传播;不引入它们的协议插件/链式/多路复用等重
  功能,不做镀金(无带超时的优雅排空、无超出必要安全网的健壮性机制)。
- **架构层完整、抽象干净**:引入一等公民的数据平面抽象(`forward.Dataplane` +
  kernel/userspace 后端 + firewall),而非把中继当作 applier 的旁挂。每块都很小,
  但边界清晰、端到端接线齐全。
- **交互层(TUI / Web 面板)做到足够好用**:mode 在 TUI 与面板都顺手可选、
  状态/计数清楚可见。

即:**完整用于结构 + 交互可用性;轻薄用于转发功能本身**。

## 2. 非目标

- **UDP 用户态中继**:UDP 无 cwnd / 无重传,split-TCP 的收益不存在;应用层
  拥塞控制(QUIC)端到端加密,透明中继无法介入。用户态对 UDP 是纯成本无收益。
  → UDP **永远走内核**。`tcp+udp` 的用户态转发拆成 `udp→内核` + `tcp→用户态`。
- **splice/零拷贝的用户态中继**:用户态模式刻意放弃零拷贝(见 §14 取舍),
  其价值在 cwnd 隔离而非吞吐峰值。
- **租户级 mode 门控**:不在 tunnel 上加「禁止用户态」开关(无此需求,YAGNI);
  mode 是每条 forward 的属性。留作未来扩展点。
- **IPv6**:项目当前 IPv4-only(`nft.Validate` 强制 IPv4),用户态中继同样
  `tcp4` only,与内核侧一致。
- **跨后端「同端口模式翻转」的零间隙切换**:见 §6,接受亚秒级间隙。
- **优雅排空 / 完整跨后端事务**:转发层不镀金。关闭直接关连接(§7.6);
  失败恢复只保留一个 `lastUserspace` 回滚安全网(§6),不建完整事务系统。
- **realm/gost 的重特性**:协议插件、链式代理、多路复用、认证等一律不做;
  只取「薄连接级双向中继」这一精华。

## 3. 概念与数据流

```
内核态 (kernel):
  client ──SYN:8443──▶ [prerouting DNAT → DestIP:DestPort] ──▶ next hop
                       (一条端到端 TCP;本跳只改 IP 头;零拷贝)

用户态 (userspace, TCP only):
  client ──SYN:8443──▶ [daemon net.Listen :8443]  ══TCP A══▶ accept
                                                    ↕ 缓冲拷贝(可选令牌桶限速)
              next hop ◀══TCP B══ [daemon net.Dial DestIP:DestPort]
  (两条独立 TCP;各自 cwnd / 丢包恢复;本跳为 client 提供干净单跳连接)
```

内核态下包永不进入本地 socket(prerouting 在路由前改写目的地)。用户态下,
该 (tcp, SrcPort) **没有** DNAT 规则,SYN 命中本机 INPUT → 交给 daemon 的
监听 socket;daemon 另开一条 TCP 到 DestIP:DestPort,双向中继。用户态无需
masquerade(出站连接以本机为源,回程自然回到 daemon socket)。

## 4. 数据模型

### 4.1 `nft.Rule.Mode`(`internal/nft/nft.go`)

`Rule` 增加字段:

```go
type Rule struct {
    // ... 现有字段 ...
    Mode string `json:"mode,omitempty"` // "" | "kernel" | "userspace";空==kernel
}
```

新增规范化助手与常量(单一默认来源):

```go
const (
    ModeKernel    = "kernel"
    ModeUserspace = "userspace"
)
// EffectiveMode 把空串归一为 kernel。所有判定 mode 的地方都经它,
// 这样旧数据(无 mode 字段)与旧 panel 推送(mode="")都默认内核态。
func (r Rule) EffectiveMode() string {
    if r.Mode == ModeUserspace { return ModeUserspace }
    return ModeKernel
}
```

### 4.2 校验矩阵(`nft.Validate` 扩展)

| proto | mode | 结果 |
|---|---|---|
| tcp / udp / tcp+udp | kernel(或空) | OK(现有行为) |
| tcp | userspace | OK(用户态 TCP 中继) |
| tcp+udp | userspace | OK(§5 Partition 拆成 udp/内核 + tcp/用户态) |
| udp | userspace | **拒绝**:`UDP 不支持用户态转发` |

`Validate` 增加:`mode` 必须 ∈ {空, kernel, userspace};`udp + userspace` 拒绝。

### 4.3 panel 侧(`wsproto` / `db`)

- `wsproto.Forward` 增加 `Mode string json:"mode,omitempty"`(register_local /
  tui_segment_changed 用)。`wsproto.ApplyRuleset.Rules` 本就是 `[]nft.Rule`,
  加了 `Rule.Mode` 后**自动过线**,panel→agent 下发无需改协议结构。
- `db.Forward` 增加 `Mode string`;`forwardCols` / `scanForward` /
  `CreateForward` 同步;新增迁移 `0005_forward_mode.sql`:
  ```sql
  ALTER TABLE forwards ADD COLUMN mode TEXT NOT NULL DEFAULT 'kernel'
      CHECK(mode IN ('kernel','userspace'));
  ```
  复合不变式「`mode='userspace'` ⇒ `proto='tcp'`」**不**落到 DB 层:SQLite 不
  支持 `ALTER TABLE ADD CONSTRAINT`,而为单条复合 CHECK 重建整张 forwards 表
  不划算。该不变式由 HTTP handler(handlers_my / handlers_admin)+
  `nft.Validate` 双重保证(panel 侧 proto 本就只有 tcp/udp,故等价于
  「udp+userspace 拒绝」);DB 仅保留 `mode IN (...)` 列级 CHECK。
- `server.buildRules`(`server.go:171`)在构造 `nft.Rule` 时 `Mode: f.Mode`。

### 4.4 状态迁移与归一(`internal/daemon/state.go` / `migrate`)

- `stateSchemaVersion` 3 → 4。
- 新增 v3 读取分支:载入旧 owners,对每条规则把空 `Mode` 显式补成
  `"kernel"`(让磁盘数据显式化),`SaveState` 写 v4。
- **双保险**:磁盘迁移让旧文件显式化;但旧 panel 经 WS 推送的规则不过状态加载,
  故 daemon 摄入侧(`Dataplane.Reconcile` / `Partition`)也以 `EffectiveMode()`
  归一。归一逻辑是默认值的**唯一权威来源**,迁移只是提前固化到磁盘。

## 5. 数据平面抽象:`internal/forward` 包

这是本特性的核心抽象。把「把一份已解析规则集落到数据平面」从 daemon 里
单一的 `Applier`(nft+tc+shim 捆绑)升级为可组合的多后端编排。

```
                 ┌─────────────────────────────────────────────┐
   daemon ──────▶│ forward.Dataplane (编排器)                    │
  (Reconcile/    │   Reconcile(ctx, rules) / Counters() / Close  │
   Counters/     │                                               │
   Close)        │   1. forward.Partition(rules)                 │
                 │        → kernelRules, userspaceRules, err     │
                 │   2. userspace.Reconcile  (开新→换标→关旧)     │
                 │   3. kernel.Reconcile     (nft 原子 + tc)      │
                 │   4. firewall.Sync        (FORWARD+INPUT,尽力)│
                 └───────┬───────────────┬───────────────┬───────┘
                         │               │               │
              forward/kernel     forward/userspace   forward.Firewall
              nft DNAT/masq      net.Listen+中继      (shim.Registry)
              + tc HTB           + 令牌桶 + 计数      FORWARD/INPUT accept
```

### 5.1 消费侧接口(定义在 `daemon` 包,符合 Go 习惯)

```go
// daemon 定义它需要的接口,*forward.Dataplane 实现之;
// 测试注入 fake。daemon→forward 单向依赖。
type Dataplane interface {
    Reconcile(ctx context.Context, rules []nft.Rule) error
    Counters() ([]forward.Counter, error)
    Close(ctx context.Context) error
}
```

替换:`Daemon.applier Applier` → `Daemon.dp Dataplane`;
`Config.Applier` → `Config.Dataplane`;`DefaultApplier()` →
`forward.New(forward.Config{Iface, Shims, ...})`。
`applySerialized(resolved)` → `dp.Reconcile(ctx, resolved)`;
`cleanupSerialized()` → `dp.Close(ctx)`;`countersFn` → `dp.Counters`。
现有 `internal/daemon/applier.go` 的 `Applier`/`nftApplier` 被 `forward` 包吸收。

> ctx 在所有调用点都现成:`setOwnerRuleset`/`demoteToTui`/`Bootstrap` 有 ctx,
> `Run` 关停有 `shutCtx`。`applierMu` 串行化语义保持不变(改为串行化
> `dp.Reconcile`/`dp.Close`)。

### 5.2 `forward.Partition`(纯函数,易测)

```go
func Partition(rules []nft.Rule) (kernel, userspace []nft.Rule, err error)
```

- 逐条按 `EffectiveMode()` 归类。
- `tcp+udp` + userspace:**拆成两条**——一条 `proto=udp` 进 kernel,一条
  `proto=tcp` 进 userspace(同 SrcPort/DestIP/DestPort/带宽)。
- **有效 (proto,port) 重叠检测**:把 `tcp+udp` 展开为同时占用 `tcp/port` 与
  `udp/port`,跨 kernel/userspace 两个最终集合做冲突检查,重叠则返回错误
  (例如「kernel 的 tcp+udp/8443」与「userspace 的 tcp/8443」相撞,或两条都想
  绑 tcp/8443)。这同时**修正**了既有的「tcp+udp 与 tcp 同端口」隐性歧义
  (现 `MergedRuleset` 按 `proto/port` 字符串去重,抓不到跨协议重叠)——
  属同一处检查,顺带做对,不算范围蔓延。
- `MergedRuleset` 保持原样(跨 owner 精确 proto/port 去重);Partition 是
  展开后的权威重叠校验点。

### 5.3 `forward/kernel`

`Reconcile(kernelRules) error` = `nft.Apply(kernelRules)`(原子) +
`tc.Apply(kernelRules, iface)`。**不含 shim**(shim 上移到 Dataplane,因为它
要同时服务两个后端,见 §8)。nft 渲染逻辑不变;由于 Partition 已剔除用户态
TCP 规则,内核后端天然不会为它们生成 DNAT,无需在 `RenderRuleset` 里加排除
分支。`Counters()` 读 `nft.Counters()` 转 `forward.Counter`。

### 5.4 `forward/userspace`

`Reconcile(userspaceRules) error` / `Counters()` / `Close(ctx)`。内部维护
「SrcPort → 运行中监听器」表。详见 §7。

### 5.5 `forward.Firewall`

包装 `shim.Registry`,`Sync(forwardRules, listenPorts)`。详见 §8。

## 6. reconcile 顺序与单一安全网(刻意从简)

`Dataplane` **只**记一个 `lastUserspace`(上次成功的用户态规则集)。**不**记
lastKernel——nft 应用是原子的,失败即保留旧表,本就无需快照。这是唯一的回滚
状态,不是完整事务系统。`Reconcile(ctx, rules)`:

1. `Partition` 出错 → 直接返回,**未触碰任何后端**。
2. `userspace.Reconcile(userspaceRules)`:用户态后端内部 make-before-break
   (先开全部新监听器,任一 bind 失败则关掉刚开的、回到原状并返回错误)。
   失败 → 返回错误,内核/防火墙未动。
3. `kernel.Reconcile(kernelRules)`:nft 原子应用 + tc。**硬失败**(nft 失败)
   → 把用户态回滚到 `lastUserspace`(`userspace.Reconcile(lastUserspace)`),
   返回错误。nft 原子:失败则内核保持旧表(与回滚后的用户态一致)。
4. `firewall.Sync(...)`:**尽力而为**,失败仅记日志不回滚(沿用现有
   「shim 失败非致命」语义)。
5. 全部成功 → 更新 `lastUserspace`。

**为何这一个安全网必须留**:`refreshOnce` 只在解析 IP **实际变化**时才重应用
(见 `rulesDiffer`),失败的写入不会自动收敛。没有这步回滚,kernel apply 失败会
留下「用户态新 / 内核旧」的错位且无人纠正。一步 `userspace.Reconcile(lastUserspace)`
即让数据平面实际状态与 daemon 逻辑状态(写入失败时不更新 `d.owners`,仍为旧)
重新一致。其余健壮性(优雅排空、双向回滚)按「转发层不过度设计」一律不做。

**顺序理由(userspace 先于 kernel)**:`kernel→userspace` 的同端口模式翻转时,
先开用户态监听器(此刻内核 DNAT 仍在,监听器收不到流量),再由 kernel 撤掉
DNAT,切换瞬间无黑洞。反向(`userspace→kernel`)翻转仍有亚秒级间隙
(关旧监听器与加 DNAT 之间),属罕见人工操作,接受并记录,不做跨后端
逐端口 make-before-break(过度工程)。

**已知收敛窗口**:nft 成功但 tc 失败时,内核态为新、用户态被回滚为旧——
瞬时不一致,下次 reconcile/DNS 刷新收敛。这与现状同类(现状 tc 失败 → nft 新、
state 旧),且 tc 失败不影响转发(只缺整形),不回滚 nft 以坚持「绝不为整形
丢包」(见现 `applier.go` 注释立场)。

## 7. 用户态 relay 内部(`forward/userspace`)

### 7.1 监听器生命周期

- 期望集 D(tcp 用户态规则,按 SrcPort)对当前集 C:
  - D 有 C 无 → `net.Listen("tcp4", ":SrcPort")` 开新(bind 失败 = reconcile 错)。
  - C 有 D 无 → **直接关**:停 accept + 关该监听器的在途连接(`l.close()`,
    一步到位,**无带超时的优雅排空**——转发层不镀金)。
  - 两者皆有同 SrcPort:目标(DestIP:DestPort)或限速变化 → 原子换
    (见 7.3),**不**重启监听器、不断在途连接。
- make-before-break:先开齐所有新监听器,任一失败回退已开的,保证用户态层
  all-or-nothing。
- 每个监听器用一个 `sync.Map` 跟踪在途连接,**仅为**关停时能把它们一并关掉
  (防僵尸连接);不是连接池,无其他用途。

### 7.2 连接处理:统一缓冲拷贝 + 可选令牌桶

每个 accept:`net.DialTimeout("tcp4", target)` → 双向各起一 goroutine 做
**带缓冲的拷贝循环**(64KB):

- 每个 chunk:读 → (限速时 `limiter.WaitN(ctx, n)`) → 写 → `atomic.Add` 累加
  该规则字节计数。
- **不用 splice/io.Copy**:刻意放弃零拷贝以换取(a)连接存活期间的**连续**
  字节计数(splice 只能在连接结束时得到总量,长连接无法实时计费),
  (b)限速/不限速代码统一(limiter 为 nil 即不限速)。这与 realm 一致——
  realm 开启流量统计时同样退回缓冲拷贝;本项目恒需按转发计费,故恒缓冲。
  见 §14 取舍。
- 实现就是双向 `io.CopyBuffer(dst, wrap(src), buf)`,`wrap` 是一个把 `Read`
  出来的字节先过 limiter(可选)、再累加计数器的极薄 reader。整段 relay
  控制在几十行——realm 级别的薄,不引入 gost 的协议/链式抽象。
- **半关传播**:一方 EOF → 对另一方 `CloseWrite()`(TCPConn),两向都完成
  才整体关闭,保证依赖单向 EOF 的协议正常工作。

### 7.3 目标 / 限速热更新(原子指针)

- 每条用户态规则持有 `atomic.Pointer[target]`(DestIP:DestPort)与
  `atomic.Pointer[rate.Limiter]`(限速,nil=不限速)。
- DNS 刷新或编辑改了目标 → 换 target 指针;**新连接** dial 时读取新值,
  在途连接保持各自 dial 时捕获的目标(不打断正在传输的连接)。
- 改了带宽 → 换 limiter 指针;**每条规则共享一个 limiter**(跨该转发所有连接,
  与 tc HTB 每端口语义一致);在途连接下个 chunk 自动采用新 limiter。
- 速率换算:`BandwidthMbps` 兆比特/秒 → `Mbps*1e6/8` 字节/秒(1 token=1 byte);
  burst 取 `max(每秒速率, chunk 大小)`(`rate.Limiter` 要求 burst ≥ 单次 WaitN
  的 n,否则报错)。

### 7.4 DNS

用户态**拨号已解析的 DestIP**(沿用 daemon 现有 resolver + 刷新环):
`setOwnerRuleset`/`Bootstrap` 已把 DestHost 解析进 DestIP,`refreshOnce`
定期重解析并再次 `Reconcile` → 触发 7.3 的 target 热更新。这与内核态一致
(内核 DNAT 也指向解析后的固定 IP,靠刷新环更新),单一 DNS 来源,不在拨号点
二次解析。

### 7.5 计数

- 每条用户态规则一个 `atomic.Int64` 字节计数。`Counters()` 产出
  `forward.Counter{Proto:"tcp", ListenPort:SrcPort, Bytes:n, Packets:0}`。
- **方向**:只计 client→target 方向字节,与 nft prerouting 计数器(仅入向)
  **对齐**,使用户态与内核态转发在 `forwards.total_bytes` / 租户配额上**口径
  一致**。Packets 在应用层无意义,恒 0。
- 局限:两种模式都只计入向(忽略回程),属既有口径,不在本特性引入/修正。

### 7.6 关闭(直接,无优雅排空)

`Close(ctx)`:停所有 accept + 关所有在途连接,**立即返回**。**不做**带超时的
优雅排空(转发层不镀金;daemon 关停时进程随即退出,残留拷贝 goroutine 无意义)。
`ctx` 仅为与 `Reconcile` 的接口对称而保留,实现忽略其截止期。由 Dataplane.Close
调用,位于 daemon `Run` 关停序列(dialer 停机 → `srv.Shutdown` → `dp.Close`,
见 daemon.go)。

## 8. 防火墙集成泛化(`internal/nft/shim` + `forward.Firewall`)

防火墙集成本质是**横切**两个后端的关注点,故从 kernel 后端上移到 Dataplane:

- 内核态需要:把 DNAT 后的转发流量在 FORWARD 型链放行
  (DOCKER-USER、ufw-user-forward)——**现有能力**。
- 用户态需要:把到本机监听端口的 SYN 在 INPUT 型链放行(ufw-user-input)——
  **新增**。ufw 默认 INPUT=drop + 白名单,不放行则用户态监听器不可达。

`ForwardShim.Sync(rules []nft.Rule)` 泛化为携带两类信息:

```go
type FirewallState struct {
    ForwardRules []nft.Rule  // 内核 DNAT 目标(FORWARD accept 用)
    ListenPorts  []PortProto // 用户态 TCP 监听端口(INPUT accept 用)
}
// 各 shim 自行取用:
//   DockerUserShim:用 ForwardRules 注 FORWARD accept;忽略 ListenPorts
//     (Docker 不过滤到宿主的 INPUT)。
//   UfwShim:ForwardRules → ufw-user-forward;ListenPorts → ufw-user-input。
```

- `OwnerComment`「nft-forward managed」标签机制、按 handle 删除、原子
  `nft -f` 脚本——全部沿用,只是 ufw shim 多渲染 INPUT accept 行。
- `Dataplane.Reconcile` 第 4 步在 kernel/userspace 都成功后调
  `firewall.Sync(kernelRules, userspaceTCPPorts)`,尽力而为。
- **探针**:`probeFirewallEnvironment` 增补——当存在用户态规则、INPUT 默认
  drop 且未检测到能放行 INPUT 的 shim 时,日志 WARN(类比现有 FORWARD=drop
  警告)。裸 nft/iptables 手工 INPUT=drop 无 ufw 的环境,需运维自行放行端口
  (与现有 FORWARD 立场一致)。

## 9. 统一计数:`forward.Counter`

- 新类型 `forward.Counter{Proto string; ListenPort int; Bytes int64; Packets int64}`
  (json:`proto/listen_port/bytes/packets`,与现 `nft.Counter` 线格式**完全一致**)。
- kernel 后端:`nft.Counter` → `forward.Counter`。userspace 后端:直接产出
  (Packets=0)。`Dataplane.Counters()` 合并两者。
- daemon 改动(纯内部类型,线格式不变):`countersFn`、`handleCounters`、
  `defaultCounters` 改用 `forward.Counter`;`/v1/counters` 响应 JSON 不变;
  `daemonclient.Counter` 镜像不变;dialer 的增量采样(按 proto+listen_port 关联)
  天然兼容——用户态 tcp 计数以 `("tcp", SrcPort)` 自然并入。
- 让 `Dataplane` 不再以 `nft.Counter` 作公共计数契约,是这层抽象的应有边界
  (`nft.Counter` 退回 nft 解析层内部类型)。

## 10. 端到端暴露

- **daemon 接线**:§5.1。摄入侧 `EffectiveMode()` 归一。
- **TUI**(`internal/tui/tui.go`):新增「模式」选择器字段(pill 选择器,复用
  现有 proto selector 的渲染/左右切换范式)。字段常量 `fMode` 加入
  `buildInputs`/`viewForm`/`enterAddMode`/`enterEditMode`/`submitAdd`/
  `submitEdit`;选项 `[kernel, userspace]`,默认 kernel。提交前本地用
  `nft.Validate` 拦 `udp+userspace`。列表视图可加一列或在备注前标 `[U]` 标识
  用户态(细节留实现计划)。
- **panel admin**(`handlers_admin.go`)与**租户**(`handlers_my.go`)的新增/
  编辑 forward 表单:加 `mode` 选择(默认 kernel);handler 解析
  `r.FormValue("mode")`,对 `udp+userspace` 返回校验错误;写 `db.Forward.Mode`。
- **hub 下发**:无需改——`SendApplyRuleset` 发 `wsproto.ApplyRuleset.Rules
  []nft.Rule`,`buildRules` 填了 `Mode` 即随之过线;agent 侧
  `SetPanelRuleset`→`setOwnerRuleset`→`Dataplane.Reconcile` 自动分流。
- **节点详情/列表**展示每条 forward 的模式(模板加列)。

## 11. 兼容性

- **新 daemon 读旧 state(v3)**:§4.4 迁移,空 mode → kernel。
- **旧 panel 推新 daemon**:`ApplyRuleset.Rules` 无 mode 字段 → daemon 摄入
  归一为 kernel,行为与升级前一致。
- **新 panel 推旧 daemon**:旧 daemon 的 `nft.Rule` 无 `Mode` 字段,JSON 多余
  键被忽略 → 旧 daemon 一律按内核态处理(优雅降级:用户态意图被忽略但不报错)。
- **register_local / tui_segment_changed** 加 `Forward.Mode`,旧端忽略多余键。

## 12. 测试策略

- **`forward.Partition`(纯函数)**:kernel/userspace 分流;`tcp+udp`+userspace
  拆分正确;有效 (proto,port) 重叠(含 tcp+udp×tcp、kernel×userspace)拒绝;
  `udp+userspace` 经 `Validate` 拒绝。
- **`nft.Validate`**:校验矩阵 §4.2 全覆盖。
- **状态迁移**:v3→v4 空 mode 补 kernel;v4 往返;摄入归一(mode="")。
- **`forward/userspace` relay**:loopback 端到端拷贝正确;半关传播;令牌桶限速
  (测速率近似);reconcile 增删监听器;目标热更新(在途连接不断、新连接走新
  目标);带宽热更新;`Close()` 直接关停(在途连接被关闭、goroutine 退出);
  bind 冲突 → make-before-break 回退。
- **`forward.Dataplane`**:kernel 失败回滚 userspace;Partition 失败不触碰后端;
  firewall 失败非致命;`lastUserspace` 维护;Counters 合并。
- **`forward.Firewall` / ufw shim**:INPUT accept 渲染;Docker 忽略 ListenPorts;
  按 handle 清理。
- **计数统一**:kernel+userspace 合并;线格式不变(`/v1/counters` 快照)。
- **daemon 集成**:fake Dataplane 注入,验证 `Reconcile/Close/Counters` 在
  `setOwnerRuleset`/`Bootstrap`/`Run` 关停 的接线与 `applierMu` 串行化。
- **panel**:handler 拒 `udp+userspace`;`buildRules` 透传 mode;DB CHECK。
- 现有 `applier_test.go` 等迁移到 fake Dataplane / 子后端 fake。

## 13. 实施顺序(依赖序)

1. 数据模型:`Rule.Mode` + `EffectiveMode` + `Validate` 矩阵(+ 单测)。
2. `internal/forward`:`Partition`(+ 单测)、`Counter` 类型。
3. `forward/userspace` relay(+ 单测,纯 loopback 不需 root)。
4. `forward/kernel`(包装现 nft+tc)、`forward.Firewall`(泛化 shim)。
5. `forward.Dataplane` 编排 + 回滚(+ 单测)。
6. daemon 接线:`Applier`→`Dataplane`,countersFn 统一,迁移 daemon 测试。
7. 状态迁移 v3→v4。
8. wsproto/db/server:`Forward.Mode`、迁移 0005、`buildRules`、handler 表单。
9. TUI mode 选择器。
10. 端到端联调(混合模式)。

## 14. 已决取舍(高层设计批准未覆盖、本 spec 定夺)

总原则:**转发层不过度设计**(取 realm/gost 之薄,不取其重),**交互层做好用**。

- **放弃 splice/零拷贝**(用户态):换连续计数 + 限速/不限速代码统一,与 realm
  「开统计即缓冲」一致。用户态价值在 cwnd 隔离(PEP),非吞吐峰值;裸 splice
  系统调用反而是更多代码。中继节点带宽量级下缓冲拷贝 CPU 成本可接受。
- **关闭直接、不优雅排空**:转发层不镀金;进程退出残留 goroutine 无意义。
- **只保留一个安全网(`lastUserspace` 回滚)**:不建完整事务系统;nft 原子 +
  这一步回滚已覆盖唯一会留错位的失败路径(§6)。
- **目标/限速原子热更新而非重启监听器**:更简单**且**不在 DNS 刷新误杀连接。
- **计数仅入向**:与 nft prerouting 口径对齐,保证两模式计费一致(代价:都
  忽略回程,属既有口径)。
- **拨号解析后 IP**:单一 DNS 来源、与内核态行为一致;不在拨号点二次解析。
- **reconcile 用户态先于内核**:消除 kernel→userspace 翻转黑洞;接受反向翻转
  亚秒间隙(罕见人工操作),不做跨后端逐端口 make-before-break。
- **mode 为 forward 属性,非 tunnel 门控**:YAGNI;留扩展点。
- **DB 复合约束「userspace⇒tcp」由 handler+Validate 保**:SQLite 不支持
  ALTER ADD CONSTRAINT,不为此重建表(除非实现计划认为值得)。
```
