import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { UserProvider, useUser } from './components/Layout'
import { Loading } from './components/ui'

import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import ChangePassword from './pages/ChangePassword'

import NodeList from './pages/nodes/List'
import NodeDetail from './pages/nodes/Detail'
import TunnelList from './pages/tunnels/List'
import ForwardList from './pages/forwards/List'
import ForwardEdit from './pages/forwards/Edit'
import ChainList from './pages/chains/List'
import ChainDetail from './pages/chains/Detail'
import ComboList from './pages/combos/List'
import TenantList from './pages/tenants/List'
import TenantDetail from './pages/tenants/Detail'
import UserList from './pages/users/List'

import MyDashboard from './pages/my/Dashboard'
import MyForwards from './pages/my/Forwards'
import MyChains from './pages/my/Chains'

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

function TenantRoute({ children }) {
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
    <div className="min-h-screen flex items-center justify-center bg-gray-50">
      <div className="text-center">
        <h1 className="text-2xl font-bold text-gray-900">404</h1>
        <p className="mt-2 text-gray-600">页面不存在</p>
      </div>
    </div>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <UserProvider>
        <Routes>
          <Route path="/login" element={<Login />} />

          {/* Root: admin gets dashboard, tenant gets /my */}
          <Route path="/" element={<RootRedirect />} />

          {/* Admin routes */}
          <Route path="/nodes" element={<AdminRoute><NodeList /></AdminRoute>} />
          <Route path="/nodes/:id" element={<AdminRoute><NodeDetail /></AdminRoute>} />
          <Route path="/tunnels" element={<AdminRoute><TunnelList /></AdminRoute>} />
          <Route path="/forwards" element={<AdminRoute><ForwardList /></AdminRoute>} />
          <Route path="/forwards/:id/edit" element={<AdminRoute><ForwardEdit /></AdminRoute>} />
          <Route path="/chains" element={<AdminRoute><ChainList /></AdminRoute>} />
          <Route path="/chains/:id" element={<AdminRoute><ChainDetail /></AdminRoute>} />
          <Route path="/combos" element={<AdminRoute><ComboList /></AdminRoute>} />
          <Route path="/tenants" element={<AdminRoute><TenantList /></AdminRoute>} />
          <Route path="/tenants/:id" element={<AdminRoute><TenantDetail /></AdminRoute>} />
          <Route path="/users" element={<AdminRoute><UserList /></AdminRoute>} />

          {/* Tenant routes */}
          <Route path="/my" element={<TenantRoute><MyDashboard /></TenantRoute>} />
          <Route path="/my/forwards" element={<TenantRoute><MyForwards /></TenantRoute>} />
          <Route path="/my/chains" element={<TenantRoute><MyChains /></TenantRoute>} />

          {/* Shared routes */}
          <Route path="/change-password" element={<ProtectedRoute><ChangePassword /></ProtectedRoute>} />

          <Route path="*" element={<NotFound />} />
        </Routes>
      </UserProvider>
    </BrowserRouter>
  )
}
