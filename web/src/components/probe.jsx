import { useCallback, useReducer, useRef } from 'react'
import { api } from '../lib/api'
import { createLimiter } from '../lib/limiter'
import { assertNever } from '../lib/resource'
import { probeStateOf, probeText, reduceProbes } from '../lib/probe'
import { Badge, IconActivity, IconButton } from './ui'

// “全部测试”时限制并发：每个链路探测在服务端会扇出到每一跳，不加限制会同时
// 压到面板和各个 agent 上。所有探测共用这一个限额。
const probeLimit = createLimiter(6)

/**
 * 探测请求：按规则 / 按链路 / 按单个目标地址。
 * @typedef {{ ruleId: number } | { chainId: number } | { target: string, nodeId?: number, proto?: string }} ProbeRequest
 */

/** @param {ProbeRequest} req */
function probePath(req) {
  if ('ruleId' in req) return `/probe-chain?rule_id=${req.ruleId}`
  if ('chainId' in req) return `/probe-chain?chain=${req.chainId}`
  const q = new URLSearchParams({ target: req.target })
  if (req.nodeId) q.set('node', String(req.nodeId))
  if (req.proto) q.set('proto', req.proto)
  return `/probe?${q}`
}

/**
 * 一组探测对象的状态表（模型见 lib/probe.ts）。由列表组件调用一次，把
 * `table` 里各行的状态分发给行组件；行重新挂载不会丢结果，也不会重新触发。
 *
 * - probe(key, request)  测一个对象；同一个 key 重复调用以最后一次为准。
 * - cancel(keys)         放弃这些对象的在途探测。还在排队、尚未发出的请求
 *                        不会再发出。
 * “全部测试”就是对每一行调用 probe；进度用 lib/probe 的 batchProgress 从
 * table 推导。
 */
export function useProbes() {
  const [table, dispatch] = useReducer(reduceProbes, {})
  // key → 该对象当前在途探测的令牌。排队中的任务发出请求前据此判断自己
  // 是否已被取消或被更新的一次取代。
  const active = useRef(new Map())
  const nextToken = useRef(0)

  const probe = useCallback((key, request) => {
    const token = ++nextToken.current
    active.current.set(key, token)
    dispatch({ type: 'start', key, token })
    return probeLimit(async () => {
      if (active.current.get(key) !== token) return
      try {
        const result = await api.get(probePath(request))
        dispatch({ type: 'done', key, token, result })
      } catch (err) {
        dispatch({ type: 'failed', key, token, message: err instanceof Error ? err.message : '请求失败' })
      }
    })
  }, [])

  const cancel = useCallback((keys) => {
    for (const key of keys) active.current.delete(key)
    dispatch({ type: 'cancel', keys })
  }, [])

  return { table, probe, cancel }
}

/**
 * 探测结果。通 / 不通用状态色的 Badge；链路探测逐跳列出延迟，失败的那一跳
 * 标成 ×（原因在 title 里）。请求本身失败时显示“未测出”，不显示“不通”。
 *
 * @param {{ state: import('../lib/probe').ProbeState }} props
 */
export function ProbeResult({ state }) {
  switch (state.status) {
    case 'idle':
      return null
    case 'probing':
      return <span className="text-caption text-ink-mut">测试中…</span>
    case 'failed':
      return <Badge tone="warn" title={state.message}>未测出</Badge>
    case 'done': {
      const r = state.result
      return (
        <span className="inline-flex items-center gap-1.5 font-mono text-caption" title={probeText(state)}>
          <Badge tone={r.ok ? 'ok' : 'danger'}>{r.ok ? '通' : '不通'}</Badge>
          {r.hops?.length
            ? r.hops.map((h, i) => (
              <span key={i} className="inline-flex items-center gap-1.5">
                {i > 0 && <span className="text-ink-mut">→</span>}
                <span className={h.error ? 'text-danger-ink' : 'text-ink-soft'}>{h.error ? '×' : `${h.latency_ms}ms`}</span>
              </span>
            ))
            : r.ok && <span className="text-ink-soft">{r.latency_ms}ms</span>}
          {r.ok && (r.hops?.length ?? 0) > 1 && <span className="text-ink">= {r.latency_ms}ms</span>}
        </span>
      )
    }
    default:
      return assertNever(state)
  }
}

/**
 * 一行里的探测控件：按钮 + 结果。状态由外面的表提供。
 * 点击不会冒泡到所在的行 / 卡片（它们通常整体可点击跳转）。
 *
 * @param {{ state: import('../lib/probe').ProbeState, onProbe: () => void }} props
 */
export function ProbeControl({ state, onProbe }) {
  return (
    <span className="inline-flex items-center gap-2" onClick={e => e.stopPropagation()}>
      <IconButton size="sm" variant="secondary" label="测试连通性" loading={state.status === 'probing'} onClick={onProbe}>
        <IconActivity />
      </IconButton>
      <ProbeResult state={state} />
    </span>
  )
}

/**
 * 自带状态的单个探测按钮，用在只有一个探测对象的地方（详情页）。
 * 列表里用 useProbes + ProbeControl，让状态留在列表上。
 *
 * @param {{ request: ProbeRequest }} props
 */
export function ProbeButton({ request }) {
  const { table, probe } = useProbes()
  return <ProbeControl state={probeStateOf(table, 'self')} onProbe={() => probe('self', request)} />
}
