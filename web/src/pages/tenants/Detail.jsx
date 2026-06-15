import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, pct, fmtDate, fmtDateInput, isExpired, nullInt, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge } from '../../components/ui'

export default function TenantDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const toast = useToast()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)

  const load = () => {
    setLoading(true)
    api.get(`/tenants/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="用户不存在" /></Layout>

  const { tenant, tenant_users = [], tunnels = [], grants = [], combos = [], combo_grants = [], all_tunnels = [], all_combos = [], forwards = [] } = data

  const toggleTenant = async () => {
    try { await api.post(`/tenants/${id}/toggle`); toast(tenant.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }
  const resetTraffic = async () => {
    if (!confirm('清零已用流量并重新启用？')) return
    try { await api.post(`/tenants/${id}/reset-traffic`); toast('已重置'); load() } catch (err) { toast(err.message) }
  }
  const deleteTenant = async () => {
    if (!confirm('删除用户？关联的转发将被一并清除。')) return
    try { await api.del(`/tenants/${id}`); toast('已删除'); navigate('/tenants') } catch (err) { toast(err.message) }
  }

  const expiresAt = tenant.expires_at?.Valid && tenant.expires_at.Int64 > 0 ? tenant.expires_at.Int64 : null

  return (
    <Layout>
      {/* Info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">基本信息</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="fl">最大转发数</span><span className="font-mono">{tenant.max_forwards}</span>
            <span className="fl">流量配额</span><span className="font-mono">{tenant.traffic_quota_bytes === 0 ? <span className="text-xl">&#x221e;（不限）</span> : `${Math.floor(tenant.traffic_quota_bytes / 1048576)} MB`}</span>
            <span className="fl">已用流量</span>
            <span className="font-mono">
              {Math.floor(tenant.traffic_used_bytes / 1048576)} MB
              {tenant.traffic_quota_bytes > 0 && ` (${pct(tenant.traffic_used_bytes, tenant.traffic_quota_bytes)}%)`}
            </span>
            <span className="fl">到期时间</span>
            <span className="font-mono">
              {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
            </span>
            <span className="fl">状态</span>
            <span>
              {tenant.disabled ? (
                <><Badge color="amber">已禁用</Badge> <span className="text-gray-500 text-xs ml-1">原因：{nullStr(tenant.disable_reason)}</span></>
              ) : <Badge color="green">正常</Badge>}
            </span>
          </div>

          <div className="flex items-center gap-2 mt-5 flex-wrap">
            <ExpiryForm tenantId={id} expiresAt={expiresAt} onDone={load} />
            <button onClick={toggleTenant} className="btn-secondary text-xs">{tenant.disabled ? '重新启用' : '禁用'}</button>
            <button onClick={resetTraffic} className="btn-secondary text-xs">重置流量并启用</button>
            <button onClick={deleteTenant} className="btn-danger-sm text-xs">删除用户</button>
          </div>
        </div>
      </div>

      {/* Users */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">登录账号</h3></div>
        {tenant_users.length ? (
          <table className="tbl">
            <thead><tr><th>ID</th><th>用户名</th><th>状态</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {tenant_users.map(u => (
                <tr key={u.id}>
                  <td className="font-mono text-xs text-gray-400">{u.id}</td>
                  <td className="font-semibold">{u.username}</td>
                  <td>{u.disabled ? <Badge color="amber">禁用</Badge> : <Badge color="green">正常</Badge>}</td>
                  <td className="text-right whitespace-nowrap">
                    <UserActions user={u} onDone={load} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚无登录账号" desc="在下方创建。" />}
        <div className="p-5 border-t border-gray-100">
          <AddUserForm tenantId={id} onDone={load} />
        </div>
      </div>

      {/* Tunnel grants */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">已授权通道</h3></div>
        {tunnels.length ? (
          <table className="tbl">
            <thead><tr><th>通道</th><th>节点</th><th>协议</th><th>端口段</th><th>本用户上限</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {tunnels.map((t, i) => (
                <tr key={t.id}>
                  <td className="font-semibold">{t.name}</td>
                  <td className="font-mono text-gray-500">#{t.node_id}</td>
                  <td>{t.proto_mask}</td>
                  <td className="font-mono">{t.port_start}-{t.port_end}</td>
                  <td className="font-mono">{grants[i]?.max_forwards}</td>
                  <td className="text-right">
                    <RevokeBtn url={`/tenants/${id}/grants/${t.id}/delete`} onDone={load} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚未授权任何通道" />}
        <div className="p-5 border-t border-gray-100">
          <GrantTunnelForm tenantId={id} tunnels={all_tunnels} onDone={load} />
        </div>
      </div>

      {/* Combo grants */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">已授权组合通道</h3></div>
        {combos.length ? (
          <table className="tbl">
            <thead><tr><th>组合</th><th>本用户上限</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {combos.map((c, i) => (
                <tr key={c.id}>
                  <td className="font-semibold">{c.name}</td>
                  <td className="font-mono">{combo_grants[i]?.max_forwards}</td>
                  <td className="text-right">
                    <RevokeBtn url={`/tenants/${id}/combo-grants/${c.id}/delete`} onDone={load} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚未授权任何组合通道" />}
        <div className="p-5 border-t border-gray-100">
          <GrantComboForm tenantId={id} combos={all_combos} onDone={load} />
        </div>
      </div>

      {/* Forwards */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">该用户的转发</h3>
          <span className="text-xs text-gray-400">{forwards.length} 条</span>
        </div>
        {forwards.length ? (
          <table className="tbl">
            <thead><tr><th>ID</th><th>节点</th><th>通道</th><th>协议</th><th>监听</th><th>目标</th><th className="text-right">累计流量</th></tr></thead>
            <tbody>
              {forwards.map(f => (
                <tr key={f.id}>
                  <td className="font-mono text-xs text-gray-400">{f.id}</td>
                  <td className="font-mono">#{f.node_id}</td>
                  <td className="font-mono">{nullInt(f.tunnel_id) ? `#${nullInt(f.tunnel_id)}` : '--'}</td>
                  <td><ProtoBadge proto={f.proto} /></td>
                  <td className="font-mono">{f.listen_port}</td>
                  <td className="font-mono">{f.target_ip}:{f.target_port}</td>
                  <td className="text-right font-mono">{fmtBytes(f.total_bytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="该用户尚无转发" />}
      </div>

      <Link to="/tenants" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回用户列表
      </Link>
    </Layout>
  )
}

function ExpiryForm({ tenantId, expiresAt, onDone }) {
  const [val, setVal] = useState(expiresAt ? fmtDateInput(expiresAt) : '')
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    try { await api.post(`/tenants/${tenantId}/expiry`, { expires_at: val || undefined }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="date" value={val} onChange={e => setVal(e.target.value)} style={{ width: 160 }} />
      <button type="submit" className="btn-secondary text-xs">设到期</button>
    </form>
  )
}

function UserActions({ user, onDone }) {
  const toast = useToast()
  const toggle = async () => { try { await api.post(`/users/${user.id}/toggle`); onDone() } catch (err) { toast(err.message) } }
  const reset = async () => { if (!confirm('重置该用户密码？')) return; try { const d = await api.post(`/users/${user.id}/reset-password`); toast(d?.new_password ? `新密码：${d.new_password}` : '已重置'); onDone() } catch (err) { toast(err.message) } }
  const del = async () => { if (!confirm('删除账号？')) return; try { await api.del(`/users/${user.id}`); toast('已删除'); onDone() } catch (err) { toast(err.message) } }
  return (
    <>
      <button onClick={toggle} className="btn-secondary text-xs mr-1.5">{user.disabled ? '启用' : '禁用'}</button>
      <button onClick={reset} className="btn-secondary text-xs mr-1.5">重置密码</button>
      <button onClick={del} className="btn-danger-sm text-xs">删除</button>
    </>
  )
}

function AddUserForm({ tenantId, onDone }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/tenants/${tenantId}/users`, { username, password })
      toast('账号已创建'); setUsername(''); setPassword('')
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3">添加账号</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">用户名</label>
          <input className="input-field" value={username} onChange={e => setUsername(e.target.value)} required />
          <label className="fl">密码</label>
          <input className="input-field" type="password" value={password} onChange={e => setPassword(e.target.value)} required />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">创建账号</button>
      </form>
    </>
  )
}

function RevokeBtn({ url, onDone }) {
  const toast = useToast()
  const revoke = async () => {
    try { await api.post(url); toast('已撤销'); onDone() } catch (err) { toast(err.message) }
  }
  return <button onClick={revoke} className="btn-danger-sm text-xs">撤销</button>
}

function GrantTunnelForm({ tenantId, tunnels, onDone }) {
  const [tunnelId, setTunnelId] = useState('')
  const [max, setMax] = useState('10')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  if (!tunnels?.length) return <Empty desc={<Link to="/tunnels" className="text-blue-600 text-xs font-semibold">请先创建通道</Link>} />
  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/tenants/${tenantId}/grants`, { tunnel_id: Number(tunnelId), max_forwards: Number(max) })
      toast('已授权'); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3">授权新通道</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">通道</label>
          <select className="input-field" value={tunnelId} onChange={e => setTunnelId(e.target.value)} required>
            <option value="">-- 选择 --</option>
            {tunnels.map(t => <option key={t.id} value={t.id}>{t.name} (节点 #{t.node_id}, {t.port_start}-{t.port_end}/{t.proto_mask})</option>)}
          </select>
          <label className="fl">本用户上限</label>
          <input className="input-field font-mono" type="number" min="1" value={max} onChange={e => setMax(e.target.value)} style={{ maxWidth: 160 }} />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">授权</button>
      </form>
    </>
  )
}

function GrantComboForm({ tenantId, combos, onDone }) {
  const [comboId, setComboId] = useState('')
  const [max, setMax] = useState('10')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  if (!combos?.length) return <Empty desc={<Link to="/combos" className="text-blue-600 text-xs font-semibold">请先创建组合通道</Link>} />
  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/tenants/${tenantId}/combo-grants`, { combo_id: Number(comboId), combo_max_forwards: Number(max) })
      toast('已授权'); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3">授权新组合</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">组合通道</label>
          <select className="input-field" value={comboId} onChange={e => setComboId(e.target.value)} required>
            <option value="">-- 选择 --</option>
            {combos.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
          <label className="fl">本用户上限</label>
          <input className="input-field font-mono" type="number" min="1" value={max} onChange={e => setMax(e.target.value)} style={{ maxWidth: 160 }} />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">授权</button>
      </form>
    </>
  )
}
