import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ProtoBadge, SensText, CopyText } from './ui'
import { fmtBytes } from '../lib/fmt'

/* Shared rule table for both the admin (`/rules`) and user (`/my/rules`) lists.
   variant drives the columns that differ: admin shows id/owner and links to a
   detail page; my shows traffic. Everything else — name, node, proto, entry,
   exit, sorting, alignment — is identical so the two pages stay in lockstep. */

const exitOf = (r) => (r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : '')

function indicator(sort, col) {
  if (sort.col !== col) return ' ↕'
  return sort.dir === 'asc' ? ' ↑' : ' ↓'
}

export function RulesTable({ rules, nodeMap, blurred, variant = 'my', onDelete, onRowClick }) {
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
          <th className="cursor-pointer select-none" onClick={() => cycleSort('node')}>节点{indicator(sort, 'node')}</th>
          <th>协议</th>
          <th>入口</th>
          <th>出口</th>
          {isAdmin && <th className="cursor-pointer select-none" onClick={() => cycleSort('owner')}>所有者{indicator(sort, 'owner')}</th>}
          {!isAdmin && <th className="text-right cursor-pointer select-none" onClick={() => cycleSort('traffic')}>流量{indicator(sort, 'traffic')}</th>}
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
              {isAdmin && <td className="font-mono text-xs text-gray-400">#{r.id}</td>}
              <td className="font-semibold">{r.name}</td>
              <td><span className="font-mono text-gray-600">{node?.name || `#${r.node_id}`}</span></td>
              <td><ProtoBadge proto={r.proto} /></td>
              <td className="font-mono text-xs" onClick={e => e.stopPropagation()}>
                {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
              </td>
              <td className="font-mono text-xs text-gray-500">
                <SensText blurred={blurred}>{exitOf(r) || '--'}</SensText>
              </td>
              {isAdmin && <td className="text-gray-500">{r.owner_name || '--'}</td>}
              {!isAdmin && <td className="text-right font-mono text-xs text-gray-400">{fmtBytes(r.total_bytes)}</td>}
              <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                {isAdmin && <Link to={`/rules/${r.id}`} className="btn-secondary text-xs mr-1.5">详情</Link>}
                <button onClick={() => onDelete(r)} className="btn-danger-sm text-xs">删除</button>
              </td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}
