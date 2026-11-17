import { useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cx } from './cx'
import { useField } from './Field'
import { IconCheck, IconChevronDown } from './icons'
import { moveFocusByArrow, useAnchoredPosition, useDismiss } from './popover'

const TRIGGER_SIZE = {
  md: 'h-control px-3 text-body',
  sm: 'h-control-sm px-2.5 text-small',
}

function CheckMark({ on }) {
  return (
    <span className={cx(
      'flex size-4 flex-none items-center justify-center rounded-chip border',
      on ? 'border-accent bg-accent text-on-accent' : 'border-line-strong',
    )}>
      {on && <IconCheck size={12} strokeWidth={2.5} />}
    </span>
  )
}

/**
 * 下拉选择，取代原生 <select>。触发器与 Input 同高、同边框、同焦点样式。
 *
 * options: `[{ value, label, icon?, disabled? }]`，或 `{ heading }` 形式的
 * 不可选分隔行。label 必须是纯字符串（搜索按它过滤），图形标记放 icon。
 *
 * - 单选（默认）：value 是标量，onChange 收到所选值的字符串，选中即关闭。
 * - 多选（multiple）：value 是数组，onChange 收到新的字符串数组，菜单保持
 *   打开以便连续勾选。
 * - groups：`[{ label, options }]` 分组；配合 tabs 时每组一个页签。
 *
 * 不变式：触发器显示的内容必须对应真实的 value。value 非空却不在 options
 * 里时（选项尚未加载、或该项已失效），显示“未知选项”而不是占位文字 ——
 * 占位文字会让表单看起来是空的，而提交出去的其实是那个看不见的旧值。
 *
 * @param {{
 *   value: string | number | Array<string | number> | null | undefined,
 *   onChange: (value: any) => void,
 *   options?: Array<any>,
 *   groups?: Array<{ label: string, options: Array<any> }>,
 *   placeholder?: string,
 *   size?: 'md' | 'sm',
 *   disabled?: boolean,
 *   invalid?: boolean,
 *   searchable?: boolean,
 *   multiple?: boolean,
 *   tabs?: boolean,
 *   selectAll?: boolean,
 *   title?: string,
 *   className?: string,
 * }} props
 */
export function Select({
  value, onChange, options = [], groups, placeholder = '请选择', size = 'md', disabled = false, invalid,
  searchable = false, multiple = false, tabs = false, selectAll = false, title, className,
}) {
  const field = useField()
  const bad = invalid ?? field.invalid
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [activeTab, setActiveTab] = useState(0)
  const triggerRef = useRef(null)
  const menuRef = useRef(null)

  // 关闭时一并清掉搜索词与页签：这两者只在“本次打开”内有意义。
  const close = (refocus = false) => {
    setOpen(false)
    setQuery('')
    setActiveTab(0)
    if (refocus) triggerRef.current?.focus()
  }
  const pos = useAnchoredPosition(triggerRef, open)
  useDismiss(open, [triggerRef, menuRef], () => close(document.activeElement !== document.body && menuRef.current?.contains(document.activeElement)))

  // 平铺的 options 与分组的 groups 归一成“带标题的分区”，共用一条渲染路径；
  // 分区标题为 null 时不渲染标题行。
  const sections = groups || [{ label: null, options }]
  const allOptions = sections.flatMap(s => s.options).filter(o => !o.heading)
  const selectedValues = multiple ? (Array.isArray(value) ? value.map(String) : []) : []
  const isSelected = (o) => (multiple ? selectedValues.includes(String(o.value)) : String(o.value) === String(value))
  const selected = multiple ? null : allOptions.find(o => String(o.value) === String(value))
  const hasValue = multiple ? selectedValues.length > 0 : value != null && value !== ''
  const unknown = !multiple && hasValue && !selected
  const triggerLabel = multiple
    ? (selectedValues.length ? `已选 ${selectedValues.length} 项` : placeholder)
    : selected ? selected.label
      : unknown ? `未知选项（${value}）`
        : placeholder

  const q = query.trim().toLowerCase()
  // 页签模式：一次只显示一组，长列表不会把第二组压到滚动条下面；页签本身
  // 已经标明组名，所以不再渲染分区标题。
  const useTabs = tabs && sections.length > 1
  const baseSections = useTabs ? [{ label: null, options: (sections[activeTab] || sections[0]).options }] : sections
  // 搜索时，分隔行只在它后面还有真实选项时保留；分隔行自身不参与匹配。
  const filterOpts = (opts) => {
    const kept = searchable && q ? opts.filter(o => o.heading || String(o.label).toLowerCase().includes(q)) : opts
    return kept.filter((o, i) => !o.heading || (kept[i + 1] && !kept[i + 1].heading))
  }
  const shownSections = baseSections
    .map(s => ({ label: s.label, options: filterOpts(s.options) }))
    .filter(s => s.options.some(o => !o.heading))
  const empty = shownSections.length === 0
  // 全选只作用于当前可见的选项：搜索后是“全部匹配项”，页签下是“本页签”。
  const shownValues = shownSections.flatMap(s => s.options).filter(o => !o.heading && !o.disabled).map(o => String(o.value))
  const allShownSelected = shownValues.length > 0 && shownValues.every(v => selectedValues.includes(v))
  const toggleAll = () => {
    if (allShownSelected) onChange(selectedValues.filter(v => !shownValues.includes(v)))
    else onChange([...new Set([...selectedValues, ...shownValues])])
  }
  const choose = (o) => {
    const sv = String(o.value)
    if (multiple) {
      onChange(selectedValues.includes(sv) ? selectedValues.filter(x => x !== sv) : [...selectedValues, sv])
    } else {
      onChange(sv)
      close(true)
    }
  }

  const onMenuKeyDown = (e) => {
    if (e.key === 'Tab') { close(true); return }
    if (!menuRef.current) return
    moveFocusByArrow(e, menuRef.current, '[data-option]:not(:disabled)')
  }
  // 没有搜索框时，打开后把焦点放到已选项（没有则第一项），方向键才有起点。
  const focusInitial = (node) => {
    menuRef.current = node
    if (!node || searchable || node.contains(document.activeElement)) return
    const target = node.querySelector('[data-option][aria-selected="true"]:not(:disabled)') || node.querySelector('[data-option]:not(:disabled)')
    target?.focus({ preventScroll: true })
  }

  const renderOption = (o, idx) => {
    if (o.heading) {
      return (
        <div key={`h-${idx}`} className="mt-1 border-t border-line-soft px-2 pb-1 pt-2 text-caption text-ink-mut select-none">
          {o.heading}
        </div>
      )
    }
    const sel = isSelected(o)
    return (
      <button
        key={String(o.value)}
        type="button"
        role="option"
        data-option
        aria-selected={sel}
        disabled={o.disabled}
        onClick={() => choose(o)}
        className={cx(
          'flex min-h-8 w-full items-center gap-2 rounded-chip px-2 text-left text-small',
          'hover:bg-subtle focus-visible:bg-subtle focus-visible:outline-none disabled:opacity-40 disabled:hover:bg-transparent',
          sel && !multiple ? 'font-medium text-accent-ink' : 'text-ink',
        )}
      >
        {multiple && <CheckMark on={sel} />}
        {o.icon}
        <span className="min-w-0 flex-1 truncate">{o.label}</span>
        {sel && !multiple && <IconCheck size={14} />}
      </button>
    )
  }

  return (
    <div className={cx('relative min-w-0', className)}>
      <button
        ref={triggerRef}
        type="button"
        id={field.id}
        title={title}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-invalid={bad || undefined}
        aria-describedby={field.describedBy}
        onClick={() => (open ? close() : setOpen(true))}
        className={cx(
          'flex w-full items-center justify-between gap-2 rounded-control border bg-surface text-left',
          'focus-visible:outline-none focus-visible:ring-2 disabled:bg-subtle disabled:text-ink-mut',
          TRIGGER_SIZE[size],
          bad
            ? 'border-danger focus-visible:ring-danger/20'
            : 'border-line-strong focus-visible:border-accent focus-visible:ring-accent/20',
          open && !bad && 'border-accent ring-2 ring-accent/20',
        )}
      >
        <span className={cx('flex min-w-0 items-center gap-1.5', (multiple ? hasValue : selected) ? 'text-ink' : unknown ? 'text-warn-ink' : 'text-ink-mut')}>
          {selected && selected.icon}
          <span className="truncate">{triggerLabel}</span>
        </span>
        <IconChevronDown className={cx('text-ink-mut', open && 'rotate-180')} />
      </button>

      {open && pos && createPortal(
        <div
          ref={focusInitial}
          role="listbox"
          aria-multiselectable={multiple || undefined}
          onKeyDown={onMenuKeyDown}
          style={{ position: 'fixed', ...pos }}
          className="z-60 flex max-w-[min(32rem,calc(100vw-1rem))] flex-col overflow-hidden rounded-popover border border-line bg-overlay shadow-popover"
        >
          {useTabs && (
            <div className="flex flex-none border-b border-line-soft px-1">
              {sections.map((s, i) => (
                <button
                  key={i}
                  type="button"
                  onClick={() => setActiveTab(i)}
                  className={cx(
                    '-mb-px flex-1 border-b-2 px-3 py-2 text-small font-medium',
                    i === activeTab ? 'border-accent text-accent-ink' : 'border-transparent text-ink-soft hover:text-ink',
                  )}
                >
                  {s.label} <span className="font-normal text-ink-mut">{s.options.filter(o => !o.heading).length}</span>
                </button>
              ))}
            </div>
          )}
          {searchable && (
            <div className="flex-none border-b border-line-soft p-1">
              <input
                autoFocus
                value={query}
                onChange={e => setQuery(e.target.value)}
                placeholder="搜索…"
                aria-label="搜索选项"
                onKeyDown={e => { if (e.key === 'Enter') e.preventDefault() }}
                className="h-control-sm w-full rounded-chip bg-transparent px-2 text-small text-ink outline-none placeholder:text-ink-mut"
              />
            </div>
          )}
          {multiple && selectAll && !empty && (
            <div className="flex-none border-b border-line-soft p-1">
              <button
                type="button"
                data-option
                onClick={toggleAll}
                className="flex min-h-8 w-full items-center gap-2 rounded-chip px-2 text-left text-small font-medium text-ink-soft hover:bg-subtle focus-visible:bg-subtle focus-visible:outline-none"
              >
                <CheckMark on={allShownSelected} />
                全选 <span className="font-normal text-ink-mut">{shownValues.length}</span>
              </button>
            </div>
          )}
          <div className="min-h-0 flex-1 overflow-auto p-1">
            {empty ? (
              <div className="px-2 py-1.5 text-small text-ink-mut">无匹配</div>
            ) : shownSections.map((s, i) => (
              <div key={i}>
                {s.label && <div className="px-2 pb-0.5 pt-1.5 text-caption font-medium text-ink-mut">{s.label}</div>}
                {s.options.map(renderOption)}
              </div>
            ))}
          </div>
        </div>,
        document.body,
      )}
    </div>
  )
}
