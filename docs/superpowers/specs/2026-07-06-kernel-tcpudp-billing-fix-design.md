# kernel + tcp+udp 计费为 0 修复设计（nftables v1.0.6 兼容）

## 1. 问题与根因

`kernel`（nftables DNAT）模式 + `tcp+udp` 协议的入口规则，在**前置节点的 nftables 为 v1.0.6** 时，计费恒为 0：前端"流量"列显示 0 B、全局配额与 per-grant 配额都不增长，但内核 counter 其实一直在正确计数（现场实测 node 235 上某端口 reply 已累计 24.8 GB）。

根因链（全部现场验证）：

1. account chain 用 conntrack 原始目的端口匹配规则：`meta l4proto <proto> ct original proto-dst <入口端口> ... counter`（`internal/nft/nft.go:254`）。DNAT 会改写目的端口，所以只能用 `ct original proto-dst` 还原入口端口。
2. 对 `tcp+udp` 规则，`l4protoMatch` 生成 `meta l4proto { tcp, udp }`（匿名 set）（`nft.go:289`）。
3. **nftables v1.0.6 缺陷**：当 `l4proto` 是 set 时，`ct original proto-dst` 无法绑定唯一传输协议、无法推断成 `inet_service`（端口）类型，`nft -j list` 把端口序列化成字符串 `"0x2cb5 [invalid type]"`（十六进制，`0x2cb5`=11445）而非数字。**nftables v1.1.3 已修此输出，返回正常十进制端口号**。
4. agent 侧 `internal/nft/counters.go:139` 用 `json.Unmarshal(m.Right, &port float64)` 解析端口，对字符串失败 → `ListenPort` 保持 0 → `counters.go:122` 的 `c.ListenPort != 0` 判假 → 整条 counter 被丢弃，agent 永不上报该端口样本。
5. panel 收不到样本 → 入口跳（`rule_hops` position=0）`total_bytes` 恒 0 → 前端"流量"列（`FillRuleTraffic` 读 position=0 的 total_bytes，`internal/db/rules.go:344`）显示 0；全局配额（`hub.go:786`，只在 position=0 累计）与 per-grant 配额（`SegmentFirstHops` 段首在 position=0）均不增长。

单协议（单 tcp / 单 udp）规则的 `proto-dst` 类型明确，任何 nftables 版本都输出数字，不受影响；所有 userspace 转发用 Go 内存计数、不解析 nft JSON，也不受影响。所有 agent 同为 v0.59.0，差异纯粹来自节点 nftables 版本。

## 2. 受影响范围

现场核实四个前置节点：node 3 / 77 / 235 = nftables **v1.0.6（中招）**，node 149 = **v1.1.3（正常）**。据此定界：

| 前置节点 | nftables | 中招规则 | 用户 | 落地跳可用性 |
|---|---|---|---|---|
| 235 volc-上海 | v1.0.6 | 181 / 198 / 199 / 203 | aifennng(25) | pos1=userspace，total 完整可用 |
| 77 po0-广州 | v1.0.6 | 55 / 178 | yccheung001(5) / echo777(8) | 无 pos1 或 pos1 也为 0 |
| 3 po0-上海 | v1.0.6 | 230 / 231 / 232 / 233 | china007(29) | pos1 total 极小（几百~几千字节） |

共 10 条规则、3 个 v1.0.6 节点、4 个用户。其余跑 v1.1.3+ 的节点上的 kernel+tcp+udp 规则计费正常，不在范围内。

节点属性（影响追回公式）：node 235/422/444 `unidirectional=0`（双向计费）；**node 3/77 `unidirectional=1`（只计上行）**。所有相关计费节点 `rate_multiplier=1.0`，四个用户 `billing_rate=1.0`、`traffic_reset_days=0`（无周期重置）。

## 3. 修复方案（治本：nft.go 拆单协议 counter）

核心：把 tcp+udp 规则的 account counter 从一条 `l4proto {tcp,udp}` 拆成 `l4proto tcp` + `l4proto udp` 两条**单协议**规则。单协议下 `ct original proto-dst` 类型永远是 `inet_service`，所有 nftables 版本都输出数字端口，从根上摆脱版本依赖。

### 3.1 nft.go 改动

三个 accounting chain（`account` / `account_local` / `account_local_reply`，`nft.go:244-281`）都按协议展开。新增一个 nft 包内 helper（语义等同 db 包的 `protoNamespaces`，此处独立定义避免跨包依赖）：

```go
// accountProtos returns the concrete l4protos a rule's counter must be split
// into. A tcp+udp rule needs one counter rule per protocol: nftables cannot
// type ct proto-dst under an l4proto set, so a combined { tcp, udp } counter
// loses the port on some nft versions. Single-proto rules keep their proto.
func accountProtos(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}
```

`account` chain（每规则、每方向）改为对每个 proto 各生成 original + reply：

```go
for _, r := range rules {
	if r.DestIP == "" || IsLoopback(r.DestIP) {
		continue
	}
	for _, p := range accountProtos(r.Proto) {
		b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction original counter\n", p, r.SrcPort))
		b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction reply counter\n", p, r.SrcPort))
	}
}
```

`account_local`（仅 original）、`account_local_reply`（仅 reply）同样以 `accountProtos` 展开。`l4protoMatch` 在 accounting chain 的用法被 `accountProtos` 取代；其在 prerouting/postrouting 的 `ProtoDportMatch`（用 `th dport` / 单协议 dport，不涉及 proto-dst）保持不变。

### 3.2 下游数据流（无需改动，已验证）

- `kernelBackend.Counters()`（`forward/kernel.go:35`）按 `(proto, port)` 聚合 nft.Counter。拆分后一条 tcp+udp 规则产出 `{tcp,port}` 与 `{udp,port}` 两个 forward.Counter（各含 up/down），行为与 userspace 落地跳一致。
- `counterSamples()`（`daemon/counters.go`）据此产出 `tcp/port` 与 `udp/port` 两条 CounterSample。
- panel `RuleHopMapByNode`（`db/queries.go:802` + `hopCounterKeys`）为每个 tcp+udp hop 注册 `tcp+udp/port`、`tcp/port`、`udp/port` 三个 key，均指向同一 hop 行。两条拆分样本各自命中同一 hop，`total_bytes` 累加。跨协议端口占用规则（`overlappingProtos`）保证同节点无两个 hop 共享 key。

这套"拆分样本 → 三-key 匹配回同一 hop"的路径是 userspace 模式既有行为，本方案 复用它，不引入新匹配逻辑。

### 3.3 测试

- nft ruleset golden test：更新 account/account_local/account_local_reply 的预期输出，验证 tcp+udp 规则渲染为两条单协议 counter、单协议规则不变。
- counter 聚合测试：构造 tcp+udp 规则的 tcp/udp 双样本，断言归并进同一 hop 的 total_bytes。

### 3.4 可选加固（YAGNI 权衡，默认不做）

可在 `counters.go:extractMatch` 增加对字符串型 proto-dst（`"0x2cb5 [invalid type]"`，strip 后缀按十六进制解析）的兜底。本方案 已从根上消除该输出，故列为可选：仅当希望"未升级 agent 的 v1.0.6 节点也能读旧 ruleset counter"时才有价值。本设计不纳入核心范围。

## 4. 历史追回

数据源分档，按第 2 节可用性区别处理。**所有追回必须在部署本方案 之前完成**：升级 agent 触发的首次 reconcile 会用新 ruleset 原子替换整表，清零内核里旧 ruleset 的累计 counter。

### 4.1 aifennng（精确，纯 SQL）

落地跳（pos1，node 422 userspace，双向）`total_bytes` 是从创建以来的完整历史 raw，直接作为待追回字节。计费节点 444 `rate_multiplier=1.0`、用户 `billing_rate=1.0` → `weighted = raw`。三处账本：

- `rule_hops.total_bytes`（position=0）**设为** weighted（当前为 0，代表该规则累计流量，直接置值）
- `users.traffic_used_bytes` **加** weighted（全局配额，position=0 only）
- `user_nodes.traffic_used_bytes`（段首逻辑节点 = 444）**加** raw（per-grant 配额记原始字节）

exit ledger（`user_landing_exits`）由落地跳 final hop 持续累计、不受此 bug 影响，**不补**。

具体值（rule 198 落地为 0，跳过）：

| rule | pos0 hop id | raw = weighted (bytes) |
|---|---|---|
| 181 | 618 | 408,864,427 |
| 199 | 622 | 1,837,547 |
| 203 | 603 | 57,449,421,610 |
| 合计 | — | 57,860,123,584（≈53.9 GB）|

```sql
BEGIN;
UPDATE rule_hops SET total_bytes = 408864427   WHERE id = 618;  -- rule 181
UPDATE rule_hops SET total_bytes = 1837547      WHERE id = 622;  -- rule 199
UPDATE rule_hops SET total_bytes = 57449421610  WHERE id = 603;  -- rule 203
UPDATE users      SET traffic_used_bytes = traffic_used_bytes + 57860123584 WHERE id = 25;
UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + 57860123584 WHERE user_id = 25 AND node_id = 444;
COMMIT;
```

追补后 aifennng 全局用量 ~101 GB（配额 1 TB）、444 grant 用量 ~59 GB（无限），均不触发超额封禁。

### 4.2 node 77 / node 3（尽力，SSH 读内核 counter）

这些规则无可用 pos1（55/178 落地为 0，china007 落地极小），完整历史已不可从 DB 重建，只能读内核 counter 当前值（仅"最近一次 reconcile 以来"的量）。流程：

1. SSH 到前置节点（77=`po0-gz`、3=`po0-shanghai`），`nft -j list table inet nft_forward`，用脚本解析 account chain：把 `"0x.. [invalid type]"` strip 后按十六进制转端口，取每个中招端口的 original（上行）与 reply（下行）字节。
2. 计费字节：**node 3/77 `unidirectional=1` → billedDelta = 上行(original) only**，不含 reply。
3. 按各规则的 `rate_multiplier`（均 1.0）× `billing_rate`（均 1.0）算 weighted（此处 = billedDelta），补入 4.1 同样的三处账本。各规则的 (pos0 hop id, 计费逻辑节点)：55→(99, 77)、178→(609, 219)、230→(627, 423)、231→(629, 423)、232→(633, 423)、233→(635, 423)。
4. china007（230-233）流量为几百~几千字节，可按象征性追补或跳过，不影响结论。

因数据源本质受限，此档追回标注为"尽力而为"，不追求完整历史。

### 4.3 部署顺序

1. 备份生产 `panel.db`。
2. 执行 4.1 的 aifennng SQL（事务内，可随时做）。
3. SSH 读取 node 77/3 内核 counter，执行 4.2 追补（**必须在升级这两个节点 agent 之前**）。
4. 合并本方案 代码、跑测试。
5. 编译新 agent，升级各节点（至少 3 个 v1.0.6 节点；v1.1.3 节点升级无害）。升级后 reconcile 成拆分 counter 的新 ruleset。
6. 验证：中招规则 position=0 的 `rule_hops.total_bytes` 开始随流量增长；`nft list table inet nft_forward` 的 account chain 中 proto-dst 全为数字、无 `[invalid type]`。

## 5. 风险与回滚

- **SQL 直改生产库**：全程事务 + 事前备份；追补 `users`/`user_nodes` 用 `+=`（幂等性靠"只执行一次"保证，重复执行会翻倍，需谨慎单次执行）。`rule_hops.total_bytes` 用 `SET` 幂等。
- **加权复现偏差**：aifennng 走 pos1 精确、公式最简（mult=rate=1、双向），风险最低；node 77/3 因单向 + 只有最近周期，本就"尽力"，偏差可接受。
- **nft ruleset 变更**：`nft.Apply` 原子替换，失败保留旧表，转发不中断；tc 失败仅降级不阻断（`kernel.go:29`）。本方案 仅改 account chain（计数，不影响转发路径），风险局限于计费。
- **回滚**：本方案 代码可直接 revert（account chain 退回 set 形式）；已追补的 DB 数值无需回滚（是真实用量）。

## 6. 不在本设计范围

- 不升级各节点的 nftables 系统包（本方案 让计费不依赖 nftables 版本，无需碰系统包）。
- 不改动 prerouting/postrouting 转发规则（转发本就正常）。
- 不改 panel 计费聚合逻辑（`applyCounters` 正确，问题只在 agent 侧样本缺失）。
