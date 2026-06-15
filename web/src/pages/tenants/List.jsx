import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, pct, nullStr, fmtDate, fmtDateInput, isExpired } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge } from '../../components/ui'

export default function TenantList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const toast = useToast()

  const load = () => {
    setLoading(true)
    api.get('/tenants').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { tenants = [] } = data || {}

  return (
    <Layout>
      {/* Tenant table */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">用户列表</h3>
          <span className="text-xs text-gray-400">{tenants.length} 个用户</span>
        </div>
        {tenants.length ? (
          <table className="tbl">
            <thead><tr><th className="w-12">ID</th><th>名称</th><th>最大转发</th><th>流量配额</th><th>已用</th><th>状态</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {tenants.map(t => (
                <tr key={t.id}>
                  <td className="font-mono text-xs text-gray-400">{t.id}</td>
                  <td><Link to={`/tenants/${t.id}`} className="text-blue-600 font-semibold hover:underline">{t.name}</Link></td>
                  <td className="font-mono">{t.max_forwards}</td>
                  <td className="font-mono">{t.traffic_quota_bytes === 0 ? <span className="text-xl">&#x221e;</span> : `${Math.floor(t.traffic_quota_bytes / 1048576)} MB`}</td>
                  <td className="font-mono">{Math.floor(t.traffic_used_bytes / 1048576)} MB</td>
                  <td>
                    {t.disabled ? (
                      <Badge color="amber" title={nullStr(t.disable_reason)}>已禁用</Badge>
                    ) : <Badge color="green">正常</Badge>}
                  </td>
                  <td className="text-right">
                    <Link to={`/tenants/${t.id}`} className="btn-secondary text-xs">详情</Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <Empty title="暂无用户" desc="使用下方「新建用户」创建第一个账号入口。" />
        )}
      </div>

      {/* Create tenant */}
      <CreateTenantCard onDone={load} />
    </Layout>
  )
}

function CreateTenantCard({ onDone }) {
  const [form, setForm] = useState({ name: '', max_forwards: '100', traffic_quota_mb: '0', expires_at: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/tenants', {
        name: form.name,
        max_forwards: Number(form.max_forwards),
        traffic_quota_mb: Number(form.traffic_quota_mb),
        expires_at: form.expires_at || undefined,
      })
      toast('用户已创建')
      setForm({ name: '', max_forwards: '100', traffic_quota_mb: '0', expires_at: '' })
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <div className="card">
      <div className="card-header"><h3 className="text-sm font-bold">新建用户</h3></div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-4 max-w-2xl">
          <div className="grid grid-cols-[150px_1fr] gap-4 items-center">
            <label className="fl">名称</label>
            <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="例如 acme" />
            <label className="fl">最大转发数</label>
            <input className="input-field font-mono" type="number" min="1" value={form.max_forwards} onChange={e => set('max_forwards', e.target.value)} style={{ maxWidth: 160 }} />
            <label className="fl">流量配额 <span className="text-gray-400 font-normal text-xs">(MB)</span></label>
            <input className="input-field font-mono" type="number" min="0" value={form.traffic_quota_mb} onChange={e => set('traffic_quota_mb', e.target.value)} style={{ maxWidth: 160 }} />
            <label className="fl">到期时间 <span className="text-gray-400 font-normal text-xs">(可选)</span></label>
            <input className="input-field font-mono" type="date" value={form.expires_at} onChange={e => set('expires_at', e.target.value)} style={{ maxWidth: 200 }} />
          </div>
          <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
            <button type="submit" disabled={loading} className="btn-primary">创建用户</button>
            <span className="text-xs text-gray-400">配额为 0 时不限制；超额后用户自动禁用并清空内核规则。</span>
          </div>
        </form>
      </div>
    </div>
  )
}
