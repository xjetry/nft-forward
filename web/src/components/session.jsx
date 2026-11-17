import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { Navigate, Outlet } from 'react-router-dom'
import { ApiError, api } from '../lib/api'
import { assertNever, createLatest } from '../lib/resource'
import { guard } from '../lib/session'
import { ErrorState, Loading } from './ui'

/* 登录会话的提供者。状态模型与守卫判定在 lib/session。

   面板名称与版本不属于会话状态：登录页在未登录时也要显示面板名称，
   所以单独存放，并用服务端注入页面的 __BRANDING__ 作初值，避免闪一下默认名。 */

/** @typedef {import('../lib/session').Session} Session */

const SessionCtx = createContext(/** @type {any} */ (null))

export function SessionProvider({ children }) {
  const [session, setSession] = useState(/** @type {Session} */ ({ status: 'loading' }))
  const [branding, setBranding] = useState({ panelName: window.__BRANDING__?.panel_name || '', version: '' })
  const [latest] = useState(createLatest)

  // 重新确认会话。并发调用时只采用最后一次的结果。
  const refresh = useCallback(async () => {
    const token = latest.begin()
    try {
      const data = await api.get('/me')
      if (!latest.isCurrent(token)) return
      setBranding({ panelName: data?.panel_name || '', version: data?.version || '' })
      setSession(data?.user ? { status: 'authenticated', user: data.user } : { status: 'anonymous' })
    } catch (err) {
      if (!latest.isCurrent(token)) return
      if (err instanceof ApiError && err.status === 401) setSession({ status: 'anonymous' })
      else setSession({ status: 'error', message: err instanceof Error ? err.message : '无法连接面板' })
    }
  }, [latest])

  useEffect(() => { void refresh() }, [refresh])
  useEffect(() => { if (branding.panelName) document.title = branding.panelName }, [branding.panelName])

  const value = useMemo(() => ({ session, refresh, ...branding }), [session, refresh, branding])
  return <SessionCtx.Provider value={value}>{children}</SessionCtx.Provider>
}

/** @returns {{ session: Session, refresh: () => Promise<void>, panelName: string, version: string }} */
export function useSession() {
  return useContext(SessionCtx)
}

/**
 * 当前登录用户。只在 <RequireAuth> 之下调用，那里 user 一定存在；
 * 在它之外（登录页）user 为 null。
 * @returns {{ user: any, panelName: string, version: string, refresh: () => Promise<void> }}
 */
export function useUser() {
  const { session, refresh, panelName, version } = useSession()
  return { user: session.status === 'authenticated' ? session.user : null, panelName, version, refresh }
}

/**
 * 路由守卫，作为布局路由使用（内容经 <Outlet/> 渲染）。应用里只有这一个
 * 守卫；对各会话状态的处理见 lib/session 的 guard。
 *
 * - 不传 role：只要求已登录。
 * - role="admin"：要求管理员；role="user"：要求普通用户。
 *
 * @param {{ role?: 'admin' | 'user' }} props
 */
export function RequireAuth({ role }) {
  const { session, refresh } = useSession()
  const decision = guard(session, role)
  switch (decision.kind) {
    case 'wait':
      return <div className="grid h-dvh place-items-center bg-app"><Loading /></div>
    case 'unavailable':
      return (
        <div className="grid h-dvh place-items-center bg-app px-4">
          <ErrorState title="无法连接面板" message={decision.message} onRetry={refresh} />
        </div>
      )
    case 'redirect':
      return <Navigate to={decision.to} replace />
    case 'allow':
      return <Outlet />
    default:
      return assertNever(decision)
  }
}
