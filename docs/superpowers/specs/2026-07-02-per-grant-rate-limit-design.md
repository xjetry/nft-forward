# （用户+节点）共享限速

限速从规则级迁移到授权（grant，`user_nodes`）级，与该级的流量配额平级：同一用户在同一节点上的所有规则**共享一个总带宽桶**，上行+下行合计（与配额的双向合计计费口径一致）。单位一律 **MB/s**（字节，二进制 1 MB = 2²⁰ B），0 = 不限速。规则级限速入口（`POST /rules/{id}/bandwidth` 及其 UI）移除。

## 语义

- 给用户 A 在节点 N 设 10 MB/s：A 在 N 上的全部规则、双向流量合计共享 10 MB/s，不是每条规则各 10。
- 限速取自**规则所属面板节点上的 grant**（组合规则即组合节点的 grant），与流量配额的挂靠节点一致；链式规则的每一跳都以该值整形。
- 整形分组按 **grant** 而非按用户：同一物理节点上，一个用户的直连规则与途经的组合链路各自出自不同 grant，各成一组、各自限额——与两份配额分别计费的口径对齐。
- 无 owner 的规则（管理员直建、无对应 grant）不限速。
- `rate_limit_mbytes = 0` 表示不限速。注意与 `traffic_quota_bytes = 0`（回退全局用户配额）语义不同，UI 文案须区分。
- 一个用户在同一节点同时有 kernel 规则和 userspace 规则时，两个数据面无法共享同一令牌桶，实际为各 X 的两个池。这是已接受的近似，不做跨数据面聚合。
- tc 整形保持 best-effort：网卡探测失败时静默禁用（维持现状）。

## DB（迁移 0027）

```sql
ALTER TABLE user_nodes ADD COLUMN rate_limit_mbytes INTEGER NOT NULL DEFAULT 0;
UPDATE rules SET bandwidth_mbps = 0;
```

- `rules.bandwidth_mbps` 列保留（SQLite 兼容惯例）但清零并移除全部代码引用：`ruleCols`/`scanRule`/`Rule.BandwidthMbps`(db 层)/`SetRuleBandwidth` 删除。
- `db.UserNode` 加 `RateLimitMBytes int`（json `rate_limit_mbytes`）。`grants.go` 中所有 SELECT 与 inline scan 同步追加——该文件多处内联 scan，漏改会静默错位。
- `GrantNode` upsert 的 ON CONFLICT 同步覆盖 `rate_limit_mbytes`（与 max_forwards、traffic_quota_bytes 一致）。

## API

- 新增 `POST /users/{uid}/nodes/{nid}/rate-limit`，body `{"rate_limit_mbytes": N}`（admin 组）。校验 N ≥ 0；落库后对该节点重下发并写审计 `grant.set_rate_limit`。与 quota 端点同形。
- grant 出现的所有响应（用户详情、my dashboard、批量预览等）带出 `rate_limit_mbytes`。
- `/grants/batch-apply` 的 grants 元素接受 `rate_limit_mbytes`。
- 删除 `POST /rules/{id}/bandwidth` 路由与 `apiSetRuleBandwidth`。

## 下发协议

分组信息随规则携带，不新增消息段：

- `nft.Rule` 新增数据面字段 `ShapeGroup int64`（json `shape_group,omitempty`，取 `user_nodes` 的 rowid：upsert 下稳定，revoke+regrant 视为新 grant、新组）与 `RateMBytes int`（json `rate_mbytes,omitempty`）。旧 agent 解 JSON 忽略未知字段，天然兼容。
- `buildRules` 按 (owner, 规则所属面板节点) 查 grant 填这两个字段；同时把线上的旧字段 `bandwidth_mbps` 填为等效 Mbit 值（≈ rate × 8.389），让**未升级的旧 agent 以"每规则各限 X"降级执行**，升级后自动变为共享总桶。
- 字段优先级不变量：agent 侧凡 `shape_group`+`rate_mbytes` 有效即走分组限速并忽略 `bandwidth_mbps`；仅有 `bandwidth_mbps`（旧 server）时保留现有每规则限速路径。同一份下发内两种形态不会混出现（新 server 只在有 grant 时才填限速，两套字段同源）。
- `computeRev` 继续只剥离 RuleID/RuleName/OwnerName 三个展示字段；`shape_group`/`rate_mbytes` 参与 rev 哈希，保证改限速能触发重下发。首次升级会因新字段引起一次性全量重推，无害。
- standalone 降级路径（面板脱管清理）把 `shape_group`/`rate_mbytes` 随面板元数据一并清空——分组限速是面板驱动的策略，脱管后不应残留；旧字段 `bandwidth_mbps` 维持既有降级行为（保留）。
- `OwnerName` 仍是纯展示字段，数据面继续不读它。

## 执行层（agent）

### userspace

- 新增按 `shape_group` 键控、跨 Reconcile 稳定的共享 `*rate.Limiter` 注册表；限速值变化用 `SetLimit` 热更而非重建，避免清空桶状态。组内所有 listener 共用同一 limiter。
- 上行、下行两个方向的 relayCopy 都过组桶（现状下行传 nil 只计数），实现双向合计。
- 换算：X MB/s → X × 1048576 B/s，burst = max(1 秒配额, 64 KiB)（沿用 makeLimiter 的 burst 下限）。
- 旧式每规则限速路径保留（服务旧 server），仍为每 listener 独立桶、仅上行。

### kernel（nft + tc）

现有 mark 机制有根本缺陷：mark 打在 nat prerouting 链，nat 链只有连接首包遍历，且无 connmark 保存/恢复，后续包与整个回程全部落入不限速的默认 class。本次一并修复：

- 首包：prerouting DNAT 规则内先 `meta mark set (0x10000 | shape_group)` 再 `ct mark set meta mark`——首包自身立即带 mark 参与分类，同时把 mark 存入 conntrack；偏移使组 mark 与旧式端口 mark（≤ 0xFFFF）空间不相交，混合过渡期互不误伤。
- 每包：新增 filter 类 prerouting 链（mangle 优先级），`ct mark != 0` 时 `meta mark set ct mark` 恢复——转发流量双向都经 prerouting，一条恢复规则覆盖两个方向。
- tc：每组一个 HTB class（`classid 1:<shape_group>`，rate=ceil=X MB/s，以 bit 精确换算 X × 8388608 bit/s），fw filter 按 mark（`0x10000 | shape_group`）分类到该 class。有效组的规则不再产生按端口的 class；旧式 `bandwidth_mbps` 每端口 class 路径保留以兼容旧 server。
- `shape_group > 0xFFFF` 时该组回退旧式每规则端口 mark 限速（tc class minor 仅 16 位；grant rowid 实际远小于此）。
- connmark 跨 re-apply 持久：组的 mark 值直接取自稳定的 `shape_group` 而非任何本地序号分配，重下发前后同一连接的分类保持正确。

## Web UI

- `users/Detail.jsx` GrantedNodesCard：加「限速」列，仿 PerNodeQuotaForm 加内嵌小表单（number 输入 + 「MB/s」后缀 + 独立保存按钮，min=0，0=不限）。
- 授权复制/粘贴文本格式扩展为 `节点名 | max=N | quota=XGB | rate=N`（N 为 MB/s）：序列化、解析正则、预览表、提交 payload 四处同步，缺一往返丢字段。`rate` 缺省为 0。
- `my/Dashboard.jsx` 已授权节点表：加只读「限速」展示（`X MB/s` / `不限`），与配额同区。
- `rules/Detail.jsx`：删除限速卡片（BandwidthForm）与信息区的 bandwidth 展示。

## 测试

- db：迁移后 grant CRUD 携带 `rate_limit_mbytes`；grants.go 各查询 scan 对齐。
- server：rate-limit 端点权限/校验/落库/触发下发（仿 `pernode_quota_api_test.go`）；buildRules 填 `owner_id`/`rate_mbytes` 且旧字段等效换算；无 grant/无 owner 时不填。
- wsproto：新字段 roundtrip；缺省字段（旧 server 形态）解码正常。
- forward：userspace 多 listener 共享桶实测 pacing（合计不超 X）、限速热更不重启监听、双向都受限；旧式每规则路径回归。
- web：粘贴格式含 `rate=` 的解析/序列化往返。

## 不做的事

- TUI 不涉及（TUI 无 grant 界面，配额同样不在 TUI）。
- 不做用户级（跨节点）默认限速回退。
- 不做每方向独立限速、不做跨数据面共享桶。
