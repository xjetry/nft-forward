import { useRef } from 'react'
import { Link } from 'react-router-dom'
import { cx, Input, IconArrowLeft, IconChevronDown, IconChevronUp, IconGrip, IconSearch } from './ui'

/* 页面骨架：所有页面共用同一套“页头 → 卡片 → 工具栏 → 表格”的结构件，
   管理端与用户端用的是同一套。

   页面的纵向节奏固定为：页头与内容之间 20px，卡片与卡片之间 20px
   （PageStack 负责）。 */

/**
 * 页面根容器。
 * - 默认：内容按文档流排列，整页滚动。
 * - `fill`：占满可用高度并排成一列，用于“页头 + 一张占满的列表卡片”的
 *   列表页 —— 只有表格行滚动，页头和工具栏不动。
 * - `width="form"` 把内容限制在表单版心内（设置、改密等单列表单页），
 *   避免输入框在宽屏上被拉得过长。
 * @param {{ fill?: boolean, width?: 'page' | 'form', className?: string, children: import('react').ReactNode }} props
 */
export function Page({ fill = false, width = 'page', className, children }) {
  return (
    <div className={cx(
      'mx-auto flex w-full flex-col gap-5',
      width === 'form' ? 'max-w-form' : 'max-w-page',
      fill && 'h-full min-h-0',
      className,
    )}>
      {children}
    </div>
  )
}

/**
 * 页头：标题 + 计数 / 副信息，右侧是页面级动作。
 *
 * 不变式：页面的主操作（“添加节点”“创建规则”）放在这里的 actions 里，
 * 每页至多一个 primary 按钮；卡片工具栏只放搜索与筛选。这样主操作在所有
 * 页面都出现在同一个位置。
 *
 * @param {{
 *   title: import('react').ReactNode,
 *   meta?: import('react').ReactNode,
 *   back?: { to: string, label: string, state?: unknown },
 *   actions?: import('react').ReactNode,
 * }} props `back` 用于详情页：在标题上方显示返回上一级的链接。
 */
export function PageHeader({ title, meta, back, actions }) {
  return (
    <header className="flex flex-none flex-col gap-1">
      {back && (
        <Link to={back.to} state={back.state} className="inline-flex w-fit items-center gap-1 text-small text-ink-mut hover:text-ink">
          <IconArrowLeft size={14} />
          {back.label}
        </Link>
      )}
      <div className="flex min-h-control flex-wrap items-center gap-x-3 gap-y-2">
        <h1 className="min-w-0 truncate text-heading text-ink">{title}</h1>
        {meta != null && <div className="flex min-w-0 flex-wrap items-center gap-2 text-small text-ink-mut">{meta}</div>}
        {actions && <div className="ml-auto flex flex-none flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </header>
  )
}

/**
 * 卡片内的工具栏：搜索、筛选、视图切换。一行放不下时自动换行。
 * @param {{ className?: string, children: import('react').ReactNode }} props
 */
export function Toolbar({ className, children }) {
  return (
    <div className={cx('flex flex-none flex-wrap items-center gap-x-3 gap-y-2 border-b border-line-soft px-4 py-3', className)}>
      {children}
    </div>
  )
}

/**
 * 搜索框。
 * @param {{ value: string, onChange: (value: string) => void, placeholder?: string, className?: string }} props
 */
export function SearchInput({ value, onChange, placeholder = '搜索…', className }) {
  return (
    <Input
      type="search"
      value={value}
      onChange={onChange}
      placeholder={placeholder}
      aria-label={placeholder}
      prefix={<IconSearch size={14} />}
      className={cx('w-full sm:w-72', className)}
    />
  )
}

/**
 * 列表卡片（<Card fill>）里的表格滚动区：只有表格行滚动，表头吸顶，卡片
 * 的工具栏保持不动。窄屏下可横向滚动并固定首列。
 */
export function TableScroll({ children }) {
  return <div className="table-scroll min-h-0 flex-1 overflow-auto">{children}</div>
}

/**
 * 非占满布局里的表格容器（概览卡片、详情页区块、弹窗）：随行数增高到上限，
 * 之后在内部滚动，不把整页撑长。
 * @param {{ className?: string, children: import('react').ReactNode }} props
 */
export function TableBox({ className, children }) {
  return <div className={cx('table-scroll max-h-112 overflow-auto', className)}>{children}</div>
}

/**
 * 可排序的表头单元格。点击在 降序 → 升序 → 不排序 之间循环；是真正的
 * <button>，键盘可操作，并通过 aria-sort 告诉读屏当前排序方向。
 *
 * @param {{
 *   col: string,
 *   sort: { col: string | null, dir: 'asc' | 'desc' | null },
 *   onSort: (col: string) => void,
 *   align?: 'left' | 'right',
 *   title?: string,
 *   className?: string,
 *   children: import('react').ReactNode,
 * }} props
 */
export function SortableTh({ col, sort, onSort, align = 'left', title, className, children }) {
  const active = sort.col === col
  const dir = active ? sort.dir : null
  return (
    <th className={cx(align === 'right' && 'num', className)} aria-sort={dir === 'asc' ? 'ascending' : dir === 'desc' ? 'descending' : 'none'}>
      <button
        type="button"
        title={title}
        onClick={() => onSort(col)}
        className={cx('inline-flex items-center gap-1 rounded-chip font-medium hover:text-ink', active && 'text-ink')}
      >
        {children}
        {/* 未排序时也保留一个弱化的箭头占位：提示这一列可排序，并且排序
            状态切换时表头宽度不变。 */}
        {dir === 'asc'
          ? <IconChevronUp size={12} strokeWidth={2.25} />
          : <IconChevronDown size={12} strokeWidth={2.25} className={active ? undefined : 'opacity-40'} />}
      </button>
    </th>
  )
}

/** 排序状态的循环：降序 → 升序 → 不排序。供各列表页的 onSort 复用。 */
export function nextSort(sort, col) {
  if (sort.col !== col) return { col, dir: 'desc' }
  if (sort.dir === 'desc') return { col, dir: 'asc' }
  return { col: null, dir: null }
}

/**
 * 调序手柄，放在可以调整顺序的表格行行首。
 *
 * 拖动只有指针能用，所以键盘路径做在手柄自己身上，而不是另放一组按钮：
 * 手柄可以聚焦，↑ / ↓ 把所在行移动一位。行的拖放目标（onDragOver / onDrop）
 * 仍由调用方挂在行上，手柄只负责“从哪一行开始拖”。
 *
 * 行按 key 复用 DOM 节点，但调换位置时浏览器可能把被移动的那个节点摘下来
 * 再插回去，焦点会跟着丢；所以每次键盘移动后把焦点放回这个手柄，才能连续
 * 按方向键。出于同样的原因，写入在途时（busy）用 aria-disabled 而不是把
 * 手柄卸载掉。
 *
 * @param {{
 *   name: string,
 *   index: number,
 *   count: number,
 *   busy?: boolean,
 *   onMove: (to: number) => void,
 *   onDragStart: () => void,
 *   onDragEnd: () => void,
 * }} props `name` 是这一行的名称，`index` / `count` 是它在当前列表里的位置与
 * 列表长度；`onMove` 收到的是目标位置。
 */
export function ReorderHandle({ name, index, count, busy = false, onMove, onDragStart, onDragEnd }) {
  const ref = useRef(/** @type {HTMLSpanElement | null} */ (null))
  const step = (e, delta) => {
    e.preventDefault()
    e.stopPropagation()
    const to = index + delta
    if (busy || to < 0 || to >= count) return
    onMove(to)
    requestAnimationFrame(() => ref.current?.focus())
  }
  return (
    <span
      ref={ref}
      role="button"
      tabIndex={0}
      draggable={!busy}
      aria-disabled={busy || undefined}
      aria-label={`调整「${name}」的顺序，现在是第 ${index + 1} 个，共 ${count} 个`}
      title="拖动调整顺序；聚焦后按 ↑ ↓ 移动"
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
      onClick={e => e.stopPropagation()}
      onKeyDown={(e) => {
        if (e.key === 'ArrowUp') step(e, -1)
        else if (e.key === 'ArrowDown') step(e, 1)
      }}
      className="inline-flex flex-none cursor-grab items-center rounded-chip text-ink-mut hover:text-ink-soft aria-disabled:cursor-default aria-disabled:opacity-50"
    >
      <IconGrip size={14} />
    </span>
  )
}

/**
 * 窄屏下代替表格行的列表项。整项可点击时传 `to`。
 * @param {{ to?: string, state?: unknown, className?: string, children: import('react').ReactNode }} props
 */
export function ListItem({ to, state, className, children }) {
  const cls = cx('block border-b border-line-soft px-4 py-3 last:border-b-0', to && 'active:bg-subtle', className)
  if (to) return <Link to={to} state={state} className={cls}>{children}</Link>
  return <div className={cls}>{children}</div>
}
