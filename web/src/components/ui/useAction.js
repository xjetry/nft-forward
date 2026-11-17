import { useCallback, useRef, useState } from 'react'
import { createSingleFlight } from '../../lib/singleFlight'
import { useToast } from './Feedback'

/**
 * 把一个异步动作（保存、删除、切换……）包成“同一时刻只执行一次”的形式，
 * 并跟踪它是否在进行中。
 *
 * 不变式：动作在途时，再次调用 run 会被直接忽略。防重复提交因此不依赖
 * 按钮是否恰好被禁用 —— 回车键、双击、别处触发的同一个动作都会被挡住。
 * 这对“翻转”语义的接口尤其重要：连点两次 toggle 等于没改。
 *
 * - 动作抛出的错误默认以 toast 报出；需要别的处理方式（表单内联错误）时
 *   传 onError。
 * - `pending` 用来给触发按钮设 loading；`pendingKey` 是在途那次调用的
 *   第一个参数（如行 id），列表里据此只让被操作的那一行显示进行中。
 *
 * @template {unknown[]} A
 * @param {(...args: A) => Promise<unknown>} fn
 * @param {{ onError?: (err: unknown) => void }} [opts]
 * @returns {{ run: (...args: A) => Promise<void>, pending: boolean, pendingKey: unknown }}
 *
 * @example
 * const save = useAction(async () => {
 *   await api.post(`/nodes/${id}/rename`, { name })
 *   toast('已保存')
 *   await reload()
 * })
 * <Button variant="primary" loading={save.pending} onClick={save.run}>保存</Button>
 */
export function useAction(fn, { onError } = {}) {
  const toast = useToast()
  // idle | pending(key)。用一个对象而不是两个变量，“在途”与“在途的是谁”
  // 不可能不一致。它只用于渲染；是否放行由下面的闸门同步判定。
  const [flight, setFlight] = useState(/** @type {{ key: unknown } | null} */ (null))
  const [gate] = useState(createSingleFlight)
  const latest = useRef({ fn, onError, toast })
  latest.current = { fn, onError, toast }

  const run = useCallback(async (...args) => {
    await gate.run(async () => {
      setFlight({ key: args[0] })
      try {
        await latest.current.fn(...args)
      } catch (err) {
        const { onError: handle, toast: notify } = latest.current
        if (handle) handle(err)
        else notify(err instanceof Error ? err.message : '操作失败', 'error')
      } finally {
        setFlight(null)
      }
    })
  }, [gate])

  return { run, pending: flight !== null, pendingKey: flight ? flight.key : undefined }
}
