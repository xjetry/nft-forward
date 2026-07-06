# kernel + tcp+udp 计费为 0 修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 kernel(nftDNAT)+ tcp+udp 规则在任意 nftables 版本下都能正确计费,并追回历史流量。

**Architecture:** 把 nft account chain 里 tcp+udp 的 counter 从一条 `l4proto {tcp,udp}` 拆成 `l4proto tcp` + `l4proto udp` 两条单协议规则。单协议下 `ct original proto-dst` 类型永远是 `inet_service`,`nft -j` 输出数字端口,agent 解析不再失败。下游(kernel.go 聚合、hub 三-key 匹配)复用 userspace 既有的拆分上报路径,无需改动。历史流量在部署前手动追回。

**Tech Stack:** Go(internal/nft、internal/forward、internal/server)、nftables、SQLite(panel.db)。

## Global Constraints

- 代码注释/commit 不得体现开发过程信息(阶段编号、方案代号、审阅轮次);只解释 WHY 与 invariant。
- 版本号严格三段 vX.Y.Z。
- 所有追回必须在部署 agent(触发首次 reconcile 清零内核 counter)之前完成。
- 受影响范围 = nftables v1.0.6 前置节点上的 kernel+tcp+udp 入口规则(现场核实:node 3/77/235=v1.0.6 中招,node 149=v1.1.3 正常)。

---

## Task 1: nft.go account chain 拆单协议 counter

**Files:**
- Modify: `internal/nft/nft.go`(account / account_local / account_local_reply 三个 chain 的生成,约 244-281 行;删除 `l4protoMatch`,约 286-293 行)
- Test: `internal/nft/nft_test.go`

**Interfaces:**
- Consumes: `RenderRuleset(rules []Rule) string`、`Rule{Proto, DestIP, DestPort, SrcPort string/int}`、`IsLoopback(ip string) bool`、测试 helper `contains(s, sub string) bool`。
- Produces: 无新导出符号(内部新增 `accountProtos(proto string) []string`)。account chain 对 tcp+udp 规则渲染为两条单协议 counter。

- [ ] **Step 1: 写失败测试**

在 `internal/nft/nft_test.go` 追加:

```go
func TestRenderRulesetAccountSplitsTCPUDP(t *testing.T) {
	rules := []Rule{{Proto: "tcp+udp", DestIP: "1.2.3.4", DestPort: 80, SrcPort: 8080}}
	out := RenderRuleset(rules)
	// The account chain must count tcp and udp separately: nftables cannot type
	// ct proto-dst under an l4proto set, so a combined { tcp, udp } counter loses
	// the port ("[invalid type]") on v1.0.6 and the agent drops the sample.
	wantTCPOrig := "meta l4proto tcp ct original proto-dst 8080 ct direction original counter"
	wantTCPReply := "meta l4proto tcp ct original proto-dst 8080 ct direction reply counter"
	wantUDPOrig := "meta l4proto udp ct original proto-dst 8080 ct direction original counter"
	wantUDPReply := "meta l4proto udp ct original proto-dst 8080 ct direction reply counter"
	for _, w := range []string{wantTCPOrig, wantTCPReply, wantUDPOrig, wantUDPReply} {
		if !contains(out, w) {
			t.Fatalf("expected split counter %q, got:\n%s", w, out)
		}
	}
	if contains(out, "l4proto { tcp, udp } ct original proto-dst") {
		t.Fatalf("account chain must not use l4proto set for ct proto-dst, got:\n%s", out)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/nft/ -run TestRenderRulesetAccountSplitsTCPUDP -v`
Expected: FAIL —— 当前 account chain 渲染的是 `meta l4proto { tcp, udp } ct original proto-dst 8080 ...`,`wantTCPOrig` 不存在。

- [ ] **Step 3: 实现拆分**

在 `internal/nft/nft.go` 新增 helper(放在原 `l4protoMatch` 附近):

```go
// accountProtos lists the concrete l4protos a rule's accounting counter is
// emitted under. A tcp+udp rule gets one counter rule per protocol so that
// ct original proto-dst keeps its inet_service type: nftables cannot type a
// proto-dst under an l4proto set, and v1.0.6 then serialises the port as an
// untyped hex "[invalid type]" that the counter parser cannot read.
func accountProtos(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}
```

把 `account` chain 的循环(原用 `l4protoMatch(r.Proto)` 的两条 WriteString)改为按协议展开:

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

`account_local` 循环(单条 original)改为:

```go
for _, r := range rules {
	if r.DestIP == "" || !IsLoopback(r.DestIP) {
		continue
	}
	for _, p := range accountProtos(r.Proto) {
		b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction original counter\n", p, r.SrcPort))
	}
}
```

`account_local_reply` 循环(单条 reply)改为:

```go
for _, r := range rules {
	if r.DestIP == "" || !IsLoopback(r.DestIP) {
		continue
	}
	for _, p := range accountProtos(r.Proto) {
		b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction reply counter\n", p, r.SrcPort))
	}
}
```

删除现在无引用的 `l4protoMatch` 函数(account chain 是其唯一调用方;prerouting/postrouting 用 `ProtoDportMatch`,不受影响)。

- [ ] **Step 4: 运行测试确认通过 + 全包回归**

Run: `go test ./internal/nft/ -v`
Expected: PASS —— 新测试通过;`TestRenderRulesetTCPUDPUsesSetSyntax` 仍通过(prerouting DNAT 经 `ProtoDportMatch` 保留 `meta l4proto { tcp, udp } th dport`,set 语法仍在整体输出中);`TestParseCountersProto`(解析 set 形式 proto-dst → "tcp+udp")保持向后兼容,不变。

Run: `go build ./... && go test ./...`
Expected: 全绿(确认删 `l4protoMatch` 无残留引用、hub 三-key 匹配相关测试不受影响)。

- [ ] **Step 5: Commit**

```bash
git add internal/nft/nft.go internal/nft/nft_test.go
git commit -m "fix(nft): 按协议拆分 tcp+udp 计费 counter

account chain 用 l4proto { tcp, udp } 匹配 ct proto-dst 时,nftables
v1.0.6 无法为 proto-dst 推断 inet_service 类型,nft -j 输出 [invalid type]
字符串,agent 解析端口失败丢弃 counter,导致 kernel+tcp+udp 规则计费为 0。
拆成单协议 tcp/udp 两条 counter,proto-dst 类型恒定,不再依赖 nftables 版本。"
```

---

## Task 2: 历史流量追回（运维，部署前执行）

**前提:** 必须在 Task 3 升级 agent 之前完成——升级触发的首次 reconcile 会原子替换 nft 表、清零内核 counter。

**记账公式（与 `applyCounters` 一致）:**
- `billedDelta = 上行 + 下行`；若**上报物理节点** `unidirectional=1` 则 `billedDelta = 上行`。
- `weighted = round(billedDelta × rate_multiplier[规则.node_id] × billing_rate[owner])`。本批所有相关 `rate_multiplier` 与 `billing_rate` 均为 1.0，故 `weighted = billedDelta`。
- 三处账本:`rule_hops.total_bytes`(position=0)**SET** weighted;`users.traffic_used_bytes` **+=** weighted;`user_nodes.traffic_used_bytes`(段首逻辑节点)**+=** billedDelta。exit ledger 不动。

- [ ] **Step 1: 备份生产库**

```bash
/usr/bin/ssh hosthatch-jp 'cp -a /var/lib/nft-forward/panel.db /var/lib/nft-forward/panel.db.bak.$(date +%Y%m%d%H%M)' && /usr/bin/ssh hosthatch-jp 'ls -la /var/lib/nft-forward/panel.db.bak.*'
```
Expected: 备份文件存在。

- [ ] **Step 2: aifennng 精确追回（基于落地跳 pos1 total，双向，纯 SQL）**

aifennng(user 25)的落地跳是 node 422 userspace，`total_bytes` 是完整历史 raw。rule 198 落地为 0，跳过。计费节点 444（段首逻辑节点）。在生产库执行:

```sql
BEGIN;
UPDATE rule_hops SET total_bytes = 408864427   WHERE id = 618;  -- rule 181
UPDATE rule_hops SET total_bytes = 1837547      WHERE id = 622;  -- rule 199
UPDATE rule_hops SET total_bytes = 57449421610  WHERE id = 603;  -- rule 203
UPDATE users      SET traffic_used_bytes = traffic_used_bytes + 57860123584 WHERE id = 25;
UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + 57860123584 WHERE user_id = 25 AND node_id = 444;
COMMIT;
```

（57860123584 = 408864427 + 1837547 + 57449421610。）执行方式:把该 SQL 存为 `recover_aifennng.sql`，`/usr/bin/scp` 到 hosthatch-jp，`sqlite3 /var/lib/nft-forward/panel.db < recover_aifennng.sql`。

- [ ] **Step 3: 验证 aifennng 追回**

```bash
/usr/bin/ssh hosthatch-jp "sqlite3 -header -column /var/lib/nft-forward/panel.db \"SELECT id,total_bytes FROM rule_hops WHERE id IN (618,622,603); SELECT traffic_used_bytes FROM users WHERE id=25; SELECT traffic_used_bytes FROM user_nodes WHERE user_id=25 AND node_id=444;\""
```
Expected: 三个 hop total_bytes = 408864427/1837547/57449421610;users ≈ 105,265,267,490（原 47405143906 + 57860123584）；user_nodes(25,444) ≈ 59,314,522,471（原 1454398887 + 57860123584）。均未超各自配额。

- [ ] **Step 4: 读取 node 77 / node 3 内核 counter（尽力）**

内核 counter 只含"最近一次 reconcile 以来"的量，且 node 3/77 `unidirectional=1`（只取 original=上行）。用脚本解析 `[invalid type]` 十六进制端口:

```bash
for h in po0-gz po0-shanghai; do
  echo "=== $h ==="
  /usr/bin/ssh -o ConnectTimeout=15 "$h" 'nft -j list table inet nft_forward' | python3 -c "
import sys,json
d=json.load(sys.stdin)
acc={}
for it in d[\"nftables\"]:
    r=it.get(\"rule\")
    if not r or r.get(\"chain\")!=\"account\": continue
    port=None; direction=None; byts=0
    for e in r[\"expr\"]:
        m=e.get(\"match\")
        if m and isinstance(m[\"left\"],dict) and \"ct\" in m[\"left\"]:
            k=m[\"left\"][\"ct\"].get(\"key\"); v=m[\"right\"]
            if k==\"proto-dst\":
                port=int(str(v).split()[0],16) if isinstance(v,str) else int(v)
            if k==\"direction\": direction=v
        c=e.get(\"counter\")
        if c: byts=c[\"bytes\"]
    if port is None: continue
    acc.setdefault(port,{})[direction]=byts
for p in sorted(acc): print(p, acc[p])
"
done
```
Expected: 打印各端口的 `{original: 上行字节, reply: 下行字节}`。记录 node 77 端口(rule 55 的 entry 端口、rule 178 的 entry 端口)、node 3 端口(rule 230-233 的 entry 端口)对应的 **original** 字节。china007（230-233）通常为几百~几千字节，可按需跳过。

- [ ] **Step 5: 生成并执行 node 77 / node 3 补录 SQL**

对每条规则，用 Step 4 读到的 `original` 字节作为 `billedDelta`（单向节点只取上行），套用记账公式。各规则的 (pos0 hop id, 段首逻辑节点):55→(99, 77)、178→(609, 219)、230→(627, 423)、231→(629, 423)、232→(633, 423)、233→(635, 423)。模板（以 rule 55、读到上行 = `<B55>` 为例）:

```sql
BEGIN;
UPDATE rule_hops SET total_bytes = <B55> WHERE id = 99;    -- rule 55
UPDATE users      SET traffic_used_bytes = traffic_used_bytes + <B55> WHERE id = 5;
UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + <B55> WHERE user_id = 5 AND node_id = 77;
-- rule 178 (owner 8, 段首 219), 230-233 (owner 29, 段首 423) 同式追加
COMMIT;
```
执行前用 `SELECT traffic_quota_bytes,traffic_used_bytes FROM users WHERE id IN (5,8,29)` 确认追补后不超额（yccheung quota 200GB/used 19GB、echo777 无限、china007 100GB/used 0，均安全）。

- [ ] **Step 6: 记录追回结果**

把实际追回的每条规则字节数、执行时间记入变更记录（如 `docs/superpowers/plans/` 同目录的执行小结或 commit message），供审计。此步无代码改动。

---

## Task 3: 部署 agent 并验证（Task 1 合并 + Task 2 追回完成后）

**前提:** Task 2 全部追回已执行并验证（内核 counter 一旦被 reconcile 清零即不可再追）。

- [ ] **Step 1: 构建 agent 二进制**

按仓库既有发布流程构建（交叉编译 amd64、生成 SHA256SUMS、gh release），参见 nft-forward release runbook。最小本地校验:

```bash
go build ./... && go vet ./internal/nft/
```
Expected: 无错误。

- [ ] **Step 2: 升级 v1.0.6 节点的 agent**

至少升级 3 个中招节点:node 235(volc-sh)、node 77(po0-gz)、node 3(po0-shanghai)；v1.1.3 节点升级无害，可一并升。用 panel 的升级机制（`TypeUpgrade`）或既有分发流程推送新二进制并触发重启。

- [ ] **Step 3: 验证 nft ruleset 已拆协议、无 invalid type**

```bash
for h in volc-sh po0-gz po0-shanghai; do
  echo "=== $h ==="
  /usr/bin/ssh -o ConnectTimeout=15 "$h" 'echo -n "invalid type 计数: "; nft list table inet nft_forward | grep -c "invalid type"; echo "account chain proto-dst 示例:"; nft list chain inet nft_forward account | grep -m4 "proto-dst"'
done
```
Expected: 每个节点 `invalid type 计数: 0`;account chain 里 tcp+udp 规则出现为成对的 `meta l4proto tcp ... proto-dst <数字>` 与 `meta l4proto udp ... proto-dst <数字>`。

- [ ] **Step 4: 验证计费恢复（观察一段流量后）**

给一条中招规则打少量流量，等一个 counter 周期（≈5s）后:

```bash
/usr/bin/ssh hosthatch-jp "sqlite3 -header -column /var/lib/nft-forward/panel.db \"SELECT rule_id,position,last_bytes,total_bytes FROM rule_hops WHERE rule_id=203 ORDER BY position;\""
```
Expected: rule 203 的 position=0（node 235）`last_bytes`/`total_bytes` 在追回值基础上**随新流量继续增长**（此前恒为 0）。前端"流量"列同步显示非 0。

- [ ] **Step 5: 合并分支**

```bash
git checkout main && git merge --no-ff fix-kernel-tcpudp-billing-counter && git log --oneline -3
```
Expected: 修复代码与文档合入 main。

---

## Self-Review 备注

- **Spec 覆盖:** 根因修复→Task 1；aifennng 精确追回→Task 2 Step 2-3；node 77/3 尽力追回→Task 2 Step 4-5；部署顺序（追回先于 reconcile）→Task 2 前提 + Task 3 前提;版本无关验证→Task 3 Step 3。
- **类型一致:** `accountProtos` 在 Task 1 定义并使用；追回三账本字段名与 `applyCounters`（`rule_hops.total_bytes` / `users.traffic_used_bytes` / `user_nodes.traffic_used_bytes`）一致。
- **单向节点:** node 3/77 `unidirectional=1`，Task 2 Step 4-5 明确只取 original(上行)，与 `applyCounters` 的 `billedDelta` 语义一致；aifennng 走 node 422(双向)pos1 total，含上下行。
