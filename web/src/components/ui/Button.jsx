import { Link } from 'react-router-dom'
import { cx } from './cx'
import { Spinner } from './Spinner'

/* 按钮几何。

   不变式：高度只来自 size 对应的高度令牌（h-control / h-control-sm），与
   Input、Select 共用同一组令牌 —— 同一行里的按钮和输入框因此必然等高。
   调用方不得再用 padding / text-* 类去改按钮的尺寸；需要更紧凑就传
   size="sm"，而不是叠一个 text-caption 上去。 */
const SIZE = {
  md: 'h-control px-3.5 gap-2 text-body',
  sm: 'h-control-sm px-2.5 gap-1.5 text-small',
}
const ICON_ONLY_SIZE = {
  md: 'size-control',
  sm: 'size-control-sm',
}

/* variant 表达层级（primary 每个视图至多一个），danger 表达破坏性，
   两者正交：primary+danger 是确认框里的“确认删除”，secondary+danger 是页面上
   的“删除节点”入口，ghost+danger 是行内的移除按钮。 */
const VARIANT = {
  primary: {
    normal: 'bg-accent text-on-accent hover:bg-accent-hover',
    danger: 'bg-danger text-on-accent hover:bg-danger-hover',
  },
  secondary: {
    normal: 'bg-surface text-ink border border-line-strong hover:bg-subtle',
    danger: 'bg-surface text-danger-ink border border-line-strong hover:bg-danger-soft hover:border-danger-line',
  },
  ghost: {
    normal: 'text-ink-soft hover:bg-subtle hover:text-ink',
    danger: 'text-danger-ink hover:bg-danger-soft',
  },
}

const BASE = 'inline-flex items-center justify-center flex-none rounded-control font-medium whitespace-nowrap select-none'
/* 不可用态：链接形态（<a>）没有 disabled 属性，用 aria-disabled 表达，
   两种形态共用同一组样式。 */
const DISABLED = 'disabled:opacity-50 disabled:pointer-events-none aria-disabled:opacity-50 aria-disabled:pointer-events-none'

function classes({ variant, danger, size, iconOnly, className }) {
  return cx(
    BASE,
    DISABLED,
    iconOnly ? ICON_ONLY_SIZE[size] : SIZE[size],
    VARIANT[variant][danger ? 'danger' : 'normal'],
    className,
  )
}

/**
 * 文字按钮（可带前置图标）。
 *
 * - 传 `to` 渲染成站内链接，传 `href` 渲染成普通链接，否则是 <button>。
 * - `type` 默认 "button"：表单里的提交按钮必须显式写 type="submit"，避免
 *   普通按钮意外触发提交。
 * - `loading` 期间按钮不可点并显示转圈；用它承接“提交中”状态，就不会出现
 *   重复提交。
 *
 * @param {{
 *   variant?: 'primary' | 'secondary' | 'ghost',
 *   danger?: boolean,
 *   size?: 'md' | 'sm',
 *   loading?: boolean,
 *   disabled?: boolean,
 *   icon?: import('react').ReactNode,
 *   to?: string,
 *   href?: string,
 *   type?: 'button' | 'submit',
 *   className?: string,
 *   children?: import('react').ReactNode,
 * } & Record<string, unknown>} props
 */
export function Button({
  variant = 'secondary', danger = false, size = 'md', loading = false, disabled = false,
  icon, to, href, type = 'button', className, children, ...rest
}) {
  const cls = classes({ variant, danger, size, iconOnly: false, className })
  const inert = disabled || loading
  const content = (
    <>
      {loading ? <Spinner size={size === 'sm' ? 12 : 14} /> : icon}
      {children}
    </>
  )
  if (to != null) {
    return <Link to={to} className={cls} aria-disabled={inert || undefined} {...rest}>{content}</Link>
  }
  if (href != null) {
    return <a href={href} className={cls} aria-disabled={inert || undefined} {...rest}>{content}</a>
  }
  return (
    <button type={type} className={cls} disabled={inert} aria-busy={loading || undefined} {...rest}>
      {content}
    </button>
  )
}

/**
 * 纯图标按钮：正方形，边长等于同 size 文字按钮的高度。
 *
 * `label` 必填：它同时是无障碍名称（aria-label）和悬停提示（title）——
 * 没有可见文字的按钮必须有名字。
 *
 * @param {{
 *   label: string,
 *   variant?: 'secondary' | 'ghost',
 *   danger?: boolean,
 *   size?: 'md' | 'sm',
 *   loading?: boolean,
 *   disabled?: boolean,
 *   to?: string,
 *   href?: string,
 *   className?: string,
 *   children: import('react').ReactNode,
 * } & Record<string, unknown>} props
 */
export function IconButton({
  label, variant = 'ghost', danger = false, size = 'md', loading = false, disabled = false,
  to, href, className, children, ...rest
}) {
  const cls = classes({ variant, danger, size, iconOnly: true, className })
  const inert = disabled || loading
  const content = loading ? <Spinner size={size === 'sm' ? 12 : 14} /> : children
  if (to != null) {
    return <Link to={to} className={cls} aria-label={label} title={label} aria-disabled={inert || undefined} {...rest}>{content}</Link>
  }
  if (href != null) {
    return <a href={href} className={cls} aria-label={label} title={label} aria-disabled={inert || undefined} {...rest}>{content}</a>
  }
  return (
    <button type="button" className={cls} aria-label={label} title={label} disabled={inert} aria-busy={loading || undefined} {...rest}>
      {content}
    </button>
  )
}

/**
 * 文字链接 / 行内文字动作：出现在正文、表格单元格里，字号跟随所在的文字，
 * 没有高度与内边距。传 `to` 是站内链接，`href` 是外部链接，否则是按钮。
 *
 * 页面里所有“看起来是一段可点的文字”的东西都用它，链接色与悬停表现因此
 * 只有一种。
 *
 * @param {{
 *   to?: string,
 *   href?: string,
 *   state?: unknown,
 *   tone?: 'accent' | 'ink' | 'danger',
 *   className?: string,
 *   children: import('react').ReactNode,
 * } & Record<string, unknown>} props `tone="ink"` 用于表格里的主名称：保持正文色，
 * 悬停才显出链接感，整列不会变成一片蓝。
 */
export function TextLink({ to, href, state, tone = 'accent', className, children, ...rest }) {
  const cls = cx(
    'inline-flex max-w-full items-center gap-1 rounded-chip text-left hover:underline disabled:opacity-50 disabled:hover:no-underline',
    tone === 'accent' ? 'text-accent-ink' : tone === 'danger' ? 'text-danger-ink' : 'text-ink hover:text-accent-ink',
    className,
  )
  if (to != null) return <Link to={to} state={state} className={cls} {...rest}>{children}</Link>
  if (href != null) return <a href={href} className={cls} {...rest}>{children}</a>
  return <button type="button" className={cls} {...rest}>{children}</button>
}
