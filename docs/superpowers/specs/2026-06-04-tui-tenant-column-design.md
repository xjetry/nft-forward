# TUI 新增「用户」列 + 列间距修复 — 设计

## 背景与目标

TUI 的统一列表中,server 下发(panel 段)的转发只显示笼统的「来源」(server / 链路 X),看不出每条规则归属哪个租户(用户)。同时当前列宽偏紧,长目标(如域名 `seednet.xjetry.fun`)会占满「目标」列、与「远程端口」列粘连(`seednet.xjetry.fun8443`)。

目标:
1. 在列表中以独立「用户」列展示 server 下发规则所属的租户名。
2. 加宽并统一列间距,使任意长度的目标都与相邻列保持可见间隔。

约束:`nft.Rule` 不得携带影响数据平面的新语义;tenant 名是纯展示元信息,与 chain 元信息(ChainID/ChainName)同等对待。

## 维度区分(不冗余)

- **来源**(已有,owner 维度):本地 / server / 链路 X —— 这条规则由谁管理、属于哪个段。
- **用户**(新增,tenant 维度):归属哪个租户。两维度正交:典型 server 下发规则为 `来源=server、用户=<租户>`;本地行、admin 直建规则、链式骨架的用户列为 `—`。

## 数据层:tenant 元信息(复用 chain 元信息路径)

1. `internal/nft/nft.go` — `Rule` 增 `TenantName string` `json:"tenant_name,omitempty"`。纯展示元信息,DNAT 渲染、userspace 转发、`MergedRuleset` 去重、DNS 解析均不读取。
2. `internal/server/server.go` `buildRules`(:218) — 仿现有 `chains` map 缓存,加 `tenants map[int64]*db.Tenant`;对 `f.TenantID.Valid` 的 forward,`GetTenant` 取 `Name` 填入 `rule.TenantName`(查不到则留空)。
3. `computeRev`(:275) — 在已有的 `r.ChainID=0; r.ChainName=""` 旁加 `r.TenantName = ""`,使租户改名不触发节点冗余 re-apply(与 chain 元信息一致)。

## TUI 层:新列 + 统一间距

`internal/tui/tui.go`。

**列顺序**:`来源 | 用户 | 协议 | 本机端口 | 目标 | 远程端口 | 备注`。

**「用户」列**:显示该行 rule 的 `TenantName`;为空(本地行、admin 规则、链式 hop)显示 `—`。`rowAt` 已返回完整 `nft.Rule`,viewList 直接读 `r.TenantName`。

**统一列尾间距**:引入常量 `colGap = 2`。所有固定列(来源、用户、协议、本机端口、目标、远程端口)的内容先 `truncateCell(content, 列宽 - colGap)`,再 `cellStyle(列宽)` 填充——保证内容与下一列之间至少 2 个空格,长内容截断为省略号后同样留间距。`renderTableRow` 内五列与 viewList 中 owner/tenant 两列统一走这套机制。

**列宽**(建议值,实现时按对齐微调;关键是「目标」列容纳常见域名后仍留间距):

| 列 | 宽度 | 内容区(宽-gap) |
|---|---|---|
| colOwner(来源) | 16 | 14(链路名过长则截断) |
| colTenant(用户) | 12 | 10 |
| colProto(协议) | 10 | 8(`tcp+udp` / `tcp (U)` 最长 7) |
| colSrcPort(本机端口) | 12 | 10 |
| colDest(目标) | 24 | 22(`seednet.xjetry.fun`=18,完整显示 + 间距) |
| colDstPort(远程端口) | 12 | 10 |
| comment(备注) | flex | 剩余宽度 |

`fixedWidth` 计入新的 colTenant;`commentWidth = innerWidth - fixedWidth`;窄终端的 fallback(`innerWidth < fixedWidth+1` → `80 - 2*colMargin`)逻辑保留。

## 测试策略

- **nft**:`Rule` 加 `TenantName` 不破坏既有 apply/merge/state 持久化;JSON round-trip 带 `tenant_name`。
- **server**:`buildRules` 对有租户 forward 填 `TenantName`、无租户留空;`computeRev` 排除 `TenantName`(同一组 forward 改租户名后 rev 不变)。
- **tui**:viewList 渲染——有租户的 panel 行「用户」列显示租户名、本地/无租户行显示 `—`;长域名行的「目标」与「远程端口」列保持固定间距(fixed-portion 宽度断言、列不粘连)。

## 非目标(YAGNI)

- 不改「来源」列的既有取值(本地/server/链路 X)。
- 不处理图中链式 hop 的 `chain_id` 为空导致「来源」显示 `server` 的现象——那是链路编排/数据的单独问题,与本布局+tenant 需求无关。
- 不引入租户的其他属性展示(配额/到期等)。
