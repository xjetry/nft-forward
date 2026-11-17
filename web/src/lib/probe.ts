/* 连通性探测的状态模型。

   一个可探测对象（一条规则、一个出口地址）的探测状态：

     idle      没测过
     probing   正在测。不保留上一次的结果：测试进行中还显示旧延迟，会被
               读成“这是刚测出来的”
     done      拿到了探测结果（通或不通）
     failed    探测请求本身失败（网络错误、403、500）—— 这不等于“不通”，
               目标是否可达此时是未知的，必须和 done 里的不通区分开

   多个对象的状态放在一张以 key 为索引的表里，由列表组件持有，而不是每行
   各自 useState。这样行因为排序、筛选、换布局而重新挂载时结果不丢，
   “全部测试”也只是往表里发事件，不需要用计数器去通知每一行。

   并发与迟到：每次开始探测领一个令牌并记在 probing 状态里；结果回来时
   令牌对不上（期间又重新测了一次，或被取消）就丢弃。 */

import { assertNever } from './resource.ts'

export interface ProbeHop {
  readonly latency_ms?: number
  readonly error?: string
}

export interface ProbeResult {
  readonly ok: boolean
  readonly latency_ms?: number
  readonly error?: string
  /** 链路探测时每一跳的结果；单跳探测没有这个字段。 */
  readonly hops?: readonly ProbeHop[]
}

export type ProbeState =
  | { readonly status: 'idle' }
  | { readonly status: 'probing'; readonly token: number }
  | { readonly status: 'done'; readonly result: ProbeResult }
  | { readonly status: 'failed'; readonly message: string }

export type ProbeTable = Readonly<Record<string, ProbeState>>

export type ProbeEvent =
  | { readonly type: 'start'; readonly key: string; readonly token: number }
  | { readonly type: 'done'; readonly key: string; readonly token: number; readonly result: ProbeResult }
  | { readonly type: 'failed'; readonly key: string; readonly token: number; readonly message: string }
  /** 放弃若干对象的在途探测（取消“全部测试”时用）；已有结果的不受影响。 */
  | { readonly type: 'cancel'; readonly keys: readonly string[] }

export const IDLE: ProbeState = { status: 'idle' }

export function probeStateOf(table: ProbeTable, key: string): ProbeState {
  return table[key] ?? IDLE
}

export function reduceProbes(table: ProbeTable, event: ProbeEvent): ProbeTable {
  switch (event.type) {
    case 'start':
      return { ...table, [event.key]: { status: 'probing', token: event.token } }
    case 'done':
    case 'failed': {
      const current = probeStateOf(table, event.key)
      // 只有“仍在等这一次”的结果才被采用。
      if (current.status !== 'probing' || current.token !== event.token) return table
      const next: ProbeState = event.type === 'done'
        ? { status: 'done', result: event.result }
        : { status: 'failed', message: event.message }
      return { ...table, [event.key]: next }
    }
    case 'cancel': {
      const next: Record<string, ProbeState> = { ...table }
      for (const key of event.keys) {
        if (next[key]?.status === 'probing') delete next[key]
      }
      return next
    }
    default:
      return assertNever(event)
  }
}

/** 一批探测的进度，从状态表推导，不单独存一份。 */
export function batchProgress(table: ProbeTable, keys: readonly string[]): { total: number; pending: number } {
  let pending = 0
  for (const key of keys) if (probeStateOf(table, key).status === 'probing') pending++
  return { total: keys.length, pending }
}

/** 探测结果的纯文字形式，用于 title / 无障碍名称。 */
export function probeText(state: ProbeState): string {
  switch (state.status) {
    case 'idle':
      return ''
    case 'probing':
      return '测试中…'
    case 'failed':
      return `测试请求失败：${state.message}`
    case 'done': {
      const r = state.result
      const verdict = r.ok ? '通' : '不通'
      if (!r.hops?.length) return r.ok ? `${verdict} ${r.latency_ms}ms` : `${verdict} ${r.error || ''}`.trim()
      const hops = r.hops.map(h => (h.error ? `×(${h.error})` : `${h.latency_ms}ms`)).join(' → ')
      return `${verdict} ${hops}${r.ok && r.hops.length > 1 ? ` = ${r.latency_ms}ms` : ''}`
    }
    default:
      return assertNever(state)
  }
}
