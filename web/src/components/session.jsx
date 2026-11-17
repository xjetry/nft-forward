import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { Navigate, Outlet } from 'react-router-dom'
import { ApiError, api } from '../lib/api'
import { assertNever, createLatest } from '../lib/resource'
import { answerFromFailure, guard, settle } from '../lib/session'
import { ErrorState, Loading } from './ui'

/* 登录会话的提供者。状态模型、/me 答复的归类与守卫判定都在 lib/session。

   面板名称与版本不属于会话状态：登录页在未登录时也要显示面板名称，所以
   单独存放。名称的来源，后到的覆盖先到的：
   - 服务端注入页面的 __BRANDING__，作初值，避免先闪一下默认名；
   - /me 的答复（已登录时）；
   - 公开的 /branding，只在未登录且页面里没有注入值时去取 —— 开发服务器
     不注入，面板名称为空时服务端也不注入（frontend.go 的 renderIndex）；
   - 设置页保存成功后交来的新名称（setPanelName）。 */

/** 没有设置面板名称时显示的名字。 */
const DEFAULT_PANEL_NAME = 'nft-forward'

/** @typedef {import('../lib/session').Session} Session */

const SessionCtx = createContext(/** @type {any} */ (null))

export function SessionProvider({ children }) {
  const [session, setSession] = useState(/** @type {Session} */ ({ status: 'loading' }))
  const [branding, setBranding] = useState({ panelName: window.__BRANDING__?.panel_name || '', version: '' })
  const [latest] = useState(createLatest)

  // 重新确认会话。并发调用时只采用最后一次的结果。返回值说明这一次有没有
  // 得到服务端的答复：没有得到时已登录的会话保持不变（见 settle），需要最新
  // 用户信息的调用方据此提示用户。被后来的调用取代的那一次不下结论，按
  // “得到了答复”返回。
  const refresh = useCallback(async () => {
    const token = latest.begin()
    let answer
    try {
      const data = await api.get('/me')
      if (!latest.isCurrent(token)) return true
      setBranding({ panelName: data?.panel_name || '', version: data?.version || '' })
      answer = data?.user ? { kind: 'user', user: data.user } : { kind: 'anonymous' }
    } catch (err) {
      if (!latest.isCurrent(token)) return true
      answer = answerFromFailure(
        err instanceof ApiError ? err.status : undefined,
        err instanceof Error ? err.message : '无法连接面板',
      )
    }
    setSession(prev => settle(prev, answer))
    return answer.kind !== 'unreachable'
  }, [latest])

  // 面板名称刚在设置页保存成功时调用：侧栏与标签页标题读的是这里的这一份。
  const setPanelName = useCallback((name) => setBranding(b => ({ ...b, panelName: name })), [])

  useEffect(() => { void refresh() }, [refresh])

  const anonymous = session.status === 'anonymous'
  useEffect(() => {
    if (!anonymous || window.__BRANDING__) return undefined
    let live = true
    // 取不到时保持默认名称：登录页照常可用，这里没有值得打断用户的后果。
    api.get('/branding').then(
      (data) => { if (live) setPanelName(data?.panel_name || '') },
      () => {},
    )
    return () => { live = false }
  }, [anonymous, setPanelName])

  const panelName = branding.panelName || DEFAULT_PANEL_NAME
  useEffect(() => { document.title = panelName }, [panelName])

  const value = useMemo(
    () => ({ session, refresh, setPanelName, panelName, version: branding.version }),
    [session, refresh, setPanelName, panelName, branding.version],
  )
  return <SessionCtx.Provider value={value}>{children}</SessionCtx.Provider>
}

/**
 * `panelName` 总是可以直接显示的名字（没有设置时是默认名）。
 * @returns {{
 *   session: Session,
 *   refresh: () => Promise<boolean>,
 *   setPanelName: (name: string) => void,
 *   panelName: string,
 *   version: string,
 * }}
 */
export function useSession() {
  return useContext(SessionCtx)
}

/**
 * 当前登录用户。只在 <RequireAuth> 之下调用，那里 user 一定存在；
 * 在它之外（登录页）user 为 null。
 * @returns {{ user: any, panelName: string, version: string, refresh: () => Promise<boolean> }}
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
