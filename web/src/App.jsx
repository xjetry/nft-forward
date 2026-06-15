import { BrowserRouter, Routes, Route, NavLink } from 'react-router-dom'

function Layout({ children }) {
  return (
    <div className="min-h-screen bg-gray-50">
      <nav className="bg-white border-b border-gray-200">
        <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
          <div className="flex items-center justify-between h-14">
            <span className="text-lg font-semibold text-gray-900">nft-forward</span>
            <div className="flex gap-4 text-sm">
              <NavLink to="/" className={({ isActive }) =>
                isActive ? 'text-blue-600 font-medium' : 'text-gray-500 hover:text-gray-700'
              } end>
                Dashboard
              </NavLink>
            </div>
          </div>
        </div>
      </nav>
      <main className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-6">
        {children}
      </main>
    </div>
  )
}

function Dashboard() {
  return (
    <Layout>
      <h1 className="text-2xl font-bold text-gray-900">Dashboard</h1>
      <p className="mt-2 text-gray-600">React frontend scaffold is working.</p>
    </Layout>
  )
}

function NotFound() {
  return (
    <Layout>
      <h1 className="text-2xl font-bold text-gray-900">404</h1>
      <p className="mt-2 text-gray-600">Page not found.</p>
    </Layout>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </BrowserRouter>
  )
}
