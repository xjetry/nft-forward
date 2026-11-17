import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from './api.js'
import { buildCatalog, effectiveMarks, fetchProxyMarks, loadLocalNodes, loadLocalMarks } from './landing.js'
import { mapResource } from './resource.ts'
import { useResource } from './useResource.ts'

/**
 * 本浏览器里的代理数据（URI、订阅缓存、本地标记）每变化一次就递增的计数。
 * 把它放进 useMemo 的依赖里，就能在数据变化后重新读取 localStorage。
 * 同一标签页的修改经 landing.js 的 nf-landing-changed 事件通知，其它标签页
 * 的修改经浏览器原生的 storage 事件通知。
 */
export function useLocalLandingVersion() {
  const [version, setVersion] = useState(0)
  useEffect(() => {
    const bump = () => setVersion(v => v + 1)
    window.addEventListener('nf-landing-changed', bump)
    window.addEventListener('storage', bump)
    return () => {
      window.removeEventListener('nf-landing-changed', bump)
      window.removeEventListener('storage', bump)
    }
  }, [])
  return version
}

/**
 * 当前用户的代理节点目录（口径见 landing.js 的 buildCatalog）。所有需要
 * “有哪些落地 / 直连节点”的页面都用这个 hook，不各自拼装。
 *
 * 服务端部分（标记表；普通用户另有管理员分配的落地节点）是一个远程资源，
 * 加载失败就是失败 —— 不会退化成“没有任何标记”，否则所有节点都会显示成
 * 未配置。本地部分同步读取，并在变化后自动重新推导。
 *
 * ready 时的 data：
 *   all / landing / direct / landingByKey   目录（见 buildCatalog）
 *   marks         生效标记表（本地覆盖优先）
 *   serverMarks   服务端标记表   ┐ 编辑本地标记时传给
 *   localMarks    本地覆盖表     ┘ toggleLocalMark / setLocalMarkBatch
 *   serverNodes   管理员分配的落地节点（含配额账本字段）
 *   hasDynamic    分配来源里是否有订阅（可刷新）
 *   stale         订阅刷新失败、当前是旧缓存
 *
 * @param {{ id: number, role: string } | null} user
 */
export function useLandingCatalog(user) {
  const userId = user?.id
  const isAdmin = user?.role === 'admin'

  // refresh 为真时让服务端先重新抓取分配来源里的订阅，再返回节点。
  const load = async (refresh) => {
    const [marks, assigned] = await Promise.all([
      fetchProxyMarks(),
      // 管理员没有“被分配的落地节点”这个概念，接口只对普通用户有意义。
      isAdmin ? null : api.get(refresh ? '/my/landing-nodes?refresh=1' : '/my/landing-nodes'),
    ])
    return { marks, nodes: assigned?.nodes || [], hasDynamic: !!assigned?.has_dynamic, stale: !!assigned?.stale }
  }
  const server = useResource(() => load(false), [userId, isAdmin])

  const version = useLocalLandingVersion()
  const state = useMemo(() => mapResource(server.state, (s) => {
    const localMarks = loadLocalMarks(userId)
    const marks = effectiveMarks(s.marks, localMarks)
    return {
      ...buildCatalog({ ...loadLocalNodes(userId), server: s.nodes, marks }),
      marks,
      serverMarks: s.marks,
      localMarks,
      serverNodes: s.nodes,
      hasDynamic: s.hasDynamic,
      stale: s.stale,
    }
    // version 不参与计算，但它变化意味着 localStorage 里的数据变了，需要重读。
  }), [server.state, userId, version])

  const { reloadWith } = server
  const loadRef = useRef(load)
  loadRef.current = load
  /**
   * 让服务端重新抓取分配来源里的订阅，并用结果更新目录。失败时抛出（目录
   * 同时带上刷新失败的提示，屏幕上仍是上一次的内容）。
   *
   * 走资源自己的加载路径（reloadWith），不直接改写 data：直接改写的话，一次
   * 更早发起、更晚返回的普通重新加载会用刷新之前的数据把结果盖掉。
   */
  const refreshAssigned = useCallback(async () => {
    let outcome = null
    let failure = null
    await reloadWith(async () => {
      try {
        const data = await loadRef.current(true)
        outcome = { stale: data.stale }
        return data
      } catch (err) {
        failure = err
        throw err
      }
    })
    if (failure) throw failure
    return outcome
  }, [reloadWith])

  return { state, reload: server.reload, refreshAssigned }
}
