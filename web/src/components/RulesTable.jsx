import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ProtoBadge, SensText, CopyText, Tooltip, ExitKindBadge, Spinner } from './ui'
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

export function RulesTable({ rules, nodeMap, blurred, variant = 'my', onDelete, onEdit, onCopy, onRowClick }) {
  const isAdmin = variant === 'admin'
  const [sort, setSort] = useState({ col: null, dir: null })
  const [copyFmt, setCopyFmt] = useState(() => localStorage.getItem('nf-copy-fmt') || 'uri')
  const toggleCopyFmt = () => {
    setCopyFmt(f => {
      const next = f === 'uri' ? 'yaml' : 'uri'
      localStorage.setItem('nf-copy-fmt', next)
      return next
    })
  }

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

  return (
    <table className="tbl">
      <thead>
        <tr>
          {isAdmin && <th className="w-12">ID</th>}
          <th>名称</th>
          <th className="cursor-pointer select-none" onClick={() => cycleSort('node')}>
            <span className="inline-flex items-center">节点<SortArrow dir={sort.col === 'node' ? sort.dir : null} /></span>
          </th>
          <th>协议</th>
          <th>入口</th>
          <th>
            <span className="inline-flex items-center gap-1.5">出口
              <button type="button" onClick={toggleCopyFmt}
                className="text-[10px] font-mono px-1.5 py-0.5 rounded border border-line bg-surface text-ink-mut hover:text-ink hover:border-ink-mut transition-colors"
                title="切换复制代理连接的格式">{copyFmt.toUpperCase()}</button>
            </span>
          </th>
          {isAdmin && (
            <th className="cursor-pointer select-none" onClick={() => cycleSort('owner')}>
              <span className="inline-flex items-center">所有者<SortArrow dir={sort.col === 'owner' ? sort.dir : null} /></span>
            </th>
          )}
          {!isAdmin && (
            <th className="text-right cursor-pointer select-none" onClick={() => cycleSort('traffic')}>
              <span className="inline-flex items-center justify-end">流量<SortArrow dir={sort.col === 'traffic' ? sort.dir : null} /></span>
            </th>
          )}
          <th className="text-right">操作</th>
        </tr>
      </thead>
      <tbody>
        {sorted.map(r => {
          const node = nodeMap[r.node_id]
          return (
            <tr key={r.id}
              className={isAdmin ? 'cursor-pointer' : ''}
              onClick={isAdmin && onRowClick ? () => onRowClick(r) : undefined}>
              {isAdmin && <td className="font-mono text-xs text-ink-mut">#{r.id}</td>}
              <td className="font-semibold">
                {r.comment
                  ? <Tooltip content={r.comment} className="border-b border-dotted border-ink-mut cursor-help">{r.name}</Tooltip>
                  : r.name}
              </td>
              <td><span className="font-mono text-ink-soft">{node?.name || `#${r.node_id}`}</span></td>
              <td><ProtoBadge proto={r.proto} /></td>
              <td className="font-mono text-xs" onClick={e => e.stopPropagation()}>
                {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
              </td>
              <td className="font-mono text-xs text-ink-soft" onClick={e => e.stopPropagation()}>
                <div className="flex items-center gap-1.5 flex-wrap">
                  <ExitKindBadge kind={r.exit_kind} />
                  {/* On the user list, a landing exit shows the node remark
                      instead of its real address. Admin keeps the address. */}
                  {!isAdmin && r.exit_kind === 'landing' && r.landing_name
                    ? <span className="font-sans">{r.landing_name}</span>
                    : <SensText blurred={blurred}>{exitOf(r) || '--'}</SensText>}
                  {r.relay_uri && (() => {
                    const yaml = copyFmt === 'yaml' ? uriToClashYaml(r.relay_uri) : null
                    const text = yaml || r.relay_uri
                    const label = yaml ? '复制YAML' : '复制代理URI'
                    return <CopyText text={text}><span className="text-blue-600 font-sans">{label}</span></CopyText>
                  })()}
                </div>
              </td>
              {isAdmin && <td className="text-ink-soft">{r.owner_name || '--'}</td>}
              {!isAdmin && <td className="text-right font-mono text-xs text-ink-mut">{fmtBytes(r.total_bytes)}</td>}
              <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                <div className="flex gap-2 justify-end">
                  <ProbeIconButton ruleId={r.id} />
                  {isAdmin && <Link to={`/rules/${r.id}`} title="详情" className="icon-btn"><IconEye /></Link>}
                  {onEdit && <button onClick={() => onEdit(r)} title="编辑" className="icon-btn"><IconPencil /></button>}
                  {onCopy && <button onClick={() => onCopy(r)} title="复制" className="icon-btn"><IconCopy /></button>}
                  <button onClick={() => onDelete(r)} title="删除" className="icon-btn-danger"><IconTrash /></button>
                </div>
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

function ProbeIconButton({ ruleId }) {
  const [state, setState] = useState('idle')
  const [result, setResult] = useState('')
  const probe = () => {
    setState('loading')
    fetch(`/api/probe-chain?rule_id=${ruleId}`).then(r => r.json()).then(d => {
      if (d.hops?.length) {
        const parts = d.hops.map(h => h.error ? 'x' : h.latency_ms + 'ms')
        setState(d.ok ? 'ok' : 'fail')
        setResult(parts.join('+') + '=' + d.latency_ms + 'ms')
      } else if (d.ok) { setState('ok'); setResult(d.latency_ms + 'ms') }
      else { setState('fail'); setResult(d.error || '不通') }
    }).catch(() => { setState('fail'); setResult('失败') })
  }
  const title = state === 'ok' ? result : state === 'fail' ? result : '测试连通性'
  return (
    <button onClick={probe} disabled={state === 'loading'} title={title}
      className={`icon-btn ${state === 'ok' ? '!text-green-500 !border-green-500/30' : state === 'fail' ? '!text-red-400 !border-red-500/30' : ''}`}>
      {state === 'loading' ? <Spinner className="w-4 h-4" /> : <IconPulse />}
    </button>
  )
}

const I = (d) => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">{d}</svg>
function IconPulse() { return I(<path d="M22 12h-4l-3 9L9 3l-3 9H2" />) }
function IconEye() { return I(<><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" /><circle cx="12" cy="12" r="3" /></>) }
function IconPencil() { return I(<><path d="M12 20h9" /><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z" /></>) }
function IconCopy() { return I(<><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" /></>) }
function IconTrash() { return I(<><path d="M3 6h18" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" /><path d="M10 11v6" /><path d="M14 11v6" /></>) }
