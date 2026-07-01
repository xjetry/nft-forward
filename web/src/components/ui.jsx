import { useState, useRef, useEffect, useCallback, createContext, useContext } from 'react'
import { copyToClipboard } from '../lib/clipboard'

/* ---------- Modal ---------- */
export function Modal({ open, onClose, title, children, wide }) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-[2px] px-4 overflow-y-auto" onClick={onClose}>
      <div className={`bg-surface border border-line rounded-2xl shadow-[0_24px_70px_-20px_rgba(0,0,0,0.7)] w-full ${wide ? 'max-w-3xl' : 'max-w-xl'} animate-in`} onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between px-[26px] py-5 border-b border-line-soft">
          <h3 className="text-[17px] font-bold text-ink">{title}</h3>
          <button onClick={onClose} className="text-ink-mut hover:text-ink text-lg leading-none">&times;</button>
        </div>
        <div className="px-[26px] py-[26px]">{children}</div>
      </div>
    </div>
  )
}

/* ---------- Confirm ---------- */
export function Confirm({ open, onClose, onConfirm, title, children }) {
  return (
    <Modal open={open} onClose={onClose} title={title || '确认操作'}>
      <div className="text-sm text-ink-soft mb-5">{children}</div>
      <div className="flex gap-3 justify-end">
        <button onClick={onClose} className="btn-secondary">取消</button>
        <button onClick={onConfirm} className="btn-danger">确认</button>
      </div>
    </Modal>
  )
}

/* ---------- Badge ---------- */
const badgeColors = {
  green: 'bg-green-500/[.12] text-green-700 dark:text-green-400 border-green-500/30',
  amber: 'bg-amber-500/[.12] text-amber-700 dark:text-amber-400 border-amber-500/30',
  red: 'bg-red-500/[.12] text-red-700 dark:text-red-400 border-red-500/30',
  gray: 'bg-raised text-ink-soft border-line',
  blue: 'bg-blue-500/[.12] text-blue-700 dark:text-blue-400 border-blue-500/30',
  violet: 'bg-violet-500/[.12] text-violet-700 dark:text-violet-400 border-violet-500/30',
}
export function Badge({ color = 'gray', children }) {
  return <span className={`inline-flex items-center gap-1.5 px-[11px] py-1 rounded-full text-[12px] font-semibold border ${badgeColors[color] || badgeColors.gray}`}>{children}</span>
}

/* ---------- NodeTypeBadge ---------- */
// Single = a solid node dot; composite = interlocking chain links (a multi-hop
// chain); self = the panel host itself.
const nodeTypeIcon = {
  single: <span className="w-1.5 h-1.5 rounded-full bg-green-500 flex-none" />,
  composite: (
    <svg className="w-[13px] h-[13px] flex-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M9 17H7A5 5 0 0 1 7 7h2" /><path d="M15 7h2a5 5 0 0 1 0 10h-2" /><path d="M8 12h8" />
    </svg>
  ),
  self: (
    <svg className="w-[13px] h-[13px] flex-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="m3 9 9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" /><path d="M9 22V12h6v10" />
    </svg>
  ),
}
export function NodeTypeBadge({ type }) {
  if (type === 'composite') return <Badge color="violet">{nodeTypeIcon.composite}组合</Badge>
  if (type === 'self') return <Badge color="blue">{nodeTypeIcon.self}自身</Badge>
  return <Badge color="green">{nodeTypeIcon.single}单点</Badge>
}

/* ---------- ExitKindBadge ---------- */
// Distinguishes a landing-node exit (resolved from the user's subscription)
// from a custom host:port exit.
export function ExitKindBadge({ kind }) {
  if (kind === 'landing') return <Badge color="blue">落地</Badge>
  return <Badge color="gray">自定义</Badge>
}

/* ---------- ProtoBadge ---------- */
export function ProtoBadge({ proto }) {
  if (!proto) return null
  const p = proto.toLowerCase()
  if (p === 'tcp') return <span className="tag-tcp">TCP</span>
  if (p === 'udp') return <span className="tag-udp">UDP</span>
  return <span className="tag-tcpudp">TCP+UDP</span>
}

/* ---------- ModeBadge ---------- */
export function ModeBadge({ mode }) {
  if (!mode) return null
  if (mode === 'kernel') return <span className="tag-kernel">kernel</span>
  return <span className="tag-user">userspace</span>
}

/* ---------- Table ---------- */
export function Table({ children }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        {children}
      </table>
    </div>
  )
}

/* ---------- Empty ---------- */
export function Empty({ title, desc, children }) {
  return (
    <div className="py-10 text-center text-ink-mut">
      <div className="w-11 h-11 rounded-xl bg-raised mx-auto mb-3 flex items-center justify-center">
        <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M12 8v4M12 16h.01"/></svg>
      </div>
      {title && <h4 className="text-sm font-semibold text-ink-soft mb-1">{title}</h4>}
      {desc && <p className="text-xs">{desc}</p>}
      {children}
    </div>
  )
}

/* ---------- Loading ---------- */
export function Loading() {
  return (
    <div className="flex items-center justify-center py-20">
      <div className="w-6 h-6 border-2 border-blue-600 border-t-transparent rounded-full animate-spin" />
    </div>
  )
}

/* ---------- Spinner (inline) ---------- */
export function Spinner({ className = 'w-4 h-4' }) {
  return <div className={`${className} border-2 border-current border-t-transparent rounded-full animate-spin`} />
}

/* ---------- CopyText ---------- */
export function CopyText({ text, children }) {
  const [copied, setCopied] = useState(false)
  const copy = (e) => {
    e.stopPropagation()
    copyToClipboard(text).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    })
  }
  return (
    <span className="cursor-pointer relative inline-flex items-center gap-1 group" onClick={copy} title="点击复制">
      {children || text}
      <svg className="w-3.5 h-3.5 opacity-30 group-hover:opacity-70 transition-opacity flex-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
      {copied && (
        <span className="absolute -top-7 left-1/2 -translate-x-1/2 bg-gray-900 text-white text-[11px] px-2 py-0.5 rounded whitespace-nowrap pointer-events-none animate-fadeout">
          已复制
        </span>
      )}
    </span>
  )
}

/* ---------- Tooltip ---------- */
/* Hover/focus bubble for cramped spots like table cells. The bubble is
   position:fixed (placed from the trigger's rect) so the Panel's
   overflow-hidden can't clip it — the same escape hatch Modal relies on; a
   plain absolute bubble would be cut off at the panel edge. bg-ink/text-surface
   invert together with the theme, so it reads as a dark bubble in light mode
   and a light one in dark mode. */
export function Tooltip({ content, children, className = '' }) {
  const [box, setBox] = useState(null)
  const ref = useRef(null)
  if (!content) return children
  const show = () => {
    const r = ref.current?.getBoundingClientRect()
    if (r) setBox({ x: r.left + r.width / 2, y: r.top })
  }
  const hide = () => setBox(null)
  return (
    <span ref={ref} className={className} tabIndex={0}
      onMouseEnter={show} onMouseLeave={hide} onFocus={show} onBlur={hide}>
      {children}
      {box && (
        <span role="tooltip" style={{ position: 'fixed', left: box.x, top: box.y }}
          className="z-50 -translate-x-1/2 -translate-y-full -mt-2 max-w-xs px-2.5 py-1.5 rounded-md text-[12px] font-normal leading-snug text-surface bg-ink shadow-lg whitespace-pre-wrap break-words pointer-events-none">
          {content}
        </span>
      )}
    </span>
  )
}

/* ---------- SensText: blurrable sensitive text ---------- */
export function SensText({ children, blurred }) {
  return (
    <span className={blurred ? 'blur-sm hover:blur-[2px] transition-all select-none' : ''}>
      {children}
    </span>
  )
}

/* ---------- ProbeButton ---------- */
export function ProbeButton({ target, nodeId }) {
  const [state, setState] = useState('idle') // idle | loading | ok | fail
  const [result, setResult] = useState('')
  const probe = () => {
    setState('loading')
    let url = `/api/probe?target=${encodeURIComponent(target)}`
    if (nodeId) url += `&node=${nodeId}`
    fetch(url).then(r => r.json()).then(d => {
      if (d.ok) { setState('ok'); setResult(d.latency_ms + 'ms') }
      else { setState('fail'); setResult('不通') }
    }).catch(() => { setState('fail'); setResult('请求失败') })
  }
  return (
    <span className="inline-flex items-center gap-2">
      <button onClick={probe} disabled={state === 'loading'}
        className="text-[11px] px-2 py-0.5 rounded border border-line bg-surface text-ink-soft hover:border-blue-500 hover:text-blue-600 disabled:opacity-50">
        {state === 'loading' ? <Spinner className="w-3 h-3" /> : '测试'}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-700 font-semibold">{result}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-600">{result}</span>}
      {state === 'loading' && <span className="text-[11px] text-ink-mut">测试中...</span>}
    </span>
  )
}

/* ---------- ProbeChainButton ---------- */
export function ProbeChainButton({ chainId, ruleId }) {
  const [state, setState] = useState('idle')
  const [result, setResult] = useState('')
  const probe = () => {
    setState('loading')
    const param = ruleId ? `rule_id=${ruleId}` : `chain=${chainId}`
    fetch(`/api/probe-chain?${param}`).then(r => r.json()).then(d => {
      if (d.hops && d.hops.length) {
        const parts = d.hops.map(h => h.error ? 'x' : h.latency_ms + 'ms')
        const joined = parts.join(' + ')
        if (d.ok) {
          setState('ok')
          setResult(d.hops.length > 1 ? joined + ' = ' + d.latency_ms + 'ms' : d.latency_ms + 'ms')
        } else {
          setState('fail')
          setResult(joined)
        }
      } else if (d.ok) {
        setState('ok'); setResult(d.latency_ms + 'ms')
      } else {
        setState('fail'); setResult(d.error || '不通')
      }
    }).catch(() => { setState('fail'); setResult('请求失败') })
  }
  return (
    <span className="inline-flex items-center gap-2">
      <button onClick={probe} disabled={state === 'loading'}
        className="text-[11px] px-2 py-0.5 rounded border border-line bg-surface text-ink-soft hover:border-blue-500 hover:text-blue-600 disabled:opacity-50">
        {state === 'loading' ? <Spinner className="w-3 h-3" /> : '测试'}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-700 font-semibold">{result}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-600">{result}</span>}
      {state === 'loading' && <span className="text-[11px] text-ink-mut">测试中...</span>}
    </span>
  )
}

/* ---------- Promise-based confirm dialog ---------- */
// useConfirm() returns confirm(opts) -> Promise<boolean>, so callers can write
// `if (!(await confirm({...}))) return` instead of the native window.confirm.
const ConfirmCtx = createContext(() => Promise.resolve(false))
export function useConfirm() { return useContext(ConfirmCtx) }

export function ConfirmProvider({ children }) {
  const [state, setState] = useState(null)
  const confirm = useCallback((opts = {}) => new Promise(resolve => {
    setState({ resolve, ...opts })
  }), [])
  const finish = (result) => setState(s => { s?.resolve(result); return null })
  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      <Modal open={!!state} onClose={() => finish(false)} title={state?.title || '确认操作'}>
        <div className="text-sm text-ink-soft mb-5 whitespace-pre-line">{state?.message}</div>
        <div className="flex gap-3 justify-end">
          <button onClick={() => finish(false)} className="btn-secondary">{state?.cancelText || '取消'}</button>
          <button onClick={() => finish(true)} className={state?.danger ? 'btn-danger' : 'btn-primary'}>{state?.confirmText || '确认'}</button>
        </div>
      </Modal>
    </ConfirmCtx.Provider>
  )
}

/* ---------- Select: styled dropdown replacing native <select> ---------- */
// options: [{ value, label }]. Single-select (default): value is a scalar,
// onChange receives the chosen value as a string and the menu closes. Multi-
// select (multiple=true): value is an array, onChange receives the next array
// of string values and the menu stays open so several can be picked.
export function Select({ value, onChange, options = [], groups, placeholder = '请选择', disabled, className = '', style, searchable = false, multiple = false, tabs = false }) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [activeTab, setActiveTab] = useState(0)
  const ref = useRef(null)
  useEffect(() => {
    if (!open) { setQuery(''); setActiveTab(0); return }
    const onDoc = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false) }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])
  // Normalize to labelled sections so flat `options` and grouped `groups` share
  // one render path. A null section label renders no header.
  const sections = groups ? groups : [{ label: null, options }]
  const allOptions = sections.flatMap(s => s.options)
  const selectedValues = multiple ? (Array.isArray(value) ? value.map(String) : []) : []
  const isSelected = (o) => multiple ? selectedValues.includes(String(o.value)) : String(o.value) === String(value)
  const selected = !multiple && allOptions.find(o => String(o.value) === String(value))
  const triggerLabel = multiple
    ? (selectedValues.length ? `已选 ${selectedValues.length} 项` : placeholder)
    : (selected ? selected.label : placeholder)
  const hasSelection = multiple ? selectedValues.length > 0 : !!selected
  const q = query.trim().toLowerCase()
  // Tab mode (with groups): show one group at a time behind a tab bar so long
  // node lists don't bury the second group below a scroll. The header per
  // section is dropped (the tab already labels it).
  const useTabs = tabs && sections.length > 1
  const baseSections = useTabs ? [{ label: null, options: (sections[activeTab] || sections[0]).options }] : sections
  const shownSections = baseSections
    .map(s => ({ label: s.label, options: searchable && q ? s.options.filter(o => String(o.label).toLowerCase().includes(q)) : s.options }))
    .filter(s => s.options.length > 0)
  const empty = shownSections.length === 0
  const choose = (o) => {
    const sv = String(o.value)
    if (multiple) {
      onChange(selectedValues.includes(sv) ? selectedValues.filter(x => x !== sv) : [...selectedValues, sv])
    } else {
      onChange(sv); setOpen(false)
    }
  }
  const renderOption = (o) => {
    const sel = isSelected(o)
    return (
      <button key={String(o.value)} type="button"
        onClick={() => choose(o)}
        className={`w-full text-left px-3 py-1.5 text-[13.5px] transition-colors hover:bg-raised flex items-center gap-2 ${sel ? 'text-blue-600 font-semibold' : 'text-ink'}`}>
        {multiple && (
          <span className={`w-3.5 h-3.5 flex-none rounded border flex items-center justify-center ${sel ? 'bg-blue-600 border-blue-600' : 'border-line'}`}>
            {sel && <svg className="w-2.5 h-2.5 text-white" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5"/></svg>}
          </span>
        )}
        <span className="truncate">{o.label}</span>
      </button>
    )
  }
  return (
    <div ref={ref} className={`relative ${className}`} style={style}>
      <button type="button" disabled={disabled} onClick={() => setOpen(o => !o)}
        className="input-field flex items-center justify-between gap-2 text-left disabled:opacity-60 disabled:cursor-not-allowed">
        <span className={`truncate ${hasSelection ? 'text-ink' : 'text-ink-mut'}`}>{triggerLabel}</span>
        <svg className={`w-4 h-4 flex-none text-ink-mut transition-transform ${open ? 'rotate-180' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      {open && (
        <div className="absolute z-50 mt-1.5 w-full bg-surface border border-line rounded-[11px] shadow-[0_20px_50px_-16px_rgba(0,0,0,0.7)] overflow-hidden">
          {useTabs && (
            <div className="flex border-b border-line-soft">
              {sections.map((s, i) => (
                <button key={i} type="button" onClick={() => setActiveTab(i)}
                  className={`flex-1 px-3 py-[13px] text-[14px] font-semibold transition-colors ${i === activeTab ? 'text-blue-500 border-b-2 border-blue-600 -mb-px' : 'text-ink-soft hover:text-ink'}`}>
                  {s.label} <span className="text-ink-mut font-normal">{s.options.length}</span>
                </button>
              ))}
            </div>
          )}
          {searchable && (
            <div className="p-3 border-b border-line-soft">
              <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder="搜索…"
                onKeyDown={e => { if (e.key === 'Enter') e.preventDefault() }}
                className="input-field w-full text-[13px]" style={{ height: 34 }} />
            </div>
          )}
          <div className="max-h-[260px] overflow-auto py-1.5 px-1.5">
            {empty ? (
              <div className="px-3 py-2 text-[13px] text-ink-mut">无匹配</div>
            ) : shownSections.map((s, i) => (
              <div key={i}>
                {s.label && <div className="px-3 pt-1.5 pb-0.5 text-[11px] font-semibold uppercase tracking-wider text-ink-mut">{s.label}</div>}
                {s.options.map(renderOption)}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
