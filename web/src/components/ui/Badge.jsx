import { cx } from './cx'

/* 标签类原子。

   分工（不变式）：
   - Badge 表达“状态”：颜色有含义，只能取五种语气之一。
   - Tag 表达“类别”：协议、模式、用途、节点类型……永远中性色。
   类别不占用状态色，所以一眼扫过表格时，带颜色的格子一定是需要注意的状态。

   两者同高（20px）、同圆角（chip），并排时边界对齐。 */

const TONE = {
  neutral: 'border-line bg-subtle text-ink-soft',
  accent: 'border-accent-line bg-accent-soft text-accent-ink',
  ok: 'border-ok-line bg-ok-soft text-ok-ink',
  warn: 'border-warn-line bg-warn-soft text-warn-ink',
  danger: 'border-danger-line bg-danger-soft text-danger-ink',
}

const DOT = {
  neutral: 'bg-ink-mut',
  accent: 'bg-accent',
  ok: 'bg-ok',
  warn: 'bg-warn',
  danger: 'bg-danger',
}

const SHAPE = 'inline-flex h-5 flex-none items-center gap-1 rounded-chip border px-1.5 text-caption font-medium whitespace-nowrap'

/**
 * 状态徽标。
 * @param {{
 *   tone?: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger',
 *   dot?: boolean,
 *   className?: string,
 *   children: import('react').ReactNode,
 * } & Record<string, unknown>} props 其余属性（如 title）透传到元素上。
 */
export function Badge({ tone = 'neutral', dot = false, className, children, ...rest }) {
  return (
    <span {...rest} className={cx(SHAPE, TONE[tone], className)}>
      {dot && <span className={cx('size-1.5 rounded-full', DOT[tone])} />}
      {children}
    </span>
  )
}

/**
 * 类别标签：中性描边，不带状态色。`mono` 用于协议名、模式名等字面量。
 * @param {{
 *   mono?: boolean,
 *   icon?: import('react').ReactNode,
 *   className?: string,
 *   children: import('react').ReactNode,
 * } & Record<string, unknown>} props
 */
export function Tag({ mono = false, icon, className, children, ...rest }) {
  return (
    <span {...rest} className={cx(SHAPE, 'border-line bg-surface text-ink-soft', mono && 'font-mono', className)}>
      {icon}
      {children}
    </span>
  )
}

/**
 * 状态圆点 + 文字：比 Badge 更轻，用于表格里每行都有的状态列——整列都是
 * 色块会喧宾夺主，圆点足以让异常行跳出来。
 * @param {{
 *   tone?: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger',
 *   className?: string,
 *   children?: import('react').ReactNode,
 * } & Record<string, unknown>} props
 */
export function StatusDot({ tone = 'neutral', className, children, ...rest }) {
  return (
    <span {...rest} className={cx('inline-flex items-center gap-1.5 whitespace-nowrap', className)}>
      <span className={cx('size-2 flex-none rounded-full', DOT[tone])} />
      {children}
    </span>
  )
}
