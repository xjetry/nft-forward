/* 登录会话的状态与路由守卫的判定。

   状态（互斥）：
     loading        还不知道是否已登录（/me 在途）
     anonymous      确认未登录
     authenticated  已登录，带 user
     error          没能确认（面板连不上、5xx）。这不是“未登录”：把它当成
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
  | { readonly status: 'anonymous' }
  | { readonly status: 'authenticated'; readonly user: SessionUser }
  | { readonly status: 'error'; readonly message: string }

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
