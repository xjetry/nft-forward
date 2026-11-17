# 面板前端

React 19 + react-router 7 + Tailwind v4 + Vite。纯 JSX；状态模型与纯函数用 TypeScript（`.ts`），由 `tsc --noEmit` 检查。

```
npm run dev          # 开发服务器；面板地址用 NFT_PANEL_URL 覆盖（默认 http://localhost:8080）
npm run check        # 提交前跑这一条：设计约束 + 类型 + 引用解析 + 单测 + 构建
```

开发时打开 `/ui-gallery.html` 可以看到每个原子组件的每种状态（不进生产构建）。

## 分层

| 位置 | 职责 | 不做什么 |
| --- | --- | --- |
| `src/index.css` | 设计令牌（`@theme`）、基础层、表格几何 | 不写页面或组件专用的类 |
| `src/components/ui/` | 原子组件：唯一给交互元素写样式的地方 | 不发请求、不认识节点 / 规则等领域概念 |
| `src/components/page.jsx` | 页面骨架：`Page` `PageHeader` `Toolbar` `TableScroll` … | 同上 |
| `src/components/*.jsx` | 领域组件：`net.jsx`（节点 / 协议标签）、`probe.jsx`、`session.jsx`、`prefs.jsx`、`Layout.jsx`、`ResourceView.jsx` | 不写原生 `<button>` / `<input>`，不写一次性样式 |
| `src/lib/` | 请求、状态模型、纯推导 | 不渲染 |
| `src/pages/` | 页面：取数、组合组件 | 同领域组件 |

数据单向流动：`lib` 取数与推导 → 页面持有状态 → 组件经 props 渲染。

## 样式规则

Tailwind 默认的调色板、字号、圆角、阴影、字重刻度都已清空，**绕开令牌的类名不生成任何 CSS**（静默失效），所以由 `npm run lint:design` 检查。

- 颜色只用语义色：表面 `app` `surface` `subtle` `overlay`；线 `line` `line-soft` `line-strong`；文字 `ink` `ink-soft` `ink-mut`；强调 `accent*`；状态 `ok*` `warn*` `danger*`（各有实心 / `-ink` / `-soft` / `-line`）。
- 字号六级：`text-caption`(12) `text-small`(13) `text-body`(14) `text-title`(16) `text-heading`(20) `text-display`(28)。字重只有 `font-normal` `font-medium` `font-semibold`。
- 圆角按层级：`rounded-chip`(4) `rounded-control`(6) `rounded-popover`(8) `rounded-card`(10) `rounded-modal`(12)。嵌套时外圆角 = 内圆角 + 内边距。
- 控件高度只有两档：`h-control`(36) 与 `h-control-sm`(28)。同一行的控件用同一档。
- 卡片不带阴影、不嵌套；阴影只用于浮层（`shadow-popover` `shadow-modal`）。
- **颜色表示状态，不表示类别**：状态用 `Badge`（有色），类别用 `Tag`（中性）。
- 数字默认等宽（`tabular-nums` 全局生效）；数值列加 `num` 类右对齐。
- 页面的主操作放在 `PageHeader` 的 `actions` 里，每页至多一个 `variant="primary"`。
- 动效默认没有。现有的四处各自说明一件事：加载转圈（正在进行）、抽屉滑入、开关滑块、toast 出现（状态转移）。新增前先问它说明了什么。
- 页面与领域组件里不写：任意值（`text-[13px]`，布局约束如 `grid-cols-[…]` `max-w-[…]` 除外）、内联 `style`、原生交互元素、`window.confirm`。缺原子就加到 `components/ui`。

对比度由 `scripts/tokens.test.mjs` 从 `index.css` 的实际色值断言；改色值后跑 `npm test`。

## 状态规则

- **远程数据**用 `useResource(fetcher, deps)`，按 `loading | error | ready` 渲染（`<ResourceView>` 会穷尽三种状态）。加载失败不是“没有数据”，也不是“不存在”：`error.notFound` 才是不存在。刷新不卸载内容。
- **提交类动作**用 `useAction(fn)`：在途时重复触发被忽略，`pending` 给按钮设 `loading`。错误默认以 toast 报出。
- **弹窗**没有 `open` 属性：条件渲染即打开，卸载即关闭，表单状态因此每次都是新的。
- `api.*` 成功返回数据，失败抛 `ApiError{status}`，不会用 `null` 表示失败。
- 互斥的状态写成一个可辨识联合（`{ status: … }`），不写成几个布尔值；`switch` 的 `default` 用 `assertNever`。
- 登录会话、探测、落地节点目录各有一个模型：`components/session.jsx`、`lib/probe.ts`、`lib/landing.js`（`buildCatalog`、`effectiveRoles`、`hostPortKey`）。需要这些数据的页面从这里取，不各自推导。

状态模型的转移由 `src/lib/*.test.ts` 覆盖（`node --test`，无需额外依赖）。
