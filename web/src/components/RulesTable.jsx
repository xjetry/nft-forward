import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ProtoBadge, SensText, CopyText, Tooltip, ProbeChainButton, ExitKindBadge } from './ui'
import { fmtBytes } from '../lib/fmt'

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
          <th>出口</th>
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
                  {r.relay_uri && (
                    <CopyText text={r.relay_uri}><span className="text-blue-600 font-sans">复制代理URI</span></CopyText>
                  )}
                </div>
              </td>
              {isAdmin && <td className="text-ink-soft">{r.owner_name || '--'}</td>}
              {!isAdmin && <td className="text-right font-mono text-xs text-ink-mut">{fmtBytes(r.total_bytes)}</td>}
              <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                <span className="inline-flex items-center align-middle mr-1.5"><ProbeChainButton ruleId={r.id} /></span>
                {isAdmin && <Link to={`/rules/${r.id}`} className="btn-secondary text-xs mr-1.5">详情</Link>}
                {onEdit && <button onClick={() => onEdit(r)} className="btn-secondary text-xs mr-1.5">编辑</button>}
                {onCopy && <button onClick={() => onCopy(r)} className="btn-secondary text-xs mr-1.5">复制</button>}
                <button onClick={() => onDelete(r)} className="btn-danger-sm text-xs">删除</button>
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}
