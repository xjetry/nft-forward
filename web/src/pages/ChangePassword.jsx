import { useState } from 'react'
import { api } from '../lib/api'
import { Layout, useToast } from '../components/Layout'

export default function ChangePassword() {
  const [form, setForm] = useState({ old_password: '', new_password: '', confirm: '' })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setError('')
    if (form.new_password !== form.confirm) {
      setError('两次输入的密码不一致')
      return
    }
    if (form.new_password.length < 6) {
      setError('新密码至少 6 位')
      return
    }
    setLoading(true)
    try {
      await api.post('/change-password', { old_password: form.old_password, new_password: form.new_password })
      toast('密码已更新')
      setForm({ old_password: '', new_password: '', confirm: '' })
    } catch (err) { setError(err.message) } finally { setLoading(false) }
  }

  return (
    <Layout>
      <div className="card" style={{ maxWidth: 560 }}>
        <div className="card-header"><h3 className="text-sm font-bold">修改密码</h3></div>
        <div className="p-5">
          {error && <div className="mb-4 px-3 py-2 bg-red-50 border border-red-200 rounded text-red-600 text-sm">{error}</div>}
          <form onSubmit={submit} className="space-y-4">
            <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
              <label className="fl">原密码</label>
              <input className="input-field" type="password" value={form.old_password} onChange={e => set('old_password', e.target.value)} required autoFocus />
              <label className="fl">新密码</label>
              <input className="input-field" type="password" minLength="6" value={form.new_password} onChange={e => set('new_password', e.target.value)} required />
              <label className="fl">再次输入</label>
              <input className="input-field" type="password" minLength="6" value={form.confirm} onChange={e => set('confirm', e.target.value)} required />
            </div>
            <div className="flex items-center gap-3 pt-4 border-t border-line-soft">
              <button type="submit" disabled={loading} className="btn-primary">更新密码</button>
              <span className="text-xs text-ink-mut">提交后其他设备/浏览器上的旧会话会被注销，仅当前页保留。</span>
            </div>
          </form>
        </div>
      </div>
    </Layout>
  )
}
