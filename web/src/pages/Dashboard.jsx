import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import { fmtBytes, fmtTime, nullStr } from '../lib/fmt'
import { Layout, useBlur } from '../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, SensText, NodeTypeBadge } from '../components/ui'

export default function Dashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const blurred = useBlur()

  useEffect(() => {
    api.get('/dashboard').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>

  const { nodes = [], rules = [], users = [], node_by_id = {} } = data
  const onlineCount = nodes.filter(n => !n.disabled && n.online === 1).length

  return (
    <Layout>
      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-3 gap-4 mb-5">
        <StatCard icon="blue" label="节点" value={nodes.length} meta={<><span className="inline-block w-1.5 h-1.5 rounded-full bg-green-500 mr-1" />{onlineCount} 在线</>} />
        <StatCard icon="green" label="规则" value={rules.length} />
        <StatCard icon="amber" label="用户" value={users.length} />
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5 items-start">
        {/* Node status */}
        <div className="card">
          <div className="card-header">
            <h3 className="text-sm font-bold">节点状态</h3>
            <Link to="/nodes" className="btn-secondary text-xs">管理节点</Link>
          </div>
          {nodes.length ? (
            <table className="tbl">
              <thead><tr><th>名称</th><th>类型</th><th>最近同步</th><th>状态</th></tr></thead>
              <tbody>
                {nodes.map(n => (
                  <tr key={n.id}>
                    <td>
                      <span className="inline-flex items-center gap-2 font-semibold">
                        <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500 shadow-[0_0_0_3px_rgba(34,197,94,0.18)]' : 'bg-gray-400 shadow-[0_0_0_3px_rgba(154,163,176,0.16)]'}`} />
                        <Link to={`/nodes/${n.id}`} className="text-blue-600 font-semibold hover:underline">{n.name}</Link>
                      </span>
                    </td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    <td className="font-mono text-gray-500 text-xs">{fmtTime(n.last_apply_at?.Int64)}</td>
                    <td><NodeStatus node={n} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <Empty title="暂无节点" />}
        </div>

        {/* Recent rules */}
        <div className="card">
          <div className="card-header">
            <h3 className="text-sm font-bold">最近规则</h3>
            <Link to="/rules" className="btn-secondary text-xs ml-auto">全部规则</Link>
          </div>
          {rules.length ? (
            <table className="tbl">
              <thead><tr><th>名称</th><th>节点</th><th>协议</th><th>入口</th><th>出口</th><th className="text-right">流量</th></tr></thead>
              <tbody>
                {rules.slice(0, 10).map(r => {
                  const node = node_by_id?.[r.node_id]
                  return (
                    <tr key={r.id}>
                      <td className="font-semibold">
                        <Link to={`/rules/${r.id}`} className="text-blue-600 hover:underline">{r.name}</Link>
                      </td>
                      <td className="font-mono text-gray-500 text-xs">{node?.name || `#${r.node_id}`}</td>
                      <td><ProtoBadge proto={r.proto} /></td>
                      <td className="font-mono text-xs"><SensText blurred={blurred}>{r.entry || '--'}</SensText></td>
                      <td className="font-mono text-xs"><SensText blurred={blurred}>{r.exit || '--'}</SensText></td>
                      <td className="text-right font-mono text-xs text-gray-400">{fmtBytes(r.total_bytes)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          ) : <Empty title="暂无规则" />}
        </div>
      </div>
    </Layout>
  )
}

function StatCard({ icon, label, value, meta }) {
  const colors = {
    blue: 'bg-blue-50 text-blue-600',
    green: 'bg-green-50 text-green-700',
    violet: 'bg-violet-50 text-violet-700',
    amber: 'bg-amber-50 text-amber-700',
  }
  return (
    <div className="card p-4 relative overflow-hidden">
      <div className={`w-[34px] h-[34px] rounded-[9px] grid place-items-center mb-3.5 ${colors[icon]}`}>
        {icon === 'blue' && <svg className="w-[18px] h-[18px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/></svg>}
        {icon === 'green' && <svg className="w-[18px] h-[18px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 12h12"/><path d="M13 7l5 5-5 5"/><path d="M20 5v14"/></svg>}
        {icon === 'amber' && <svg className="w-[18px] h-[18px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="9" cy="8" r="3.2"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/></svg>}
      </div>
      <div className="text-xs text-gray-500 font-medium">{label}</div>
      <div className="text-[28px] font-bold tracking-tight mt-0.5">{value}</div>
      {meta && <div className="text-[12px] text-gray-400 mt-1.5 flex items-center gap-1.5 font-mono">{meta}</div>}
    </div>
  )
}

function NodeStatus({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red">错误</Badge>
  if (node.last_apply_at?.Valid) return <Badge color="green">在线</Badge>
  return <Badge color="amber">未同步</Badge>
}
