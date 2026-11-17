import { useState } from 'react'
import { api } from '../lib/api'
import { GRANT_SEAT_OPTIONS } from '../lib/seats'
import { Select, useAction, useToast } from './ui'

/**
 * 某个用户在某个节点上的用途覆盖。节点详情按用户列出授权，每一行用这个组件改。
 *
 * 选中即保存：这是一个可随时改回的单值设置，不需要再点一次确认。
 * 显示的值以 props 里的 `seats`（服务端的值，取自授权的 roles 字段）为准；保存在途
 * 时显示用户刚选的值，保存失败后回到服务端的值 —— 界面不会停在一个没有存下来的
 * 选项上。
 *
 * @param {{
 *   userId: number,
 *   nodeId: number,
 *   seats?: number | null,
 *   onDone?: () => unknown,
 * }} props `onDone` 在保存成功后调用，用来刷新持有 `seats` 的那份数据；
 *   返回 Promise 时会等它完成，这样新值到达之前控件一直显示用户选的值。
 */
export function GrantSeatsSelect({ userId, nodeId, seats, onDone }) {
  const toast = useToast()
  const saved = seats ?? 0
  const [choice, setChoice] = useState(/** @type {number | null} */ (null))

  const save = useAction(async (next) => {
    setChoice(next)
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/roles`, { roles: next })
      toast('用途已更新')
      await onDone?.()
    } finally {
      setChoice(null)
    }
  })

  return (
    <Select
      size="sm"
      className="w-32"
      title="该用户在此节点上的用途"
      value={choice ?? saved}
      disabled={save.pending}
      onChange={(next) => { if (next !== saved) save.run(next) }}
      options={GRANT_SEAT_OPTIONS}
    />
  )
}
