import { Outlet, useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { hasLocalProxies } from '../lib/landing'
import { useLocalLandingVersion } from '../lib/useLandingCatalog'
import { usePrefs } from './prefs'
import { useUser } from './session'
import {
  AppShell, IconButton, IconDashboard, IconEye, IconEyeOff, IconMoon, IconRoute, IconServer, IconSettings,
  IconShield, IconSun, IconUsers, IconLink, Menu, Segmented, useAction,
} from './ui'

const REPO_URL = 'https://github.com/xjetry/nft-forward'

/** 侧栏导航项，按角色给出。分组之间由外壳画分隔线。 */
function navFor(user, hasProxies) {
  if (user.role === 'admin') {
    return [
      [
        { to: '/', label: '概览', icon: <IconDashboard />, end: true },
        { to: '/nodes', label: '节点', icon: <IconServer /> },
        { to: '/rules', label: '规则', icon: <IconRoute /> },
        { to: '/users', label: '用户', icon: <IconUsers /> },
        ...(hasProxies ? [{ to: '/proxies', label: '我的代理', icon: <IconLink /> }] : []),
      ],
      [{ to: '/settings', label: '系统设置', icon: <IconSettings /> }],
    ]
  }
  // 普通用户只有在有落地来源（管理员分配的，或自己在本浏览器填的）时才有
  // 这两项；没有来源时页面里无内容可看。
  const hasLanding = hasProxies || user.has_landing_source
  return [[
    { to: '/my', label: '概览', icon: <IconDashboard />, end: true },
    { to: '/my/rules', label: '我的规则', icon: <IconRoute /> },
    ...(hasLanding ? [
      { to: '/my/landing', label: '落地节点', icon: <IconShield /> },
      { to: '/proxies', label: '我的代理', icon: <IconLink /> },
    ] : []),
  ]]
}

/**
 * 登录后的应用布局，作为布局路由使用：页面经 <Outlet/> 渲染在内容区。
 * 外壳只挂载一次，切换页面时侧栏不会重建。
 */
export function AppLayout() {
  const { user, panelName, version, endSession } = useUser()
  const navigate = useNavigate()
  const { isDark, toggleTheme, blurred, toggleBlur, copyFmt, setCopyFmt } = usePrefs()
  // 本地代理数据变化会改变导航项的有无，订阅它的变化以便重新计算。
  useLocalLandingVersion()

  // 退出请求成功，服务端已经删掉会话并清掉 cookie（apiLogout），会话就此结束。
  // 这里不自己跳转：转到登录页由 RequireAuth 按会话状态决定。
  const logout = useAction(async () => {
    await api.post('/logout')
    endSession()
  })

  const accountItems = (collapsed) => [
    // 折叠时侧栏放不下格式切换，把它并进账户菜单，保证这个偏好始终可达。
    ...(collapsed ? [{
      label: copyFmt === 'yaml' ? '复制格式改为 URI' : '复制格式改为 YAML',
      onSelect: () => setCopyFmt(copyFmt === 'yaml' ? 'uri' : 'yaml'),
    }] : []),
    { label: '修改密码', onSelect: () => navigate('/change-password') },
    { label: '项目主页', onSelect: () => window.open(REPO_URL, '_blank', 'noopener,noreferrer') },
    { label: '退出登录', onSelect: logout.run },
  ]

  const displayToggles = (
    <>
      <IconButton size="sm" label={blurred ? '显示敏感信息' : '模糊敏感信息'} aria-pressed={blurred} onClick={toggleBlur}>
        {blurred ? <IconEyeOff /> : <IconEye />}
      </IconButton>
      <IconButton size="sm" label={isDark ? '切换到浅色' : '切换到深色'} onClick={toggleTheme}>
        {isDark ? <IconSun /> : <IconMoon />}
      </IconButton>
    </>
  )

  return (
    <AppShell
      brand={{
        name: panelName,
        sub: version ? <span className="font-mono">{version}</span> : null,
        mark: <IconRoute />,
      }}
      nav={navFor(user, hasLocalProxies(user.id))}
      footer={(collapsed) => (collapsed ? (
        <>
          {displayToggles}
          <Menu size="sm" label={`账户：${user.username}`} items={accountItems(true)} />
        </>
      ) : (
        <>
          <div className="flex items-center gap-1">
            <Segmented
              size="sm"
              label="复制代理链接的格式"
              value={copyFmt}
              onChange={setCopyFmt}
              options={[{ value: 'uri', label: 'URI' }, { value: 'yaml', label: 'YAML' }]}
            />
            <span className="flex-1" />
            {displayToggles}
          </div>
          <div className="flex items-center gap-2">
            <div className="min-w-0 flex-1">
              <div className="truncate text-small font-medium text-ink">{user.username}</div>
              <div className="text-caption text-ink-mut">{user.role === 'admin' ? '管理员' : '用户'}</div>
            </div>
            <Menu size="sm" label="账户" items={accountItems(false)} />
          </div>
        </>
      ))}
    >
      <Outlet />
    </AppShell>
  )
}
