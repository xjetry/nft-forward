import assert from 'node:assert/strict'
import { test } from 'node:test'
import { NODE_TYPE_COMPOSITE, NODE_TYPE_REMOTE, NODE_TYPE_SELF, isComposite } from './nodeType.js'

test('只有组合是组合：两种机器（remote、self）都算“单点”', () => {
  assert.equal(isComposite({ node_type: NODE_TYPE_COMPOSITE }), true)
  assert.equal(isComposite({ node_type: NODE_TYPE_REMOTE }), false)
  assert.equal(isComposite({ node_type: NODE_TYPE_SELF }), false)
})

test('没有节点、或对象上没有 node_type 时不是组合', () => {
  assert.equal(isComposite(undefined), false)
  assert.equal(isComposite(null), false)
  assert.equal(isComposite({}), false)
})

test('三个取值就是接口里 node_type 给出的字面值', () => {
  assert.deepEqual([NODE_TYPE_REMOTE, NODE_TYPE_SELF, NODE_TYPE_COMPOSITE], ['remote', 'self', 'composite'])
})
