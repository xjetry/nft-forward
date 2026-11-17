import { assertNever } from '../lib/resource'
import { Alert, ErrorState, Loading, TextLink } from './ui'

/**
 * 按资源状态渲染：加载中 / 失败 / 有内容，三者互斥且必须全部处理。
 *
 * - 失败与“不存在”是两种不同的结论：失败给出原因和重试，不存在只陈述事实。
 *   两者都不会被渲染成“暂无数据”。
 * - 有内容之后的刷新失败不会把内容换掉，而是在内容上方留一条提示：屏幕上
 *   的数据可能已经过期，并给出重试。
 *
 * @template T
 * @param {{
 *   resource: import('../lib/useResource').ResourceHandle<T>,
 *   notFound?: string,
 *   children: (data: T) => import('react').ReactNode,
 * }} props `notFound` 是资源不存在时的标题，如“节点不存在”。
 */
export function ResourceView({ resource, notFound = '内容不存在', children }) {
  const { state, reload } = resource
  switch (state.status) {
    case 'loading':
      return <Loading />
    case 'error':
      return state.notFound
        ? <ErrorState title={notFound} />
        : <ErrorState message={state.message} onRetry={reload} />
    case 'ready':
      return (
        <>
          {state.refreshError && (
            <Alert tone="warn" className="flex-none">
              刷新失败，当前显示的可能不是最新数据：{state.refreshError}{' '}
              <TextLink onClick={reload}>重试</TextLink>
            </Alert>
          )}
          {children(state.data)}
        </>
      )
    default:
      return assertNever(state)
  }
}
