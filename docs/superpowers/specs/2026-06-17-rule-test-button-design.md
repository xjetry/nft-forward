# 转发规则测试按钮

## 背景

转发规则列表缺少连通性测试入口。后端 `/api/probe-chain` 与前端 `ProbeChainButton`
组件在更早的版本里已实现，但在统一规则管理重构（旧 `chains/Detail` 页被删除）后，
按钮失去了挂载点，组件成为未被引用的孤儿。本设计把该能力重新接回当前规则 UI。

## 行为

在共享的 `RulesTable` 操作列加「测试」按钮，管理端 `/rules` 与用户端 `/my/rules`
同时生效。点击调用 `GET /api/probe-chain?rule_id=<id>`，让规则的每一跳节点去拨各自的
下一跳目标，就地显示逐跳延迟（如 `50ms + 30ms = 80ms`）；任一跳不通则该跳显示 `x`。

## 组件边界

- `RulesTable`：仅新增按钮挂载，不改既有列与排序。
- `ProbeChainButton`（`ui.jsx`）：自包含的探测交互与内联结果展示，复用现有样式。
- `probeChainEndpoint`（`probe.go`）：解析规则、归属校验、并发逐跳探测、汇总。

## 安全

`probe-chain` 此前不校验规则归属。按钮对普通用户开放后，需限制：非 admin 用户只能
测试自己拥有的规则（`rule.OwnerID == user.ID`），否则返回 403；admin 不限。否则普通
用户可传任意 `rule_id` 探测他人规则，泄露其节点名与目标地址。校验沿用 `apiMyDeleteRule`
的既有模式。

## 参数对齐

前端发送 `rule_id`，而后端原仅识别 `rule`/`chain`，导致孤儿组件从未真正跑通。后端改为
以 `rule_id` 为规范参数，保留 `rule`/`chain` 作为旧别名。

## 测试

- 后端单测覆盖归属门禁：他人规则→403、owner→放行、admin→放行。
- 实际逐跳探测依赖 agent 连接，不在单测范围；前端构建验证。

## 范围外

`/api/probe`（按 target+node 直接探测，管理端节点详情使用）的访问控制不在本次改动内。
