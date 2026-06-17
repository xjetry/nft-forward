# 升级走 WS 传输 + 规则响应去 path + 修复陈旧升级错误

日期:2026-06-17

本批次三件事,一起实现并部署:

- A. agent 升级二进制改为经 WS 传输(解决 po0 等无法访问非白名单 HTTP 的节点升不了级)。
- B. 规则列表/详情响应移除 `path` 字段并删除详情页「路径」行(顺带去掉为算 path 而做的每跳 GetNode 的 N+1 查询)。
- C. 修复:节点已是最新版本时,详情页不再显示陈旧的升级失败/告警。

---

## A. 升级二进制经 WS 传输

### 背景

升级链路:面板经 WS 发 `Upgrade{Version,SHA256,Size,DownloadAt}`(仅元信息),daemon 收到后**自己发 HTTP GET 去 `DownloadAt`(面板 /v1/binary)下载 ~13MB 二进制**(`daemon/upgrade.go` 的 `downloadBinary`)。`po0` 开头的主机够不到非白名单 HTTP/HTTPS,卡在下载这一步。WS 连接本身是通的(否则节点不会在线),是这类节点唯一可靠通道。

### 设计

把二进制字节直接塞进 WS 的 `Upgrade` 消息(面板内存里本就有 `selfBinaryBytes`),daemon 用消息里的字节、跳过 HTTP。保留 `DownloadAt` 让过渡期旧 daemon 仍能 HTTP 下载(向后兼容)。

改动:

1. **协议** `internal/wsproto/messages.go` 的 `Upgrade` 加字段 `Data []byte` (`json:"data,omitempty"`,encoding/json 自动 base64)。
2. **面板** `apiUpgradeNode` / `apiUpgradeAllNodes`(`internal/server/api.go`):构造 `Upgrade` 时同时带 `Data: selfBinaryBytes` 与原有 `DownloadAt`。
3. **daemon 读取上限** `internal/daemon/dialer.go:259` dial 成功后 `ws.SetReadLimit(64 << 20)`。coder/websocket 默认 32KB,装不下 ~17MB 帧。
4. **daemon 取二进制** `internal/daemon/upgrade.go` `handleUpgrade`:抽出 `upgradeBinary(u wsproto.Upgrade) ([]byte, error)`——`u.Data` 非空则校验其 sha256 等于 `u.SHA256` 后返回;否则回退现有 HTTP `downloadBinary(u)`(兼容旧面板)。`handleUpgrade` 用它取字节,后续替换/重启逻辑不变。

### 兼容性

| | 旧 daemon | 新 daemon |
|---|---|---|
| 新面板 | 忽略 Data,走 DownloadAt(HTTP) | 用 Data 走 WS |
| 旧面板 | HTTP | 无 Data,回退 HTTP |

过渡期:新面板升级任何节点都会经 WS 发 13MB;仍在旧 daemon 的节点会忽略它、照旧 HTTP 下载(短暂多收 13MB)。无害,全部升到新 daemon 后消失。

### po0 首次引导

po0 当前是旧 daemon,只会 HTTP,无法靠本特性自举。其首次升到 WS-capable 新 daemon 由用户手动完成(scp 二进制 + 重启)。此后 po0 走 WS 自动升级。本 spec 不含 po0 引导自动化。

### 测试

- `internal/wsproto`:`Upgrade` 带 `Data` 的 JSON round-trip(确认 base64 编解码)。
- `internal/daemon`:`upgradeBinary` —— Data 正确 → 返回字节;Data 的 sha 与 SHA256 不符 → 报错;Data 为空 → 进入下载分支(下载本身不在单测内,断言分支选择即可,或仅覆盖前两种)。

---

## B. 规则响应移除 path

### 现状

`path` 是链路字符串(`节点A → 节点B → 出口`),由 `buildRuleView`(`internal/server/shared.go`)生成,出现在:

- `ruleListItem`(列表响应):带 `path`,但列表表格 `RulesTable` 从不渲染它。
- `ruleView`(详情响应):详情页 `web/src/pages/rules/Detail.jsx:60-61` 展示「路径」行。

`buildRuleView` 为算 path,对每跳都 `GetNode`——列表每条规则触发 N+1 查询。

### 设计(列表 + 详情都移除)

1. `internal/server/shared.go`:`ruleView` 与 `ruleListItem` 删除 `Path` 字段;`buildRuleListItem` 不再传 Path。
2. `buildRuleView` 删除 `names` 循环(为 path 而做的每跳 GetNode),只保留 `hops`(用于 `hops[0]` 与 len 判断)、`exit`、`entry`、`entryNodeID`。返回 `ruleView{Rule, Entry, Exit, EntryNodeID}`。
3. 若删除后 `fmt` 在 shared.go 仍被其它函数使用(`parseExit`、`checkUserRuleQuota` 等仍用 `fmt.Errorf`),保留 import;否则移除。(实测仍有多处 `fmt.Errorf`,保留。)
4. 前端 `web/src/pages/rules/Detail.jsx`:删除「路径」行(label + `{rule.path || '--'}` 两行,第 60-61 行)。

### 测试

- 后端:现有规则列表/详情接口测试应继续通过(无 path 不影响 Entry/Exit/EntryNodeID)。
- 前端:`npm run build` 通过,详情页不再有「路径」行。

---

## C. 节点已是最新时不显示陈旧升级错误

### 问题

`deriveUpgradeStatus`(`internal/server/upgrade_status.go`)先判 `error`、再判版本,且只跟 `last_upgrade_version`(那次推送目标)比,不跟 server 当前版本比。结果:节点带过一次失败记录后,即便后来已升到最新(`agent_version == server_version`、详情页显示「最新」),「升级状态」仍永久显示「升级失败」。

### 设计

`deriveUpgradeStatus` 增加 `serverVersion string` 形参。在 `LastUpgradeAt` 有效后、其余判断之前,加一条:节点已是当前版本(`n.AgentVersion != "" && n.AgentVersion == serverVersion`)则返回 `none`(前端隐藏该行)——已是最新,陈旧的推送结果无需展示。其余 error/ok/pending/stuck 逻辑不变。

调用方 `apiGetNode` 改为 `deriveUpgradeStatus(n, serverVersion(), time.Now())`。前端无需改动(已对 `status === 'none'` 隐藏)。

### 测试

`internal/server/upgrade_status_test.go` 的 `deriveUpgradeStatus` 调用补 `serverVersion` 实参;新增用例:`agent == server` 且 `LastUpgradeStatus == "error"` → `none`(陈旧错误被抑制)。其余用例传一个与 agent 不同的 serverVersion 以保持原判定路径。

---

## 部署

实现 + 全量验证(`go test ./...`、`go vet`、`web build`)后,发版 v0.17.1 并部署面板(hosthatch-jp `nft-forward-upgrade`)。po0 由用户手动引导一次到新 daemon。

## 非目标

- 不自动化 po0 首次引导。
- 不分块传输(单条消息内嵌,升级是低频手动操作)。
- 不移除面板 `/v1/binary` HTTP 端点(过渡期旧 daemon 仍需)。
