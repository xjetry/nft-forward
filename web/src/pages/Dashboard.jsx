import { useState, useEffect } from 'react'
import { api } from '../lib/api'
import { fmtBytes, fmtTime } from '../lib/fmt'
import { Layout, useBlur } from '../components/Layout'
import { Loading, Empty, Badge, SensText, NodeTypeBadge } from '../components/ui'

export default function Dashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const blurred = useBlur()

  useEffect(() => {
    api.get('/dashboard').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>

  const { nodes = [], rules = [], users = [] } = data
  const onlineCount = nodes.filter(n => !n.disabled && n.online === 1).length
  const offline = nodes.filter(n => n.disabled || n.online !== 1).map(n => n.name)
  const totalBytes = rules.reduce((s, r) => s + (r.total_bytes || 0), 0)
  const ruleCount = {}
  rules.forEach(r => { ruleCount[r.node_id] = (ruleCount[r.node_id] || 0) + 1 })

  return (
    <Layout>
      <div className="flex items-center justify-between mb-5">
        <h1 className="m-0 text-lg font-bold tracking-[-0.01em] text-ink">概览</h1>
        <span className="inline-flex items-center gap-1.5 text-[13px] font-semibold text-green-700 bg-green-50 border border-green-200 px-3 py-1 rounded-full">
          <span className="w-1.5 h-1.5 rounded-full bg-green-500" />{onlineCount} 节点在线
        </span>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-5">
        <StatCard label="活跃转发" value={rules.length} />
        <StatCard label="在线节点" value={onlineCount} unit={`/${nodes.length}`}
          sub={offline.length ? `${offline.slice(0, 2).join('、')}${offline.length > 2 ? ` 等 ${offline.length} 个` : ''} 离线` : '全部在线'} accent />
        <StatCard label="总流量" value={fmtBytes(totalBytes)} />
        <StatCard label="用户" value={users.length} />
      </div>

      {/* Node status */}
      <div className="card">
        <div className="card-header"><h3 className="text-sm font-bold">节点状态</h3></div>
        {nodes.length ? (
          <table className="tbl">
            <thead><tr><th>节点名</th><th>地址</th><th>类型</th><th>规则</th><th>状态</th><th>心跳</th></tr></thead>
            <tbody>
              {nodes.map(n => (
                <tr key={n.id}>
                  <td className="font-semibold">{n.name}</td>
                  <td className="font-mono text-xs text-ink-soft"><SensText blurred={blurred}>{n.relay_host || n.address || '--'}</SensText></td>
                  <td><NodeTypeBadge type={n.node_type} /></td>
                  <td className="font-mono text-ink-soft">{ruleCount[n.id] || 0}</td>
                  <td><NodeStatus node={n} /></td>
                  <td className="font-mono text-ink-mut text-xs">{fmtTime(n.last_seen)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="暂无节点" />}
      </div>
    </Layout>
  )
}

function StatCard({ label, value, unit, sub, accent }) {
  return (
    <div className="card p-4">
      <div className="text-xs text-ink-soft font-medium">{label}</div>
      <div className="mt-1 flex items-baseline gap-1">
        <span className={`text-[28px] font-bold tracking-tight ${accent ? 'text-green-600' : 'text-ink'}`}>{value}</span>
        {unit && <span className="text-sm font-semibold text-ink-mut">{unit}</span>}
      </div>
      <div className="text-[12px] text-ink-mut mt-1 font-mono truncate">{sub || ' '}</div>
    </div>
  )
}

// Overview status is simply online/offline (disabled shown distinctly). A
// composite node's online state is resolved server-side from its children, so
// here it follows the same online flag — no sync/error states on the overview.
function NodeStatus({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
}
