import { useState, useEffect } from 'react'
import { api } from '../lib/api'
import { Layout, useToast } from '../components/Layout'
import { Loading } from '../components/ui'

export default function Settings() {
  const [form, setForm] = useState({ panel_url: '', panel_name: '', show_rate_to_user: false, pool_size: 4 })
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const toast = useToast()

  useEffect(() => {
    api.get('/settings').then(data => {
      setForm({
        panel_url: data.panel_url || '',
        panel_name: data.panel_name || '',
        show_rate_to_user: !!data.show_rate_to_user,
        pool_size: data.pool_size ?? 4,
      })
    }).catch(e => setError(e.message)).finally(() => setLoading(false))
  }, [])

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setError('')
    const ps = parseInt(form.pool_size, 10)
    if (isNaN(ps) || ps < 0 || ps > 64) {
      setError('TCP 连接池数必须在 0-64 之间')
      return
    }
    setSaving(true)
    try {
      await api.post('/settings', {
        panel_url: form.panel_url,
        panel_name: form.panel_name,
        show_rate_to_user: form.show_rate_to_user,
        pool_size: ps,
      })
      toast('设置已保存')
    } catch (err) { setError(err.message) } finally { setSaving(false) }
  }

  if (loading) return <Layout><Loading /></Layout>

  return (
    <Layout>
      <h1 className="m-0 text-2xl font-bold text-ink mb-[22px]">系统设置</h1>
      <div className="card" style={{ maxWidth: 980 }}>
        <div className="card-header"><h3 className="text-[16px] font-bold">面板信息</h3></div>
        <div className="px-6 py-[26px]">
          {error && <div className="mb-4 px-3 py-2 bg-red-50 border border-red-200 rounded text-red-600 text-sm">{error}</div>}
          <form onSubmit={submit}>
            <div className="flex items-center gap-6 mb-[22px]">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">面板地址</label>
              <input className="input-field max-w-[560px]" type="text" placeholder="https://panel.example.com" value={form.panel_url} onChange={e => set('panel_url', e.target.value)} />
            </div>
            <div className="flex items-center gap-6 pb-[22px] border-b border-line-soft">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">面板名称</label>
              <input className="input-field max-w-[560px]" type="text" placeholder="nft-forward" value={form.panel_name} onChange={e => set('panel_name', e.target.value)} />
            </div>

            <div className="pt-[22px]">
              <h3 className="text-[16px] font-bold text-ink mb-[22px]">转发设置</h3>
            </div>

            <div className="flex items-center gap-6 mb-[22px]">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">显示倍率</label>
              <button type="button" role="switch" aria-checked={form.show_rate_to_user}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${form.show_rate_to_user ? 'bg-blue-600' : 'bg-gray-600'}`}
                onClick={() => set('show_rate_to_user', !form.show_rate_to_user)}>
                <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${form.show_rate_to_user ? 'translate-x-6' : 'translate-x-1'}`} />
              </button>
              <span className="text-[13px] text-ink-mut">向普通用户展示节点/链路倍率</span>
            </div>

            <div className="flex items-center gap-6 pb-[22px] border-b border-line-soft">
              <label className="w-[110px] flex-shrink-0 text-[14px] text-ink-soft">TCP 连接池</label>
              <input className="input-field w-[100px]" type="number" min="0" max="64" value={form.pool_size} onChange={e => set('pool_size', e.target.value)} />
              <span className="text-[13px] text-ink-mut">每端口预建立连接数（0 = 禁用，默认 4）</span>
            </div>

            <div className="flex items-center gap-4 mt-[22px]">
              <button type="submit" disabled={saving} className="btn-primary">{saving ? '保存中…' : '保存设置'}</button>
            </div>
          </form>
        </div>
      </div>
    </Layout>
  )
}
