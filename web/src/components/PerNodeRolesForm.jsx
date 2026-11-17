import { useState } from 'react'
import { api } from '../lib/api'
import { Select, useAction, useToast } from './ui'

/* 用途掩码：入口 = 1，中转 = 2。0 不是“没有用途”，而是“不覆盖”——该用户
   在这个节点上沿用节点自身的用途设置。 */
const OPTIONS = [
  { value: 0, label: '跟随节点' },
  { value: 1, label: '仅入口' },
  { value: 2, label: '仅中转' },
  { value: 3, label: '入口+中转' },
]

/**
 * 某个用户在某个节点上的用途覆盖。用户详情（按节点列出）与节点详情
 * （按用户列出）显示的是同一条授权记录的同一个字段，所以共用这一个组件。
 *
 * 选中即保存：这是一个可随时改回的单值设置，不需要再点一次确认。
 * 显示的值以 props 里的 `roles`（服务端的值）为准；保存在途时显示用户刚选的
 * 值，保存失败后回到服务端的值 —— 界面不会停在一个没有存下来的选项上。
 *
 * @param {{
 *   userId: number,
 *   nodeId: number,
 *   roles?: number | null,
 *   onDone?: () => unknown,
 * }} props `onDone` 在保存成功后调用，用来刷新持有 `roles` 的那份数据；
 *   返回 Promise 时会等它完成，这样新值到达之前控件一直显示用户选的值。
 */
export function PerNodeRolesForm({ userId, nodeId, roles, onDone }) {
  const toast = useToast()
  const saved = roles ?? 0
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
      className="w-28"
      title="该用户在此节点上的用途"
      value={choice ?? saved}
      disabled={save.pending}
      onChange={(next) => { if (next !== saved) save.run(next) }}
      options={OPTIONS}
    />
  )
}
