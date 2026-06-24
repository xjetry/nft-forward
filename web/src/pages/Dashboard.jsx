import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
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
      <div className="flex items-center justify-between mb-[22px]">
        <h1 className="m-0 text-2xl font-bold text-ink">概览</h1>
        <div className="inline-flex items-center gap-2 px-3.5 py-[7px] rounded-full text-[13px] font-semibold text-green-700 dark:text-green-400 bg-green-500/10 border border-green-500/[.28]">
          <span className="w-[7px] h-[7px] rounded-full bg-green-500 shadow-[0_0_0_3px_rgba(34,197,94,0.2)]" />{onlineCount} 节点在线
        </div>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-[22px]">
        <StatCard label="活跃转发" value={rules.length} sub="正在转发的规则" />
        <StatCard label="在线节点" value={onlineCount} unit={` /${nodes.length}`}
          sub={offline.length ? `${offline.slice(0, 2).join('、')}${offline.length > 2 ? ` 等 ${offline.length} 个` : ''} 离线` : '全部在线'} accent />
        <StatCard label="总流量" value={fmtBytes(totalBytes)} sub="累计上下行" />
        <StatCard label="用户" value={users.length} sub="系统用户数" />
      </div>

      {/* Node status */}
      <div className="card">
        <div className="card-header justify-between"><h3 className="text-[15px] font-bold">节点状态</h3><span className="text-[12.5px] text-ink-mut">{nodes.length} 个节点</span></div>
        {nodes.length ? (
          <table className="tbl">
            <thead><tr><th>节点名</th><th>地址</th><th>类型</th><th>规则</th><th>状态</th><th>心跳</th></tr></thead>
            <tbody>
              {nodes.map(n => (
                <tr key={n.id}>
                  <td><Link to={`/nodes/${n.id}`} className="font-semibold text-blue-600 hover:underline">{n.name}</Link></td>
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
    <div className="card stat-card">
      <div className="text-[13px] text-ink-soft font-medium">{label}</div>
      <div className="mt-3.5 flex items-baseline gap-0.5">
        <span className={`text-[38px] font-bold leading-none tracking-tight ${accent ? 'text-green-600 dark:text-green-400' : 'text-ink'}`}>{value}</span>
        {unit && <span className="text-[18px] font-semibold text-ink-mut">{unit}</span>}
      </div>
      <div className="stat-sub text-[12.5px] text-ink-mut truncate">{sub || ' '}</div>
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
