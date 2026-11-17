import { test } from 'node:test'
import assert from 'node:assert/strict'
import { guard, homeOf, type Session } from './session.ts'

const admin: Session = { status: 'authenticated', user: { id: 1, username: 'admin', role: 'admin' } }
const user: Session = { status: 'authenticated', user: { id: 2, username: 'alice', role: 'user' } }

test('还不知道是否登录时等待，不跳转', () => {
  for (const role of [undefined, 'admin', 'user'] as const) assert.deepEqual(guard({ status: 'loading' }, role), { kind: 'wait' })
})

test('未登录去登录页', () => {
  for (const role of [undefined, 'admin', 'user'] as const) assert.deepEqual(guard({ status: 'anonymous' }, role), { kind: 'redirect', to: '/login' })
})

test('没能确认会话不等于未登录：显示不可用，不去登录页', () => {
  assert.deepEqual(guard({ status: 'error', message: '网络错误' }, 'admin'), { kind: 'unavailable', message: '网络错误' })
})

test('只要求登录的路由对两种角色都放行', () => {
  assert.deepEqual(guard(admin, undefined), { kind: 'allow' })
  assert.deepEqual(guard(user, undefined), { kind: 'allow' })
})

test('角色相符放行', () => {
  assert.deepEqual(guard(admin, 'admin'), { kind: 'allow' })
  assert.deepEqual(guard(user, 'user'), { kind: 'allow' })
})

test('角色不符时回到该用户自己的首页', () => {
  assert.deepEqual(guard(user, 'admin'), { kind: 'redirect', to: '/my' })
  assert.deepEqual(guard(admin, 'user'), { kind: 'redirect', to: '/' })
  assert.equal(homeOf(admin.user), '/')
  assert.equal(homeOf(user.user), '/my')
})
