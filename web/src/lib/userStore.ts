/* 每个用户存在本浏览器里的数据的唯一出入口。

   不变式：键由用户 id 与数据名组成（nf-user:<id>:<名>）。用户名可以修改而
   id 不变，所以数据跟着人走，不跟着某个时刻的名字走。

   读：存储读不出来时按“没有存过”返回 null —— 存储不可用时本来也存不进东西。
   写：失败时抛出 StoreWriteError。调用方保存的往往是用户刚输入的内容，写不
       进去却不说，用户会以为已经保存了。只影响观感的偏好（文本框高度、列表
       顺序）可以由调用方接住后忽略，那是调用方显式做出的选择。 */

export type Slot =
  | 'landing-uris'
  | 'sub-urls'
  | 'sub-cache'
  | 'local-roles'
  | 'sub-textarea-h'
  | 'manual-textarea-h'
  | 'node-order'

export type KeyValueStore = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>

export class StoreWriteError extends Error {
  constructor() {
    super('没能保存到本浏览器：本地存储不可用或已满')
    this.name = 'StoreWriteError'
  }
}

/** 浏览器的本地存储；被禁用或不在浏览器里时为 null。 */
export function browserStore(): KeyValueStore | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage
  } catch {
    return null
  }
}

export function slotKey(userId: number, slot: Slot): string {
  return `nf-user:${userId}:${slot}`
}

export function readSlot(store: KeyValueStore | null, userId: number, slot: Slot): string | null {
  if (!store) return null
  try {
    return store.getItem(slotKey(userId, slot))
  } catch {
    return null
  }
}

/** 写入一项；value 为 null 表示删掉这一项。 */
export function writeSlot(store: KeyValueStore | null, userId: number, slot: Slot, value: string | null): void {
  if (!store) throw new StoreWriteError()
  try {
    if (value === null) store.removeItem(slotKey(userId, slot))
    else store.setItem(slotKey(userId, slot), value)
  } catch {
    throw new StoreWriteError()
  }
}

/* 浏览器里可能存着按另一种布局放的数据：按用户名分键（<前缀><用户名>），
   节点顺序按 id 分键但用的是自己的键名。 */
const BY_USERNAME: ReadonlyArray<readonly [Slot, string]> = [
  ['landing-uris', 'nf-landing-uris:'],
  ['sub-urls', 'nf-sub-urls:'],
  ['sub-cache', 'nf-sub-cache:'],
  ['local-roles', 'nf-local-roles:'],
  ['sub-textarea-h', 'nf-sub-textarea-h:'],
  ['manual-textarea-h', 'nf-manual-textarea-h:'],
]
const orderKeyById = (userId: number) => `my.nodeOrder.${userId}`

/**
 * 把一个用户按上述布局存放的数据搬到按 id 的键下。在确认登录之后、任何读取
 * 之前调用；重复调用不会再改动。
 *
 * 只有此刻的用户名能找到按用户名存放的数据，所以迁移必须发生在改名之前：
 * 一个浏览器在迁移前就经历过改名，旧名字下的数据是找不回来的。
 *
 * 按 id 的键已经有值时不搬、也不动那个按用户名的键：不能用它覆盖已有的
 * 数据，它也不一定属于这个用户（用户名可以被别的账号用过）。
 */
export function adoptUserData(store: KeyValueStore, user: { readonly id: number; readonly username: string }): void {
  const moves: Array<readonly [string, Slot]> = BY_USERNAME.map(([slot, prefix]) => [prefix + user.username, slot] as const)
  moves.push([orderKeyById(user.id), 'node-order'])
  for (const [from, slot] of moves) {
    const value = store.getItem(from)
    if (value === null) continue
    const to = slotKey(user.id, slot)
    if (store.getItem(to) !== null) continue
    // 先写后删：写入失败时数据仍在原处，下次再搬。
    store.setItem(to, value)
    store.removeItem(from)
  }
}
