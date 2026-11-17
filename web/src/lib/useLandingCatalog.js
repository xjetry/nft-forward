import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from './api.js'
import { buildCatalog, effectiveRoles, fetchNodeRoles, loadLocalNodes, loadLocalRoles } from './landing.js'
import { mapResource } from './resource.ts'
import { useResource } from './useResource.ts'

/**
 * 本浏览器里的代理数据（URI、订阅缓存、本地角色）每变化一次就递增的计数。
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
 * 服务端部分（角色表；普通用户另有管理员分配的落地节点）是一个远程资源，
 * 加载失败就是失败 —— 不会退化成“没有任何角色”，否则所有节点都会显示成
 * 未配置。本地部分同步读取，并在变化后自动重新推导。
 *
 * ready 时的 data：
 *   all / landing / direct / landingByKey   目录（见 buildCatalog）
 *   roles         生效角色表（本地覆盖优先）
 *   serverRoles   服务端角色表   ┐ 编辑本地角色时传给
 *   localRoles    本地覆盖表     ┘ toggleLocalRole / setLocalRoleBatch
 *   serverNodes   管理员分配的落地节点（含配额账本字段）
 *   hasDynamic    分配来源里是否有订阅（可刷新）
 *   stale         订阅刷新失败、当前是旧缓存
 *
 * @param {{ username: string, role: string } | null} user
 */
export function useLandingCatalog(user) {
  const username = user?.username
  const isAdmin = user?.role === 'admin'

  const server = useResource(async () => {
    const [roles, assigned] = await Promise.all([
      fetchNodeRoles(),
      // 管理员没有“被分配的落地节点”这个概念，接口只对普通用户有意义。
      isAdmin ? null : api.get('/my/landing-nodes'),
    ])
    return { roles, nodes: assigned?.nodes || [], hasDynamic: !!assigned?.has_dynamic, stale: !!assigned?.stale }
  }, [username, isAdmin])

  const version = useLocalLandingVersion()
  const state = useMemo(() => mapResource(server.state, (s) => {
    const localRoles = loadLocalRoles(username)
    const roles = effectiveRoles(s.roles, localRoles)
    return {
      ...buildCatalog({ ...loadLocalNodes(username), server: s.nodes, roles }),
      roles,
      serverRoles: s.roles,
      localRoles,
      serverNodes: s.nodes,
      hasDynamic: s.hasDynamic,
      stale: s.stale,
    }
    // version 不参与计算，但它变化意味着 localStorage 里的数据变了，需要重读。
  }), [server.state, username, version])

  const { patch } = server
  /** 让服务端重新抓取分配来源里的订阅，并用结果更新目录。失败时抛出。 */
  const refreshAssigned = useCallback(async () => {
    const d = await api.get('/my/landing-nodes?refresh=1')
    patch(s => ({ ...s, nodes: d?.nodes || [], hasDynamic: !!d?.has_dynamic, stale: !!d?.stale }))
    return { stale: !!d?.stale }
  }, [patch])

  return { state, reload: server.reload, refreshAssigned }
}
