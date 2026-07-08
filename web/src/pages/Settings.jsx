import { useState, useEffect } from 'react'
import { api } from '../lib/api'
import { Layout, useToast } from '../components/Layout'
import { Loading } from '../components/ui'
import { PageHeader, Panel } from '../components/page'

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
      <div className="admin-settings-page">
        <PageHeader title="系统设置" />
        <Panel className="admin-settings-panel">
          <div className="settings-panel-head">
            <div className="settings-panel-title-wrap">
              <span className="settings-panel-icon" aria-hidden="true">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
              </span>
              <div>
                <h3>面板配置</h3>
                <p>基础展示与转发运行参数</p>
              </div>
            </div>
          </div>

          <form onSubmit={submit} className="settings-form">
            {error && <div className="settings-error">{error}</div>}

            <section className="settings-section">
              <div className="settings-section-copy">
                <span>面板信息</span>
                <p>登录页、侧栏和用户通知里展示的基础信息</p>
              </div>
              <div className="settings-fields">
                <div className="settings-row">
                  <label>面板地址</label>
                  <input className="input-field settings-input" type="text" placeholder="https://panel.example.com" value={form.panel_url} onChange={e => set('panel_url', e.target.value)} />
                </div>
                <div className="settings-row">
                  <label>面板名称</label>
                  <input className="input-field settings-input" type="text" placeholder="nft-forward" value={form.panel_name} onChange={e => set('panel_name', e.target.value)} />
                </div>
              </div>
            </section>

            <section className="settings-section">
              <div className="settings-section-copy">
                <span>转发设置</span>
                <p>影响普通用户可见信息和连接池行为</p>
              </div>
              <div className="settings-fields">
                <div className="settings-row settings-row-inline">
                  <label>显示倍率</label>
                  <div className="settings-control-line">
                    <button type="button" role="switch" aria-checked={form.show_rate_to_user}
                      className={`settings-switch ${form.show_rate_to_user ? 'is-on' : ''}`}
                      onClick={() => set('show_rate_to_user', !form.show_rate_to_user)}>
                      <span />
                    </button>
                    <span className="settings-help">向普通用户展示节点/链路倍率</span>
                  </div>
                </div>

                <div className="settings-row settings-row-inline">
                  <label>TCP 连接池</label>
                  <div className="settings-control-line">
                    <input className="input-field settings-number-input" type="number" min="0" max="64" value={form.pool_size} onChange={e => set('pool_size', e.target.value)} />
                    <span className="settings-help">每端口预建立连接数（0 = 禁用，默认 4）</span>
                  </div>
                </div>
              </div>
            </section>

            <div className="settings-actions">
              <button type="submit" disabled={saving} className="btn-primary">{saving ? '保存中…' : '保存设置'}</button>
            </div>
          </form>
        </Panel>
      </div>
    </Layout>
  )
}
