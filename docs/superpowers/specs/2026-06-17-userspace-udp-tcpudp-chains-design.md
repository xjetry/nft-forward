# 用户态转发兼容 udp / tcp+udp(贯通到中继链)

日期:2026-06-17

## 背景与目标

用户态转发(`internal/forward/userspace.go`)是 TCP-only 的 split-relay,价值在于
中继链(composite 多跳节点)上的连接池/限速/分段。UDP 无需用户态——它在 nftables
里是零拷贝 DNAT。

目标:让 composite 中继链支持 `udp` 与 `tcp+udp` 协议,且 `tcp+udp` 规则在用户态
模式下自动拆分——**TCP 走用户态 relay,UDP 走 nftables 内核 DNAT**。这是性能与
易用性的权衡:用户只勾一个 `TCP+UDP`,系统内部按协议分流,UDP 不付出用户态的额外
延迟。

## 当前真实状态

数据平面(daemon)已天然支持本特性:

- `internal/forward/partition.go:51-62`:`proto=tcp+udp` 且 `mode=userspace` 的规则
  被拆为 udp 内核 DNAT 规则 + tcp 用户态规则(同 target/带宽)。
- 逐跳级联 DNAT 在链上成立:每跳的 udp 内核规则 DNAT 到下一跳的 relay_host:listen_port,
  下一跳再 DNAT 到它的下一跳,直至出口。
- `internal/db/rules.go:506-507`:每个 hop 行存 `proto = r.Proto`,daemon 收到后由
  `Partition` 完成拆分。

缺口全部在服务端编排、计费、UI 三层:

| 层 | 缺口 |
|---|---|
| API | `internal/server/api.go:689,838,1311` 校验 `proto != "tcp" && proto != "udp"`,拒收 `tcp+udp` |
| 端口占用 | `OccupiedPortsOnNode`(`internal/db/rules.go`)按精确 proto 串匹配,`tcp+udp` 与单独的 tcp/udp 规则不互斥 |
| 计费 | `internal/server/hub.go:451` 用 `proto/port` 查 hopMap,key 为 `tcp+udp/port`(`internal/db/queries.go:406`),但 daemon 拆分后上报 `tcp/port` + `udp/port` 两条样本,匹配不上 |
| Web 表单 | `web/src/pages/my/Rules.jsx:73-74` 把 composite 节点强制为 tcp |

纯 udp 中继链在 API + daemon 层已可运行(`rules.go:426` 把 udp 链路 mode 强制为 kernel,
逐跳 DNAT 级联),仅被 Web 表单挡住。

## 设计

不引入任何新的转发机制,把已有的 `tcp+udp` 语义从"直连规则"贯通到"中继链",并补齐
配套的端口占用、计费、UI 三层。

### 语义约定

- **`tcp+udp` + 用户态**:TCP 走用户态 relay,UDP 走内核 DNAT。每跳 mode 仅作用于
  TCP 腿;UDP 永远内核。
- **`tcp+udp` + 内核**:两者都走内核(本已支持)。
- **计费**:穿过某跳的总字节 = 该跳 TCP 字节 + UDP 字节,合并记入同一 hop 行。
- **纯 udp 中继链**:一并放开;mode 始终内核。

### 改动点

**1. API 放行 `tcp+udp`**

`internal/server/api.go` 三处协议校验(创建/更新/相关入口,约 689、838、1311)由
`proto != "tcp" && proto != "udp"` 改为同时接受 `tcp+udp`。校验逻辑统一收敛到一处
helper,避免三处漂移。

**2. 端口占用跨协议**

`OccupiedPortsOnNode`(`internal/db/rules.go`)的占用判定需与 daemon 端 `Partition`
的重叠定义一致:`tcp+udp` 占用 tcp 与 udp 两个命名空间。

- 查询某协议占用时:`tcp` 规则应视 `tcp` 与 `tcp+udp` 的端口为已占;`udp` 规则应视
  `udp` 与 `tcp+udp` 为已占;`tcp+udp` 规则应视 `tcp`、`udp`、`tcp+udp` 全部为已占。
- 实现上把单条 proto 查询展开为其覆盖的协议集合(`tcp`→{tcp,tcp+udp};
  `udp`→{udp,tcp+udp};`tcp+udp`→{tcp,udp,tcp+udp}),对 `rule_hops.proto` 做 IN 匹配。

这样 server 端分配端口时就不会产生 daemon 端 `Partition` 才能发现的重叠,避免整份
ruleset 被 daemon 拒绝。

**3. 计费 fan-in**

`internal/server/hub.go` 的 `applyCounters` 与 `internal/db/queries.go` 的 hopMap:
对 `proto=tcp+udp` 的 hop,daemon 上报的 `tcp/port` 与 `udp/port` 两条样本都要落到该
hop 行并累加。

- 方案:hopMap 在为 `tcp+udp` 建索引时,额外注册 `tcp/port` 与 `udp/port` 两个别名键
  指向同一 hop 行(纯 tcp / 纯 udp 仍只注册自身键)。`applyCounters` 查样本 key 时即可
  命中,字节自然累加进同一行。
- 入口跳计费(`EntryRuleHopIDs` / 用户用量归账)逻辑不变,因为仍是同一 hop 行,只是
  字节来自两个协议样本之和。

**4. Web 表单**

`web/src/pages/my/Rules.jsx`:移除对 composite 节点强制 `proto=tcp` 的逻辑
(73-74 行),让 composite 与普通节点一样可选 `tcp` / `udp` / `tcp+udp`。
检查规则详情/列表(`web/src/pages/rules/Detail.jsx`、`List.jsx`)对 `tcp+udp`
的展示无回归。

## 数据流(tcp+udp 链路,A→B→C→出口)

1. Server 物化链路:每跳 `rule_hops` 行 `proto=tcp+udp`、`mode=用户态`、
   `target=下一跳 relay:listen_port`(末跳 target=出口)。
2. 各 daemon 收到自己那条 hop 规则,`Partition` 拆为:
   - udp 内核 DNAT:`udp dport listen_port → 下一跳:port`
   - tcp 用户态:listener 在 `listen_port`,dial `下一跳:port`
3. UDP:A 内核 DNAT→B,B 内核 DNAT→C,C 内核 DNAT→出口(级联)。
4. TCP:A 用户态 relay→B,B→C,C→出口(逐跳 split-relay)。
5. 计数:每个 daemon 上报 `tcp/listen_port` 与 `udp/listen_port`,server 经别名键
   累加进该跳的 `rule_hops` 行;入口跳之和计入用户用量。

## 测试

- `internal/db/rules_test.go`:`tcp+udp` 链路物化后每跳 `proto=tcp+udp`;端口占用
  跨协议互斥(tcp 链路与 tcp+udp 链路抢同端口应冲突)。
- `internal/forward/partition_test.go`:已覆盖 tcp+udp 拆分;补一条与单 tcp/单 udp
  端口重叠应报错的用例(确认 server 端占用模型与之一致)。
- 计费:server 侧单测,`tcp+udp` hop 接收 `tcp/port`+`udp/port` 两样本,断言累加进
  同一行且入口跳用量正确。
- API:`tcp+udp` 创建/更新 composite 规则成功;非法 proto 仍被拒。
- 端到端(docker):tcp+udp 双跳链路,TCP 与 UDP 同时连通,计数双协议累加。

## 非目标

- 不实现 UDP 的用户态转发(连接池/限速对 UDP 无意义,且 nftables 已零拷贝处理)。
- 不改动直连规则的 tcp+udp 行为(已工作)。
- 不引入新的转发协议或拓扑。
