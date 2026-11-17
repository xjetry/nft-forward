/* 登录会话的状态、/me 的答复如何改变它，以及路由守卫的判定。

   状态（互斥）：
     loading        还不知道是否已登录（/me 在途）
     anonymous      服务端明确答复：现在没有可用的登录。reason 是服务端给出
                    的原因（账号被禁用），单纯没登录时没有 reason。
     authenticated  已登录，带 user
     error          没能得到答复（面板连不上、5xx）。这不是“未登录”：把它当成
                    未登录会把用户踢到登录页，而他的会话其实还有效。 */

import { assertNever } from './resource.ts'

export interface SessionUser {
  readonly id: number
  readonly username: string
  readonly role: string
  readonly [key: string]: unknown
}

export type Session =
  | { readonly status: 'loading' }
  | { readonly status: 'anonymous'; readonly reason?: string }
  | { readonly status: 'authenticated'; readonly user: SessionUser }
  | { readonly status: 'error'; readonly message: string }

/**
 * 一次 /me 请求的结论。
 *
 * refused 与 unreachable 都是“请求失败”，但含义相反：refused 是服务端认得
 * 这个会话并拒绝它（requireAPIAuth 对被禁用的账号回 403），重试不会有别的
 * 结果；unreachable 是根本没有得到答复，会话是否有效仍然未知。
 */
export type MeAnswer =
  | { readonly kind: 'user'; readonly user: SessionUser }
  | { readonly kind: 'anonymous' }
  | { readonly kind: 'refused'; readonly reason: string }
  | { readonly kind: 'unreachable'; readonly message: string }

/** 把失败的 /me 请求归类。status 是 HTTP 状态码，请求没能发出去时为 undefined。 */
export function answerFromFailure(status: number | undefined, message: string): MeAnswer {
  if (status === 401) return { kind: 'anonymous' }
  if (status === 403) return { kind: 'refused', reason: message }
  return { kind: 'unreachable', message }
}

/**
 * 会话在一次 /me 结论之后的状态。
 *
 * 不变式：已经确认的登录只会被服务端的明确答复（未登录、被拒绝）收回，
 * 不会被一次没有答复的请求收回 —— 那只说明这一次没问到，用户正在用的
 * 页面不应该因此被换成整屏错误。还没确认过的会话（loading / error）遇到
 * 没有答复时才进入 error。
 */
export function settle(prev: Session, answer: MeAnswer): Session {
  switch (answer.kind) {
    case 'user':
      return { status: 'authenticated', user: answer.user }
    case 'anonymous':
      return { status: 'anonymous' }
    case 'refused':
      return { status: 'anonymous', reason: answer.reason }
    case 'unreachable':
      return prev.status === 'authenticated' ? prev : { status: 'error', message: answer.message }
    default:
      return assertNever(answer)
  }
}

/** 路由要求的角色。'user' 指“非管理员”：两种角色的页面集合不相交。 */
export type RequiredRole = 'admin' | 'user' | undefined

export type GuardDecision =
  | { readonly kind: 'wait' }
  | { readonly kind: 'unavailable'; readonly message: string }
  | { readonly kind: 'redirect'; readonly to: string }
  | { readonly kind: 'allow' }

/** 某个用户登录后的首页。 */
export function homeOf(user: SessionUser): string {
  return user.role === 'admin' ? '/' : '/my'
}

/**
 * 受保护路由对一个会话状态的处理。应用里所有受保护的路由都经过这一个
 * 函数，所以它们对四种状态的处理是一致的。
 *
 * 角色不符时去往该用户自己的首页，而不是显示“无权限”：管理员没有理由
 * 看到 /my，普通用户没有理由看到 /nodes，把人送回他能用的地方即可。
 */
export function guard(session: Session, role: RequiredRole): GuardDecision {
  switch (session.status) {
    case 'loading':
      return { kind: 'wait' }
    case 'error':
      return { kind: 'unavailable', message: session.message }
    case 'anonymous':
      return { kind: 'redirect', to: '/login' }
    case 'authenticated': {
      const isAdmin = session.user.role === 'admin'
      if (role && (role === 'admin') !== isAdmin) return { kind: 'redirect', to: homeOf(session.user) }
      return { kind: 'allow' }
    }
    default:
      return assertNever(session)
  }
}
