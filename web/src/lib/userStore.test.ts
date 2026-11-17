import { test } from 'node:test'
import assert from 'node:assert/strict'
import { StoreWriteError, adoptUserData, readSlot, slotKey, writeSlot, type KeyValueStore } from './userStore.ts'

function fakeStore(init: Record<string, string> = {}) {
  const m = new Map(Object.entries(init))
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
    dump: () => Object.fromEntries(m),
  }
}
const brokenStore: KeyValueStore = {
  getItem: () => { throw new Error('SecurityError') },
  setItem: () => { throw new Error('QuotaExceededError') },
  removeItem: () => { throw new Error('SecurityError') },
}
const alice = { id: 2, username: 'alice' }

test('键只由用户 id 与数据名决定，与用户名无关', () => {
  assert.equal(slotKey(2, 'landing-uris'), 'nf-user:2:landing-uris')
})

test('读写一项；写入 null 即删除', () => {
  const st = fakeStore()
  writeSlot(st, 2, 'sub-urls', 'https://a.example/sub')
  assert.equal(readSlot(st, 2, 'sub-urls'), 'https://a.example/sub')
  assert.equal(readSlot(st, 3, 'sub-urls'), null)
  writeSlot(st, 2, 'sub-urls', null)
  assert.deepEqual(st.dump(), {})
})

test('读不出来按没有存过处理；写不进去必须抛出', () => {
  assert.equal(readSlot(brokenStore, 2, 'landing-uris'), null)
  assert.equal(readSlot(null, 2, 'landing-uris'), null)
  assert.throws(() => writeSlot(brokenStore, 2, 'landing-uris', 'x'), StoreWriteError)
  assert.throws(() => writeSlot(brokenStore, 2, 'landing-uris', null), StoreWriteError)
  assert.throws(() => writeSlot(null, 2, 'landing-uris', 'x'), StoreWriteError)
})

test('迁移：按用户名存放的各项搬到按 id 的键下，原键删掉', () => {
  const st = fakeStore({
    'nf-landing-uris:alice': 'vless://x',
    'nf-sub-urls:alice': 'https://a.example/sub',
    'nf-sub-cache:alice': '{"https://a.example/sub":[]}',
    'nf-local-roles:alice': '{"vless:1.1.1.1:443":0}',
    'nf-sub-textarea-h:alice': '64',
    'nf-manual-textarea-h:alice': '80',
    'my.nodeOrder.2': '[4,2,8]',
  })
  adoptUserData(st, alice)
  assert.deepEqual(st.dump(), {
    'nf-user:2:landing-uris': 'vless://x',
    'nf-user:2:sub-urls': 'https://a.example/sub',
    'nf-user:2:sub-cache': '{"https://a.example/sub":[]}',
    'nf-user:2:local-roles': '{"vless:1.1.1.1:443":0}',
    'nf-user:2:sub-textarea-h': '64',
    'nf-user:2:manual-textarea-h': '80',
    'nf-user:2:node-order': '[4,2,8]',
  })
})

test('迁移之后改名不影响数据：新用户名下再迁移一次，什么都不变', () => {
  const st = fakeStore({ 'nf-landing-uris:alice': 'vless://x' })
  adoptUserData(st, alice)
  const adopted = st.dump()
  adoptUserData(st, { id: 2, username: 'alice-renamed' })
  assert.deepEqual(st.dump(), adopted)
  assert.equal(readSlot(st, 2, 'landing-uris'), 'vless://x')
})

test('迁移：只搬这个用户名与这个 id 的数据，别的键不动', () => {
  const init = {
    'nf-landing-uris:bob': 'vless://bob',
    'my.nodeOrder.3': '[1]',
    'nf-theme': 'dark',
    'nf-landing-uris:alice2': 'vless://other',
  }
  const st = fakeStore(init)
  adoptUserData(st, alice)
  assert.deepEqual(st.dump(), init)
})

test('迁移：按 id 的键已有数据时不覆盖，按用户名的那一份原样留着', () => {
  const init = { 'nf-user:2:landing-uris': 'vless://mine', 'nf-landing-uris:alice': 'vless://someone-else' }
  const st = fakeStore(init)
  adoptUserData(st, alice)
  assert.deepEqual(st.dump(), init)
})

test('迁移：写入失败时数据仍在原处', () => {
  const m = new Map([['nf-landing-uris:alice', 'vless://x']])
  const st: KeyValueStore = {
    getItem: k => m.get(k) ?? null,
    setItem: () => { throw new Error('QuotaExceededError') },
    removeItem: k => void m.delete(k),
  }
  assert.throws(() => adoptUserData(st, alice))
  assert.deepEqual(Object.fromEntries(m), { 'nf-landing-uris:alice': 'vless://x' })
})
