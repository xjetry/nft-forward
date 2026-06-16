import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()

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
    <div className="min-h-screen grid place-items-center" style={{ background: 'linear-gradient(180deg, #10151c, #0b0f15)' }}>
      <div className="bg-surface rounded-xl p-9 w-[380px] shadow-[0_16px_48px_rgba(0,0,0,0.25)]">
        <div className="flex items-center gap-3 mb-7">
          <div className="w-[38px] h-[38px] rounded-[10px] grid place-items-center text-white shadow-[0_4px_14px_rgba(37,99,235,0.4)]"
            style={{ background: 'linear-gradient(150deg, #3b82f6, #1e40af)' }}>
            <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 7h11M4 7l3-3M4 7l3 3"/><path d="M20 17H9M20 17l-3-3M20 17l-3 3"/></svg>
          </div>
          <div>
            <div className="text-[15px] font-bold">nft-forward</div>
            <div className="text-[11px] text-ink-mut font-mono mt-px">转发管理面板</div>
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
