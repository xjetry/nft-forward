// @ts-check
import { nullStr } from './fmt.js'

/**
 * @typedef {'ok' | 'warn' | 'danger' | 'neutral'} HealthTone
 * @typedef {{ tone: HealthTone, label: string, detail?: string }} NodeHealth
 */

/**
 * 把节点的若干原始字段归并成一个“健康状态”。全站所有显示节点状态的地方
 * （概览、节点列表、节点详情、用户侧节点表）都从这里取，保证同一个节点在
 * 不同页面上是同一个结论、同一种颜色。
 *
 * 判定优先级（前者成立就不再看后者）：
 *   已禁用 > 错误 > 警告 > 离线 > 待同步 > 在线
 * 错误与警告排在“离线”之前：它们解释了节点为什么需要处理，不能被一个
 * 沉默的“离线”盖住。
 *
 * 两类节点没有“同步”这回事，只分在线 / 离线：
 * - 组合节点：自己没有 agent，在线状态由服务端按子节点聚合，下发错误也
 *   不属于它。
 * - `detailed: false`（用户侧）：接口不返回同步与错误字段；字段缺失不能
 *   被读成“待同步”。
 *
 * @param {{
 *   disabled?: boolean,
 *   online?: number,
 *   node_type?: string,
 *   last_error?: unknown,
 *   last_warning?: string,
 *   last_apply_at?: { Valid?: boolean } | null,
 * }} node
 * @param {{ detailed?: boolean }} [opts]
 * @returns {NodeHealth}
 */
export function nodeHealth(node, { detailed = true } = {}) {
  if (node.disabled) return { tone: 'warn', label: '已禁用' }
  const online = node.online === 1
  if (!detailed || node.node_type === 'composite') {
    return online ? { tone: 'ok', label: '在线' } : { tone: 'neutral', label: '离线' }
  }
  const error = nullStr(node.last_error)
  if (error) return { tone: 'danger', label: '错误', detail: error }
  if (node.last_warning) return { tone: 'warn', label: '警告', detail: node.last_warning }
  if (!online) return { tone: 'neutral', label: '离线' }
  if (!node.last_apply_at?.Valid) return { tone: 'warn', label: '待同步' }
  return { tone: 'ok', label: '在线' }
}

/**
 * 节点的 IP 栈能力。组合节点的入口可达地址来自首跳，出口的 IPv6 转发能力
 * 来自尾跳（与服务端校验“出口是否为 IPv6”时看最后一跳 relay_host_v6 的
 * 口径一致）；单点节点入口出口都是它自己。
 *
 * @param {{
 *   node_type?: string,
 *   relay_host?: string, relay_host_v6?: string,
 *   entry_relay_host?: string, entry_relay_host_v6?: string, exit_relay_host_v6?: string,
 * }} node
 * @returns {{ entryV4: boolean, entryV6: boolean, exitV6: boolean }}
 */
export function nodeStack(node) {
  const composite = node.node_type === 'composite'
  return {
    entryV4: composite ? !!node.entry_relay_host : !!node.relay_host,
    entryV6: composite ? !!node.entry_relay_host_v6 : !!node.relay_host_v6,
    exitV6: composite ? !!node.exit_relay_host_v6 : !!node.relay_host_v6,
  }
}
