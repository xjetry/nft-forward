import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  ROLE_DIRECT, ROLE_LANDING, buildCatalog, effectiveRoles, enrichRule, hostPortKey, landingIndex, mergeLanding,
  nodeKey, relayFor, setLocalRoleBatch, toggleLocalRole,
} from './landing.js'

const node = (host: string, port: number, extra: Record<string, unknown> = {}) =>
  ({ name: `${host}`, protocol: 'vless', host, port, uri: `vless://uuid@${host.includes(':') ? `[${host}]` : host}:${port}?x=1#n`, ...extra })
const roleKey = (n: { protocol: string; host: string; port: number }) => `${n.protocol}:${n.host}:${n.port}`

test('规范键：IPv6 带方括号，且与索引 / 合并用的是同一个键', () => {
  assert.equal(hostPortKey('1.2.3.4', 443), '1.2.3.4:443')
  assert.equal(hostPortKey('2001:db8::1', 443), '[2001:db8::1]:443')
  const v6 = node('2001:db8::1', 443)
  assert.equal(nodeKey(v6), '[2001:db8::1]:443')
  assert.ok(landingIndex([v6]).has(hostPortKey('2001:db8::1', 443)))
  assert.equal(mergeLanding([v6], [node('2001:db8::1', 443, { name: 'dup' })]).length, 1)
})

test('生效角色：本地覆盖优先，本地的 0 盖掉服务端的角色', () => {
  const server = { a: ROLE_LANDING, b: ROLE_DIRECT, c: ROLE_LANDING }
  const local = { b: ROLE_LANDING | ROLE_DIRECT, c: 0, d: ROLE_DIRECT }
  assert.deepEqual(effectiveRoles(server, local), { a: ROLE_LANDING, b: ROLE_LANDING | ROLE_DIRECT, d: ROLE_DIRECT })
})

test('翻转角色：服务端已标的位可以在本地关掉，再点一次回到服务端值且不留覆盖', () => {
  const n = node('1.2.3.4', 443)
  const server = { [roleKey(n)]: ROLE_LANDING }
  const off = toggleLocalRole(server, {}, n, ROLE_LANDING)
  assert.deepEqual(off, { [roleKey(n)]: 0 })
  assert.deepEqual(effectiveRoles(server, off), {})
  const back = toggleLocalRole(server, off, n, ROLE_LANDING)
  assert.deepEqual(back, {})
  assert.deepEqual(effectiveRoles(server, back), server)
})

test('翻转角色：只动被点的那一位，另一位保持生效值', () => {
  const n = node('1.2.3.4', 443)
  const server = { [roleKey(n)]: ROLE_LANDING }
  const local = toggleLocalRole(server, {}, n, ROLE_DIRECT)
  assert.deepEqual(effectiveRoles(server, local), { [roleKey(n)]: ROLE_LANDING | ROLE_DIRECT })
})

test('批量设置：对生效值打开 / 关闭，结果与服务端一致的项不留覆盖', () => {
  const a = node('1.1.1.1', 1), b = node('2.2.2.2', 2)
  const server = { [roleKey(a)]: ROLE_LANDING }
  const on = setLocalRoleBatch(server, {}, [a, b], ROLE_LANDING, true)
  assert.deepEqual(on, { [roleKey(b)]: ROLE_LANDING })
  const off = setLocalRoleBatch(server, on, [a, b], ROLE_LANDING, false)
  assert.deepEqual(off, { [roleKey(a)]: 0 })
  assert.deepEqual(effectiveRoles(server, off), {})
})

test('目录：三路来源按规范键去重，用户自己的优先，并标出来源', () => {
  const mine = node('1.1.1.1', 443, { name: 'mine' })
  const sub = node('2.2.2.2', 443, { name: 'sub' })
  const assigned = node('1.1.1.1', 443, { name: 'assigned' })
  const assignedOnly = node('3.3.3.3', 443, { name: 'assigned-only' })
  const roles = { [roleKey(mine)]: ROLE_LANDING, [roleKey(sub)]: ROLE_DIRECT, [roleKey(assignedOnly)]: ROLE_LANDING | ROLE_DIRECT }
  const c = buildCatalog({ manual: [mine], sub: [sub], server: [assigned, assignedOnly], roles })
  assert.deepEqual(c.all.map((n: any) => [n.name, n.source]), [['mine', 'local'], ['sub', 'local'], ['assigned-only', 'admin']])
  assert.deepEqual(c.landing.map((n: any) => n.name), ['mine', 'assigned-only'])
  assert.deepEqual(c.direct.map((n: any) => n.name), ['sub', 'assigned-only'])
  assert.ok(c.landingByKey.has('3.3.3.3:443'))
})

test('目录：没有角色的节点既不是落地也不是直连', () => {
  const c = buildCatalog({ manual: [node('1.1.1.1', 443)], roles: {} })
  assert.equal(c.all.length, 1)
  assert.deepEqual([c.landing.length, c.direct.length], [0, 0])
})

test('中转 URI：IPv6 出口的规则能匹配到落地节点，端点换成规则入口', () => {
  const landing = node('2001:db8::1', 8443)
  const idx = landingIndex([landing])
  const rule = { exit_host: '2001:db8::1', exit_port: 8443, entry: '9.9.9.9:20001', entry_v6: '[2001:db8::9]:20001' }
  const hit = relayFor(rule, idx)
  assert.ok(hit)
  assert.equal(hit.relay_uri, 'vless://uuid@9.9.9.9:20001?x=1#n')
  assert.equal(hit.relay_uri_v6, 'vless://uuid@[2001:db8::9]:20001?x=1#n')
  const enriched = enrichRule(rule, idx) as any
  assert.equal(enriched.exit_kind, 'landing')
  assert.equal(enriched.relay_uri_v6, 'vless://uuid@[2001:db8::9]:20001?x=1#n')
})

test('中转 URI：出口不是落地节点、或入口不是可解析的地址时，规则原样返回', () => {
  const idx = landingIndex([node('1.1.1.1', 443)])
  const other = { exit_host: '8.8.8.8', exit_port: 53, entry: '9.9.9.9:1' }
  assert.equal(relayFor(other, idx), null)
  assert.equal(enrichRule(other, idx), other)
  // 服务端在规则还没有入口时返回占位符 “—”，不是地址
  const noEntry = { exit_host: '1.1.1.1', exit_port: 443, entry: '—' }
  assert.equal(enrichRule(noEntry, idx), noEntry)
})
