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
      <div className="card" style={{ maxWidth: 980 }}>
        <div className="card-header"><h3 className="text-[16px] font-bold">修改密码</h3></div>
        <div className="px-6 py-[26px]">
          {error && <div className="mb-4 px-3 py-2 bg-red-50 border border-red-200 rounded text-red-600 text-sm">{error}</div>}
          <form onSubmit={submit}>
            <div className="flex items-center gap-6 mb-[22px]">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">原密码</label>
              <input className="input-field max-w-[560px]" type="password" value={form.old_password} onChange={e => set('old_password', e.target.value)} required autoFocus />
            </div>
            <div className="flex items-center gap-6 mb-[22px]">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">新密码</label>
              <input className="input-field max-w-[560px]" type="password" minLength="6" value={form.new_password} onChange={e => set('new_password', e.target.value)} required />
            </div>
            <div className="flex items-center gap-6 pb-[22px] border-b border-line-soft">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">再次输入</label>
              <input className="input-field max-w-[560px]" type="password" minLength="6" value={form.confirm} onChange={e => set('confirm', e.target.value)} required />
            </div>
            <div className="flex items-center gap-4 mt-[22px]">
              <button type="submit" disabled={loading} className="btn-primary">更新密码</button>
              <span className="text-[13px] text-ink-mut">提交后其他设备/浏览器上的旧会话会被注销，仅当前页保留。</span>
            </div>
          </form>
        </div>
      </div>
    </Layout>
  )
}
