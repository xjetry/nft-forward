import { useCallback, useEffect, useReducer, useRef, useState } from 'react'
import { isNotFound } from './api.js'
import { LOADING, createLatest, reduceResource, runLoad, type Resource, type ResourceEvent } from './resource.ts'

export interface ResourceHandle<T> {
  /** 当前状态。按 status 分支渲染，或交给 <ResourceView>。 */
  readonly state: Resource<T>
  /**
   * 重新拉取。已有内容时是后台刷新（内容不离开屏幕），否则是重试。
   * 返回的 Promise 在这次拉取落定后完成，保存动作可以 `await reload()`。
   */
  readonly reload: () => Promise<void>
  /**
   * 用另一种取法做一次拉取（例如让服务端先重新抓取再返回）。与 reload 走同
   * 一条路径、共用同一个请求序号，所以它的结果同样只在“自己是最后发起的
   * 那次”时才被采用，不会被更早发起、更晚返回的请求盖掉。
   */
  readonly reloadWith: (fetcher: () => Promise<T>) => Promise<void>
  /** 本地改写 data（乐观更新）；失败时调用 reload() 以服务端为准纠正回来。 */
  readonly patch: (update: (data: T) => T) => void
}

function describeError(err: unknown) {
  return { message: err instanceof Error ? err.message : '加载失败', notFound: isNotFound(err) }
}

function sameDeps(a: readonly unknown[], b: readonly unknown[]) {
  return a.length === b.length && a.every((v, i) => Object.is(v, b[i]))
}

/**
 * 加载一个远程资源并跟踪它的状态。
 *
 * `deps` 标识“是哪一个资源”（通常是路由参数）。deps 变化意味着换了资源：
 * 状态在同一次渲染里就回到 loading，上一个资源的内容一帧都不会带着新的
 * deps 被画出来 —— 否则页面会短暂地显示 A 的数据、而操作按钮已经指向 B。
 *
 * 竞态由请求序号处理（见 resource.ts 的 createLatest）：多次加载并发时只
 * 采用最后发起的那次；卸载或换资源后，在途的响应全部作废。
 *
 * @example
 * const nodes = useResource(() => api.get('/nodes'), [])
 * const node = useResource(() => api.get(`/nodes/${id}`), [id])
 */
export function useResource<T>(fetcher: () => Promise<T>, deps: readonly unknown[]): ResourceHandle<T> {
  const [state, dispatch] = useReducer(
    reduceResource as (state: Resource<T>, event: ResourceEvent<T>) => Resource<T>,
    LOADING as Resource<T>,
  )
  const [latest] = useState(createLatest)
  // fetcher 每次渲染都是新函数；reload 需要调用最新的那个。
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  // 渲染期间发现 deps 变了就立即重置（React 会在提交前用新状态重渲染），
  // 而不是等 effect：effect 在提交之后才运行，那时旧内容已经被画出来了。
  const [trackedDeps, setTrackedDeps] = useState(deps)
  if (!sameDeps(trackedDeps, deps)) {
    setTrackedDeps(deps)
    dispatch({ type: 'reset' })
  }

  const reload = useCallback(
    () => runLoad(latest, dispatch, () => fetcherRef.current(), describeError),
    [latest],
  )

  useEffect(() => {
    void reload()
    // 卸载或 deps 变化：在途的响应属于上一个资源，全部作废。
    return () => latest.invalidate()
    // 依赖数组里展开的 deps 由调用方声明：它们变了就是换了资源，要重新加载。
  }, [reload, latest, ...deps])

  const reloadWith = useCallback(
    (via: () => Promise<T>) => runLoad(latest, dispatch, via, describeError),
    [latest],
  )

  const patch = useCallback((update: (data: T) => T) => dispatch({ type: 'patch', update }), [])

  return { state, reload, reloadWith, patch }
}
