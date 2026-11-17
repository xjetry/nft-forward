import { test } from 'node:test'
import assert from 'node:assert/strict'
import { upgradeAllReport, upgradeVerdict, upgradeWorthShowing, type UpgradeStatus } from './upgrade.ts'

const ALL: UpgradeStatus[] = ['none', 'pending', 'ok', 'stuck', 'error', 'superseded']

test('只有 ok 算升级成功；none 与 superseded 不是结论', () => {
  assert.equal(upgradeVerdict('ok'), 'succeeded')
  assert.equal(upgradeVerdict('error'), 'failed')
  assert.equal(upgradeVerdict('stuck'), 'stuck')
  assert.equal(upgradeVerdict('pending'), 'running')
  assert.equal(upgradeVerdict('none'), 'running')
  assert.equal(upgradeVerdict('superseded'), 'running')
  assert.equal(upgradeVerdict(undefined), 'running')
  // 成功的来源只有一个取值。
  assert.deepEqual(ALL.filter(s => upgradeVerdict(s) === 'succeeded'), ['ok'])
})

test('没有可展示记录的两种状态不显示“升级状态”一行', () => {
  assert.deepEqual(ALL.filter(s => !upgradeWorthShowing(s)), ['none', 'superseded'])
  assert.equal(upgradeWorthShowing(undefined), false)
})

test('批量升级：全部推到', () => {
  assert.deepEqual(upgradeAllReport({ upgraded: 3, offline: 0, failed: 0 }), {
    text: '升级已推送到 3 个节点，等待它们重启', incomplete: false,
  })
})

test('批量升级：没有对象时不说“已推送到 0 个节点”', () => {
  const report = upgradeAllReport({ upgraded: 0, offline: 0, failed: 0 })
  assert.equal(report.incomplete, false)
  assert.match(report.text, /^没有需要升级的节点/)
})

test('批量升级：不在线与推送失败分开说，且算没推完', () => {
  assert.deepEqual(upgradeAllReport({ upgraded: 2, offline: 1, failed: 0 }), {
    text: '已推送到 2 个节点；1 个不在线、没有推送（原因见各节点的升级状态）', incomplete: true,
  })
  assert.deepEqual(upgradeAllReport({ upgraded: 2, offline: 0, failed: 1 }), {
    text: '已推送到 2 个节点；1 个推送失败（原因见各节点的升级状态）', incomplete: true,
  })
  assert.deepEqual(upgradeAllReport({ upgraded: 0, offline: 2, failed: 3 }), {
    text: '已推送到 0 个节点；2 个不在线、没有推送，3 个推送失败（原因见各节点的升级状态）', incomplete: true,
  })
})
