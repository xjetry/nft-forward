import { test } from 'node:test'
import assert from 'node:assert/strict'
import { IDLE, batchProgress, probeStateOf, probeText, reduceProbes, type ProbeEvent, type ProbeTable } from './probe.ts'

const apply = (table: ProbeTable, ...events: ProbeEvent[]) => events.reduce(reduceProbes, table)
const OK = { ok: true, latency_ms: 12 }

test('没测过的对象是 idle', () => {
  assert.deepEqual(probeStateOf({}, 'r1'), IDLE)
})

test('一次完整探测：idle → probing → done', () => {
  const probing = apply({}, { type: 'start', key: 'r1', token: 1 })
  assert.deepEqual(probeStateOf(probing, 'r1'), { status: 'probing', token: 1 })
  const done = apply(probing, { type: 'done', key: 'r1', token: 1, result: OK })
  assert.deepEqual(probeStateOf(done, 'r1'), { status: 'done', result: OK })
})

test('探测请求失败与“不通”是两种状态', () => {
  const failed = apply({}, { type: 'start', key: 'r1', token: 1 }, { type: 'failed', key: 'r1', token: 1, message: '没有权限执行该操作' })
  assert.deepEqual(probeStateOf(failed, 'r1'), { status: 'failed', message: '没有权限执行该操作' })
  const unreachable = apply({}, { type: 'start', key: 'r1', token: 1 }, { type: 'done', key: 'r1', token: 1, result: { ok: false, error: 'timeout' } })
  assert.deepEqual(probeStateOf(unreachable, 'r1'), { status: 'done', result: { ok: false, error: 'timeout' } })
})

test('重新测试时不保留上一次的结果', () => {
  const table = apply({}, { type: 'start', key: 'r1', token: 1 }, { type: 'done', key: 'r1', token: 1, result: OK }, { type: 'start', key: 'r1', token: 2 })
  assert.deepEqual(probeStateOf(table, 'r1'), { status: 'probing', token: 2 })
})

test('竞态：同一对象连测两次，第一次迟到的结果被丢弃', () => {
  const table = apply({},
    { type: 'start', key: 'r1', token: 1 },
    { type: 'start', key: 'r1', token: 2 },
    { type: 'done', key: 'r1', token: 2, result: { ok: true, latency_ms: 20 } },
    { type: 'done', key: 'r1', token: 1, result: { ok: false, error: '旧' } },
  )
  assert.deepEqual(probeStateOf(table, 'r1'), { status: 'done', result: { ok: true, latency_ms: 20 } })
})

test('竞态：第二次还在途时，第一次的结果不能提前结束它', () => {
  const table = apply({},
    { type: 'start', key: 'r1', token: 1 },
    { type: 'start', key: 'r1', token: 2 },
    { type: 'done', key: 'r1', token: 1, result: OK },
  )
  assert.deepEqual(probeStateOf(table, 'r1'), { status: 'probing', token: 2 })
})

test('取消只放弃在途的探测，已有结果保留；取消后迟到的结果被丢弃', () => {
  const table = apply({},
    { type: 'start', key: 'r1', token: 1 },
    { type: 'done', key: 'r1', token: 1, result: OK },
    { type: 'start', key: 'r2', token: 2 },
    { type: 'cancel', keys: ['r1', 'r2'] },
    { type: 'done', key: 'r2', token: 2, result: OK },
  )
  assert.deepEqual(probeStateOf(table, 'r1'), { status: 'done', result: OK })
  assert.deepEqual(probeStateOf(table, 'r2'), IDLE)
})

test('各对象互不影响', () => {
  const table = apply({}, { type: 'start', key: 'r1', token: 1 }, { type: 'start', key: 'r2', token: 2 }, { type: 'done', key: 'r2', token: 2, result: OK })
  assert.equal(probeStateOf(table, 'r1').status, 'probing')
  assert.equal(probeStateOf(table, 'r2').status, 'done')
})

test('批次进度从状态表推导', () => {
  const table = apply({}, { type: 'start', key: 'a', token: 1 }, { type: 'start', key: 'b', token: 2 }, { type: 'done', key: 'a', token: 1, result: OK })
  assert.deepEqual(batchProgress(table, ['a', 'b', 'c']), { total: 3, pending: 1 })
})

test('文字形式', () => {
  assert.equal(probeText(IDLE), '')
  assert.equal(probeText({ status: 'done', result: { ok: true, latency_ms: 12 } }), '通 12ms')
  assert.equal(probeText({ status: 'done', result: { ok: false, error: 'timeout' } }), '不通 timeout')
  assert.equal(probeText({ status: 'done', result: { ok: true, latency_ms: 30, hops: [{ latency_ms: 10 }, { latency_ms: 20 }] } }), '通 10ms → 20ms = 30ms')
  assert.equal(probeText({ status: 'done', result: { ok: false, hops: [{ latency_ms: 10 }, { error: 'refused' }] } }), '不通 10ms → ×(refused)')
  assert.equal(probeText({ status: 'failed', message: '没有权限执行该操作' }), '测试请求失败：没有权限执行该操作')
})
