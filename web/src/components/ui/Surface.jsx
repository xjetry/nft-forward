import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { copyToClipboard } from '../../lib/clipboard'
import { cx } from './cx'
import { IconButton } from './Button'
import { IconAlert, IconCheck, IconChevronRight, IconCopy, IconMore } from './icons'
import { moveFocusByArrow, useAnchoredPosition, useDismiss } from './popover'

/* ---------- 卡片 ---------- */

/**
 * 卡片：页面上唯一的“容器”层级。浅色靠 1px 边框、深色靠亮度阶梯立起来，
 * 不带阴影。
 *
 * 不变式：卡片不嵌套卡片。卡片内部需要分组时用 CardSection（一条分隔线）
 * 或留白，而不是再套一个带边框圆角的盒子。
 *
 * `fill` 让卡片在纵向 flex 页面里占满剩余高度并把子元素排成一列，这样
 * 里面的 TableScroll 可以独立滚动、工具栏保持不动。
 *
 * @param {{ fill?: boolean, className?: string, children: import('react').ReactNode } & Record<string, unknown>} props
 */
export function Card({ fill = false, className, children, ...rest }) {
  return (
    <section
      {...rest}
      className={cx(
        'rounded-card border border-line bg-surface',
        fill && 'flex min-h-0 flex-1 flex-col overflow-hidden',
        className,
      )}
    >
      {children}
    </section>
  )
}

/**
 * 卡片标题行：标题 +（可选）说明 + 右侧动作。行高固定 48px，动作区的
 * 按钮请用 size="sm"，这样标题行高度不被按钮撑开。
 * @param {{
 *   title: import('react').ReactNode,
 *   description?: import('react').ReactNode,
 *   actions?: import('react').ReactNode,
 * }} props
 */
export function CardHeader({ title, description, actions }) {
  return (
    <div className="flex min-h-12 flex-wrap items-center gap-x-3 gap-y-1 border-b border-line-soft px-5 py-2">
      <h2 className="text-title text-ink">{title}</h2>
      {description && <span className="min-w-0 text-small text-ink-mut">{description}</span>}
      {actions && <div className="ml-auto flex flex-none items-center gap-2">{actions}</div>}
    </div>
  )
}

/**
 * 卡片内的一段内容：统一 20px 内边距；相邻的 CardSection 之间自动出现一条
 * 分隔线。
 * @param {{ className?: string, children: import('react').ReactNode }} props
 */
export function CardSection({ className, children }) {
  return <div className={cx('border-line-soft p-5 [&+&]:border-t', className)}>{children}</div>
}

/* ---------- 键值列表 ---------- */

/**
 * 只读的“标签：值”列表，用于详情页的属性区。标签列定宽，所有值的左边缘
 * 对齐在同一条线上。
 * @param {{ className?: string, children: import('react').ReactNode }} props
 */
export function DescList({ className, children }) {
  return <dl className={cx('grid grid-cols-[7rem_minmax(0,1fr)] gap-x-4 gap-y-2.5 text-small', className)}>{children}</dl>
}

/**
 * @param {{ label: import('react').ReactNode, children: import('react').ReactNode }} props
 */
export function DescItem({ label, children }) {
  return (
    <>
      <dt className="text-ink-mut">{label}</dt>
      <dd className="min-w-0 break-words text-ink">{children}</dd>
    </>
  )
}

/* ---------- 数值 ---------- */

/**
 * 关键数字：标签在上、数值在下、可选的补充说明在最下。多个 Stat 放进同一张
 * 卡片用分隔线分开，不要一数一卡。
 * @param {{
 *   label: import('react').ReactNode,
 *   value: import('react').ReactNode,
 *   unit?: import('react').ReactNode,
 *   hint?: import('react').ReactNode,
 *   children?: import('react').ReactNode,
 * }} props children 放在数值下方（如进度条）。
 */
export function Stat({ label, value, unit, hint, children }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div className="text-small text-ink-mut">{label}</div>
      <div className="flex items-baseline gap-1">
        <span className="truncate text-display text-ink">{value}</span>
        {unit && <span className="text-small text-ink-mut">{unit}</span>}
      </div>
      {children}
      {hint && <div className="truncate text-caption text-ink-mut">{hint}</div>}
    </div>
  )
}

const METER_TONE = {
  accent: 'bg-accent',
  ok: 'bg-ok',
  warn: 'bg-warn',
  danger: 'bg-danger',
}

/**
 * 占比条：已用 / 总量。`tone` 表达占比所处的状态（例如接近上限时用 warn），
 * 不是装饰色。
 * @param {{ value: number, max: number, tone?: 'accent' | 'ok' | 'warn' | 'danger', label?: string, className?: string }} props
 */
export function Meter({ value, max, tone = 'accent', label, className }) {
  const ratio = max > 0 ? Math.max(0, Math.min(1, value / max)) : 0
  return (
    <div
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={value}
      className={cx('h-1 w-full overflow-hidden rounded-full bg-subtle', className)}
    >
      <div className={cx('h-full rounded-full', METER_TONE[tone])} style={{ width: `${ratio * 100}%` }} />
    </div>
  )
}

/* ---------- 复制 ---------- */

/* 复制的结果就落在用户刚点的位置上：成功换成对勾，失败换成警示并说明
   怎么办，片刻后都回到原样。失败必须可见 —— 非安全上下文（HTTP 且不是
   localhost）下剪贴板 API 不可用，execCommand 兜底也可能被浏览器拒绝，
   这时如果什么都不显示，用户分不清“已经复制了”和“没有反应”。 */
const COPY_LABEL = { idle: '复制', copied: '已复制', failed: '复制失败，请手动选中后复制' }

/** @returns {readonly ['idle' | 'copied' | 'failed', (text: string) => void]} */
function useCopy() {
  const [status, setStatus] = useState(/** @type {'idle' | 'copied' | 'failed'} */ ('idle'))
  const timer = useRef(/** @type {ReturnType<typeof setTimeout> | undefined} */ (undefined))
  useEffect(() => () => clearTimeout(timer.current), [])
  const copy = (/** @type {string} */ text) => {
    const settle = (/** @type {'copied' | 'failed'} */ next) => {
      setStatus(next)
      clearTimeout(timer.current)
      timer.current = setTimeout(() => setStatus('idle'), next === 'failed' ? 4000 : 1500)
    }
    copyToClipboard(text).then(() => settle('copied'), () => settle('failed'))
  }
  return /** @type {const} */ ([status, copy])
}

/** @param {{ status: 'idle' | 'copied' | 'failed', size: number }} props */
function CopyGlyph({ status, size }) {
  if (status === 'copied') return <IconCheck size={size} strokeWidth={2.5} />
  if (status === 'failed') return <IconAlert size={size} />
  return <IconCopy size={size} />
}

const COPY_TONE = { idle: undefined, copied: 'text-ok-ink', failed: 'text-danger-ink' }

/**
 * 可点击复制的行内文字（表格里的地址等）。是真正的 <button>，键盘可达。
 *
 * `label` 是无障碍名称。显示的文字每一行都相同时（如“复制链接”）必须给：
 * 读屏与语音控制只靠名称分辨这是哪一行的按钮。名称要包含显示的文字，否则
 * 照着屏幕上的字说出来的语音指令匹配不上。
 *
 * @param {{ text: string, label?: string, className?: string, children?: import('react').ReactNode }} props
 */
export function CopyText({ text, label, className, children }) {
  const [status, copy] = useCopy()
  return (
    <button
      type="button"
      aria-label={label}
      onClick={(e) => { e.stopPropagation(); copy(text) }}
      title={status === 'idle' ? '点击复制' : COPY_LABEL[status]}
      className={cx('group inline-flex max-w-full items-center gap-1 rounded-chip text-left', className)}
    >
      <span className="min-w-0 truncate">{children ?? text}</span>
      <span className={cx('flex-none', COPY_TONE[status] ?? 'text-ink-mut opacity-60 group-hover:opacity-100')}>
        <CopyGlyph status={status} size={12} />
      </span>
      {/* 行内没有位置放文字，结果另外读给读屏软件。 */}
      <span className="sr-only" role="status">{status === 'idle' ? '' : COPY_LABEL[status]}</span>
    </button>
  )
}

/**
 * 代码 / 字面量块：等宽、可横向滚动，右上角可选复制按钮。用于安装命令、
 * token、URI。给右侧留出按钮的位置，长文本不会被按钮盖住。
 * @param {{ text: string, copyable?: boolean, wrap?: boolean, className?: string, children?: import('react').ReactNode }} props
 * children 用于需要自定义渲染（如脱敏）的情况，复制的始终是 `text`。
 */
export function CodeBlock({ text, copyable = true, wrap = true, className, children }) {
  const [status, copy] = useCopy()
  return (
    <div className={cx('flex items-start gap-1 rounded-control border border-line bg-subtle py-1 pl-3 pr-1', className)}>
      <pre className={cx(
        'min-w-0 flex-1 py-1.5 font-mono text-small text-ink',
        wrap ? 'whitespace-pre-wrap break-all' : 'overflow-x-auto whitespace-pre',
      )}>
        {children ?? text}
      </pre>
      {copyable && (
        <IconButton size="sm" label={COPY_LABEL[status]} onClick={() => copy(text)} className={COPY_TONE[status]}>
          <CopyGlyph status={status} size={14} />
        </IconButton>
      )}
    </div>
  )
}

/**
 * 敏感文字：全局“脱敏”开启时模糊显示。只是防截图 / 旁观的视觉遮挡，
 * 不是访问控制。
 * @param {{ blurred: boolean, children: import('react').ReactNode }} props
 */
export function SensText({ blurred, children }) {
  return <span className={blurred ? 'select-none blur-sm' : undefined}>{children}</span>
}

/* ---------- 操作菜单 ---------- */

/**
 * 行尾的“更多操作”菜单。低频、破坏性的动作收在这里，让行内只留最常用的
 * 一两个按钮。
 *
 * 菜单圆角 popover(8) = 选项圆角 chip(4) + 4px 内边距，两层圆角同心。
 *
 * @param {{
 *   label?: string,
 *   items: Array<{ label: string, onSelect: () => void, danger?: boolean, disabled?: boolean, icon?: import('react').ReactNode }>,
 *   size?: 'md' | 'sm',
 * }} props
 */
export function Menu({ label = '更多操作', items, size = 'md' }) {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef(null)
  const menuRef = useRef(null)
  const pos = useAnchoredPosition(triggerRef, open, { align: 'end', preferredHeight: 240 })
  const close = () => {
    setOpen(false)
    triggerRef.current?.focus()
  }
  useDismiss(open, [triggerRef, menuRef], close)

  const attach = (node) => {
    menuRef.current = node
    if (node && !node.contains(document.activeElement)) node.querySelector('button:not(:disabled)')?.focus({ preventScroll: true })
  }

  return (
    <>
      <IconButton
        ref={triggerRef}
        size={size}
        label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={e => { e.stopPropagation(); setOpen(o => !o) }}
      >
        <IconMore />
      </IconButton>
      {open && pos && createPortal(
        <div
          ref={attach}
          role="menu"
          style={{ position: 'fixed', ...pos, minWidth: Math.max(pos.minWidth, 144) }}
          onClick={e => e.stopPropagation()}
          onKeyDown={e => {
            if (e.key === 'Tab') { close(); return }
            moveFocusByArrow(e, e.currentTarget, 'button:not(:disabled)')
          }}
          className="z-60 overflow-auto rounded-popover border border-line bg-overlay p-1 shadow-popover"
        >
          {items.map(item => (
            <button
              key={item.label}
              type="button"
              role="menuitem"
              disabled={item.disabled}
              onClick={() => { close(); item.onSelect() }}
              className={cx(
                'flex min-h-8 w-full items-center gap-2 rounded-chip px-2 text-left text-small whitespace-nowrap',
                'focus-visible:outline-none disabled:opacity-40',
                item.danger
                  ? 'text-danger-ink hover:bg-danger-soft focus-visible:bg-danger-soft'
                  : 'text-ink hover:bg-subtle focus-visible:bg-subtle',
              )}
            >
              {item.icon}
              {item.label}
            </button>
          ))}
        </div>,
        document.body,
      )}
    </>
  )
}

/* ---------- 折叠区 ---------- */

/**
 * 可折叠的内容区：一行可点击的标题（箭头 + 标题 + 右侧摘要），展开后内容
 * 接在下面。展开状态由调用方持有，方便做成“同组互斥”。
 *
 * 箭头的旋转是这里唯一的状态指示，不做展开动画。
 *
 * @param {{
 *   title: import('react').ReactNode,
 *   summary?: import('react').ReactNode,
 *   open: boolean,
 *   onToggle: () => void,
 *   children: import('react').ReactNode,
 * }} props
 */
export function Collapsible({ title, summary, open, onToggle, children }) {
  return (
    <div>
      <button
        type="button"
        aria-expanded={open}
        onClick={onToggle}
        className="flex min-h-control w-full items-center gap-2 rounded-control text-left text-body font-medium text-ink hover:bg-subtle"
      >
        <IconChevronRight size={14} className={cx('text-ink-mut', open && 'rotate-90')} />
        <span className="min-w-0 flex-1 truncate">{title}</span>
        {summary != null && <span className="flex-none text-small font-normal text-ink-mut">{summary}</span>}
      </button>
      {open && <div className="pt-3">{children}</div>}
    </div>
  )
}
