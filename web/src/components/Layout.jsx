import { createContext, useContext, useState, useEffect, useCallback, useRef } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { resolvedDark, getStoredTheme, setStoredTheme } from '../lib/theme'
import { hasLocalURIs, hasLocalProxies } from '../lib/landing'

/* ---------- User context ---------- */
const UserCtx = createContext(null)
const ToastCtx = createContext(() => {})

export function useUser() { return useContext(UserCtx) }
export function useToast() { return useContext(ToastCtx) }

export function UserProvider({ children }) {
  const [user, setUser] = useState(undefined) // undefined = loading, null = not logged in
  const [toasts, setToasts] = useState([])
  const idRef = useRef(0)

  useEffect(() => {
    api.get('/me').then(data => setUser(data?.user ?? null)).catch(() => setUser(null))
  }, [])

  const toast = useCallback((msg) => {
    const id = ++idRef.current
    setToasts(prev => [...prev, { id, msg }])
    setTimeout(() => setToasts(prev => prev.filter(t => t.id !== id)), 2000)
  }, [])

  return (
    <UserCtx.Provider value={{ user, setUser }}>
      <ToastCtx.Provider value={toast}>
        {children}
        {/* Toast stack */}
        <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-[100] flex flex-col gap-2 items-center">
          {toasts.map(t => (
            <div key={t.id} className="bg-gray-900 text-white px-4 py-2.5 rounded-lg text-sm font-medium shadow-lg flex items-center gap-2 animate-toast">
              <svg className="w-4 h-4 text-green-400" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5"/></svg>
              {t.msg}
            </div>
          ))}
        </div>
      </ToastCtx.Provider>
    </UserCtx.Provider>
  )
}

/* ---------- Layout (sidebar + content) ---------- */
export function Layout({ children }) {
  const { user } = useUser()
  const navigate = useNavigate()
  const [sideOpen, setSideOpen] = useState(false)
  const { blurred, toggleBlur } = useContext(BlurCtx)
  const { copyFmt, toggleCopyFmt } = useContext(CopyFmtCtx)
  const [theme, setThemeState] = useState(getStoredTheme())
  const isDark = resolvedDark(theme)

  // The landing-nodes entry shows when the user has an admin-assigned source or
  // their own browser-local URIs. Local URIs change in the same tab, which the
  // native 'storage' event misses, so re-check on our custom event too.
  const [, bumpLanding] = useState(0)
  useEffect(() => {
    const h = () => bumpLanding(t => t + 1)
    window.addEventListener('nf-landing-changed', h)
    window.addEventListener('storage', h)
    return () => { window.removeEventListener('nf-landing-changed', h); window.removeEventListener('storage', h) }
  }, [])

  const toggleTheme = () => {
    const next = isDark ? 'light' : 'dark'
    setStoredTheme(next)
    setThemeState(next)
  }

  const handleLogout = async () => {
    try { await fetch('/api/logout', { method: 'POST' }) } catch {}
    window.location.href = '/login'
  }

  if (!user) return null

  const isAdmin = user.role === 'admin'

  return (
    <div className="flex h-screen overflow-hidden bg-app">
        {/* Mobile overlay */}
        {sideOpen && <div className="fixed inset-0 bg-black/30 z-30 lg:hidden" onClick={() => setSideOpen(false)} />}

        {/* Sidebar */}
        <aside className={`fixed inset-y-0 left-0 z-40 w-[248px] flex flex-col transition-transform lg:translate-x-0 lg:static lg:z-auto ${sideOpen ? 'translate-x-0' : '-translate-x-full'}`}
          style={{ background: '#0c0e13', borderRight: '1px solid #181b22' }}>

          {/* Brand */}
          <div className="flex items-center gap-3 px-5 pt-5 pb-5">
            <div className="w-[42px] h-[42px] rounded-[11px] flex-none grid place-items-center text-white shadow-[0_6px_18px_-6px_rgba(74,108,247,0.7)]"
              style={{ background: 'linear-gradient(150deg, #5b7cfa, #3a5bef)' }}>
              <svg className="w-[22px] h-[22px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M17 7 21 11 17 15"/><path d="M21 11H7"/><path d="M7 17 3 13 7 9"/><path d="M3 13H17"/></svg>
            </div>
            <div>
              <div className="text-[16px] font-bold tracking-wide text-[#f5f7fa]">nft-forward</div>
              <div className="text-[12px] text-[#6b7280] mt-0.5">{isAdmin ? '管理面板' : '用户面板'}</div>
            </div>
          </div>

          {/* Nav */}
          <nav className="flex-1 overflow-y-auto px-4 py-2">
            {isAdmin ? (
              <>
                <NavGroup label="监控">
                  <SideLink to="/" icon={<IconDashboard />} end>概览</SideLink>
                </NavGroup>
                <NavGroup label="资源">
                  <SideLink to="/nodes" icon={<IconNodes />}>节点</SideLink>
                  <SideLink to="/rules" icon={<IconForwards />}>规则</SideLink>
                  <SideLink to="/users" icon={<IconUserGroup />}>用户</SideLink>
                  {hasLocalProxies(user.username) && <SideLink to="/proxies" icon={<IconProxy />}>我的代理</SideLink>}
                </NavGroup>
              </>
            ) : (
              <>
                <NavGroup label="概况">
                  <SideLink to="/my" icon={<IconDashboard />} end>概览</SideLink>
                </NavGroup>
                <NavGroup label="转发">
                  <SideLink to="/my/rules" icon={<IconForwards />}>我的规则</SideLink>
                  {hasLocalProxies(user.username) && <SideLink to="/proxies" icon={<IconProxy />}>我的代理</SideLink>}
                  {(user.has_landing_source || hasLocalURIs(user.username)) && <SideLink to="/my/landing" icon={<IconNodes />}>落地节点</SideLink>}
                </NavGroup>
              </>
            )}
          </nav>

          {/* Footer */}
          <div className="border-t border-[#181b22] p-4 pt-3.5">
            <div className="flex items-center gap-[11px] px-2 py-1.5 mb-3.5">
              <div className="w-[34px] h-[34px] rounded-[9px] bg-[#1b1f27] border border-[#262b34] grid place-items-center font-bold text-[14px] text-[#cbd2dd] flex-none">
                {user.username?.charAt(0).toUpperCase()}
              </div>
              <div className="min-w-0">
                <div className="text-[13.5px] text-[#e7eaf0] font-semibold leading-tight truncate">{user.username}</div>
                <div className="text-[12px] text-[#6b7280] mt-px">{user.role}</div>
              </div>
            </div>
            <div className="flex gap-2">
              <NavLink to="/change-password" className="flex-1 text-center text-[12.5px] text-[#aeb6c2] py-2 rounded-lg bg-[#15181f] border border-[#232730] hover:bg-[#161d27] hover:text-[#cdd6e2] transition-colors">修改密码</NavLink>
              <button onClick={handleLogout} className="flex-1 text-center text-[12.5px] text-[#aeb6c2] py-2 rounded-lg bg-[#15181f] border border-[#232730] hover:bg-[#161d27] hover:text-[#cdd6e2] transition-colors">退出登录</button>
            </div>
          </div>
        </aside>

        {/* Content */}
        <main className="flex-1 min-w-0 flex flex-col">
          {/* Topbar */}
          <div className="sticky top-0 z-20 h-[60px] flex-shrink-0 bg-app/85 backdrop-blur-sm border-b border-line px-4 sm:px-7 flex items-center gap-2">
            <button onClick={() => setSideOpen(true)} className="lg:hidden p-1 text-ink-soft hover:text-ink">
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M3 12h18M3 6h18M3 18h18"/></svg>
            </button>
            <div className="flex-1" />
            <button onClick={toggleTheme} title={isDark ? '切换到浅色' : '切换到深色'}
              className="inline-flex items-center gap-1.5 text-[12px] px-2.5 py-1 rounded-md border border-transparent text-ink-mut hover:bg-raised transition-colors">
              {isDark ? (
                <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
              ) : (
                <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>
              )}
              {isDark ? '浅色' : '深色'}
            </button>
            <button onClick={toggleCopyFmt} title="切换复制代理连接的格式（URI / YAML）"
              className={`inline-flex items-center gap-1.5 text-[12px] px-2.5 py-1 rounded-md border transition-colors ${copyFmt === 'yaml' ? 'text-blue-600 bg-blue-50 border-blue-200' : 'text-ink-mut border-transparent hover:bg-raised'}`}>
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M16 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V8Z"/><path d="M15 3v4a2 2 0 0 0 2 2h4"/></svg>
              {copyFmt === 'yaml' ? 'YAML' : 'URI'}
            </button>
            <button onClick={toggleBlur} title="模糊敏感信息"
              className={`inline-flex items-center gap-1.5 text-[12px] px-2.5 py-1 rounded-md border transition-colors ${blurred ? 'text-blue-600 bg-blue-50 border-blue-200' : 'text-ink-mut border-transparent hover:bg-raised'}`}>
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
              脱敏
            </button>
          </div>

          {/* Page content */}
          <div className="flex-1 overflow-y-auto px-4 sm:px-7 py-7 pb-12">
            <div className="max-w-[1680px] mx-auto">
              {children}
            </div>
          </div>
        </main>
    </div>
  )
}

/* ---------- Blur context ---------- */
/* The provider is mounted above the routes (App) so the topbar toggle inside
   Layout and the pages reading useBlur() share one state. When the provider
   sat inside Layout, every page rendered Layout as its own child, so the
   page's useBlur() resolved above the provider and always read the default —
   the toggle never reached the page content. */
const BlurCtx = createContext({ blurred: false, toggleBlur: () => {} })
export function useBlur() { return useContext(BlurCtx).blurred }

export function BlurProvider({ children }) {
  const [blurred, setBlurred] = useState(() => localStorage.getItem('nf-blur') === '1')
  const toggleBlur = useCallback(() => {
    setBlurred(v => {
      localStorage.setItem('nf-blur', v ? '0' : '1')
      return !v
    })
  }, [])
  return <BlurCtx.Provider value={{ blurred, toggleBlur }}>{children}</BlurCtx.Provider>
}

/* ---------- Copy-format context ---------- */
const CopyFmtCtx = createContext({ copyFmt: 'uri', toggleCopyFmt: () => {} })
export function useCopyFmt() { return useContext(CopyFmtCtx) }

export function CopyFmtProvider({ children }) {
  const [copyFmt, setCopyFmt] = useState(() => localStorage.getItem('nf-copy-fmt') || 'uri')
  const toggleCopyFmt = useCallback(() => {
    setCopyFmt(f => {
      const next = f === 'uri' ? 'yaml' : 'uri'
      localStorage.setItem('nf-copy-fmt', next)
      return next
    })
  }, [])
  return <CopyFmtCtx.Provider value={{ copyFmt, toggleCopyFmt }}>{children}</CopyFmtCtx.Provider>
}

/* ---------- Nav helpers ---------- */
function NavGroup({ label, children }) {
  return (
    <div className="mt-4">
      <div className="px-[10px] pb-2 text-[11px] font-semibold tracking-[1px] uppercase text-[#5a616d]">{label}</div>
      <div className="flex flex-col gap-1">{children}</div>
    </div>
  )
}

function SideLink({ to, icon, end, children }) {
  return (
    <NavLink to={to} end={end}
      className={({ isActive }) =>
        `flex items-center gap-3 px-3 py-2.5 rounded-[9px] text-[14px] font-medium transition-colors relative ${isActive
          ? 'bg-[#181c24] text-[#f3f5f8] border border-[#262b35] shadow-[inset_0_1px_0_rgba(255,255,255,0.03)]'
          : 'text-[#8b93a0] border border-transparent hover:bg-[#161d27] hover:text-[#cdd6e2]'}`
      }>
      <span className="w-5 h-5 flex-none opacity-85">{icon}</span>
      <span>{children}</span>
    </NavLink>
  )
}

/* ---------- Icons ---------- */
function IconDashboard() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/></svg>
}
function IconNodes() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/></svg>
}
function IconUserGroup() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="9" cy="8" r="3.2"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/><path d="M16 8.5a3 3 0 0 1 0 5.5M18 19a5 5 0 0 0-3-4.6"/></svg>
}
function IconForwards() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 12h12"/><path d="M13 7l5 5-5 5"/><path d="M20 5v14"/></svg>
}
function IconProxy() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
}
