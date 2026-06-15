import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast, useUser } from '../../components/Layout'
import { Loading, Badge } from '../../components/ui'

export default function UserList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const { user: currentUser } = useUser()
  const toast = useToast()

  const load = () => {
    setLoading(true)
    api.get('/users').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { users = [], tenant_by_id = {} } = data || {}

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
    if (!confirm('删除该账号？若是用户记录账号且为其唯一账号，用户记录与转发会一并清除。')) return
    try { await api.del(`/users/${u.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  return (
    <Layout>
      <div className="card">
        <div className="card-header"><h3 className="text-sm font-bold">账号列表</h3></div>
        <table className="tbl">
          <thead><tr><th>ID</th><th>用户名</th><th>角色</th><th>用户记录</th><th>状态</th><th className="text-right">操作</th></tr></thead>
          <tbody>
            {users.map(u => {
              const tenantId = u.tenant_id?.Valid ? u.tenant_id.Int64 : null
              const tenant = tenantId ? tenant_by_id?.[tenantId] : null
              const isSelf = u.id === currentUser?.id
              return (
                <tr key={u.id}>
                  <td className="font-mono text-xs text-gray-400">{u.id}</td>
                  <td className="font-semibold">{u.username}</td>
                  <td><span className="inline-flex items-center font-mono text-xs bg-gray-100 text-gray-600 px-1.5 py-0.5 rounded">{u.role}</span></td>
                  <td>{tenant ? tenant.name : <span className="text-gray-300">--</span>}</td>
                  <td>{u.disabled ? <Badge color="amber">禁用</Badge> : <Badge color="green">正常</Badge>}</td>
                  <td className="text-right whitespace-nowrap">
                    {isSelf ? (
                      <span className="text-xs text-gray-400">(当前用户)</span>
                    ) : (
                      <>
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
        <div className="p-5 border-t border-gray-100">
          <p className="text-xs text-gray-400">用户记录账号在 <a href="/tenants" className="text-blue-600 font-semibold">用户记录详情页</a> 创建。Admin 账号需直接通过 CLI 或 SQL 添加。</p>
        </div>
      </div>
    </Layout>
  )
}
