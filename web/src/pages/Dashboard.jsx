import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import { fmtBytes, fmtTime, nullStr } from '../lib/fmt'
import { Layout, useBlur, useUser } from '../components/Layout'
import { Loading, Empty, Badge, SensText, NodeTypeBadge } from '../components/ui'
import { ProxyURIEditor } from '../components/ProxyURIEditor'
import { TableBox } from '../components/page'
import { useIsMobile } from '../lib/useIsMobile'

export default function Dashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const blurred = useBlur()
  const isMobile = useIsMobile()
  const { user } = useUser()

  useEffect(() => {
    api.get('/dashboard').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>

  const { nodes = [], node_raw_traffic = {}, rule_count = 0, rule_count_by_node = {}, total_bytes = 0, user_count = 0 } = data
  const onlineCount = nodes.filter(n => !n.disabled && n.online === 1).length
  const offline = nodes.filter(n => n.disabled || n.online !== 1).map(n => n.name)
  const totalBytes = total_bytes
  const ruleCount = rule_count_by_node

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
        <StatCard label="活跃转发" value={rule_count} sub="正在转发的规则"
          icon={<path d="M5 12h14M13 6l6 6-6 6"/>} />
        <StatCard label="在线节点" value={onlineCount} unit={` /${nodes.length}`}
          sub={offline.length ? `${offline.slice(0, 2).join('、')}${offline.length > 2 ? ` 等 ${offline.length} 个` : ''} 离线` : '全部在线'} accent
          icon={<><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/></>} />
        <StatCard label="总流量" value={fmtBytes(totalBytes)} sub="累计上下行"
          icon={<><path d="M3 3v18h18"/><path d="M7 14l4-4 3 3 5-6"/></>} />
        <StatCard label="用户" value={user_count} sub="系统用户数"
          icon={<><path d="M16 21v-2a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v2"/><circle cx="9.5" cy="7" r="3.5"/></>} />
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-[1fr_1fr] gap-[18px] mb-[22px]">
        {/* Node status */}
        <div className="card">
          <div className="card-header justify-between"><h3 className="text-[15px] font-bold">节点状态</h3><span className="text-[12.5px] text-ink-mut">{nodes.length} 个节点</span></div>
          {nodes.length ? (<>
            {/* Desktop table */}
            {!isMobile && <TableBox>
            <table className="tbl">
              <thead><tr><th>节点名</th><th>地址</th><th>类型</th><th>规则</th><th>流量</th><th>状态</th><th>心跳</th></tr></thead>
              <tbody>
                {nodes.map(n => (
                  <tr key={n.id}>
                    <td><Link to={`/nodes/${n.id}`} className="font-semibold text-blue-600 hover:underline">{n.name}</Link></td>
                    <td className="font-mono text-xs text-ink-soft"><SensText blurred={blurred}>{n.relay_host || n.address || '--'}</SensText></td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    <td className="font-mono text-ink-soft">{ruleCount[n.id] || 0}</td>
                    <td className="font-mono text-xs text-ink-mut">{fmtBytes(node_raw_traffic[n.id] || 0)}</td>
                    <td><NodeStatus node={n} /></td>
                    <td className="font-mono text-ink-mut text-xs">{fmtTime(n.last_seen)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            </TableBox>}
            {/* Mobile cards */}
            {isMobile && <div>
              {nodes.map(n => (
                <Link key={n.id} to={`/nodes/${n.id}`} className="mobile-card block no-underline text-ink">
                  <div className="flex items-center justify-between mb-1.5">
                    <span className="font-semibold text-blue-600">{n.name}</span>
                    <NodeStatus node={n} />
                  </div>
                  <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                    <NodeTypeBadge type={n.node_type} />
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono">{ruleCount[n.id] || 0} 条规则</span>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono text-ink-mut">{fmtBytes(node_raw_traffic[n.id] || 0)}</span>
                  </div>
                </Link>
              ))}
            </div>}
          </>) : <Empty title="暂无节点" />}
        </div>

        {/* My proxy URIs (browser-local) — desktop only */}
        <div className="hidden md:block">
          <ProxyURIEditor username={user?.username} blurred={blurred} />
        </div>
      </div>
    </Layout>
  )
}

function StatCard({ label, value, unit, sub, accent, icon }) {
  return (
    <div className="card stat-card">
      <div className="flex items-center justify-between">
        <span className="text-[13px] text-ink-soft font-medium">{label}</span>
        {icon && <svg className="w-[18px] h-[18px] text-ink-mut opacity-50" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">{icon}</svg>}
      </div>
      <div className="mt-3.5 flex items-baseline gap-0.5">
        <span className={`text-[28px] sm:text-[38px] font-bold leading-[1.05] tracking-tight ${accent ? 'text-green-600 dark:text-green-400' : 'text-ink'}`}>{value}</span>
        {unit && <span className="text-[18px] font-semibold text-ink-mut">{unit}</span>}
      </div>
      <div className="stat-sub text-[12.5px] text-ink-mut truncate">{sub || ' '}</div>
    </div>
  )
}

function NodeStatus({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  // A composite node has no agent of its own to sync, so a dispatch error
  // meant for a single node doesn't apply to it — only online/offline does.
  if (node.node_type === 'composite') {
    return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
  }
  // Surface a sync error (hover for detail) rather than hiding it behind 离线.
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red" title={lastErr}>错误</Badge>
  return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
}
