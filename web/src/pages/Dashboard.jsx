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
  const ruleSeries = nodes
    .map(n => ({ label: n.name, value: Number(ruleCount[n.id] || 0) }))
    .sort((a, b) => b.value - a.value)
    .slice(0, 10)
  const trafficSeries = nodes
    .map(n => ({ label: n.name, value: Number(node_raw_traffic[n.id] || 0) }))
    .sort((a, b) => b.value - a.value)
    .slice(0, 10)
  const onlineRatio = nodes.length ? onlineCount / nodes.length : 0
  const maxNodeTraffic = Math.max(...nodes.map(n => Number(node_raw_traffic[n.id] || 0)), 1)

  return (
    <Layout>
      <div className="admin-overview">
      {/* Stat cards */}
      <div className="admin-stat-grid grid grid-cols-2 lg:grid-cols-4 gap-4 mb-[22px]">
        <StatCard label="活跃转发" value={rule_count} sub="节点规则 Top3"
          tone="blue" chart={<MiniMetricChart values={ruleSeries} tone="blue" label="节点规则" formatValue={v => `${v} 条`} />}
          icon={<path d="M5 12h14M13 6l6 6-6 6"/>} />
        <StatCard label="在线节点" value={onlineCount} unit={`/${nodes.length}`}
          sub={offline.length ? `${offline.slice(0, 2).join('、')}${offline.length > 2 ? ` 等 ${offline.length} 个` : ''} 离线` : '全部在线'} accent
          tone="green" chart={<DonutChart ratio={onlineRatio} />}
          icon={<><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/></>} />
        <StatCard label="总流量" value={fmtBytes(totalBytes)} sub="节点流量 Top3"
          tone="purple" chart={<MiniMetricChart values={trafficSeries} tone="purple" label="节点流量" formatValue={fmtBytes} />}
          icon={<><path d="M3 3v18h18"/><path d="M7 14l4-4 3 3 5-6"/></>} />
        <StatCard label="用户" value={user_count} sub="系统用户数"
          tone="orange" chart={<DotGrid count={user_count} />}
          icon={<><path d="M16 21v-2a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v2"/><circle cx="9.5" cy="7" r="3.5"/></>} />
      </div>

      {/* Both cards share the same lg:max-h cap; under it, grid stretch keeps
          them aligned to the taller card, and past it each scrolls inside. */}
      <div className="grid grid-cols-1 lg:grid-cols-[1fr_1fr] gap-[18px] mb-[22px]">
        {/* Node status — flex column so the table stretches to the card's
            grid-given height (the taller sibling card) instead of stopping
            at TableBox's default max-height. */}
        <div className="card admin-node-panel flex flex-col lg:max-h-[640px]">
          <div className="admin-node-head">
            <div className="admin-node-title-wrap">
              <span className="admin-node-title-icon" aria-hidden="true">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.1" strokeLinecap="round" strokeLinejoin="round"><rect x="4" y="5" width="16" height="5" rx="1.5"/><rect x="4" y="14" width="16" height="5" rx="1.5"/><path d="M8 10v4M16 10v4"/></svg>
              </span>
              <div>
                <h3>节点状态</h3>
                <p>{onlineCount} 在线 · {offline.length} 离线/禁用</p>
              </div>
            </div>
            <div className="admin-node-count"><b>{nodes.length}</b><span>节点</span></div>
          </div>
          {nodes.length ? (<>
            {/* Desktop table */}
            {!isMobile && <TableBox className="flex-1 min-h-0 !max-h-none">
            <table className="tbl admin-node-table">
              <thead><tr><th>节点</th><th>入口地址</th><th>类型</th><th>规则</th><th>流量</th><th>状态</th><th>心跳</th></tr></thead>
              <tbody>
                {nodes.map(n => {
                  const traffic = Number(node_raw_traffic[n.id] || 0)
                  return (
                  <tr key={n.id} className="admin-node-row">
                    <td>
                      <Link to={`/nodes/${n.id}`} className="admin-node-link">
                        <span className={`admin-node-dot ${!n.disabled && n.online === 1 ? 'is-online' : 'is-offline'}`} />
                        <span className="truncate">{n.name}</span>
                      </Link>
                    </td>
                    <td className="font-mono text-xs text-ink-soft">
                      <span className="admin-node-address"><SensText blurred={blurred}>{n.relay_host || n.address || '--'}</SensText></span>
                    </td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    <td><span className="admin-node-rule-count">{ruleCount[n.id] || 0}</span></td>
                    <td><NodeTrafficCell bytes={traffic} max={maxNodeTraffic} /></td>
                    <td><NodeStatus node={n} /></td>
                    <td className="admin-node-time font-mono text-ink-mut text-xs">{fmtTime(n.last_seen)}</td>
                  </tr>
                )})}
              </tbody>
            </table>
            </TableBox>}
            {/* Mobile cards */}
            {isMobile && <div className="admin-node-mobile-list">
              {nodes.map(n => (
                <Link key={n.id} to={`/nodes/${n.id}`} className="mobile-card admin-node-mobile-card block no-underline text-ink">
                  <div className="flex items-center justify-between mb-1.5">
                    <span className="admin-node-link"><span className={`admin-node-dot ${!n.disabled && n.online === 1 ? 'is-online' : 'is-offline'}`} />{n.name}</span>
                    <NodeStatus node={n} />
                  </div>
                  <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                    <NodeTypeBadge type={n.node_type} />
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono">{ruleCount[n.id] || 0} 条规则</span>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono text-ink-mut">{fmtBytes(node_raw_traffic[n.id] || 0)}</span>
                  </div>
                  <NodeTrafficCell bytes={Number(node_raw_traffic[n.id] || 0)} max={maxNodeTraffic} />
                </Link>
              ))}
            </div>}
          </>) : <Empty title="暂无节点" />}
        </div>

        {/* My proxy URIs (browser-local) — desktop only */}
        <div className="hidden md:flex flex-col lg:max-h-[640px]">
          <ProxyURIEditor username={user?.username} blurred={blurred} className="admin-proxy-panel flex-1 min-h-0" />
        </div>
      </div>
      </div>
    </Layout>
  )
}

function StatCard({ label, value, unit, sub, accent, icon, chart, tone = 'blue' }) {
  return (
    <div className={`card stat-card admin-stat-card admin-stat-card-${tone} ${accent ? 'admin-stat-card-accent' : ''}`}>
      <div className="admin-stat-head">
        <span>{label}</span>
        {icon && <svg className="admin-stat-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">{icon}</svg>}
      </div>
      <div className="admin-stat-body">
        <div className="admin-stat-copy min-w-0">
          <div className="admin-stat-value-row">
            <span className={`admin-stat-value text-[28px] sm:text-[38px] font-bold leading-[1.05] tracking-tight ${accent ? 'text-green-600 dark:text-green-400' : 'text-ink'}`}>{value}</span>
            {unit && <span className="admin-stat-unit text-[18px] font-semibold text-ink-mut">{unit}</span>}
          </div>
          <div className="stat-sub text-[12.5px] text-ink-mut truncate">{sub || ' '}</div>
        </div>
        {chart && <div className="admin-stat-chart">{chart}</div>}
      </div>
    </div>
  )
}

function MiniMetricChart({ values = [], tone = 'blue', label = '节点 Top3', formatValue = v => v }) {
  const series = values.map((item) => {
    if (typeof item === 'number') return { label: '', value: Number(item) || 0 }
    return { label: item.label || '', value: Number(item.value || 0) }
  }).filter(item => item.value > 0)
  const topRows = series.slice(0, 3)
  const max = Math.max(...topRows.map(item => item.value), 1)
  const title = topRows.length
    ? `${label}：${topRows.map((item, i) => `${i + 1}. ${item.label}: ${formatValue(item.value)}`).join('；')}`
    : `${label}：暂无节点数据`
  return (
    <div className={`mini-metric-chart mini-metric-chart-${tone}`} title={title}>
      <div className="mini-metric-title">{label}</div>
      <div className="mini-metric-rows">
        {topRows.length ? topRows.map((item, i) => (
          <div key={`${item.label}-${i}`} className="mini-metric-row">
            <span className="mini-metric-rank">{i + 1}</span>
            <span className="mini-metric-bar"><span style={{ width: `${Math.max(8, (item.value / max) * 100)}%` }} /></span>
            <b>{formatValue(item.value)}</b>
          </div>
        )) : <div className="mini-metric-empty">暂无数据</div>}
      </div>
    </div>
  )
}

function DonutChart({ ratio }) {
  const pct = Math.max(0, Math.min(1, ratio || 0))
  return (
    <div className="donut-chart" title={`${Math.round(pct * 100)}% 在线`}>
      <svg viewBox="0 0 72 72" aria-hidden="true">
        <circle className="donut-track" cx="36" cy="36" r="28" pathLength="100" />
        <circle className="donut-value" cx="36" cy="36" r="28" pathLength="100" strokeDasharray={`${pct * 100} ${100 - pct * 100}`} />
      </svg>
      <span>{Math.round(pct * 100)}%</span>
    </div>
  )
}

function DotGrid({ count }) {
  const total = 24
  const active = Math.max(0, Math.min(total, Number(count) || 0))
  return (
    <div className="dot-grid" title={`${count || 0} 个用户`} aria-label={`${count || 0} 个用户`}>
      {Array.from({ length: total }, (_, i) => <span key={i} className={i < active ? 'on' : ''} />)}
    </div>
  )
}

function NodeTrafficCell({ bytes, max }) {
  const pct = bytes > 0 && max > 0 ? Math.max(4, Math.min(100, (bytes / max) * 100)) : 0
  return (
    <div className="admin-node-traffic" title={fmtBytes(bytes)}>
      <span className="admin-node-traffic-value">{fmtBytes(bytes)}</span>
      <span className="admin-node-traffic-track"><span style={{ width: `${pct}%` }} /></span>
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
