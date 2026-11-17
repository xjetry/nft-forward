import { test } from 'node:test'
import assert from 'node:assert/strict'
import { GB, bytesToGBText, fmtGrantTraffic, fmtTrafficGB } from './fmt.js'

test('配额为 0 表示不限', () => {
  assert.equal(fmtTrafficGB(2 * GB, 0), '2.0 GB / ∞')
  assert.equal(fmtTrafficGB(2 * GB, 10 * GB), '2.0 GB / 10.0 GB')
})

test('节点授权：自己有配额时显示自己的配额，与账户配额无关', () => {
  assert.equal(fmtGrantTraffic(2 * GB, 5 * GB, 0), '2.0 GB / 5.0 GB')
  assert.equal(fmtGrantTraffic(2 * GB, 5 * GB, 100 * GB), '2.0 GB / 5.0 GB')
})

test('节点授权：没有单独的配额时受账户配额约束，不能显示成不限', () => {
  assert.equal(fmtGrantTraffic(2 * GB, 0, 100 * GB), '2.0 GB / 账户配额')
  assert.equal(fmtGrantTraffic(undefined, undefined, 100 * GB), '0.0 MB / 账户配额')
})

test('节点授权：节点与账户都没有配额才是不限', () => {
  assert.equal(fmtGrantTraffic(2 * GB, 0, 0), '2.0 GB / ∞')
  assert.equal(fmtGrantTraffic(2 * GB, 0, undefined), '2.0 GB / ∞')
})

/* ---------- GB 文本 ---------- */

// 文本被读回成字节数的两种算法，与 fmt.js 的 readsBackAs 描述的一致。
const readers = [(n: number) => Math.round(n * GB), (n: number) => Math.floor(n * GB)]

test('GB 文本：按 GB 填写的常见值显示成填写时的样子', () => {
  assert.equal(bytesToGBText(0), '0')
  assert.equal(bytesToGBText(undefined), '0')
  assert.equal(bytesToGBText(5 * GB), '5')
  assert.equal(bytesToGBText(1.5 * GB), '1.5')
  // 0.1 与 0.7 GB 不是整数个字节：前端四舍五入、服务端截断，存下的值差 1 字节。
  for (const gb of [0.1, 0.7, 12.34]) {
    assert.equal(bytesToGBText(Math.round(gb * GB)), String(gb))
    assert.equal(bytesToGBText(Math.floor(gb * GB)), String(gb))
  }
})

test('GB 文本：很小的配额不能写成 0（0 的含义是不限）', () => {
  for (const bytes of [1, 2, 1024, 1048576, 5 * 1048576]) {
    const text = bytesToGBText(bytes)
    assert.notEqual(Number(text), 0, `${bytes} 字节写成了 ${text}`)
    for (const read of readers) assert.notEqual(read(Number(text)), 0, `${bytes} 字节的 ${text} 读回成了 0`)
  }
})

test('GB 文本：任何字节数写出的都是普通小数，两种读法都读回原值 ±1 字节', () => {
  // 固定种子的线性同余序列：覆盖从几字节到几 TB 的量级，结果可复现。
  let seed = 20261117
  const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648)
  for (let i = 0; i < 4000; i++) {
    const bytes = Math.floor(next() / 2 ** (next() % 31)) * (next() % 4096 + 1)
    const text = bytesToGBText(bytes)
    assert.match(text, /^\d+(\.\d+)?$/, `${bytes} 字节写成了 ${text}`)
    for (const read of readers) {
      const back = read(Number(text))
      assert.ok(Math.abs(back - bytes) <= 1, `${bytes} 字节的 ${text} 读回成了 ${back}`)
      assert.equal(back === 0, bytes === 0, `${bytes} 字节的 ${text} 读回成了 ${back}`)
    }
  }
})
