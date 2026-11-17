import { nodeHealth, nodeStack } from '../lib/nodeHealth'
import { Badge, IconHome, IconLink, Tag } from './ui'

/* 领域展示原子：节点、规则相关的小标签。

   颜色规则沿用 ui/Badge 的分工：节点“状态”用 Badge（有语气色），节点类型、
   协议、转发模式、出口种类这些“类别”用 Tag（中性色）。 */

/**
 * 节点健康状态徽标。判定逻辑在 lib/nodeHealth.js，这里只负责显示；有详情
 * （错误 / 警告原文）时放进 title，悬停可见。
 * @param {{ node: object, detailed?: boolean }} props `detailed={false}` 用于用户侧。
 */
export function NodeStatus({ node, detailed = true }) {
  const h = nodeHealth(node, { detailed })
  return <Badge tone={h.tone} dot title={h.detail}>{h.label}</Badge>
}

/**
 * 节点类型标签：单点 / 组合 / 自身。
 * @param {{ type?: string }} props
 */
export function NodeTypeTag({ type }) {
  if (type === 'composite') return <Tag icon={<IconLink size={12} />}>组合</Tag>
  if (type === 'self') return <Tag icon={<IconHome size={12} />}>自身</Tag>
  return <Tag>单点</Tag>
}

/**
 * 紧凑位置（表格单元格里的节点名旁）用的类型图标。单点是默认类型，不画
 * 标记；只有组合与自身才需要被认出来。
 * @param {{ type?: string }} props
 */
export function NodeTypeIcon({ type }) {
  if (type === 'composite') return <span className="inline-flex text-ink-mut" title="组合节点"><IconLink size={14} /></span>
  if (type === 'self') return <span className="inline-flex text-ink-mut" title="面板自身"><IconHome size={14} /></span>
  return null
}

/**
 * 节点的 IP 栈：入口支持的地址族；出口的 IPv6 能力与入口不一致时额外标出
 * （只有组合节点会不一致）。“出口不支持 v6”是一个能力缺口而不是类别，
 * 所以用警示语气的 Badge；“出口支持 v6”只是补充说明，用中性 Tag。
 * @param {{ node: object }} props
 */
export function NodeStackTags({ node }) {
  const { entryV4, entryV6, exitV6 } = nodeStack(node)
  const entry = [entryV4 && 'v4', entryV6 && 'v6'].filter(Boolean).join('+')
  if (!entry && exitV6 === entryV6) return null
  return (
    <span className="inline-flex items-center gap-1">
      {entry && <Tag mono>{entry}</Tag>}
      {exitV6 !== entryV6 && (exitV6 ? <Tag>出口支持 v6</Tag> : <Badge tone="warn">出口不支持 v6</Badge>)}
    </span>
  )
}

/**
 * 出口种类：落地节点出口显示具体协议（vless / ss / snell…），这才是用户
 * 关心的信息；没有记录协议的旧数据显示“落地”。自定义 host:port 显示“自定义”。
 * @param {{ kind?: string, protocol?: string }} props
 */
export function ExitKindTag({ kind, protocol }) {
  if (kind === 'landing') return <Tag mono={!!protocol}>{protocol || '落地'}</Tag>
  return <Tag>自定义</Tag>
}

/** @param {{ proto?: string }} props */
export function ProtoTag({ proto }) {
  if (!proto) return null
  const p = proto.toLowerCase()
  return <Tag mono>{p === 'tcp' ? 'TCP' : p === 'udp' ? 'UDP' : 'TCP+UDP'}</Tag>
}

/** @param {{ mode?: string }} props */
export function ModeTag({ mode }) {
  if (!mode) return null
  return <Tag mono>{mode === 'kernel' ? 'kernel' : 'userspace'}</Tag>
}
