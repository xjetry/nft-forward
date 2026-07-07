import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  // The server stamps window.__BRANDING__ into index.html, so the name is
  // known before any request completes and the default never flashes; the
  // /branding fetch only covers dev serving where nothing is injected.
  const [panelName, setPanelName] = useState(window.__BRANDING__?.panel_name || '')
  const navigate = useNavigate()

  useEffect(() => {
    if (window.__BRANDING__) return
    api.get('/branding').then(d => setPanelName(d?.panel_name || '')).catch(() => {})
  }, [])

  useEffect(() => {
    if (panelName) document.title = panelName
  }, [panelName])

  const submit = async (e) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      await api.post('/login', { username, password })
      window.location.href = '/'
    } catch (err) {
      setError(err.message || '登录失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen grid place-items-center bg-app">
      <div className="bg-surface border border-line rounded-2xl p-9 w-[380px] shadow-[0_24px_70px_-20px_rgba(0,0,0,0.7)]">
        <div className="flex items-center gap-3 mb-7">
          <div className="w-[42px] h-[42px] rounded-[11px] grid place-items-center text-white shadow-[0_6px_18px_-6px_rgba(74,108,247,0.7)]"
            style={{ background: 'linear-gradient(150deg, #5b7cfa, #3a5bef)' }}>
            <svg className="w-[22px] h-[22px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M17 7 21 11 17 15"/><path d="M21 11H7"/><path d="M7 17 3 13 7 9"/><path d="M3 13H17"/></svg>
          </div>
          <div>
            <div className="text-[16px] font-bold">{panelName || 'nft-forward'}</div>
            <div className="text-[12px] text-ink-mut mt-0.5">转发管理面板</div>
          </div>
        </div>

        {error && (
          <div className="mb-4 px-3 py-2 bg-red-50 border border-red-200 rounded text-red-600 text-sm">{error}</div>
        )}

        <form onSubmit={submit} className="flex flex-col gap-3.5">
          <div>
            <label className="block text-[13px] font-semibold text-ink-soft mb-1.5">用户名</label>
            <input className="input-field" value={username} onChange={e => setUsername(e.target.value)} required autoFocus />
          </div>
          <div>
            <label className="block text-[13px] font-semibold text-ink-soft mb-1.5">密码</label>
            <input className="input-field" type="password" value={password} onChange={e => setPassword(e.target.value)} required />
          </div>
          <button type="submit" disabled={loading}
            className="mt-3 w-full h-10 bg-blue-600 text-white rounded-[7px] text-[13px] font-semibold hover:bg-blue-700 disabled:opacity-60 transition-colors flex items-center justify-center">
            {loading ? <div className="w-4 h-4 border-2 border-white border-t-transparent rounded-full animate-spin" /> : '登录'}
          </button>
        </form>
      </div>
    </div>
  )
}
