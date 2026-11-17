import { test } from 'node:test'
import assert from 'node:assert/strict'
import { POOL_SIZE_MAX, parsePoolSize, sameForm, toForm, toSettings, type Settings } from './settingsForm.ts'

const STORED: Settings = { panel_url: 'https://panel.example.com', panel_name: 'edge', show_rate_to_user: true, pool_size: 4 }

test('刚读到的设置与它自己的表单值相同：没有改动', () => {
  assert.equal(sameForm(toForm(STORED), toForm({ ...STORED })), true)
})

test('任何一个字段变了都算改动', () => {
  const form = toForm(STORED)
  assert.equal(sameForm({ ...form, panel_url: 'https://other.example.com' }, form), false)
  assert.equal(sameForm({ ...form, panel_name: 'edge-2' }, form), false)
  assert.equal(sameForm({ ...form, show_rate_to_user: false }, form), false)
  assert.equal(sameForm({ ...form, pool_size: '5' }, form), false)
  // 输入中的空白也是改动：它还没有被整理成要提交的形式。
  assert.equal(sameForm({ ...form, panel_name: 'edge ' }, form), false)
})

test('提交的设置去掉了首尾空白，并把连接池大小读成整数', () => {
  assert.deepEqual(
    toSettings({ panel_url: '  https://panel.example.com ', panel_name: ' edge\t', show_rate_to_user: false, pool_size: ' 08 ' }),
    { panel_url: 'https://panel.example.com', panel_name: 'edge', show_rate_to_user: false, pool_size: 8 },
  )
})

test('提交之后的表单值与服务端存下后读回的相同', () => {
  // 只改了空白与写法：整理之后与已存的设置是同一份，草稿因此能在刷新回来时结束。
  const submitted = toSettings({ panel_url: ' https://panel.example.com', panel_name: 'edge ', show_rate_to_user: true, pool_size: '04' })
  assert.ok(submitted)
  assert.equal(sameForm(toForm(submitted), toForm(STORED)), true)
})

test('连接池大小只接受 0 到上限之间的整数', () => {
  assert.equal(parsePoolSize('0'), 0)
  assert.equal(parsePoolSize(String(POOL_SIZE_MAX)), POOL_SIZE_MAX)
  for (const bad of ['', ' ', '-1', '1.5', '1e1', 'abc', String(POOL_SIZE_MAX + 1)]) {
    assert.equal(parsePoolSize(bad), null, `输入 ${JSON.stringify(bad)}`)
    assert.equal(toSettings({ ...toForm(STORED), pool_size: bad }), null, `输入 ${JSON.stringify(bad)}`)
  }
})
