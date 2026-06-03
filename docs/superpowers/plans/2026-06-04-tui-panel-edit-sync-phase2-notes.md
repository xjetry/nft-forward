# Phase 2 实现恢复笔记(非链式 panel 编辑 + 同步回 server)

> 压缩上下文后从这里接续。写 complete-code plan 前,按下面的 file:line 重读对应代码拿到镜像范式,再展开成正式 plan,然后用 subagent-driven-development 执行,最后 finishing-a-development-branch。

## 当前状态
- phase 1(只读可见)已合并到 `main`(commit `e36b67f`),全套测试绿。
- phase 2 分支 **`feat/tui-panel-edit-sync`** 已建,**当前就在此分支**。工作区有 `install.sh`(M,用户既有改动,勿动)。
- 设计 spec:`docs/superpowers/specs/2026-06-03-tui-panel-visibility-design.md`(phase 2 = 非链式编辑同步)。

## UX 决策(用户已定):统一列表 + 来源标记
- 保留 `model.rules`/`model.panelRules` 两个切片,**不引入 ruleRow 重构**。
- 统一跨段光标:`cursor` 0..len(rules)-1 = tui 段;len(rules).. = panel 段(index = cursor-len(rules))。helper `rowAt(i) -> (rule nft.Rule, owner string, editable bool)`;editable = owner=="tui" || rule.ChainID==0。
- `viewList` 合并渲染成一个列表 + 来源标记列(本地 / server / 链路 X);当前光标行高亮跨两段。
- 按键:`a` 新增→恒 tui 段;`d` 删除→仅 tui 段;`e` 编辑→按当前行 owner 路由,panel 链式行拒绝(提示端口/目标只读)。
- 提交按 owner:tui 行→`PostRuleset("tui", rules)`;panel 非链式行→`PostRuleset("panel", panelRules)`。model 加 `editingOwner` 记录编辑来源。

## Task 分解(写 plan 时每个展开为 TDD complete-code)

**T1 — wsproto 新帧**:`internal/wsproto/messages.go` 加常量 `TypePanelSegmentEdit = "panel_segment_edit"`(常量块 :16-27)+ payload `PanelSegmentEdit{Forwards []Forward}`(镜像 `TuiSegmentChanged` :127-129)。

**T2 — db.UpdateForward**:`internal/db/queries.go` 新增,只改非链式可编辑字段:
```
UPDATE forwards SET target_ip=?, target_port=?, comment=?, mode=?
WHERE node_id=? AND proto=? AND listen_port=? AND chain_id IS NULL
```
`chain_id IS NULL` 是 server 端兜底(即便上报含链式行也不会动)。已有 `GetForwardByNodeProtoPort`(:353) 查 chain_id;参照 `CreateForward`(:339)/`DeleteForward`(:359)/`NormalizeForwardMode`。返回受影响行数,便于判定是否命中。

**T3 — daemon 上报 panel 编辑(panelHook 全链路)**:
- `handlers.go`:加 `panelHook func([]nft.Rule)` 字段(仿 `tuiHook` :63);`setOwnerRuleset` 末尾(:239-245)改为同时取 `tuiHook`/`panelHook`,`owner=="tui"`调 tui、`owner=="panel"`调 panel(都在 d.mu 解锁后)。
- `daemon.go`(:169-196):`DialerConfig` 传 `OnPanelNotice: func(_ []wsproto.Forward){}`(non-nil marker);设 `d.panelHook = func(rules){ if dl:=d.Dialer(); dl!=nil { dl.NotifyPanelEdited(rules) } }`。
- `dialer.go`:`DialerConfig`(:35-46)加 `OnPanelNotice func([]wsproto.Forward)`;`Dialer` struct(:49-58)加 `panelCh chan []nft.Rule` + `pendingPanel atomic.Pointer[[]nft.Rule]`;`NewDialer`(:60-67)加 `panelCh: make(chan []nft.Rule, 1)`;加 `NotifyPanelEdited`(逐字镜像 `NotifyTuiChanged` :81-103,把 tuiCh/pendingTui 换成 panelCh/pendingPanel);write loop select(:273-279 那个 case 旁)加:
```
case rules := <-d.panelCh:
    if d.cfg.OnPanelNotice == nil { continue }
    fwds := rulesToForwards(rules)            // 复用 :284-303
    pp, _ := json.Marshal(wsproto.PanelSegmentEdit{Forwards: fwds})
    _ = writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypePanelSegmentEdit, Payload: pp})
```

**T4 — server hub 接收 + 校验 + 落库**:`internal/server/hub.go` readerLoop switch(:209-262,参照 `TypeTuiSegmentChanged` 分支 :233-242)加:
```
case wsproto.TypePanelSegmentEdit:
    var pse wsproto.PanelSegmentEdit
    if json.Unmarshal(env.Payload, &pse) != nil { log; continue }
    h.applyPanelEdits(ac.nodeID, pse.Forwards)
```
`applyPanelEdits(nodeID, fwds)`:逐条 `GetForwardByNodeProtoPort`;存在且 `!ChainID.Valid` → `UpdateForward`(只改 target_ip/target_port/comment/mode);链式或不存在 → 跳过 + log。(T2 的 `chain_id IS NULL` 是第二道保险。)

**T5 — TUI 统一列表(渲染 + 导航)**:`internal/tui/tui.go`。加 helper `totalRows()`、`rowAt(i)`。`viewList`(:617-699)把现在的"主列表 + 独立只读 panel 区"改为**一个**列表:遍历 tui 段再 panel 段,每行加来源标记(tui="本地"、panel 非链式="server"、panel 链式="链路 X"),按统一 `cursor` 高亮。`updateList`(:177-213)上下移动跨 `totalRows()`。删掉 phase 1 的独立只读 panel 区块(:680-699)。

**T6 — TUI 编辑路由 + 按 owner 提交**:`model` 加 `editingOwner string`。`enterEditMode`(:231-268)用 `rowAt(cursor)`;若 panel 链式行→不进编辑,设 `m.status="链式规则端口/目标只读,请在面板修改"`。`submitEdit`(:354-408)按 `editingOwner`:tui→改 `m.rules`+`PostRuleset("tui")`;panel→改 `m.panelRules`+`PostRuleset("panel")`。`enterAddMode`/`submitAdd`→恒 tui。`d` 删除(updateList :199)→仅当 `cursor<len(rules)`。`commit`(:542-550)改为接受 owner 参数或新增 `commitOwner`。`daemonclient.PostRuleset` 已接受任意 owner(client.go:162);`setOwnerRuleset("panel",...,"")` rev 空、不更新 LastAppliedRev(符合预期)。

## 镜像范式重读清单(写 plan 前读这些拿 snippet)
- `internal/daemon/dialer.go:81-103`(NotifyTuiChanged)、:273-279(write loop tui case)、:284-303(rulesToForwards)
- `internal/daemon/daemon.go:169-196`(tuiHook 接线)
- `internal/daemon/handlers.go:63`(tuiHook 字段)、:192-247(setOwnerRuleset,触发在 :239-245)
- `internal/server/hub.go:209-262`(readerLoop,tui 分支 :233-242)
- `internal/db/queries.go:339/353/359`(Create/Get/Delete Forward)、:317(forwardCols)
- `internal/tui/tui.go:74-92`(model)、:177-213(updateList)、:231-268(enterEditMode)、:354-408(submitEdit)、:542-550(commit)、:617-699(viewList,含 phase1 panel 只读区 :680-699)

## 流程
writing-plans 展开成 `docs/superpowers/plans/2026-06-04-tui-panel-edit-sync-phase2.md` → 用户 review → subagent-driven-development(implementer→spec审→质量审,每 Task) → finishing-a-development-branch(merge 到 main)。注意:派 subagent 时传达"禁止在代码注释/commit 体现过程信息(Task/Phase 编号等)"。
