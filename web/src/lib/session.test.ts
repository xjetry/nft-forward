import { test } from 'node:test'
import assert from 'node:assert/strict'
import { answerFromFailure, guard, homeOf, settle, type Session } from './session.ts'

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

test('/me 失败的归类：401 是未登录，403 是被拒绝，其余是没有得到答复', () => {
  assert.deepEqual(answerFromFailure(401, '未登录'), { kind: 'anonymous' })
  assert.deepEqual(answerFromFailure(403, '账号已被禁用'), { kind: 'refused', reason: '账号已被禁用' })
  assert.deepEqual(answerFromFailure(502, 'HTTP 502'), { kind: 'unreachable', message: 'HTTP 502' })
  assert.deepEqual(answerFromFailure(undefined, 'Failed to fetch'), { kind: 'unreachable', message: 'Failed to fetch' })
})

test('没有得到答复不会收回已经确认的登录', () => {
  assert.equal(settle(admin, { kind: 'unreachable', message: 'HTTP 502' }), admin)
})

test('还没确认过的会话遇到没有答复时进入 error', () => {
  for (const prev of [{ status: 'loading' }, { status: 'error', message: '上一次的原因' }] as Session[]) {
    assert.deepEqual(settle(prev, { kind: 'unreachable', message: 'HTTP 502' }), { status: 'error', message: 'HTTP 502' })
  }
})

test('服务端的明确答复总是生效，包括收回已确认的登录', () => {
  assert.deepEqual(settle(admin, { kind: 'anonymous' }), { status: 'anonymous' })
  assert.deepEqual(settle(admin, { kind: 'refused', reason: '账号已被禁用' }), { status: 'anonymous', reason: '账号已被禁用' })
  assert.deepEqual(settle({ status: 'error', message: 'x' }, { kind: 'user', user: user.user }), user)
})

test('被拒绝的会话去登录页，而不是停在“无法连接”', () => {
  const refused = settle({ status: 'loading' }, answerFromFailure(403, '账号已被禁用'))
  assert.deepEqual(guard(refused, 'user'), { kind: 'redirect', to: '/login' })
})
