// @ts-check
import { useState } from 'react'

/**
 * 表单草稿：只存“用户改了、还没保存”的值；没有草稿时界面直接显示服务端的值。
 *
 * 不变式：草稿与服务端的值相等，即不存在草稿。刷新回来的值追上了草稿
 * （刚保存成功），或用户把输入改回了原样，草稿都在这一次渲染里被清掉。
 * 因此：
 * - 数据刷新（包括轮询）只会换掉 `saved`，不会动用户没保存的输入；
 * - 保存成功后不需要再“把表单同步成服务端的值”，刷新到达时两者自然相等。
 * 提交时要先把输入整理成服务端会存下的形式（去掉首尾空白等），否则两者
 * 永远对不上，草稿会一直留着。
 *
 * 草稿的取值不能是 null：null 表示没有草稿。
 *
 * @template T
 * @param {T} saved 服务端的值
 * @param {(draft: T, saved: T) => boolean} [same]
 */
export function useDraft(saved, same = Object.is) {
  const [draft, setDraft] = useState(/** @type {T | null} */ (null))
  const caughtUp = draft !== null && same(draft, saved)
  if (caughtUp) setDraft(null)
  const dirty = draft !== null && !caughtUp
  return { value: dirty ? draft : saved, dirty, set: setDraft, discard: () => setDraft(null) }
}
