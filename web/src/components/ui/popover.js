import { useEffect, useLayoutEffect, useRef, useState } from 'react'

/* 浮层定位。

   浮层（下拉菜单、操作菜单）渲染到 document.body 并用 position: fixed 摆放，
   坐标取自触发器的视口矩形。这样它不属于任何祖先的滚动或裁切上下文：
   放在 overflow-hidden 的卡片里、放在可滚动的弹窗正文里都不会被切掉。

   代价是浮层不会跟着祖先滚动自动移动，所以打开期间要监听滚动与窗口尺寸
   变化并重新计算；监听用捕获阶段，才能收到任意祖先滚动容器的 scroll。 */

const GAP = 4
const VIEWPORT_MARGIN = 8

/**
 * @typedef {{
 *   left?: number, right?: number, top?: number, bottom?: number,
 *   minWidth: number, maxHeight: number,
 * }} PopoverStyle
 */

/**
 * 计算锚定在触发器上的浮层位置；open 为 false 时返回 null。
 *
 * 下方空间不足且上方更宽裕时翻到上方。`align="end"` 让浮层右边缘对齐
 * 触发器右边缘（用于贴在行尾的操作菜单）。
 *
 * @param {{ current: HTMLElement | null }} anchorRef
 * @param {boolean} open
 * @param {{ align?: 'start' | 'end', preferredHeight?: number }} [opts]
 * @returns {PopoverStyle | null}
 */
export function useAnchoredPosition(anchorRef, open, { align = 'start', preferredHeight = 320 } = {}) {
  const [style, setStyle] = useState(/** @type {PopoverStyle | null} */ (null))

  useLayoutEffect(() => {
    if (!open) { setStyle(null); return }
    const update = () => {
      const anchor = anchorRef.current
      if (!anchor) return
      const r = anchor.getBoundingClientRect()
      const below = window.innerHeight - r.bottom - GAP - VIEWPORT_MARGIN
      const above = r.top - GAP - VIEWPORT_MARGIN
      const flip = below < preferredHeight && above > below
      setStyle({
        ...(align === 'end' ? { right: window.innerWidth - r.right } : { left: r.left }),
        ...(flip ? { bottom: window.innerHeight - r.top + GAP } : { top: r.bottom + GAP }),
        minWidth: r.width,
        maxHeight: Math.max(120, Math.min(preferredHeight, flip ? above : below)),
      })
    }
    update()
    window.addEventListener('resize', update)
    window.addEventListener('scroll', update, true)
    return () => {
      window.removeEventListener('resize', update)
      window.removeEventListener('scroll', update, true)
    }
  }, [anchorRef, open, align, preferredHeight])

  return style
}

/**
 * 浮层打开期间：点在触发器与浮层之外、或按 Esc 时调用 onDismiss。
 * 浮层在 portal 里，不是触发器的 DOM 后代，所以两个引用都要判断。
 *
 * @param {boolean} open
 * @param {Array<{ current: HTMLElement | null }>} insideRefs
 * @param {() => void} onDismiss
 */
export function useDismiss(open, insideRefs, onDismiss) {
  // 调用方每次渲染都会传入新的数组与回调；把最新值放进 ref，监听器只在
  // open 变化时重挂，而触发时读到的总是最新的回调。
  const latest = useRef({ insideRefs, onDismiss })
  latest.current = { insideRefs, onDismiss }

  useEffect(() => {
    if (!open) return
    const onPointer = (e) => {
      if (latest.current.insideRefs.some(ref => ref.current?.contains(e.target))) return
      latest.current.onDismiss()
    }
    const onKey = (e) => {
      if (e.key !== 'Escape') return
      // 浮层开在弹窗里时，这一次 Esc 只关浮层：在捕获阶段拦下，弹窗挂在
      // document 冒泡阶段的 Esc 监听就收不到它。
      e.stopPropagation()
      latest.current.onDismiss()
    }
    document.addEventListener('mousedown', onPointer)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('mousedown', onPointer)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])
}

/** 浮层内按上下方向键 / Home / End 在可聚焦的选项之间移动焦点。 */
export function moveFocusByArrow(e, container, selector) {
  const dir = e.key === 'ArrowDown' ? 1 : e.key === 'ArrowUp' ? -1 : 0
  if (!dir && e.key !== 'Home' && e.key !== 'End') return false
  const items = Array.from(container.querySelectorAll(selector))
  if (items.length === 0) return false
  e.preventDefault()
  const at = items.indexOf(document.activeElement)
  const next = e.key === 'Home' ? 0
    : e.key === 'End' ? items.length - 1
      : at < 0 ? (dir > 0 ? 0 : items.length - 1)
        : (at + dir + items.length) % items.length
  items[next].focus()
  return true
}
