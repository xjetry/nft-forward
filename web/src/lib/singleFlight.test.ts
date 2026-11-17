import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createSingleFlight } from './singleFlight.ts'

function deferred() {
  let resolve!: () => void
  let reject!: (err: unknown) => void
  const promise = new Promise<void>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

test('在途时的第二次调用被丢弃，不执行', async () => {
  const gate = createSingleFlight()
  const first = deferred()
  let calls = 0
  const p1 = gate.run(() => { calls++; return first.promise })
  // 同一个事件循环里紧接着再触发一次（双击）
  const p2 = gate.run(async () => { calls++ })
  assert.equal(await p2, false)
  assert.equal(calls, 1)
  assert.equal(gate.busy, true)
  first.resolve()
  assert.equal(await p1, true)
  assert.equal(gate.busy, false)
})

test('上一次落定后可以再次执行', async () => {
  const gate = createSingleFlight()
  let calls = 0
  assert.equal(await gate.run(async () => { calls++ }), true)
  assert.equal(await gate.run(async () => { calls++ }), true)
  assert.equal(calls, 2)
})

test('动作失败后闸门会释放，错误原样抛出', async () => {
  const gate = createSingleFlight()
  await assert.rejects(gate.run(async () => { throw new Error('保存失败') }), /保存失败/)
  assert.equal(gate.busy, false)
  assert.equal(await gate.run(async () => {}), true)
})
