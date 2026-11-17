/* 远程资源的加载状态模型。

   用 data / loading / error 三个独立变量表示加载状态时，三者能组合出没有
   意义的状态（loading 且 error、无 data 也无 error），调用方漏掉一个分支
   就会把“加载失败”渲染成“没有数据”或“不存在”。所以这里用一个可辨识
   联合：状态之间互斥，渲染时必须逐个分支处理。

   状态：
     loading  还没有任何可显示的内容。
     error    没能拿到内容。notFound 区分“资源不存在”与其它失败。
     ready    有内容可显示。之后的刷新不会回到 loading：
              refreshing   正在后台刷新，屏幕上仍是上一次的 data；
              refreshError 最近一次刷新失败的原因（屏幕上的 data 可能已过期）。

   不变式：一旦进入 ready，除非资源被重置（换了一个资源）或刷新发现它已
   不存在，否则不会离开 ready —— 刷新失败不能把屏幕上已有的内容换成错误页，
   更不能卸载用户正在编辑的子树。 */

export type Resource<T> =
  | { readonly status: 'loading' }
  | { readonly status: 'error'; readonly message: string; readonly notFound: boolean }
  | { readonly status: 'ready'; readonly data: T; readonly refreshing: boolean; readonly refreshError: string | null }

export type ResourceEvent<T> =
  /** 换了一个资源（例如路由里的 id 变了）：丢弃一切，回到 loading。 */
  | { readonly type: 'reset' }
  /** 发起了一次加载或刷新。 */
  | { readonly type: 'fetch' }
  | { readonly type: 'resolve'; readonly data: T }
  | { readonly type: 'reject'; readonly message: string; readonly notFound: boolean }
  /** 本地改写 data（乐观更新）。只在 ready 下有意义。 */
  | { readonly type: 'patch'; readonly update: (data: T) => T }

export const LOADING = { status: 'loading' } as const

export function reduceResource<T>(state: Resource<T>, event: ResourceEvent<T>): Resource<T> {
  switch (event.type) {
    case 'reset':
      return LOADING
    case 'fetch':
      // 失败后的重试回到 loading（错误页换成加载占位）；已有内容时只标记
      // 正在刷新，内容留在屏幕上。
      if (state.status === 'ready') return { ...state, refreshing: true }
      return LOADING
    case 'resolve':
      return { status: 'ready', data: event.data, refreshing: false, refreshError: null }
    case 'reject':
      // 刷新时发现资源已不存在：继续显示旧内容会让用户对一个不存在的对象
      // 操作，所以这是唯一一种从 ready 退到 error 的情况。
      if (state.status === 'ready' && !event.notFound) {
        return { ...state, refreshing: false, refreshError: event.message }
      }
      return { status: 'error', message: event.message, notFound: event.notFound }
    case 'patch':
      if (state.status !== 'ready') return state
      return { ...state, data: event.update(state.data) }
    default:
      return assertNever(event)
  }
}

/* “只认最后一次”的请求序号。

   同一个资源可能同时有多次加载在途（连续两次保存各触发一次刷新、切换筛选
   条件）。响应到达的顺序不保证与发出的顺序一致，所以每次加载领一个令牌，
   只有仍然是最新令牌的那次响应才允许写入状态；其余的直接丢弃。
   invalidate() 让所有在途响应作废（组件卸载、资源被重置时调用）。 */
export interface Latest {
  begin(): number
  isCurrent(token: number): boolean
  invalidate(): void
}

export function createLatest(): Latest {
  let current = 0
  return {
    begin: () => ++current,
    isCurrent: token => token === current,
    invalidate: () => { current++ },
  }
}

/**
 * 发起一次加载，并把结果以事件的形式交给 dispatch。迟到的响应（不再是最新
 * 令牌）不产生任何事件。返回的 Promise 在这次加载落定后完成，无论它的结果
 * 是否被采用。
 */
export async function runLoad<T>(
  latest: Latest,
  dispatch: (event: ResourceEvent<T>) => void,
  fetcher: () => Promise<T>,
  describeError: (err: unknown) => { message: string; notFound: boolean },
): Promise<void> {
  const token = latest.begin()
  dispatch({ type: 'fetch' })
  try {
    const data = await fetcher()
    if (latest.isCurrent(token)) dispatch({ type: 'resolve', data })
  } catch (err) {
    if (latest.isCurrent(token)) dispatch({ type: 'reject', ...describeError(err) })
  }
}

/** 对 ready 里的 data 做变换，其余状态原样传递（用于从原始响应推导视图数据）。 */
export function mapResource<T, U>(state: Resource<T>, f: (data: T) => U): Resource<U> {
  return state.status === 'ready' ? { ...state, data: f(state.data) } : state
}

/* 把两个资源并成一个：页面需要两份数据都到齐才能渲染时用。
   任一失败即失败（“不存在”优先于其它失败，因为它是结论而不是可重试的故障），
   否则任一仍在加载即加载中，两者都就绪才就绪。 */
export function combineResources<A, B>(a: Resource<A>, b: Resource<B>): Resource<readonly [A, B]> {
  const failed = [a, b].filter(s => s.status === 'error')
  if (failed.length) return failed.find(s => s.notFound) ?? failed[0]!
  if (a.status !== 'ready' || b.status !== 'ready') return LOADING
  return {
    status: 'ready',
    data: [a.data, b.data],
    refreshing: a.refreshing || b.refreshing,
    refreshError: a.refreshError ?? b.refreshError,
  }
}

/** 穷尽性检查：联合类型新增成员而某个 switch 没处理时，这里会编译失败。 */
export function assertNever(value: never): never {
  throw new Error(`未处理的分支：${JSON.stringify(value)}`)
}
