import { useState, useRef, useEffect } from 'react'

/* ---------- Modal ---------- */
export function Modal({ open, onClose, title, children, wide }) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/35 pt-16 px-4 overflow-y-auto" onClick={onClose}>
      <div className={`bg-white rounded-lg shadow-xl w-full ${wide ? 'max-w-3xl' : 'max-w-xl'} animate-in`} onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between px-6 pt-5 pb-4 border-b border-gray-100">
          <h3 className="text-sm font-bold">{title}</h3>
          <button onClick={onClose} className="text-gray-400 hover:text-gray-600 text-lg leading-none">&times;</button>
        </div>
        <div className="px-6 py-5">{children}</div>
      </div>
    </div>
  )
}

/* ---------- Confirm ---------- */
export function Confirm({ open, onClose, onConfirm, title, children }) {
  return (
    <Modal open={open} onClose={onClose} title={title || '确认操作'}>
      <div className="text-sm text-gray-600 mb-5">{children}</div>
      <div className="flex gap-3 justify-end">
        <button onClick={onClose} className="btn-secondary">取消</button>
        <button onClick={onConfirm} className="btn-danger">确认</button>
      </div>
    </Modal>
  )
}

/* ---------- Badge ---------- */
const badgeColors = {
  green: 'bg-green-50 text-green-700',
  amber: 'bg-amber-50 text-amber-700',
  red: 'bg-red-50 text-red-700',
  gray: 'bg-gray-100 text-gray-500',
  blue: 'bg-blue-50 text-blue-700',
  violet: 'bg-violet-50 text-violet-700',
}
export function Badge({ color = 'gray', children }) {
  return <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-semibold ${badgeColors[color] || badgeColors.gray}`}>{children}</span>
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
    <div className="py-10 text-center text-gray-400">
      <div className="w-11 h-11 rounded-xl bg-gray-100 mx-auto mb-3 flex items-center justify-center">
        <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M12 8v4M12 16h.01"/></svg>
      </div>
      {title && <h4 className="text-sm font-semibold text-gray-500 mb-1">{title}</h4>}
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
    navigator.clipboard.writeText(text).then(() => {
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
        className="text-[11px] px-2 py-0.5 rounded border border-gray-200 bg-white text-gray-500 hover:border-blue-500 hover:text-blue-600 disabled:opacity-50">
        {state === 'loading' ? <Spinner className="w-3 h-3" /> : '测试'}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-700 font-semibold">{result}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-600">{result}</span>}
      {state === 'loading' && <span className="text-[11px] text-gray-400">测试中...</span>}
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
        setState(d.ok ? 'ok' : 'fail')
        setResult(parts.join(' + ') + ' = ' + d.latency_ms + 'ms')
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
        className="text-[11px] px-2 py-0.5 rounded border border-gray-200 bg-white text-gray-500 hover:border-blue-500 hover:text-blue-600 disabled:opacity-50">
        {state === 'loading' ? <Spinner className="w-3 h-3" /> : '测试'}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-700 font-semibold">{result}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-600">{result}</span>}
      {state === 'loading' && <span className="text-[11px] text-gray-400">测试中...</span>}
    </span>
  )
}
