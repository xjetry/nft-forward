import { test } from 'node:test'
import assert from 'node:assert/strict'
import { LOADING, combineResources, createLatest, mapResource, reduceResource, runLoad, type Resource, type ResourceEvent } from './resource.ts'

type R = Resource<string>
const ready = (data: string, extra: Partial<Extract<R, { status: 'ready' }>> = {}): R =>
  ({ status: 'ready', data, refreshing: false, refreshError: null, ...extra })
const apply = (state: R, ...events: ResourceEvent<string>[]): R => events.reduce(reduceResource<string>, state)

test('首次加载：loading → ready', () => {
  assert.deepEqual(apply(LOADING, { type: 'fetch' }, { type: 'resolve', data: 'a' }), ready('a'))
})

test('首次加载失败：loading → error，并区分“不存在”', () => {
  assert.deepEqual(apply(LOADING, { type: 'reject', message: '502', notFound: false }), { status: 'error', message: '502', notFound: false })
  assert.deepEqual(apply(LOADING, { type: 'reject', message: '不存在', notFound: true }), { status: 'error', message: '不存在', notFound: true })
})

test('失败后重试：error → loading → ready', () => {
  const failed: R = { status: 'error', message: 'x', notFound: false }
  assert.deepEqual(apply(failed, { type: 'fetch' }), LOADING)
  assert.deepEqual(apply(failed, { type: 'fetch' }, { type: 'resolve', data: 'a' }), ready('a'))
})

test('刷新期间内容留在屏幕上，不回到 loading', () => {
  assert.deepEqual(apply(ready('a'), { type: 'fetch' }), ready('a', { refreshing: true }))
})

test('刷新失败：保留已有内容并记下原因，不变成错误页', () => {
  const state = apply(ready('a'), { type: 'fetch' }, { type: 'reject', message: '网络错误', notFound: false })
  assert.deepEqual(state, ready('a', { refreshing: false, refreshError: '网络错误' }))
})

test('刷新成功会清掉上一次的刷新错误', () => {
  const stale = ready('a', { refreshError: '网络错误' })
  assert.deepEqual(apply(stale, { type: 'fetch' }, { type: 'resolve', data: 'b' }), ready('b'))
})

test('刷新时发现资源已不存在：从 ready 退到 error', () => {
  const state = apply(ready('a'), { type: 'fetch' }, { type: 'reject', message: '不存在', notFound: true })
  assert.deepEqual(state, { status: 'error', message: '不存在', notFound: true })
})

test('reset 丢弃一切：换资源后不会继续显示上一个资源的内容', () => {
  assert.deepEqual(apply(ready('a', { refreshError: 'x' }), { type: 'reset' }), LOADING)
})

test('patch 只在 ready 下生效', () => {
  assert.deepEqual(apply(ready('a'), { type: 'patch', update: d => d + '!' }), ready('a!'))
  assert.deepEqual(apply(LOADING, { type: 'patch', update: d => d + '!' }), LOADING)
})

/** 可手动落定的 Promise，用来控制两次加载的完成顺序。 */
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (err: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function harness() {
  let state: R = LOADING
  const latest = createLatest()
  const dispatch = (event: ResourceEvent<string>) => { state = reduceResource(state, event) }
  const describe = (err: unknown) => ({ message: String(err), notFound: false })
  const load = (fetcher: () => Promise<string>) => runLoad(latest, dispatch, fetcher, describe)
  return { latest, load, get state() { return state } }
}

test('竞态：先发的请求后到，不能覆盖后发请求的结果', async () => {
  const h = harness()
  const first = deferred<string>()
  const second = deferred<string>()
  const p1 = h.load(() => first.promise)
  const p2 = h.load(() => second.promise)
  second.resolve('新')
  await p2
  assert.deepEqual(h.state, ready('新'))
  first.resolve('旧')
  await p1
  assert.deepEqual(h.state, ready('新'))
})

test('竞态：先发请求迟到的失败，也不能把后发请求的成功结果改成刷新失败', async () => {
  const h = harness()
  const first = deferred<string>()
  const second = deferred<string>()
  const p1 = h.load(() => first.promise)
  const p2 = h.load(() => second.promise)
  second.resolve('新')
  await p2
  first.reject('超时')
  await p1
  assert.deepEqual(h.state, ready('新'))
})

test('invalidate 之后在途响应全部作废（卸载 / 换资源）', async () => {
  const h = harness()
  const pending = deferred<string>()
  const p = h.load(() => pending.promise)
  h.latest.invalidate()
  pending.resolve('迟到')
  await p
  assert.deepEqual(h.state, LOADING)
})

test('mapResource 只变换 ready 的 data', () => {
  assert.deepEqual(mapResource(ready('a', { refreshing: true }), d => d.length), { status: 'ready', data: 1, refreshing: true, refreshError: null })
  assert.deepEqual(mapResource<string, number>(LOADING, d => d.length), LOADING)
  const failed: R = { status: 'error', message: 'x', notFound: false }
  assert.deepEqual(mapResource<string, number>(failed, d => d.length), failed)
})

test('combineResources：都就绪才就绪，并合并刷新标记', () => {
  assert.deepEqual(combineResources(ready('a'), ready('b', { refreshing: true })), { status: 'ready', data: ['a', 'b'], refreshing: true, refreshError: null })
  assert.deepEqual(combineResources(ready('a', { refreshError: 'x' }), ready('b')).status, 'ready')
  assert.equal((combineResources(ready('a'), ready('b', { refreshError: 'y' })) as Extract<Resource<unknown>, { status: 'ready' }>).refreshError, 'y')
})

test('combineResources：一个还在加载就是加载中；失败优先于加载中', () => {
  assert.deepEqual(combineResources(ready('a'), LOADING as R), LOADING)
  const failed: R = { status: 'error', message: 'x', notFound: false }
  assert.deepEqual(combineResources(LOADING as R, failed), failed)
})

test('combineResources：“不存在”优先于其它失败', () => {
  const failed: R = { status: 'error', message: 'x', notFound: false }
  const missing: R = { status: 'error', message: '不存在', notFound: true }
  assert.deepEqual(combineResources(failed, missing), missing)
  assert.deepEqual(combineResources(missing, failed), missing)
})
