import { useState, useRef, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { Badge, ProtoBadge, SensText, CopyText, Tooltip, ExitKindBadge, Spinner } from './ui'
import { useCopyFmt } from './Layout'
import { fmtBytes } from '../lib/fmt'
import { uriToClashYaml } from '../lib/yaml-convert'

/* Shared rule table for both the admin (`/rules`) and user (`/my/rules`) lists.
   variant drives the columns that differ: admin shows id/owner and links to a
   detail page; my shows traffic. Everything else — name, node, proto, entry,
   exit, sorting, alignment — is identical so the two pages stay in lockstep. */

const exitOf = (r) => (r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : '')

/* Geometric triangles render as plain text glyphs; the arrow characters
   (↑↓↕) get emoji-styled on some platforms, which breaks column alignment. */
function SortArrow({ dir }) {
  return (
    <span className="inline-flex flex-col leading-[0.55] text-[9px] ml-1">
      <span className={dir === 'asc' ? 'text-blue-600' : 'text-ink-mut opacity-50'}>▲</span>
      <span className={dir === 'desc' ? 'text-blue-600' : 'text-ink-mut opacity-50'}>▼</span>
    </span>
  )
}

export function RulesTable({ rules, nodeMap, blurred, variant = 'my', onDelete, onEdit, onCopy, onRowClick, probeAllTrigger }) {
  const isAdmin = variant === 'admin'
  const [sort, setSort] = useState({ col: null, dir: null })
  const { copyFmt } = useCopyFmt()

  const cycleSort = (col) => {
    setSort(s => {
      if (s.col !== col) return { col, dir: 'asc' }
      if (s.dir === 'asc') return { col, dir: 'desc' }
      return { col: null, dir: null }
    })
  }

  const sorted = !sort.col ? rules : [...rules].sort((a, b) => {
    if (sort.col === 'traffic') {
      const d = (a.total_bytes || 0) - (b.total_bytes || 0)
      return sort.dir === 'asc' ? d : -d
    }
    const va = sort.col === 'node' ? (nodeMap[a.node_id]?.name || '') : (a.owner_name || '')
    const vb = sort.col === 'node' ? (nodeMap[b.node_id]?.name || '') : (b.owner_name || '')
    const c = va.localeCompare(vb)
    return sort.dir === 'asc' ? c : -c
  })

  return (<>
    {/* Desktop table */}
    <table className="tbl hidden md:table">
      <thead>
        <tr>
          {isAdmin && <th className="w-12">ID</th>}
          <th>名称</th>
          <th className="cursor-pointer select-none" onClick={() => cycleSort('node')}>
            <span className="inline-flex items-center">节点<SortArrow dir={sort.col === 'node' ? sort.dir : null} /></span>
          </th>
          <th>入口 / 出口</th>
          <th>协议</th>
          {isAdmin && (
            <th className="cursor-pointer select-none" onClick={() => cycleSort('owner')}>
              <span className="inline-flex items-center">所有者<SortArrow dir={sort.col === 'owner' ? sort.dir : null} /></span>
            </th>
          )}
          <th>备注</th>
          <th className="text-right cursor-pointer select-none" onClick={() => cycleSort('traffic')}>
            <span className="inline-flex items-center justify-end">流量<SortArrow dir={sort.col === 'traffic' ? sort.dir : null} /></span>
          </th>
          <th className="text-right">操作</th>
        </tr>
      </thead>
      <tbody>
        {sorted.map(r => {
          const node = nodeMap[r.node_id]
          return (
            <tr key={r.id}
              className={onRowClick ? 'cursor-pointer' : ''}
              onClick={onRowClick ? () => onRowClick(r) : undefined}>
              {isAdmin && <td className="font-mono text-xs text-ink-mut">#{r.id}</td>}
              <td className="font-semibold">{r.name}</td>
              <td><span className="font-mono text-ink-soft">{node?.name || `#${r.node_id}`}</span></td>
              <td className="font-mono text-xs !whitespace-normal">
                <div className="inline-block" onClick={e => e.stopPropagation()}>
                  <div className="flex items-center gap-1.5 mb-1">
                    <Badge color="gray">入口</Badge>
                    {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
                  </div>
                  <div className="flex items-center gap-1.5 flex-wrap text-ink-soft">
                    <ExitKindBadge kind={r.exit_kind} protocol={r.landing_protocol} />
                    {(() => {
                      const exitLabel = !isAdmin && r.exit_kind === 'landing' && r.landing_name
                        ? <span className="font-sans">{r.landing_name}</span>
                        : <SensText blurred={blurred}>{exitOf(r) || '--'}</SensText>
                      if (r.relay_uri) {
                        const yaml = copyFmt === 'yaml' ? uriToClashYaml(r.relay_uri) : null
                        return <CopyText text={yaml || r.relay_uri}>{exitLabel}</CopyText>
                      }
                      return exitLabel
                    })()}
                  </div>
                </div>
              </td>
              <td><ProtoBadge proto={r.proto} /></td>
              {isAdmin && <td className="text-ink-soft">{r.owner_name || '--'}</td>}
              <td className="text-xs text-ink-soft">
                {r.comment
                  ? r.comment.length > 8
                    ? <Tooltip content={r.comment}><span className="cursor-help">{r.comment.slice(0, 8)}…</span></Tooltip>
                    : r.comment
                  : <span className="text-ink-mut">-</span>}
              </td>
              <td className="text-right font-mono text-xs text-ink-mut">{fmtBytes(r.total_bytes || 0)}</td>
              <td className="text-right whitespace-nowrap">
                <div className="inline-flex gap-2 justify-end items-center" onClick={e => e.stopPropagation()}>
                  <ProbeIconButton ruleId={r.id} probeAllTrigger={probeAllTrigger} />
                  <MoreMenu items={[
                    onEdit && { label: '编辑', onClick: () => onEdit(r) },
                    onCopy && { label: '复制', onClick: () => onCopy(r) },
                    { label: '删除', onClick: () => onDelete(r), danger: true },
                  ].filter(Boolean)} />
                </div>
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
    {/* Mobile cards */}
    <div className="md:hidden">
      {sorted.map(r => {
        const node = nodeMap[r.node_id]
        return (
          <div key={r.id} className={`mobile-card ${onRowClick ? 'cursor-pointer' : ''}`}
            onClick={onRowClick ? () => onRowClick(r) : undefined}>
            <div className="flex items-center justify-between mb-1">
              <span className="font-semibold text-[14px]">{r.name}</span>
              <div className="flex items-center gap-2">
                <ProbeIconButton ruleId={r.id} probeAllTrigger={probeAllTrigger} />
                <ProtoBadge proto={r.proto} />
              </div>
            </div>
            <div className="flex items-center gap-2 text-xs text-ink-soft mb-1.5 flex-wrap">
              <span className="font-mono">{node?.name || `#${r.node_id}`}</span>
              {isAdmin && r.owner_name && <><span className="text-ink-mut">·</span><span>{r.owner_name}</span></>}
              <span className="text-ink-mut">·</span>
              <span className="font-mono text-ink-mut">{fmtBytes(r.total_bytes || 0)}</span>
            </div>
            <div className="text-xs text-ink-mut font-mono truncate">
              {r.entry ? <SensText blurred={blurred}>{r.entry}</SensText> : '--'}
              <span className="mx-1.5">→</span>
              <span className="text-ink-soft">
                {!isAdmin && r.exit_kind === 'landing' && r.landing_name
                  ? <span className="font-sans">{r.landing_name}</span>
                  : <SensText blurred={blurred}>{exitOf(r) || '--'}</SensText>}
              </span>
            </div>
          </div>
        )
      })}
    </div>
  </>)
}

function ProbeIconButton({ ruleId, probeAllTrigger }) {
  const [state, setState] = useState('idle')
  const [label, setLabel] = useState('')
  const [tip, setTip] = useState('')
  useEffect(() => {
    if (probeAllTrigger) probe()
  }, [probeAllTrigger])
  const probe = () => {
    setState('loading')
    fetch(`/api/probe-chain?rule_id=${ruleId}`).then(r => r.json()).then(d => {
      if (d.hops?.length) {
        const parts = d.hops.map(h => h.error ? 'x' : h.latency_ms + 'ms')
        const joined = parts.join(' → ')
        if (d.ok) {
          setState('ok')
          setLabel(d.hops.length > 1 ? joined + ' = ' + d.latency_ms + 'ms' : d.latency_ms + 'ms')
          setTip(joined + ' = ' + d.latency_ms + 'ms')
        } else {
          setState('fail')
          setLabel(joined)
          setTip(joined)
        }
      } else if (d.ok) { setState('ok'); setLabel(d.latency_ms + 'ms'); setTip('') }
      else { setState('fail'); setLabel(d.error || '不通'); setTip('') }
    }).catch(() => { setState('fail'); setLabel('失败'); setTip('') })
  }
  return (
    <span className="inline-flex items-center gap-1">
      <button onClick={probe} disabled={state === 'loading'} title={tip || label || '测试连通性'}
        className={`icon-btn ${state === 'ok' ? '!text-green-500 !border-green-500/30' : state === 'fail' ? '!text-red-400 !border-red-500/30' : ''}`}>
        {state === 'loading' ? <Spinner className="w-4 h-4" /> : <IconPulse />}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-600 font-mono font-semibold">{label}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-500 font-mono">{label}</span>}
    </span>
  )
}

function MoreMenu({ items }) {
  const [open, setOpen] = useState(false)
  const [dropUp, setDropUp] = useState(false)
  const ref = useRef(null)
  const menuRef = useRef(null)
  useEffect(() => {
    if (!open) return
    const h = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false) }
    document.addEventListener('mousedown', h)
    return () => document.removeEventListener('mousedown', h)
  }, [open])
  useEffect(() => {
    if (!open || !menuRef.current) return
    const rect = menuRef.current.getBoundingClientRect()
    let maxBottom = window.innerHeight - 8
    for (let el = menuRef.current.parentElement; el; el = el.parentElement) {
      const s = getComputedStyle(el)
      if (s.overflow === 'hidden' || s.overflow === 'auto' || s.overflowY === 'hidden' || s.overflowY === 'auto') {
        maxBottom = Math.min(maxBottom, el.getBoundingClientRect().bottom - 8)
        break
      }
    }
    if (rect.bottom > maxBottom) setDropUp(true)
  }, [open])
  const toggle = () => { setDropUp(false); setOpen(o => !o) }
  const pos = dropUp ? 'bottom-[calc(100%+4px)]' : 'top-[calc(100%+4px)]'
  return (
    <div ref={ref} className="relative">
      <button onClick={toggle} className="icon-btn">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="5" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="12" cy="19" r="1"/></svg>
      </button>
      {open && (
        <div ref={menuRef} className={`absolute right-0 ${pos} z-50 min-w-[100px] bg-surface border border-line rounded-lg shadow-[0_8px_30px_-8px_rgba(0,0,0,0.5)] py-1`}>
          {items.map((item, i) => item.href ? (
            <Link key={i} to={item.href} className="block px-3.5 py-2 text-[13px] text-ink hover:bg-raised transition-colors no-underline">{item.label}</Link>
          ) : (
            <button key={i} onClick={() => { setOpen(false); item.onClick() }}
              className={`block w-full text-left px-3.5 py-2 text-[13px] transition-colors bg-transparent border-0 cursor-pointer ${item.danger ? 'text-red-600 hover:bg-red-50' : 'text-ink hover:bg-raised'}`}>{item.label}</button>
          ))}
        </div>
      )}
    </div>
  )
}

const I = (d) => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">{d}</svg>
function IconPulse() { return I(<path d="M22 12h-4l-3 9L9 3l-3 9H2" />) }
