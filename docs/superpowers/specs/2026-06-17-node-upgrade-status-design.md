# 节点详情页展示 agent 升级状态/错误

日期:2026-06-17

## 背景与目标

管理员在节点详情页点「推送升级」后,有些节点的 `agent_version` 始终不变,且界面只弹一个
瞬时 toast,详情页看不到任何错误/进度,无法判断卡在哪。

目标:把每个节点**最近一次**升级的结果持久化,并在节点详情页展示;尤其要让"已确认接收
升级、但版本始终不变"这种**静默失败**从不可见变为可见。

## 根因(决定抓什么)

升级链路:`apiUpgradeNode` / `apiUpgradeAllNodes` → `Hub.SendUpgrade`(同步,最多等
60s 拿 `UpgradeAck`)→ daemon `handleUpgrade` 下载/校验/替换二进制 → 回 `OK:true` →
`go restartSelf()` 异步重启。

- **ack 之前**的失败(下载失败 / SHA 不匹配 / 替换失败):daemon 会把错误放进
  `UpgradeAck.Error`,`SendUpgrade` 返回该 error。当前 `apiUpgradeNode` 只 `jsonErr`
  弹 toast,不持久化。
- **ack 之后**的失败(`restartSelf` 的 `systemctl restart` 失败 / 新二进制启动即崩):
  daemon 在重启**之前**已回 `OK:true`,且 `restartSelf` 的错误被吞(`upgrade.go:124`
  的 `.Start()` 忽略 error)。server 认为成功,版本却永不更新——**这是本问题最可能的
  元凶**。

本设计**不改 daemon**。ack 之前的错误靠持久化 `SendUpgrade` 的返回值即可见;ack 之后的
静默失败靠 server 端**派生**(记录的目标版本 vs 节点重连后上报的 `agent_version`)。

## 数据模型

新增 migration `internal/db/migrations/0006_node_upgrade_status.sql`,给 `nodes` 加 4 列
(均可空,旧行默认空):

```sql
ALTER TABLE nodes ADD COLUMN last_upgrade_at INTEGER;
ALTER TABLE nodes ADD COLUMN last_upgrade_version TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_status TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_error TEXT;
```

- `last_upgrade_at` — 推送时间(Unix 秒)
- `last_upgrade_version` — 推送的目标版本(= `serverVersion()`)
- `last_upgrade_status` — `acked`(daemon 回 OK) 或 `error`(发送失败/ack 报错/超时/断连)
- `last_upgrade_error` — 错误文本(`acked` 时为空)

迁移由 `internal/db/db.go` 的 `//go:embed migrations/*.sql` 按版本号排序自动应用。

`db.Node` 结构体相应新增 4 个字段;`nodeCols` 常量与 `scanNode` 同步在末尾追加 4 列
(顺序必须与 SELECT 列表一致,二者是不变量)。`last_upgrade_at` 用 `sql.NullInt64`
(对齐既有 `LastApplyAt`);三个文本列用 `sql.NullString` 扫描后取 `.String`(空串)。

> 注:`internal/db/grants.go` 的 granted-node 查询有独立的列清单与扫描,不经 `nodeCols`/
> `scanNode`,本次不动它——升级状态只在管理员节点详情用到。

## 组件

### 写入:`db.RecordUpgradeResult`

```go
func RecordUpgradeResult(d DBTX, nodeID int64, version, status, errText string) error {
	_, err := d.Exec(
		`UPDATE nodes SET last_upgrade_at=?, last_upgrade_version=?, last_upgrade_status=?, last_upgrade_error=? WHERE id=?`,
		now(), version, status, errText, nodeID)
	return err
}
```

调用点(两条 live 路径,死代码 `upgrade.go` 的 SSR handler 不动):

- `apiUpgradeNode`:在 `SendUpgrade` 返回后、`jsonErr`/`jsonOK` 之前记录:
  ```go
  err := s.Hub.SendUpgrade(id, up)
  status, errText := "acked", ""
  if err != nil { status, errText = "error", err.Error() }
  db.RecordUpgradeResult(s.DB, id, serverVersion(), status, errText)
  if err != nil { jsonErr(...); return }
  ```
- `apiUpgradeAllNodes`:循环内每个被推送的节点(ok 与 fail 两支)都记录。被跳过
  (已是最新版本)的节点不记录。

### 派生:`deriveUpgradeStatus`(纯函数,server 包)

```go
const upgradeGrace = 90 * time.Second

type upgradeView struct {
	At      int64  `json:"at,omitempty"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"`           // none|ok|error|pending|stuck
	Error   string `json:"error,omitempty"`
}

func deriveUpgradeStatus(n *db.Node, now time.Time) upgradeView {
	if !n.LastUpgradeAt.Valid {
		return upgradeView{Status: "none"}
	}
	v := upgradeView{At: n.LastUpgradeAt.Int64, Version: n.LastUpgradeVersion, Error: n.LastUpgradeError}
	switch {
	case n.LastUpgradeStatus == "error":
		v.Status = "error"
	case n.LastUpgradeVersion != "" && n.AgentVersion == n.LastUpgradeVersion:
		v.Status = "ok"
	case now.Unix()-n.LastUpgradeAt.Int64 <= int64(upgradeGrace/time.Second):
		v.Status = "pending"
	default:
		v.Status = "stuck"
	}
	return v
}
```

派生取值与详情页表现:

| status | 条件 | 详情页 |
|---|---|---|
| `none` | `last_upgrade_at` 空 | 不展示升级区块 |
| `ok` | `agent_version == last_upgrade_version` | 绿:升级成功 |
| `error` | `last_upgrade_status == error` | 红:升级失败 + 错误文本 |
| `pending` | `acked` 且版本≠目标 且 `now - at ≤ 90s` | 蓝:升级中 |
| `stuck` | `acked` 且版本≠目标 且 超过 90s | 琥珀:可能未生效——已确认接收,版本仍为旧值,多半重启失败 |

`stuck` 不区分在线/离线:超过宽限期仍未带新版本回来,要么新二进制崩了(节点离线)、
要么重启没发生(在线但旧版本),都是失败。

### 接入详情接口

`apiGetNode`(`internal/server/api.go:285`)的响应 map 增加一项:
```go
resp["upgrade"] = deriveUpgradeStatus(n, time.Now())
```

### 前端

`web/src/pages/nodes/Detail.jsx`:在 Agent 版本行附近新增升级状态小区块,依据
`data.upgrade.status` 渲染对应颜色的 `Badge` 与文案;`error`/`stuck` 显示
`data.upgrade.error` 或派生提示文本;`none` 不渲染。复用现有 `Badge`、`fmtTime`。

## 数据流

1. 管理员点升级 → `apiUpgradeNode` → `SendUpgrade`(同步等 ack)。
2. 返回后 `RecordUpgradeResult` 写入 `nodes` 的 4 列(`acked` 或 `error`)。
3. 节点(若 ack 成功)重启 → 重连 → Hello 上报新 `agent_version` → hub 更新该列。
4. 管理员打开/刷新详情 → `apiGetNode` 调 `deriveUpgradeStatus` → 前端展示。
   - 重启成功:`agent_version` 已变 → `ok`。
   - 重启失败/崩溃:版本不变,超 90s → `stuck`(高亮)。

## 测试

- `internal/db`:`RecordUpgradeResult` 写入后 `GetNode` 读回 4 列正确(含 `acked` 与
  `error` 两种)。
- `internal/server`:`deriveUpgradeStatus` 表驱动覆盖 5 种取值,含 `pending`/`stuck`
  的 90s 宽限期边界(注入固定 `now`)。
- 前端:`npm run build` 通过。

## 非目标

- 不改 daemon、不新增日志通道、不抓 journalctl(本次只做结构化升级记录)。
- 不保留历史(只留最近一次,故用 nodes 列而非独立事件表)。
- 不清理 `internal/server/upgrade.go` 中未路由的 SSR handler(无关改动)。
