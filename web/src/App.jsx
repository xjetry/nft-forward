import { Fragment } from 'react'
import { BrowserRouter, Routes, Route, useParams } from 'react-router-dom'
import { AppLayout } from './components/Layout'
import { PrefsProvider } from './components/prefs'
import { RequireAuth, SessionProvider } from './components/session'
import { ConfirmProvider, ErrorState, ToastProvider } from './components/ui'

import Login from './pages/Login'
import Settings from './pages/Settings'
import Dashboard from './pages/Dashboard'
import ChangePassword from './pages/ChangePassword'

import NodeList from './pages/nodes/List'
import NodeDetail from './pages/nodes/Detail'
import RulesList from './pages/rules/List'
import RulesDetail from './pages/rules/Detail'
import UserList from './pages/users/List'
import UserDetail from './pages/users/Detail'

import MyDashboard from './pages/my/Dashboard'
import MyRules from './pages/my/Rules'
import MyRuleDetail from './pages/my/RuleDetail'
import MyLandingNodes from './pages/my/LandingNodes'
import Proxies from './pages/Proxies'

/**
 * 详情页按路由里的 id 取 key：从一个对象直接跳到另一个对象时整页重新挂载。
 * 详情页里有大量本地表单状态（输入到一半的名称、展开的编辑区），它们属于
 * 某一个对象；不重新挂载的话，A 的未保存输入会留在 B 的页面上并可能被保存到 B。
 */
function PerId({ children }) {
  const { id } = useParams()
  return <Fragment key={id}>{children}</Fragment>
}

function NotFound() {
  return <ErrorState title="页面不存在" />
}

export default function App() {
  return (
    <BrowserRouter>
      <ToastProvider>
        <SessionProvider>
          <PrefsProvider>
            <ConfirmProvider>
              <Routes>
                <Route path="/login" element={<Login />} />

                <Route element={<RequireAuth />}>
                  <Route element={<AppLayout />}>
                    <Route element={<RequireAuth role="admin" />}>
                      <Route path="/" element={<Dashboard />} />
                      <Route path="/nodes" element={<NodeList />} />
                      <Route path="/nodes/:id" element={<PerId><NodeDetail /></PerId>} />
                      <Route path="/rules" element={<RulesList />} />
                      <Route path="/rules/:id" element={<PerId><RulesDetail /></PerId>} />
                      <Route path="/users" element={<UserList />} />
                      <Route path="/users/:id" element={<PerId><UserDetail /></PerId>} />
                      <Route path="/settings" element={<Settings />} />
                    </Route>

                    <Route element={<RequireAuth role="user" />}>
                      <Route path="/my" element={<MyDashboard />} />
                      <Route path="/my/rules" element={<MyRules />} />
                      <Route path="/my/rules/:id" element={<PerId><MyRuleDetail /></PerId>} />
                      <Route path="/my/landing" element={<MyLandingNodes />} />
                    </Route>

                    <Route path="/proxies" element={<Proxies />} />
                    <Route path="/change-password" element={<ChangePassword />} />
                    <Route path="*" element={<NotFound />} />
                  </Route>
                </Route>
              </Routes>
            </ConfirmProvider>
          </PrefsProvider>
        </SessionProvider>
      </ToastProvider>
    </BrowserRouter>
  )
}
