import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { UserProvider, useUser, BlurProvider, CopyFmtProvider } from './components/Layout'
import { Loading, ConfirmProvider } from './components/ui'

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

function ProtectedRoute({ children }) {
  const { user } = useUser()
  if (user === undefined) return <Loading />
  if (user === null) return <Navigate to="/login" replace />
  return children
}

function AdminRoute({ children }) {
  const { user } = useUser()
  if (user === undefined) return <Loading />
  if (user === null) return <Navigate to="/login" replace />
  if (user.role !== 'admin') return <Navigate to="/my" replace />
  return children
}

function UserRoute({ children }) {
  const { user } = useUser()
  if (user === undefined) return <Loading />
  if (user === null) return <Navigate to="/login" replace />
  if (user.role === 'admin') return <Navigate to="/" replace />
  return children
}

function RootRedirect() {
  const { user } = useUser()
  if (user === undefined) return <Loading />
  if (user === null) return <Navigate to="/login" replace />
  if (user.role !== 'admin') return <Navigate to="/my" replace />
  return <Dashboard />
}

function NotFound() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-app">
      <div className="text-center">
        <h1 className="text-2xl font-bold text-ink">404</h1>
        <p className="mt-2 text-ink-soft">页面不存在</p>
      </div>
    </div>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <UserProvider>
        <ConfirmProvider>
        <BlurProvider>
        <CopyFmtProvider>
        <Routes>
          <Route path="/login" element={<Login />} />

          <Route path="/" element={<RootRedirect />} />

          {/* Admin routes */}
          <Route path="/nodes" element={<AdminRoute><NodeList /></AdminRoute>} />
          <Route path="/nodes/:id" element={<AdminRoute><NodeDetail /></AdminRoute>} />
          <Route path="/rules" element={<AdminRoute><RulesList /></AdminRoute>} />
          <Route path="/rules/:id" element={<AdminRoute><RulesDetail /></AdminRoute>} />
          <Route path="/users" element={<AdminRoute><UserList /></AdminRoute>} />
          <Route path="/users/:id" element={<AdminRoute><UserDetail /></AdminRoute>} />
          <Route path="/settings" element={<AdminRoute><Settings /></AdminRoute>} />

          {/* Regular user routes */}
          <Route path="/my" element={<UserRoute><MyDashboard /></UserRoute>} />
          <Route path="/my/rules" element={<UserRoute><MyRules /></UserRoute>} />
          <Route path="/my/rules/:id" element={<UserRoute><MyRuleDetail /></UserRoute>} />
          <Route path="/my/landing" element={<UserRoute><MyLandingNodes /></UserRoute>} />

          {/* Shared routes */}
          <Route path="/proxies" element={<ProtectedRoute><Proxies /></ProtectedRoute>} />
          <Route path="/change-password" element={<ProtectedRoute><ChangePassword /></ProtectedRoute>} />

          <Route path="*" element={<NotFound />} />
        </Routes>
        </CopyFmtProvider>
        </BlurProvider>
        </ConfirmProvider>
      </UserProvider>
    </BrowserRouter>
  )
}
