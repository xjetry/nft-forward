import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtGB, fmtTrafficGB, pct, fmtDate, fmtDateInput, isExpired, nullInt, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, NodeTypeBadge } from '../../components/ui'

export default function UserDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const toast = useToast()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)

  const load = () => {
    setLoading(true)
    api.get(`/users/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="用户不存在" /></Layout>

  const { user, nodes = [], grants = [], all_nodes = [], rules = [] } = data

  const isRegularUser = user.role === 'user'

  const toggleUser = async () => {
    try { await api.post(`/users/${id}/toggle`); toast(user.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }
  const resetTraffic = async () => {
    if (!confirm('清零该用户的已用流量？')) return
    try { await api.post(`/users/${id}/reset-traffic`); toast('已重置'); load() } catch (err) { toast(err.message) }
  }
  const deleteUser = async () => {
    if (!confirm('删除用户？关联的规则将被一并清除。')) return
    try { await api.del(`/users/${id}`); toast('已删除'); navigate('/users') } catch (err) { toast(err.message) }
  }
  const resetPassword = async () => {
    if (!confirm('重置该用户密码？新密码会一次性显示。')) return
    try {
      const d = await api.post(`/users/${id}/reset-password`)
      toast(d?.new_password ? `新密码：${d.new_password}` : '已重置')
    } catch (err) { toast(err.message) }
  }

  const expiresAt = user.expires_at?.Valid && user.expires_at.Int64 > 0 ? user.expires_at.Int64 : null

  return (
    <Layout>
      {/* Info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">基本信息</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="fl">用户名</span><span className="font-semibold">{user.username}</span>
            <span className="fl">角色</span><span className="font-mono">{user.role}</span>
            {isRegularUser && (
              <>
                <span className="fl">规则配额</span><span className="font-mono">{rules.length} / {user.max_forwards}</span>
                <span className="fl">流量</span>
                <span className="font-mono">
                  {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
                  {user.traffic_quota_bytes > 0 && ` (${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)`}
                </span>
                <span className="fl">到期时间</span>
                <span className="font-mono">
                  {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
                </span>
                <span className="fl">状态</span>
                <span>
                  {user.disabled ? (
                    <><Badge color="amber">已禁用</Badge> <span className="text-gray-500 text-xs ml-1">原因：{nullStr(user.disable_reason)}</span></>
                  ) : <Badge color="green">正常</Badge>}
                </span>
              </>
            )}
          </div>

          <div className="flex items-center gap-2 mt-5 flex-wrap">
            {isRegularUser && <ExpiryForm userId={id} expiresAt={expiresAt} onDone={load} />}
            {isRegularUser && <QuotaForm userId={id} quotaBytes={user.traffic_quota_bytes} onDone={load} />}
            {isRegularUser && <button onClick={toggleUser} className="btn-secondary text-xs">{user.disabled ? '启用' : '禁用'}</button>}
            {isRegularUser && <button onClick={resetTraffic} className="btn-secondary text-xs">重置流量</button>}
            <button onClick={resetPassword} className="btn-secondary text-xs">重置密码</button>
            <button onClick={deleteUser} className="btn-danger-sm text-xs">删除用户</button>
          </div>
        </div>
      </div>

      {/* Node grants (regular users only) */}
      {isRegularUser && (
        <div className="card mb-5">
          <div className="card-header"><h3 className="text-sm font-bold">已授权节点</h3></div>
          {nodes.length ? (
            <table className="tbl">
              <thead><tr><th>节点</th><th>类型</th><th>本用户上限</th><th className="text-right">操作</th></tr></thead>
              <tbody>
                {nodes.map((n, i) => (
                  <tr key={n.id}>
                    <td className="font-semibold">
                      <Link to={`/nodes/${n.id}`} className="text-blue-600 hover:underline">{n.name}</Link>
                    </td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    <td className="font-mono">{grants[i]?.max_forwards ?? '--'}</td>
                    <td className="text-right">
                      <RevokeBtn url={`/users/${id}/grants/${n.id}`} onDone={load} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <Empty title="尚未授权任何节点" />}
          <div className="p-5 border-t border-gray-100">
            <GrantNodeForm userId={id} nodes={all_nodes} onDone={load} />
          </div>
        </div>
      )}

      {/* Rules */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">该用户的规则</h3>
          <span className="text-xs text-gray-400">{rules.length} 条</span>
        </div>
        {rules.length ? (
          <table className="tbl">
            <thead><tr><th>ID</th><th>名称</th><th>节点</th><th>协议</th><th>入口</th><th>出口</th><th className="text-right">流量</th></tr></thead>
            <tbody>
              {rules.map(r => (
                <tr key={r.id}>
                  <td className="font-mono text-xs text-gray-400">{r.id}</td>
                  <td className="font-semibold">
                    <Link to={`/rules/${r.id}`} className="text-blue-600 hover:underline">{r.name}</Link>
                  </td>
                  <td className="font-mono text-gray-500">{r.node_name || `#${r.node_id}`}</td>
                  <td><ProtoBadge proto={r.proto} /></td>
                  <td className="font-mono text-xs">{r.entry || '--'}</td>
                  <td className="font-mono text-xs">{r.exit || '--'}</td>
                  <td className="text-right font-mono">{fmtGB(r.total_bytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="该用户尚无规则" />}
      </div>

      <Link to="/users" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回用户列表
      </Link>
    </Layout>
  )
}

function ExpiryForm({ userId, expiresAt, onDone }) {
  const [val, setVal] = useState(expiresAt ? fmtDateInput(expiresAt) : '')
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    try { await api.post(`/users/${userId}/expiry`, { expires_at: val || undefined }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="date" value={val} onChange={e => setVal(e.target.value)} style={{ width: 160 }} />
      <button type="submit" className="btn-secondary text-xs">设到期</button>
    </form>
  )
}

// Quota is stored in bytes server-side but edited in MB here, matching the
// read-only display above. Empty/0 means unlimited.
function QuotaForm({ userId, quotaBytes, onDone }) {
  const [mb, setMb] = useState(String(Math.floor((quotaBytes || 0) / 1048576)))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.floor(Number(mb) || 0)) * 1048576
    try { await api.post(`/users/${userId}/quota`, { traffic_quota_bytes: bytes }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" value={mb} onChange={e => setMb(e.target.value)} style={{ width: 120 }} title="0 = 不限" />
      <span className="text-xs text-gray-400">MB</span>
      <button type="submit" className="btn-secondary text-xs">设配额</button>
    </form>
  )
}

function RevokeBtn({ url, onDone }) {
  const toast = useToast()
  const revoke = async () => {
    try { await api.del(url); toast('已撤销'); onDone() } catch (err) { toast(err.message) }
  }
  return <button onClick={revoke} className="btn-danger-sm text-xs">撤销</button>
}

function GrantNodeForm({ userId, nodes, onDone }) {
  const [nodeId, setNodeId] = useState('')
  const [max, setMax] = useState('10')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  if (!nodes?.length) return <Empty desc={<Link to="/nodes" className="text-blue-600 text-xs font-semibold">请先创建节点</Link>} />
  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/users/${userId}/grants`, { node_id: Number(nodeId), max_forwards: Number(max) })
      toast('已授权'); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3">授权新节点</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点</label>
          <select className="input-field" value={nodeId} onChange={e => setNodeId(e.target.value)} required>
            <option value="">-- 选择 --</option>
            {nodes.map(n => <option key={n.id} value={n.id}>{n.name} ({n.node_type === 'composite' ? '组合' : n.node_type === 'self' ? '自身' : '单点'})</option>)}
          </select>
          <label className="fl">本用户上限</label>
          <input className="input-field font-mono" type="number" min="1" value={max} onChange={e => setMax(e.target.value)} style={{ maxWidth: 160 }} />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">授权</button>
      </form>
    </>
  )
}
