import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { pct, fmtTrafficGB, fmtDate, isExpired, nullStr } from '../../lib/fmt'
import { useSpeed, fmtSpeed } from '../../lib/useSpeed'
import { useIsMobile } from '../../lib/useIsMobile'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, NodeTypeBadge } from '../../components/ui'
import { ProxyURIEditor } from '../../components/ProxyURIEditor'
import { TableBox } from '../../components/page'
import { ApiTokenSection } from '../../components/ApiTokenSection'

export default function MyDashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [tab, setTab] = useState('single')
  const [editingUsername, setEditingUsername] = useState(false)
  const [newUsername, setNewUsername] = useState('')
  const speeds = useSpeed()
  const isMobile = useIsMobile()
  const toast = useToast()

  // 授权节点的展示顺序是个人偏好，只存本浏览器不上服务器；键按用户 id 区分，
  // 同一浏览器切换账号互不串扰。不在名单里的节点（新授权）按服务器顺序垫底。
  const [nodeOrder, setNodeOrder] = useState([])
  const [dragIdx, setDragIdx] = useState(null)
  useEffect(() => {
    if (!data?.user?.id) return
    try { setNodeOrder(JSON.parse(localStorage.getItem(`my.nodeOrder.${data.user.id}`) || '[]')) } catch { setNodeOrder([]) }
  }, [data?.user?.id])

  useEffect(() => {
    api.get('/my').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>
  const { user, nodes = [], grants = [], rules = [], show_rate } = data

  const expiresAt = user.expires_at && user.expires_at > 0 ? user.expires_at : null
  const trafficPercent = user.traffic_quota_bytes > 0 ? Number(pct(user.traffic_used_bytes, user.traffic_quota_bytes)) : 0
  const rulePercent = user.max_forwards > 0 ? Math.min(100, (rules.length / user.max_forwards) * 100) : 0

  const grantByNode = {}
  nodes.forEach((n, i) => { grantByNode[n.id] = grants[i] })
  // 排序在 grantByNode 建好之后：nodes 与 grants 按下标对齐，排序副本不动原数组。
  const orderIdx = new Map(nodeOrder.map((id, i) => [id, i]))
  const orderedNodes = [...nodes].sort((a, b) => {
    const ia = orderIdx.get(a.id) ?? Infinity
    const ib = orderIdx.get(b.id) ?? Infinity
    return ia === ib ? 0 : ia - ib
  })
  const singleNodes = orderedNodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = orderedNodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes
  const saveNodeOrder = (ids) => {
    setNodeOrder(ids)
    localStorage.setItem(`my.nodeOrder.${user.id}`, JSON.stringify(ids))
  }
  const onDropRow = (toIdx) => {
    if (dragIdx === null || dragIdx === toIdx) { setDragIdx(null); return }
    const list = [...tabNodes]
    const [moved] = list.splice(dragIdx, 1)
    list.splice(toIdx, 0, moved)
    setDragIdx(null)
    const other = tab === 'composite' ? singleNodes : compositeNodes
    const ids = tab === 'composite'
      ? [...other.map(n => n.id), ...list.map(n => n.id)]
      : [...list.map(n => n.id), ...other.map(n => n.id)]
    saveNodeOrder(ids)
  }

  return (
    <Layout>
      <div className="user-page">
        {user.disabled && (
          <div className="user-alert">
            您的账号已被禁用：{nullStr(user.disable_reason)}。请联系管理员。
          </div>
        )}

        <div className="grid grid-cols-1 xl:grid-cols-[minmax(0,1.12fr)_minmax(420px,0.88fr)] gap-5">
          <section className="user-hero">
            <div className="user-hero-main">
              <div className="user-account-row">
                <div className="user-avatar">{user.username?.charAt(0).toUpperCase()}</div>
                <div className="min-w-0 flex-1">
                  <div className="user-eyebrow">账户概览</div>
                  {editingUsername ? (
                    <form className="mt-2 flex flex-col sm:flex-row sm:items-center gap-2" onSubmit={async (e) => {
                      e.preventDefault()
                      const name = newUsername.trim()
                      if (!name) return
                      try {
                        await api.post('/my/username', { username: name })
                        setEditingUsername(false)
                        window.location.reload()
                      } catch (err) { toast(err.message, 'error') }
                    }}>
                      <input className="input-field font-mono text-sm sm:max-w-[240px]" value={newUsername} onChange={e => setNewUsername(e.target.value)} autoFocus />
                      <div className="flex items-center gap-2">
                        <button type="submit" className="btn-primary text-xs">保存</button>
                        <button type="button" onClick={() => setEditingUsername(false)} className="btn-secondary text-xs">取消</button>
                      </div>
                    </form>
                  ) : (
                    <>
                      <div className="user-title truncate">{user.username}</div>
                      <div className="user-subline">
                        <Badge color={user.disabled ? 'red' : 'green'}>{user.disabled ? '已禁用' : '可用'}</Badge>
                        <span>授权节点 {nodes.length} 个</span>
                        <button onClick={() => { setNewUsername(user.username); setEditingUsername(true) }} className="text-blue-600 text-xs font-semibold hover:underline">修改用户名</button>
                      </div>
                    </>
                  )}
                </div>
              </div>

              <div className="user-stat-grid">
                <div className="user-stat">
                  <div>
                    <div className="user-stat-label">规则配额</div>
                    <div className="user-stat-value font-mono">{rules.length}<span className="text-[14px] text-ink-mut font-semibold"> / {user.max_forwards}</span></div>
                  </div>
                  <div>
                    <div className="user-progress"><span style={{ width: `${rulePercent}%` }} /></div>
                    <div className="user-stat-foot">当前已创建规则</div>
                  </div>
                </div>
                <div className="user-stat">
                  <div>
                    <div className="user-stat-label">流量</div>
                    <div className="user-stat-value text-[18px] sm:text-[20px] font-mono">{fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}</div>
                  </div>
                  <div>
                    <div className="user-progress"><span style={{ width: `${Math.min(100, trafficPercent)}%` }} /></div>
                    <div className="user-stat-foot">{user.traffic_quota_bytes > 0 ? `${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}% 已用` : '不限流量'}</div>
                  </div>
                </div>
                <div className="user-stat">
                  <div>
                    <div className="user-stat-label">计费倍率</div>
                    <div className="user-stat-value font-mono">×{user.billing_rate ?? 1}</div>
                  </div>
                  <div className="user-stat-foot">按节点和规则计费</div>
                </div>
                <div className="user-stat">
                  <div>
                    <div className="user-stat-label">到期时间</div>
                    <div className="user-stat-value text-[18px] sm:text-[20px]">{expiresAt ? fmtDate(expiresAt) : '永不过期'}</div>
                  </div>
                  <div className="user-stat-foot">
                    {expiresAt && isExpired(expiresAt) ? <Badge color="red">已过期</Badge> : '账户有效期'}
                  </div>
                </div>
              </div>
            </div>
          </section>

          {/* My proxy URIs (browser-local) — desktop only */}
          <div className="hidden md:block min-w-0">
            <ProxyURIEditor username={user.username} className="h-full" />
          </div>
        </div>

        <ApiTokenSection />

        {/* Granted nodes */}
        <section className="user-section user-panel">
          <div className="user-section-head">
            <div>
              <h3 className="user-section-title">已授权节点</h3>
              <div className="user-section-sub">拖拽节点名称前的手柄可调整本浏览器展示顺序</div>
            </div>
            {nodes.length > 0 && (
              <div className="user-tabs">
                {[['single', '单点', singleNodes.length], ['composite', '组合', compositeNodes.length]].map(([key, label, n]) => (
                  <button key={key} onClick={() => setTab(key)}
                    className={`user-tab ${tab === key ? 'user-tab-active' : ''}`}>{label} {n}</button>
                ))}
              </div>
            )}
          </div>
          {tabNodes.length > 0 ? (<>
            {/* Desktop table */}
            {!isMobile && <TableBox>
            <table className="tbl">
              <thead><tr><th>节点</th><th>类型</th>{show_rate && <th>倍率</th>}<th>状态</th><th>速度</th><th>已用流量</th><th>限速</th><th>本节点上限</th></tr></thead>
              <tbody>
                {tabNodes.map((n, i) => {
                  const g = grantByNode[n.id]
                  return (
                    <tr key={n.id}
                      onDragOver={e => e.preventDefault()}
                      onDrop={() => onDropRow(i)}
                      className={dragIdx === i ? 'opacity-50' : ''}>
                      <td className="font-semibold">
                        <span className="text-ink-mut mr-2 select-none cursor-move" title="拖拽排序（仅保存在本浏览器）"
                          draggable onDragStart={() => setDragIdx(i)}>⠿</span>
                        <span className="route-name">{n.name}</span>
                        {(n.roles & 2) !== 0 && <Badge color="blue" className="ml-1.5">中转</Badge>}
                      </td>
                      <td><NodeTypeBadge type={n.node_type} /></td>
                      {show_rate && <td><Badge color="blue">×{n.rate_multiplier ?? 1}</Badge>{n.unidirectional && <Badge color="amber" className="ml-1">单向</Badge>}</td>}
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
                      <td className="font-mono text-xs">{fmtTrafficGB(g?.traffic_used_bytes, g?.traffic_quota_bytes)}</td>
                      <td className="font-mono text-xs">{g?.rate_limit_mbytes > 0 ? `${g.rate_limit_mbytes} MB/s` : '不限'}</td>
                      <td className="font-mono">{g?.max_forwards ?? '--'}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
            </TableBox>}
            {/* Mobile cards */}
            {isMobile && <div className="user-card-list">
              {tabNodes.map(n => {
                const g = grantByNode[n.id]
                return (
                  <div key={n.id} className="user-node-card">
                    <div className="flex items-start justify-between gap-3 mb-2">
                      <span className="font-semibold min-w-0">
                        {n.name}
                        {(n.roles & 2) !== 0 && <Badge color="blue" className="ml-1.5">中转</Badge>}
                      </span>
                      <NodeOnline node={n} />
                    </div>
                    <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                      <NodeTypeBadge type={n.node_type} />
                      {show_rate && <Badge color="blue">×{n.rate_multiplier ?? 1}</Badge>}
                      {n.unidirectional && <Badge color="amber">单向</Badge>}
                      {speeds[n.id] && <>
                        <span className="text-ink-mut">·</span>
                        <span className="font-mono text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                        <span className="font-mono text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                      </>}
                      <span className="text-ink-mut">·</span>
                      <span className="font-mono">{fmtTrafficGB(g?.traffic_used_bytes, g?.traffic_quota_bytes)}</span>
                      {g?.rate_limit_mbytes > 0 && <>
                        <span className="text-ink-mut">·</span>
                        <span className="font-mono">{g.rate_limit_mbytes} MB/s</span>
                      </>}
                    </div>
                  </div>
                )
              })}
            </div>}
          </>) : nodes.length > 0 ? (
            <Empty title={tab === 'composite' ? '暂无已授权的组合节点' : '暂无已授权的单点节点'} />
          ) : <Empty title="管理员尚未为您授权任何节点" desc="请联系管理员。" />}
        </section>
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
