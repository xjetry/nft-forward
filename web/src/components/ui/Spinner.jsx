import { cx } from './cx'

/**
 * 行内转圈。颜色取 currentColor，放进按钮或文字旁自动跟随其颜色。
 * @param {{ size?: number, className?: string }} props
 */
export function Spinner({ size = 16, className }) {
  return (
    <span
      aria-hidden="true"
      style={{ width: size, height: size }}
      className={cx('inline-block flex-none rounded-full border-2 border-current border-t-transparent animate-spin', className)}
    />
  )
}

/**
 * 区块级加载占位：资源首次加载、还没有任何内容可显示时使用。
 * @param {{ label?: string }} props
 */
export function Loading({ label = '加载中' }) {
  return (
    <div role="status" aria-label={label} className="flex items-center justify-center py-16 text-ink-mut">
      <Spinner size={20} />
    </div>
  )
}
