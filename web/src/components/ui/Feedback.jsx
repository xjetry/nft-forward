import { createContext, useCallback, useContext, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cx } from './cx'
import { Button } from './Button'
import { IconAlert, IconCheck, IconInfo } from './icons'

/* ---------- 空状态 / 错误状态 ---------- */

/**
 * 空状态：这里“确实没有内容”。加载失败不是空状态，用 ErrorState。
 * 文案写下一步能做什么，而不是只陈述“没有”。
 * @param {{ title: string, desc?: import('react').ReactNode, children?: import('react').ReactNode }} props
 * children 放可选的引导动作（按钮）。
 */
export function Empty({ title, desc, children }) {
  return (
    <div className="flex flex-col items-center px-4 py-12 text-center">
      <p className="text-body font-medium text-ink-soft">{title}</p>
      {desc && <p className="mt-1 max-w-prose text-small text-ink-mut">{desc}</p>}
      {children && <div className="mt-4 flex items-center gap-2">{children}</div>}
    </div>
  )
}

/**
 * 错误状态：内容没能加载出来。与 Empty 分开，是为了不把“加载失败”说成
 * “没有数据”或“不存在”——那会让用户以为数据丢了。
 * @param {{ title?: string, message?: string, onRetry?: () => void }} props
 */
export function ErrorState({ title = '加载失败', message, onRetry }) {
  return (
    <div role="alert" className="flex flex-col items-center px-4 py-12 text-center">
      <p className="text-body font-medium text-ink">{title}</p>
      {message && <p className="mt-1 max-w-prose text-small text-ink-mut">{message}</p>}
      {onRetry && <Button className="mt-4" onClick={onRetry}>重试</Button>}
    </div>
  )
}

/* ---------- 行内提示 ---------- */

const ALERT_TONE = {
  neutral: 'border-line bg-subtle text-ink-soft',
  accent: 'border-accent-line bg-accent-soft text-accent-ink',
  ok: 'border-ok-line bg-ok-soft text-ok-ink',
  warn: 'border-warn-line bg-warn-soft text-warn-ink',
  danger: 'border-danger-line bg-danger-soft text-danger-ink',
}

/**
 * 行内提示条：留在页面上、与某块内容相关的说明或警示。转瞬即逝的操作
 * 反馈用 toast。danger 语气带 role="alert"，出现时会被读屏播报。
 * @param {{
 *   tone?: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger',
 *   className?: string,
 *   children: import('react').ReactNode,
 * }} props
 */
export function Alert({ tone = 'neutral', className, children }) {
  const Icon = tone === 'ok' ? IconCheck : tone === 'warn' || tone === 'danger' ? IconAlert : IconInfo
  return (
    <div
      role={tone === 'danger' ? 'alert' : undefined}
      className={cx('flex items-start gap-2 rounded-control border px-3 py-2 text-small', ALERT_TONE[tone], className)}
    >
      {/* 图标高度取一行文字的行高，和首行文字居中对齐，多行时不跟着下沉。 */}
      <span className="flex h-5 flex-none items-center"><Icon size={14} /></span>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  )
}

/* ---------- Tooltip ---------- */

/**
 * 悬停 / 聚焦提示。气泡用 position: fixed 并渲染到 body，不会被表格或卡片
 * 的 overflow 裁掉。只放补充信息：触屏没有悬停，关键信息不能只放在这里。
 * @param {{ content?: import('react').ReactNode, className?: string, children: import('react').ReactNode }} props
 */
export function Tooltip({ content, className, children }) {
  const [at, setAt] = useState(/** @type {{ x: number, y: number } | null} */ (null))
  const ref = useRef(null)
  if (!content) return children
  const show = () => {
    const r = ref.current?.getBoundingClientRect()
    if (r) setAt({ x: r.left + r.width / 2, y: r.top })
  }
  const hide = () => setAt(null)
  return (
    <span ref={ref} className={className} tabIndex={0} onMouseEnter={show} onMouseLeave={hide} onFocus={show} onBlur={hide}>
      {children}
      {at && createPortal(
        <span
          role="tooltip"
          style={{ position: 'fixed', left: at.x, top: at.y - 6 }}
          className="pointer-events-none z-60 max-w-xs -translate-x-1/2 -translate-y-full rounded-control bg-ink px-2 py-1 text-caption font-normal whitespace-pre-wrap break-words text-surface shadow-popover"
        >
          {content}
        </span>,
        document.body,
      )}
    </span>
  )
}

/* ---------- Toast ---------- */

/**
 * @typedef {'success' | 'error'} ToastKind
 * @typedef {(message: string, kind?: ToastKind) => void} ToastFn
 */

const ToastCtx = createContext(/** @type {ToastFn} */ (() => {}))

/** `const toast = useToast()` → `toast('已保存')` / `toast(err.message, 'error')`。 */
export function useToast() {
  return useContext(ToastCtx)
}

/* 错误信息通常更长、也更需要读完，停留时间比成功提示长。 */
const TOAST_MS = { success: 2500, error: 6000 }

export function ToastProvider({ children }) {
  const [toasts, setToasts] = useState(/** @type {Array<{ id: number, message: string, kind: ToastKind }>} */ ([]))
  const nextId = useRef(0)

  const toast = useCallback((message, kind = 'success') => {
    const id = ++nextId.current
    setToasts(list => [...list, { id, message, kind }])
    setTimeout(() => setToasts(list => list.filter(t => t.id !== id)), TOAST_MS[kind] ?? TOAST_MS.success)
  }, [])

  return (
    <ToastCtx.Provider value={toast}>
      {children}
      {createPortal(
        <div aria-live="polite" className="pointer-events-none fixed inset-x-0 bottom-6 z-70 flex flex-col items-center gap-2 px-4">
          {toasts.map(t => (
            <div
              key={t.id}
              role={t.kind === 'error' ? 'alert' : 'status'}
              className="pointer-events-auto flex max-w-md items-start gap-2 rounded-popover bg-ink px-3 py-2 text-small text-surface shadow-popover animate-toast-in"
            >
              <span className="flex h-5 flex-none items-center">
                {t.kind === 'error' ? <IconAlert size={14} /> : <IconCheck size={14} />}
              </span>
              <span className="min-w-0 break-words">{t.message}</span>
            </div>
          ))}
        </div>,
        document.body,
      )}
    </ToastCtx.Provider>
  )
}
