import { test } from 'node:test'
import assert from 'node:assert/strict'
import { GB } from './fmt.js'
import { GRANT_DEFAULTS, formatGrantLine, parseGrantText } from './grantText.js'

const nodes = [{ id: 1, name: 'gateway-hk' }, { id: 2, name: 'relay-jp' }]
const one = (line: string) => parseGrantText(line, nodes)[0]

test('每行一个节点；空行与 # 开头的行跳过', () => {
  const rows = parseGrantText('\n# 注释\ngateway-hk | max=3\n\n  relay-jp  \n', nodes)
  assert.deepEqual(rows.map(r => r.name), ['gateway-hk', 'relay-jp'])
})

test('三项设置都能读出；没写的项取默认值', () => {
  assert.deepEqual(one('gateway-hk | max=3 | quota=5GB | rate=20'),
    { name: 'gateway-hk', maxRules: 3, quotaGB: 5, rateMBytes: 20, nodeId: 1, found: true, invalid: [] })
  assert.deepEqual(one('relay-jp'),
    { name: 'relay-jp', ...GRANT_DEFAULTS, nodeId: 2, found: true, invalid: [] })
  assert.equal(one('gateway-hk | QUOTA=0.5 gb').quotaGB, 0.5)
})

test('节点名对不上节点时标记为没有这个节点', () => {
  const row = one('ghost | max=3')
  assert.equal(row.found, false)
  assert.equal(row.nodeId, null)
})

test('读不懂的数字不会变成默认值，而是记进 invalid', () => {
  for (const part of ['quota=1.2.3GB', 'quota=1e3GB', 'quota=-1GB', 'quota=GB', 'quota=5', 'max=1.5', 'max=-2', 'rate=2.5', 'rate=fast']) {
    const row = one(`gateway-hk | ${part}`)
    assert.deepEqual(row.invalid, [part], part)
    assert.deepEqual({ maxRules: row.maxRules, quotaGB: row.quotaGB, rateMBytes: row.rateMBytes }, { ...GRANT_DEFAULTS }, part)
  }
})

test('规则数上限至少为 1：max=0 记进 invalid', () => {
  const row = one('gateway-hk | max=0 | rate=5')
  assert.deepEqual(row.invalid, ['max=0'])
  assert.equal(row.rateMBytes, 5)
})

test('不认识的项记进 invalid；连续的分隔符之间的空项不算', () => {
  assert.deepEqual(one('gateway-hk | speed=10 | max=4').invalid, ['speed=10'])
  assert.deepEqual(one('gateway-hk || max=4 |').invalid, [])
})

test('写出的行能读回同样的三项设置', () => {
  const grants = [
    { max_forwards: 10, traffic_quota_bytes: 0, rate_limit_mbytes: 0 },
    { max_forwards: 3, traffic_quota_bytes: 5 * GB, rate_limit_mbytes: 20 },
    { max_forwards: 1, traffic_quota_bytes: Math.round(0.7 * GB), rate_limit_mbytes: 1 },
    // 不足 0.005 GB 的配额：写成两位小数会变成 0，读回去就是不限。
    { max_forwards: 7, traffic_quota_bytes: 1048576, rate_limit_mbytes: 0 },
    { max_forwards: 2, traffic_quota_bytes: 123456789, rate_limit_mbytes: 3 },
  ]
  for (const grant of grants) {
    const row = one(formatGrantLine('gateway-hk', grant))
    assert.deepEqual(row.invalid, [], JSON.stringify(grant))
    assert.equal(row.maxRules, grant.max_forwards)
    assert.equal(row.rateMBytes, grant.rate_limit_mbytes)
    // 弹窗提交时就是这样换算回字节的。
    assert.ok(Math.abs(Math.round(row.quotaGB * GB) - grant.traffic_quota_bytes) <= 1, JSON.stringify(grant))
    assert.equal(row.quotaGB === 0, grant.traffic_quota_bytes === 0, JSON.stringify(grant))
  }
})
