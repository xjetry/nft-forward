# 落地出口流量限额

管理员分配给用户的落地节点（订阅 + 手动 URI 解析出的全集）全部视为**特殊出口**：可按目标 host:port 为 key 配置独立流量限额。出口账本与用户/授权配额**互不相干**——字节累加与加权计费路径不动，这是纯增量的第二本账：用户配额计"用了多少转发资源"，出口账本计"给这个目标打了多少量"，同一份流量有意同时记两本。未配置限额（quota=0）= 不限量，但照常计量供展示。

## 语义

- 特殊出口 = 该用户落地来源**最近一次成功同步**的 host:port 集合（物化于 DB，见"来源解析与同步"；手动 URI 在 host:port 冲突时优先，与 `landingIndex` 口径一致）。用户浏览器本地节点服务端不可见，不参与。
- 账本按 (用户, host, port) 独立，不跨用户共享。key 不含协议：同 host:port 不同协议的落地条目共享一份额度（与 `landingIndex` 的去重口径一致）。
- 规则出口命中特殊出口时：现有计费（用户全局配额、grant 配额、倍率、billing_rate、unidirectional）全部照旧；**另外**在末跳按原始字节（上+下行合计，不乘任何倍率、不受 unidirectional 影响——账本记录的是打到该目标的真实流量，与计费加权无关）累入出口账本一次。
- 链式规则只在**末跳**（position 最大的 hop）计出口账本——中间跳的 target 是系统生成的 relay 地址。末跳判定按 position 而非 target 匹配，防止 relay 地址恰好等于出口 host:port 时重复计。
- 超额（quota>0 且 used>=quota）只使"该用户打到该 host:port"的规则从下发集消失，其他规则不受影响；恢复 = 周期重置 / 手动重置 / 调额，动作都是"清零或调额 + 重推"。
- host:port 为字符串精确匹配（与 `exit_kind` 徽章口径一致），不做域名↔IP 归一化：用户手填 IP 而落地写的是域名时不算特殊出口。
- 订阅里消失的出口标 `present=0`：打到它的规则回归普通口径（不再累出口账本、超额排除随之解除——同步时须触发重推，见执行节），quota/used 保留，节点回归订阅时无缝续账。
- 无 owner 的规则不参与（落地来源是按用户配置的，无用户即无出口集合）。

## DB（迁移 0028）

```sql
CREATE TABLE user_landing_exits (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host        TEXT    NOT NULL,
  port        INTEGER NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',
  protocol    TEXT    NOT NULL DEFAULT '',
  uri         TEXT    NOT NULL DEFAULT '',
  present     INTEGER NOT NULL DEFAULT 1,
  quota_bytes INTEGER NOT NULL DEFAULT 0,
  used_bytes  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, host, port)
);
```

- 一张表同时承担"物化的落地集合"（name/protocol/uri/present，供归类与展示）和"限额账本"（quota/used）。`present` 代替删行，订阅抖动（节点消失又回来）不丢账。
- `uri` 存原始代理 URI，使 `classifyExit` 的徽章与 RelayURI 改由本表驱动（见"一致性"节）。DB 本就持有 `users.landing_uris` 原文，无新增暴露面。
- `host` 存裸主机名（与 `rules.exit_host` 同口径，IPv6 不带方括号）；SQL 按 (host, port) 两列匹配，内存索引键用 `net.JoinHostPort` 生成，与 `landingIndex` 一致。
- `updated_at`：该行任意写入（同步/计量/调额/重置）时刷新，仅供运维排查，无业务消费方。

## 来源解析与同步

**错误感知的解析入口**：现有 `landingNodesFor` 静默吞掉订阅错误（err 丢弃后返回仅手动 URI 的集合），且 `Fetcher.Subscription` 会把错误负缓存 60s——直接拿它的返回值做同步，一次网络抖动就会把订阅出口整批翻成 `present=0`，恰好造成本设计要防的计费漂移。因此新增 `resolveLandingExits(u, force) (nodes []landing.Node, ok bool)`：手动 URI 解析（无网络，恒成功）+ 订阅拉取，**用户配置了订阅 URL 且拉取失败时 ok=false**。展示用途的 `landingNodesFor` 维持现状。

**`SyncUserLandingExits(d, userID, nodes, srcSubURL, srcURIs)`**：
- 仅在解析 ok 时调用（失败整轮跳过，含手动部分——集合是原子快照，不做半同步）。
- 以 `landingIndex` 去重后的集合 upsert 为 `present=1`（刷新 name/protocol/uri/updated_at），本次缺席的旧行置 `present=0`；**不触碰 quota/used**。
- 事务内重读 `users.landing_sub_url/landing_uris` 与本轮解析所用的来源值比对，不一致则整体丢弃——后台循环的 fetch 窗口（最长 10s）内管理员改了来源时，防止旧来源的解析结果覆盖新集合。
- 返回 present 发生翻转且 `quota_bytes>0 AND used_bytes>=quota_bytes` 的 (host, port) 集合——这些行的下发排除状态随翻转改变（1→0 解除排除、0→1 施加排除），调用方对其 `NodesForUserExit` 异步重推。被排除的规则不产生流量、不会经 OnTrafficUpdate 自愈，这里是 1→0 方向唯一的恢复触发器。

**同步点**：
1. `apiSetUserLanding` 保存来源后（force 解析）；
2. `apiMyLandingNodes`（含 `?refresh=1`）解析 ok 后；
3. admin 用户详情响应构建 `landing_nodes` 预览、解析 ok 时顺带同步；
4. 后台同步循环（仿 `expiryEnforcer`，30 分钟一轮，仅处理配了订阅 URL 的用户）——没人开页面时订阅变化也能进入计费归类；
5. 服务启动异步跑一轮（goroutine，不阻塞启动）：纯手动 URI 用户先行（无网络开销），订阅用户随后串行拉取，完成升级后的存量回填。

**冷启动窗口（明示接受）**：升级后首次启动时表为空，回填完成前徽章退化为 custom、出口账本不累计、RelayURI 缺失；手动用户在启动后立即恢复，订阅用户在回填一轮内恢复。表数据持久化，后续重启无此窗口。

## 计量（applyCounters）

- 批内一次性加载两份辅助数据（与现有读操作一样在事务外）：本批 `ruleMap` 涉及的 owner 的 `present=1` 出口三元组集合（`(user_id, host, port)`）；`rule_id → MAX(position)` 映射。
- 样本命中末跳且 `(owner, exit_host, exit_port)` 在集合中 → `exitAdds[(owner, host, port)] += 原始 up+down`，并将 `(owner, nodeID)` 置入 `touched`——出口账本的增长必须独立于计费加权触发超额检查：unidirectional 节点的纯下行批次、0 倍率节点的样本 weighted 为 0，现有路径不会置 touched。
- 与现有 hopWrites/userNodeAdds/userAdds 在**同一事务** flush：`UPDATE user_landing_exits SET used_bytes = used_bytes + ?, updated_at = ? WHERE user_id=? AND host=? AND port=?`。命中 0 行（集合加载与 flush 的间隙内该行被同步置 present=0 且被管理员删除）时忽略该批增量——删除表达的就是不再关心该账本。
- 现有字节累加与加权计费路径零改动。

## 超额执行与恢复

- `ActiveRuleHopsForPush` 追加第五个 NOT EXISTS：

```sql
AND NOT EXISTS (
  SELECT 1 FROM rules r4
  JOIN user_landing_exits ule ON ule.user_id = r4.owner_id
    AND ule.host = r4.exit_host AND ule.port = r4.exit_port
  WHERE r4.id = rh.rule_id
    AND ule.present = 1
    AND ule.quota_bytes > 0
    AND ule.used_bytes >= ule.quota_bytes
)
```

- `NodesForUserExit(d, userID, host, port)`：该用户 `exit_host/exit_port` 匹配的规则的 `DISTINCT rh.node_id`。只返回物理 hop 节点——composite 入口在 `rule_hops` 中已展开为物理 hop，composite 虚拟节点无 agent 连接，放进下发集只会制造虚假同步错误。
- `OnTrafficUpdate` 回调追加 `enforceExitQuota(userID)`（仿 `enforcePerNodeQuota`）：`ExitsExceedingQuota(userID)`（`present=1 AND quota_bytes>0 AND used_bytes>=quota_bytes`）→ 每个超额出口 `NodesForUserExit` → `dispatchToNode`。
- present 翻转触发的重推见"来源解析与同步"节（同步返回翻转集合，异步重推，不阻塞请求路径）。
- `CheckAndResetTrafficCycle` 同事务追加清零该用户 `user_landing_exits.used_bytes`；`ResetAllUserTraffic` 同——"重置该用户流量"语义覆盖全部三本账。
- 周期重置后的重推改为**无条件** re-dispatch 该用户 `DistinctUserNodes`（applyCounters 内联与 `cycleResetEnforcer` 两处）。现状只在用户因"流量超额"被全局禁用时才重推，per-grant 配额压制的规则在重置清零后要等某次无关下发才恢复——这是既有缺口，与本功能无关、可独立先行合入；出口配额的周期恢复依赖同一机制，本 spec 依赖该修复。

## API

| Method | Path | 说明 |
|---|---|---|
| GET | `/api/users/{id}/landing-exits` | admin。物化列表（name/protocol/host/port/present/quota_bytes/used_bytes，不带 uri）；`?refresh=1` 先 force 解析、ok 则同步再返回 |
| POST | `/api/users/{id}/landing-exits/quota` | admin。body `{host, port, quota_bytes}`，校验 ≥0，0=不限；行不存在返回 404；落库后异步重推 `NodesForUserExit`（调小可能立即触发排除，调大/清零解除排除；present=0 行跳过重推），审计 `user.set_exit_quota` |
| POST | `/api/users/{id}/landing-exits/reset` | admin。body `{host, port}`，清零 used，异步重推 `NodesForUserExit`（present=0 行跳过），审计 `user.reset_exit_traffic` |
| POST | `/api/users/{id}/landing-exits/delete` | admin。body `{host, port}`，仅允许删 `present=0` 的残留行（在册出口由同步维护，409 拒绝）；此类行不在排除条件内，无需重推；审计 `user.delete_exit` |
| GET | `/api/my/landing-nodes` | 解析 ok：同步后返回实时列表与顺序，每个节点按 host:port join 物化表附 `quota_bytes`/`used_bytes`/`exceeded`（`exceeded := quota_bytes>0 && used_bytes>=quota_bytes`；join 顺序为解析→同步→join，此时必命中）。解析失败：回退返回物化表 `present=1` 行（构造同形状响应）并附 `stale: true`，供 UI 标注"订阅刷新失败，显示上次结果"。浏览器本地节点是客户端合并的，无账本字段 |

## Web UI

- **管理员**（`users/Detail.jsx`）：「落地节点来源」卡片下方新增「落地出口限额」卡片，数据源 `GET /users/{id}/landing-exits`，带刷新按钮（`?refresh=1`）。每行：名称、协议、host:port、限额（仿 `PerNodeQuotaForm` 内联 GB 输入 + 独立保存，0=不限）、已用、重置按钮、超额红标；`present=0` 行置灰标注"已不在来源"并提供删除。
- **用户**（`my/LandingNodes.jsx`）：现有五列（名称、协议、地址（脱敏）、来源、复制操作）之间插入「已用/总量」列（`fmtTrafficGB` 口径，quota=0 显示"不限"），超额行红色徽章；工具栏的条件"刷新订阅"按钮保留；本地节点该列显示"—"；`stale` 时提示订阅刷新失败。
- **侧边栏**（`Layout.jsx`）：用户导航补「落地节点」入口指向 `/my/landing`——该路由现为孤儿页面，用户要看用量必须有入口。

## 一致性：classifyExit 改由物化表驱动

`api.go` 四处 `landingIndex(s.landingNodesFor(u, false))` 调用点改为从 `user_landing_exits`（`present=1`）构建索引（表含 uri，`RewriteEndpoint` 照常工作）。效果：规则列表/详情渲染不再触发订阅网络请求（去掉热路径网络依赖），且徽章口径与计费/排除口径出自同一张表、严格一致（冷启动窗口除外，见同步节）。admin 用户详情响应中的 `landing_nodes` 是**来源解析预览**，保留实时口径（并作为同步点 3 顺带回填）。

## 协议与数据面

零改动：无新下发字段、`computeRev` 不变（不会引发升级重推风暴）、agent 不感知本功能。超额执行完全靠服务端下发排除。

## 实现顺序

单一 spec，但实现计划分三个可独立验证的阶段：① 周期重置无条件重推（既有缺口修复，先行）；② 后端全量（迁移、同步、计量、排除、API）；③ Web UI 与 classifyExit 驱动源切换。

## 测试

- db：迁移；Sync 语义（present 翻转、quota/used 保留、manual 优先去重、来源变更时丢弃过期同步、返回需重推的翻转集合）；`ExitsExceedingQuota` / `NodesForUserExit`（含链式规则只出物理 hop 节点）。
- 同步失败路径：订阅拉取失败时 `apiMyLandingNodes`/后台循环不翻转 present（负缓存窗口内亦然）；纯手动用户恒同步。
- server 计量：仅末跳累加（含构造中间跳 target == 出口 host:port 的链式规则，验证不重复计）；原始字节不加权、不受 unidirectional 影响，且纯下行批次也触发 `enforceExitQuota`；quota=0 照常计量但不排除；present=0 / 无 owner 不计；与现有三本账同事务。
- server 执行：push 排除条件命中/解除；超额只排除打到该出口的规则、同用户其他规则不受影响；host:port 精确匹配（域名≠IP）；present 1→0 翻转触发重推、规则恢复下发；周期重置清零出口账本并无条件重推。
- API：quota/reset/delete 端点的权限、校验、审计、重推口径（present=0 跳过）；my/landing-nodes 的 exceeded 计算与 stale 回退。
- classifyExit：物化表驱动的徽章 / LandingURI / RelayURI 回归（`landing_test.go` 现有用例迁移口径）。
- web：限额表单往返、超额红标、present=0 置灰与删除、侧边栏入口。

## 不做的事

- 不做出口级限速（限量不限速，数据面无需分组）。
- 不做全局共享额度池（账本按用户独立）。
- 不做域名解析归一化（字符串精确匹配）。
- 浏览器本地落地节点不参与（服务端不可见）。
- TUI 不涉及。
