/* 节点类型：接口里 node_type 字段的三个取值（服务端 db.NodeTypeRemote / NodeTypeSelf /
   NodeTypeComposite，库里是 nodes.node_type）。

   remote 与 self 是机器——remote 是面板之外的，self 是面板所在的这一台——界面上都叫
   “单点”；composite 是组合，由几台机器按序组成，自己不是机器。所以页面上“单点 / 组合”
   两栏的划分是“是不是组合”，self 节点在“单点”一栏。 */
export const NODE_TYPE_REMOTE = 'remote'
export const NODE_TYPE_SELF = 'self'
export const NODE_TYPE_COMPOSITE = 'composite'

/** 这个节点（或组合成员、链上的一环：凡是带 node_type 字段的对象）是不是组合。 */
export function isComposite(node) {
  return node?.node_type === NODE_TYPE_COMPOSITE
}
