import { createContext, useCallback, useContext, useEffect, useId, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cx } from './cx'
import { Button, IconButton } from './Button'
import { IconX } from './icons'

const FOCUSABLE = 'a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])'

const WIDTH = {
  sm: 'max-w-md',
  md: 'max-w-xl',
  lg: 'max-w-3xl',
}

/* 当前打开的弹窗，按打开顺序排列；末尾是最上层。见 Modal 里的 Esc 处理。 */
const openStack = []

/**
 * 弹窗。
 *
 * 不变式：Modal 没有 `open` 属性 —— 挂载即打开，卸载即关闭。调用方用条件
 * 渲染控制它（`{dialog && <FooModal onClose=… />}`），这样弹窗里的表单状态
 * 随每次打开重新初始化，不存在“上次没提交的输入、上次没复位的 loading
 * 留到下一次打开”这类残留状态。
 *
 * 行为：
 * - Esc、点遮罩、点右上角关闭都调用 onClose。
 * - `persistent`：Esc 与点遮罩不再关闭，只能通过右上角或底部按钮显式关闭。
 *   用于内容关掉就再也拿不回来的弹窗（一次性显示的密码 / token）。
 * - `busy`：三条关闭路径全部禁用，提交进行中的弹窗不会被意外关掉。
 * - 打开时焦点移入弹窗，Tab 在弹窗内循环，关闭时焦点回到打开前的元素。
 * - 正文超高时只滚动正文，标题栏与底部操作栏固定。
 *
 * @param {{
 *   title: import('react').ReactNode,
 *   onClose: () => void,
 *   size?: 'sm' | 'md' | 'lg',
 *   persistent?: boolean,
 *   busy?: boolean,
 *   footer?: import('react').ReactNode,
 *   children: import('react').ReactNode,
 * }} props `footer` 通常是 <ModalActions>；表单弹窗把整个 Modal 包在
 * <form> 里，或给提交按钮加 form 属性，让底栏按钮能触发提交。
 */
export function Modal({ title, onClose, size = 'md', persistent = false, busy = false, footer, children }) {
  const titleId = useId()
  const panelRef = useRef(null)
  // 遮罩关闭要求按下与抬起都落在遮罩上：在输入框里按下、拖到遮罩上松开
  // （选中文字时很常见）不应关闭弹窗。
  const pressedOnScrim = useRef(false)
  // 隐式关闭（Esc、点遮罩）是否被允许。
  const implicitClose = !persistent && !busy
  const latest = useRef({ onClose, implicitClose })
  latest.current = { onClose, implicitClose }

  useEffect(() => {
    const previous = document.activeElement
    const panel = panelRef.current
    const first = panel?.querySelector('[data-autofocus]') || panel?.querySelector(FOCUSABLE)
    ;(first || panel)?.focus({ preventScroll: true })

    // 弹窗可以叠放（表单弹窗之上再弹确认框）。每个弹窗都在 document 上听
    // Esc，所以用一个打开顺序栈判定归属：只有栈顶的弹窗响应，一次 Esc
    // 只关最上面那一层。
    const token = {}
    openStack.push(token)
    const onKey = (e) => {
      if (e.key !== 'Escape' || !latest.current.implicitClose) return
      if (openStack[openStack.length - 1] !== token) return
      latest.current.onClose()
    }
    // 冒泡阶段监听：弹窗内的浮层（Select 菜单）会在捕获阶段拦下属于它的
    // Esc，那一次按键就不会再到这里。
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      openStack.splice(openStack.indexOf(token), 1)
      if (previous instanceof HTMLElement) previous.focus({ preventScroll: true })
    }
  }, [])

  const trapTab = (e) => {
    if (e.key !== 'Tab') return
    const panel = panelRef.current
    if (!panel) return
    const items = Array.from(panel.querySelectorAll(FOCUSABLE)).filter(el => el.offsetParent !== null)
    if (items.length === 0) { e.preventDefault(); return }
    const first = items[0]
    const last = items[items.length - 1]
    // 只在首尾两端接管：焦点在弹窗内部时让浏览器按 DOM 顺序走。焦点若在
    // 弹窗之外的 portal 里（下拉菜单），由那个浮层自己处理 Tab。
    if (!panel.contains(document.activeElement)) return
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus() }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus() }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim p-4"
      onMouseDown={e => { pressedOnScrim.current = e.target === e.currentTarget }}
      onMouseUp={e => {
        if (pressedOnScrim.current && e.target === e.currentTarget && implicitClose) onClose()
        pressedOnScrim.current = false
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onKeyDown={trapTab}
        className={cx(
          'flex max-h-full w-full flex-col rounded-modal border border-line bg-overlay shadow-modal outline-none',
          WIDTH[size],
        )}
      >
        <div className="flex flex-none items-center justify-between gap-3 border-b border-line-soft py-2.5 pl-5 pr-3">
          <h2 id={titleId} className="min-w-0 truncate text-title text-ink">{title}</h2>
          <IconButton label="关闭" onClick={onClose} disabled={busy}><IconX /></IconButton>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-5">{children}</div>
        {footer && <div className="flex-none border-t border-line-soft px-5 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  )
}

/**
 * 弹窗底部操作行：右对齐，次要动作在左、主动作在最右。全站弹窗统一用它，
 * 主按钮的位置因此是固定的。
 * @param {{ note?: import('react').ReactNode, children: import('react').ReactNode }} props
 */
export function ModalActions({ note, children }) {
  return (
    <div className="flex items-center gap-2">
      {note && <span className="min-w-0 flex-1 text-caption text-ink-mut">{note}</span>}
      <div className="ml-auto flex flex-none items-center gap-2">{children}</div>
    </div>
  )
}

/* ---------- 确认框 ---------- */

/**
 * @typedef {{
 *   title?: string,
 *   message: import('react').ReactNode,
 *   confirmText?: string,
 *   cancelText?: string,
 *   danger?: boolean,
 * }} ConfirmOptions
 */

const ConfirmCtx = createContext(/** @type {(opts: ConfirmOptions) => Promise<boolean>} */ (() => Promise.resolve(false)))

/**
 * `const confirm = useConfirm()` → `if (!(await confirm({ … }))) return`。
 * 只用于不可撤销或影响面大的动作；可撤销的操作直接执行，不要弹确认。
 */
export function useConfirm() {
  return useContext(ConfirmCtx)
}

export function ConfirmProvider({ children }) {
  const [pending, setPending] = useState(/** @type {(ConfirmOptions & { resolve: (ok: boolean) => void }) | null} */ (null))
  const confirm = useCallback((opts) => new Promise(resolve => {
    setPending({ ...opts, resolve })
  }), [])
  const settle = (ok) => {
    pending?.resolve(ok)
    setPending(null)
  }
  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      {pending && (
        <Modal
          size="sm"
          title={pending.title || '确认操作'}
          onClose={() => settle(false)}
          footer={
            // 初始焦点：普通确认落在“确认”上，回车即可继续；破坏性确认落在
            // “取消”上，误按回车不会直接执行删除。
            <ModalActions>
              <Button data-autofocus={pending.danger || undefined} onClick={() => settle(false)}>{pending.cancelText || '取消'}</Button>
              <Button variant="primary" danger={pending.danger} data-autofocus={pending.danger ? undefined : true} onClick={() => settle(true)}>
                {pending.confirmText || '确认'}
              </Button>
            </ModalActions>
          }
        >
          <div className="max-w-prose whitespace-pre-line text-body text-ink-soft">{pending.message}</div>
        </Modal>
      )}
    </ConfirmCtx.Provider>
  )
}
