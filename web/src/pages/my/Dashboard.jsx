import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { pct, fmtTrafficGB, fmtDate, isExpired, nullStr } from '../../lib/fmt'
import { useSpeed, fmtSpeed } from '../../lib/useSpeed'
import { Layout } from '../../components/Layout'
import { Loading, Empty, Badge, NodeTypeBadge } from '../../components/ui'
import { ProxyURIEditor } from '../../components/ProxyURIEditor'

export default function MyDashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [tab, setTab] = useState('single')
  const [editingUsername, setEditingUsername] = useState(false)
  const [newUsername, setNewUsername] = useState('')
  const speeds = useSpeed()

  useEffect(() => {
    api.get('/my').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>
  const { user, nodes = [], grants = [], rules = [], show_rate } = data

  const expiresAt = user.expires_at && user.expires_at > 0 ? user.expires_at : null

  const grantByNode = {}
  nodes.forEach((n, i) => { grantByNode[n.id] = grants[i] })
  const singleNodes = nodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes

  return (
    <Layout>
      {user.disabled && (
        <div className="mb-4 px-4 py-3 bg-red-50 border border-red-200 rounded-lg text-red-600 text-sm font-medium">
          您的账号已被禁用：{nullStr(user.disable_reason)}。请联系管理员。
        </div>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-[1.15fr_1fr] gap-[18px] mb-[22px]">
        {/* Quota */}
        <div className="card flex flex-col">
          <div className="px-6 py-[22px] flex-1 flex flex-col">
            <h3 className="text-[16px] font-bold mb-5">我的配额</h3>
            <div className="flex items-center gap-4 py-3 border-b border-line-soft">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">用户名</div>
              {editingUsername ? (
                <form className="flex items-center gap-2" onSubmit={async (e) => {
                  e.preventDefault()
                  const name = newUsername.trim()
                  if (!name) return
                  try {
                    await api.post('/my/username', { username: name })
                    setEditingUsername(false)
                    window.location.reload()
                  } catch (err) { alert(err.message) }
                }}>
                  <input className="input-field font-mono text-sm" value={newUsername} onChange={e => setNewUsername(e.target.value)} autoFocus style={{ width: 180 }} />
                  <button type="submit" className="btn-primary text-xs">保存</button>
                  <button type="button" onClick={() => setEditingUsername(false)} className="btn-secondary text-xs">取消</button>
                </form>
              ) : (
                <div className="text-[14.5px] flex items-center gap-2">
                  <span className="font-semibold">{user.username}</span>
                  <button onClick={() => { setNewUsername(user.username); setEditingUsername(true) }} className="text-blue-600 text-xs hover:underline">修改</button>
                </div>
              )}
            </div>
            <div className="flex items-center gap-4 py-3 border-b border-line-soft">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">规则配额</div>
              <div className="text-[14.5px]"><span className="font-mono">{rules.length}</span> <span className="text-ink-mut">/</span> <span className="font-mono">{user.max_forwards}</span></div>
            </div>
            <div className="flex items-center gap-4 py-3 border-b border-line-soft">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">流量</div>
              <div className="text-[14.5px] font-mono">
                {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
                {user.traffic_quota_bytes > 0 && <span className="text-green-600 dark:text-green-400"> ({pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)</span>}
              </div>
            </div>
            <div className="flex items-center gap-4 py-3">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">到期时间</div>
              <div className="text-[14.5px]">
                {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
              </div>
            </div>
          </div>
        </div>

        {/* My proxy URIs (browser-local) — desktop only */}
        <div className="hidden md:block">
          <ProxyURIEditor username={user.username} />
        </div>
      </div>

      {/* Granted nodes */}
      <div className="card">
        <div className="card-header">
          <h3 className="text-[15px] font-bold">已授权节点</h3>
        </div>
        {nodes.length > 0 && (
          <div className="flex items-center gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
            {[['single', '单点', singleNodes.length], ['composite', '组合', compositeNodes.length]].map(([key, label, n]) => (
              <button key={key} onClick={() => setTab(key)}
                className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                  tab === key ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
                }`}>{label} {n}</button>
            ))}
          </div>
        )}
        {tabNodes.length > 0 ? (<>
          {/* Desktop table */}
          <table className="tbl hidden md:table">
            <thead><tr><th>节点</th><th>类型</th>{show_rate && <th>倍率</th>}<th>状态</th><th>速度</th><th>已用流量</th><th>本节点上限</th></tr></thead>
            <tbody>
              {tabNodes.map(n => {
                const g = grantByNode[n.id]
                return (
                  <tr key={n.id}>
                    <td className="font-semibold">{n.name}</td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    {show_rate && <td><Badge color="blue">×{n.rate_multiplier ?? 1}</Badge></td>}
                    <td><NodeOnline node={n} /></td>
                    <td className="font-mono text-xs whitespace-nowrap">
                      {speeds[n.id] ? (
                        <>
                          <span className="text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                          {' '}
                          <span className="text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                        </>
                      ) : (
                        <span className="text-ink-mut">--</span>
                      )}
                    </td>
                    <td className="font-mono text-xs">{fmtTrafficGB(Math.round((g?.traffic_used_bytes || 0) * (n.rate_multiplier || 1)), g?.traffic_quota_bytes)}</td>
                    <td className="font-mono">{g?.max_forwards ?? '--'}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          {/* Mobile cards */}
          <div className="md:hidden">
            {tabNodes.map(n => {
              const g = grantByNode[n.id]
              return (
                <div key={n.id} className="mobile-card">
                  <div className="flex items-center justify-between mb-1">
                    <span className="font-semibold">{n.name}</span>
                    <NodeOnline node={n} />
                  </div>
                  <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                    <NodeTypeBadge type={n.node_type} />
                    {show_rate && <Badge color="blue">×{n.rate_multiplier ?? 1}</Badge>}
                    {speeds[n.id] && <>
                      <span className="text-ink-mut">·</span>
                      <span className="font-mono text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                      <span className="font-mono text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                    </>}
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono">{fmtTrafficGB(Math.round((g?.traffic_used_bytes || 0) * (n.rate_multiplier || 1)), g?.traffic_quota_bytes)}</span>
                  </div>
                </div>
              )
            })}
          </div>
        </>) : nodes.length > 0 ? (
          <Empty title={tab === 'composite' ? '暂无已授权的组合节点' : '暂无已授权的单点节点'} />
        ) : <Empty title="管理员尚未为您授权任何节点" desc="请联系管理员。" />}
      </div>
    </Layout>
  )
}

// Online/offline (or disabled) status for a granted node. The server resolves
// composite nodes' online state from their children before sending.
function NodeOnline({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
}

