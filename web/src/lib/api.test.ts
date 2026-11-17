import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { ApiError, api, setSessionEndedHandler } from './api.js'

const realFetch = globalThis.fetch

/** 让之后的请求都得到同一个答复。 */
function answerWith(status: number, body: unknown) {
  globalThis.fetch = async () => new Response(JSON.stringify(body), {
    status, headers: { 'content-type': 'application/json' },
  })
}

type Outcome =
  | { readonly kind: 'pending' }
  | { readonly kind: 'ok'; readonly value: unknown }
  | { readonly kind: 'failed'; readonly error: unknown }

/**
 * 一个请求的结局。request 在会话结束时返回永不落定的 Promise，所以这里不能
 * 直接 await：等一小段时间，还没落定就报告 pending。
 */
function outcomeOf(promise: Promise<unknown>): Promise<Outcome> {
  return Promise.race<Outcome>([
    promise.then(value => ({ kind: 'ok', value }), error => ({ kind: 'failed', error })),
    new Promise(resolve => setTimeout(() => resolve({ kind: 'pending' }), 20)),
  ])
}

/** 结局是带着这个状态码的 ApiError。 */
function assertFailedWith(outcome: Outcome, status: number, message?: string) {
  assert.equal(outcome.kind, 'failed', `请求的结局是 ${outcome.kind}，应该是抛出`)
  const error = (outcome as { error: unknown }).error
  assert.ok(error instanceof ApiError)
  assert.equal(error.status, status)
  if (message !== undefined) assert.equal(error.message, message)
}

/** 登记一个只计数的会话结束处理，返回读取次数的函数。 */
function countSessionEnded(): () => number {
  let ended = 0
  setSessionEndedHandler(() => { ended++ })
  return () => ended
}

afterEach(() => {
  globalThis.fetch = realFetch
  setSessionEndedHandler(null)
})

test('普通请求被答复 401：通知会话结束，调用方停在原地', async () => {
  const ended = countSessionEnded()
  answerWith(401, { error: '会话已过期' })
  assert.equal((await outcomeOf(api.get('/nodes'))).kind, 'pending')
  assert.equal(ended(), 1)
})

test('写请求被答复 401 同样算会话结束', async () => {
  const ended = countSessionEnded()
  answerWith(401, { error: '会话已过期' })
  assert.equal((await outcomeOf(api.post('/rules', {}))).kind, 'pending')
  assert.equal((await outcomeOf(api.put('/rules/1', {}))).kind, 'pending')
  assert.equal((await outcomeOf(api.patch('/rules/1', {}))).kind, 'pending')
  assert.equal((await outcomeOf(api.del('/rules/1'))).kind, 'pending')
  assert.equal(ended(), 4)
})

test('声明 own401 的请求：401 交还给调用方，不算会话结束', async () => {
  const ended = countSessionEnded()
  answerWith(401, { error: '用户名或密码错误' })
  assertFailedWith(await outcomeOf(api.post('/login', { username: 'a', password: 'b' }, { own401: true })), 401, '用户名或密码错误')
  assertFailedWith(await outcomeOf(api.get('/me', { own401: true })), 401)
  assert.equal(ended(), 0)
})

test('没有人登记会话结束的处理时，401 照常抛出', async () => {
  answerWith(401, { error: '未登录' })
  assertFailedWith(await outcomeOf(api.get('/nodes')), 401, '未登录')
})

test('401 以外的答复不算会话结束', async () => {
  const ended = countSessionEnded()
  answerWith(200, { ok: true })
  assert.deepEqual(await outcomeOf(api.get('/nodes')), { kind: 'ok', value: { ok: true } })
  answerWith(403, { error: '权限不足' })
  assertFailedWith(await outcomeOf(api.get('/nodes')), 403, '权限不足')
  answerWith(500, { error: 'boom' })
  assertFailedWith(await outcomeOf(api.get('/nodes')), 500, 'boom')
  assert.equal(ended(), 0)
})
