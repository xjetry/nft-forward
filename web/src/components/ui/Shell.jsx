import { useEffect, useState } from 'react'
import { NavLink, useLocation } from 'react-router-dom'
import { cx } from './cx'
import { IconButton } from './Button'
import { IconChevronsLeft, IconMenu } from './icons'

const LG = '(min-width: 64rem)'

/** 视口是否达到侧栏常驻的宽度（Tailwind 的 lg）。 */
function useWide() {
  const [wide, setWide] = useState(() => window.matchMedia(LG).matches)
  useEffect(() => {
    const mql = window.matchMedia(LG)
    const onChange = () => setWide(mql.matches)
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [])
  return wide
}

/**
 * 应用外壳：侧栏 + 内容区。不认识具体的导航项与用户，这些由调用方传入。
 *
 * 侧栏有两种形态，由视口宽度决定，不由调用方决定：
 * - 宽屏：常驻在左侧，可折叠成只有图标的窄栏（折叠状态记在 localStorage）。
 * - 窄屏：收在屏幕外，从顶栏的菜单按钮滑入（抽屉）。抽屉关闭时整棵子树
 *   是 inert 的，键盘焦点不会落到屏幕外的链接上；路由变化或按 Esc 会关闭它。
 *
 * 抽屉的滑入是外壳里唯一的动画：它说明这块面板从哪里来、回到哪里去。
 *
 * @param {{
 *   brand: { name: string, sub?: import('react').ReactNode, mark: import('react').ReactNode },
 *   nav: Array<Array<{ to: string, label: string, icon: import('react').ReactNode, end?: boolean }>>,
 *   footer: (collapsed: boolean) => import('react').ReactNode,
 *   children: import('react').ReactNode,
 * }} props `nav` 是分组后的导航项，组与组之间画一条分隔线。
 */
export function AppShell({ brand, nav, footer, children }) {
  const wide = useWide()
  const location = useLocation()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [collapsedPref, setCollapsedPref] = useState(() => localStorage.getItem('nf-sidebar') === '1')
  // 折叠只对常驻形态有意义；抽屉总是完整宽度。
  const collapsed = wide && collapsedPref

  useEffect(() => { setDrawerOpen(false) }, [location.pathname])
  useEffect(() => {
    if (!drawerOpen) return
    const onKey = (e) => { if (e.key === 'Escape') setDrawerOpen(false) }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [drawerOpen])

  const toggleCollapsed = () => setCollapsedPref(v => {
    localStorage.setItem('nf-sidebar', v ? '0' : '1')
    return !v
  })

  return (
    <div className="flex h-dvh overflow-hidden bg-app text-ink">
      {drawerOpen && !wide && (
        <div className="fixed inset-0 z-40 bg-scrim" onClick={() => setDrawerOpen(false)} aria-hidden="true" />
      )}

      <aside
        inert={!wide && !drawerOpen}
        aria-label="导航"
        className={cx(
          'fixed inset-y-0 left-0 z-40 flex w-58 flex-none flex-col border-r border-line bg-surface',
          'transition-transform duration-250 ease-out lg:static lg:translate-x-0 lg:transition-none',
          drawerOpen ? 'translate-x-0' : '-translate-x-full',
          collapsed && 'lg:w-14',
        )}
      >
        <div className={cx('flex h-14 flex-none items-center gap-2.5', collapsed ? 'justify-center' : 'px-4')}>
          <span className="grid size-7 flex-none place-items-center rounded-control bg-accent text-on-accent" title={collapsed ? brand.name : undefined}>
            {brand.mark}
          </span>
          {!collapsed && (
            <span className="min-w-0">
              <span className="block truncate text-body font-semibold">{brand.name}</span>
              {brand.sub && <span className="block truncate text-caption text-ink-mut">{brand.sub}</span>}
            </span>
          )}
        </div>

        <nav className={cx('min-h-0 flex-1 overflow-y-auto py-2', collapsed ? 'px-2' : 'px-3')}>
          {nav.filter(group => group.length > 0).map((group, i) => (
            <ul key={i} className={cx('flex flex-col gap-0.5', i > 0 && 'mt-2 border-t border-line-soft pt-2')}>
              {group.map(item => (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={item.end}
                    title={collapsed ? item.label : undefined}
                    aria-label={collapsed ? item.label : undefined}
                    className={({ isActive }) => cx(
                      'flex h-control items-center gap-2.5 rounded-control text-body font-medium',
                      collapsed ? 'justify-center' : 'px-2.5',
                      isActive ? 'bg-accent-soft text-accent-ink' : 'text-ink-soft hover:bg-subtle hover:text-ink',
                    )}
                  >
                    {item.icon}
                    {!collapsed && <span className="truncate">{item.label}</span>}
                  </NavLink>
                </li>
              ))}
            </ul>
          ))}
        </nav>

        <div className={cx('flex flex-none flex-col gap-2 border-t border-line-soft py-3', collapsed ? 'items-center px-2' : 'px-3')}>
          {footer(collapsed)}
          {wide && (
            <IconButton size="sm" label={collapsed ? '展开侧栏' : '收起侧栏'} onClick={toggleCollapsed} className={collapsed ? undefined : 'self-end'}>
              <IconChevronsLeft className={collapsed ? 'rotate-180' : undefined} />
            </IconButton>
          )}
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        {!wide && (
          <div className="flex h-12 flex-none items-center gap-2 border-b border-line bg-surface px-2">
            <IconButton label="打开导航" aria-expanded={drawerOpen} onClick={() => setDrawerOpen(true)}>
              <IconMenu size={18} />
            </IconButton>
            <span className="truncate text-body font-semibold">{brand.name}</span>
          </div>
        )}
        <main className="min-h-0 flex-1 overflow-y-auto px-4 py-5 sm:px-6 sm:py-6">
          {children}
        </main>
      </div>
    </div>
  )
}
