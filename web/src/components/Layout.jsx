import { createContext, useContext, useState, useEffect, useCallback, useRef } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { api } from '../lib/api'

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
  const [blurred, setBlurred] = useState(() => localStorage.getItem('nf-blur') === '1')

  const toggleBlur = () => {
    setBlurred(v => {
      localStorage.setItem('nf-blur', v ? '0' : '1')
      return !v
    })
  }

  const handleLogout = async () => {
    try { await fetch('/api/logout', { method: 'POST' }) } catch {}
    window.location.href = '/login'
  }

  if (!user) return null

  const isAdmin = user.role === 'admin'

  return (
    <BlurCtx.Provider value={blurred}>
      <div className="flex min-h-screen bg-[#eef1f5]">
        {/* Mobile overlay */}
        {sideOpen && <div className="fixed inset-0 bg-black/30 z-30 lg:hidden" onClick={() => setSideOpen(false)} />}

        {/* Sidebar */}
        <aside className={`fixed inset-y-0 left-0 z-40 w-[248px] flex flex-col transition-transform lg:translate-x-0 lg:static lg:z-auto ${sideOpen ? 'translate-x-0' : '-translate-x-full'}`}
          style={{ background: 'linear-gradient(180deg, #10151c, #0b0f15)' }}>

          {/* Brand */}
          <div className="flex items-center gap-3 px-5 pt-5 pb-4">
            <div className="w-[34px] h-[34px] rounded-[9px] flex-none grid place-items-center text-white shadow-[0_4px_14px_rgba(37,99,235,0.4)]"
              style={{ background: 'linear-gradient(150deg, #3b82f6, #1e40af)' }}>
              <svg className="w-[18px] h-[18px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 7h11M4 7l3-3M4 7l3 3"/><path d="M20 17H9M20 17l-3-3M20 17l-3 3"/></svg>
            </div>
            <div>
              <div className="text-[15px] font-bold tracking-wide text-[#f3f6fa]">nft-forward</div>
              <div className="text-[11px] text-[#6b7686] font-mono mt-px">{isAdmin ? '管理面板' : '用户面板'}</div>
            </div>
          </div>

          {/* Nav */}
          <nav className="flex-1 overflow-y-auto px-3 py-2">
            {isAdmin ? (
              <>
                <NavGroup label="监控">
                  <SideLink to="/" icon={<IconDashboard />} end>概览</SideLink>
                </NavGroup>
                <NavGroup label="资源">
                  <SideLink to="/nodes" icon={<IconNodes />}>节点</SideLink>
                  <SideLink to="/tunnels" icon={<IconTunnels />}>通道</SideLink>
                  <SideLink to="/combos" icon={<IconCombos />}>组合通道</SideLink>
                  <SideLink to="/tenants" icon={<IconTenants />}>用户</SideLink>
                </NavGroup>
                <NavGroup label="转发">
                  <SideLink to="/forwards" icon={<IconForwards />}>转发规则</SideLink>
                  <SideLink to="/chains" icon={<IconChains />}>中继链路</SideLink>
                </NavGroup>
                <NavGroup label="系统">
                  <SideLink to="/users" icon={<IconUsers />}>账号</SideLink>
                </NavGroup>
              </>
            ) : (
              <>
                <NavGroup label="概况">
                  <SideLink to="/my" icon={<IconDashboard />} end>概览</SideLink>
                </NavGroup>
                <NavGroup label="转发">
                  <SideLink to="/my/forwards" icon={<IconForwards />}>我的转发</SideLink>
                  <SideLink to="/my/chains" icon={<IconChains />}>我的链路</SideLink>
                </NavGroup>
              </>
            )}
          </nav>

          {/* Footer */}
          <div className="border-t border-[#1e2632] p-3">
            <div className="flex items-center gap-2.5 px-2 py-1.5">
              <div className="w-[30px] h-[30px] rounded-lg bg-[#26323f] text-[#cdd6e2] grid place-items-center font-bold text-[13px] flex-none">
                {user.username?.charAt(0).toUpperCase()}
              </div>
              <div className="min-w-0">
                <div className="text-[13px] text-[#e2e8f0] font-semibold leading-tight truncate">{user.username}</div>
                <div className="text-[11px] text-[#6b7686] font-mono">{user.role}</div>
              </div>
            </div>
            <div className="flex gap-1.5 mt-2">
              <NavLink to="/change-password" className="flex-1 text-center text-[12px] text-[#9aa6b6] py-1.5 rounded-[7px] border border-[#1e2632] hover:bg-[#161d27] hover:text-[#cdd6e2] transition-colors">修改密码</NavLink>
              <button onClick={handleLogout} className="flex-1 text-center text-[12px] text-[#9aa6b6] py-1.5 rounded-[7px] border border-[#1e2632] hover:bg-[#161d27] hover:text-[#cdd6e2] transition-colors">退出登录</button>
            </div>
          </div>
        </aside>

        {/* Content */}
        <main className="flex-1 min-w-0 flex flex-col">
          {/* Topbar */}
          <div className="sticky top-0 z-20 bg-[#eef1f5]/85 backdrop-blur-sm border-b border-[#e3e7ec] px-4 sm:px-8 py-4 flex items-center gap-3">
            <button onClick={() => setSideOpen(true)} className="lg:hidden p-1 text-gray-500 hover:text-gray-700">
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M3 12h18M3 6h18M3 18h18"/></svg>
            </button>
            <div className="flex-1" />
            <button onClick={toggleBlur} title="模糊敏感信息"
              className={`inline-flex items-center gap-1.5 text-[12px] px-2.5 py-1 rounded-md border transition-colors ${blurred ? 'text-blue-600 bg-blue-50 border-blue-200' : 'text-gray-400 border-transparent hover:bg-gray-100'}`}>
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
              脱敏
            </button>
          </div>

          {/* Page content */}
          <div className="flex-1 overflow-y-auto px-4 sm:px-8 py-6 pb-14">
            {children}
          </div>
        </main>
      </div>
    </BlurCtx.Provider>
  )
}

/* ---------- Blur context ---------- */
const BlurCtx = createContext(false)
export function useBlur() { return useContext(BlurCtx) }

/* ---------- Nav helpers ---------- */
function NavGroup({ label, children }) {
  return (
    <div className="mt-4">
      <div className="px-3 pb-1.5 text-[10.5px] font-semibold tracking-wider uppercase text-[#6b7686]">{label}</div>
      {children}
    </div>
  )
}

function SideLink({ to, icon, end, children }) {
  return (
    <NavLink to={to} end={end}
      className={({ isActive }) =>
        `flex items-center gap-2.5 px-3 py-2 rounded-lg text-[13.5px] font-medium transition-colors relative ${isActive
          ? 'bg-[#1b2531] text-[#e8edf4] before:content-[""] before:absolute before:-left-3 before:top-1/2 before:-translate-y-1/2 before:w-[3px] before:h-[18px] before:rounded-r before:bg-blue-600'
          : 'text-[#9aa6b6] hover:bg-[#161d27] hover:text-[#cdd6e2]'}`
      }>
      <span className="w-[17px] h-[17px] flex-none opacity-85">{icon}</span>
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
function IconTunnels() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 6h16M4 12h16M4 18h16"/><circle cx="8" cy="6" r="1.6" fill="currentColor" stroke="none"/><circle cx="16" cy="12" r="1.6" fill="currentColor" stroke="none"/><circle cx="10" cy="18" r="1.6" fill="currentColor" stroke="none"/></svg>
}
function IconCombos() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><path d="M17.5 14v7M14 17.5h7"/></svg>
}
function IconTenants() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="9" cy="8" r="3.2"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/><path d="M16 8.5a3 3 0 0 1 0 5.5M18 19a5 5 0 0 0-3-4.6"/></svg>
}
function IconForwards() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 12h12"/><path d="M13 7l5 5-5 5"/><path d="M20 5v14"/></svg>
}
function IconChains() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9.5 14.5l5-5"/><path d="M8 9l-2.5 2.5a3.5 3.5 0 0 0 5 5L13 14"/><path d="M16 15l2.5-2.5a3.5 3.5 0 0 0-5-5L11 10"/></svg>
}
function IconUsers() {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/></svg>
}
