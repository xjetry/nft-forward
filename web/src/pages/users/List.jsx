import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTrafficGB, nullStr } from '../../lib/fmt'
import { Layout, useToast, useUser } from '../../components/Layout'
import { Loading, Empty, Badge, Modal } from '../../components/ui'

export default function UserList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const { user: currentUser } = useUser()
  const toast = useToast()

  const load = () => {
    setLoading(true)
    api.get('/users').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { users = [] } = data || {}

  const toggleUser = async (u) => {
    try { await api.post(`/users/${u.id}/toggle`); toast(u.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }
  const resetPassword = async (u) => {
    if (!confirm('重置该用户密码？新密码会一次性显示。')) return
    try {
      const d = await api.post(`/users/${u.id}/reset-password`)
      toast(d?.new_password ? `新密码：${d.new_password}` : '已重置')
    } catch (err) { toast(err.message) }
  }
  const deleteUser = async (u) => {
    if (!confirm('删除该用户？关联的转发将被一并清除。')) return
    try { await api.del(`/users/${u.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  return (
    <Layout>
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">用户列表</h3>
          <span className="text-xs text-gray-400">{users.length} 个用户</span>
          <button onClick={() => setShowCreate(true)} className="btn-primary text-xs ml-auto">+ 新建用户</button>
        </div>
        {users.length ? (
          <table className="tbl">
            <thead><tr><th className="w-12">ID</th><th>用户名</th><th>角色</th><th>规则配额</th><th>流量</th><th>状态</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {users.map(u => {
                const isSelf = u.id === currentUser?.id
                return (
                  <tr key={u.id}>
                    <td className="font-mono text-xs text-gray-400">{u.id}</td>
                    <td><Link to={`/users/${u.id}`} className="text-blue-600 font-semibold hover:underline">{u.username}</Link></td>
                    <td><span className="inline-flex items-center font-mono text-xs bg-gray-100 text-gray-600 px-1.5 py-0.5 rounded">{u.role}</span></td>
                    <td className="font-mono">{u.role === 'user' ? `${u.rule_count || 0} / ${u.max_forwards}` : '--'}</td>
                    <td className="font-mono">{u.role === 'user' ? fmtTrafficGB(u.traffic_used_bytes, u.traffic_quota_bytes) : '--'}</td>
                    <td>
                      {u.disabled ? (
                        <Badge color="amber" title={nullStr(u.disable_reason)}>已禁用</Badge>
                      ) : <Badge color="green">正常</Badge>}
                    </td>
                    <td className="text-right whitespace-nowrap">
                      {isSelf ? (
                        <span className="text-xs text-gray-400">(当前用户)</span>
                      ) : (
                        <>
                          <Link to={`/users/${u.id}`} className="btn-secondary text-xs mr-1.5">详情</Link>
                          <button onClick={() => toggleUser(u)} className="btn-secondary text-xs mr-1.5">{u.disabled ? '启用' : '禁用'}</button>
                          <button onClick={() => resetPassword(u)} className="btn-secondary text-xs mr-1.5">重置密码</button>
                          <button onClick={() => deleteUser(u)} className="btn-danger-sm text-xs">删除</button>
                        </>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        ) : (
          <Empty title="暂无用户" desc="点击上方「新建用户」创建。" />
        )}
      </div>

      <CreateUserModal open={showCreate} onClose={() => setShowCreate(false)} onDone={() => { setShowCreate(false); load() }} />
    </Layout>
  )
}

function CreateUserModal({ open, onClose, onDone }) {
  const [form, setForm] = useState({ username: '', password: '', role: 'user', max_forwards: '100', traffic_quota_mb: '0', expires_at: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/users', {
        username: form.username,
        password: form.password,
        role: form.role,
        max_forwards: Number(form.max_forwards),
        traffic_quota_mb: Number(form.traffic_quota_mb),
        expires_at: form.expires_at || undefined,
      })
      toast('用户已创建')
      setForm({ username: '', password: '', role: 'user', max_forwards: '100', traffic_quota_mb: '0', expires_at: '' })
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="新建用户">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[150px_1fr] gap-4 items-center">
          <label className="fl">用户名</label>
          <input className="input-field" value={form.username} onChange={e => set('username', e.target.value)} required placeholder="登录用户名" />
          <label className="fl">密码</label>
          <input className="input-field" type="password" value={form.password} onChange={e => set('password', e.target.value)} required />
          <label className="fl">角色</label>
          <select className="input-field" value={form.role} onChange={e => set('role', e.target.value)} style={{ maxWidth: 200 }}>
            <option value="user">user (普通用户)</option>
            <option value="admin">admin (管理员)</option>
          </select>
          {form.role === 'user' && (
            <>
              <label className="fl">最大转发数</label>
              <input className="input-field font-mono" type="number" min="1" value={form.max_forwards} onChange={e => set('max_forwards', e.target.value)} style={{ maxWidth: 160 }} />
              <label className="fl">流量配额 <span className="text-gray-400 font-normal text-xs">(MB)</span></label>
              <input className="input-field font-mono" type="number" min="0" value={form.traffic_quota_mb} onChange={e => set('traffic_quota_mb', e.target.value)} style={{ maxWidth: 160 }} />
              <label className="fl">到期时间 <span className="text-gray-400 font-normal text-xs">(可选)</span></label>
              <input className="input-field font-mono" type="date" value={form.expires_at} onChange={e => set('expires_at', e.target.value)} style={{ maxWidth: 200 }} />
            </>
          )}
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
          <button type="submit" disabled={loading} className="btn-primary">创建用户</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
          {form.role === 'user' && <span className="text-xs text-gray-400">配额为 0 时不限制；超额后自动禁用。</span>}
        </div>
      </form>
    </Modal>
  )
}
