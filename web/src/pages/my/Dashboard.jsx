import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { pct, fmtDate, isExpired, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge } from '../../components/ui'

export default function MyDashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    api.get('/my').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>

  const { user, nodes = [], grants = [] } = data

  const expiresAt = user.expires_at?.Valid && user.expires_at.Int64 > 0 ? user.expires_at.Int64 : null

  return (
    <Layout>
      {user.disabled && (
        <div className="mb-4 px-4 py-3 bg-red-50 border border-red-200 rounded-lg text-red-600 text-sm font-medium">
          您的账号已被禁用：{nullStr(user.disable_reason)}。请联系管理员。
        </div>
      )}

      {/* Quota */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">我的配额</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="fl">最大转发数</span><span className="font-mono">{user.max_forwards}</span>
            <span className="fl">流量配额</span><span className="font-mono">{user.traffic_quota_bytes === 0 ? <span className="text-xl">&#x221e;（不限）</span> : `${Math.floor(user.traffic_quota_bytes / 1048576)} MB`}</span>
            <span className="fl">已用流量</span>
            <span className="font-mono">
              {Math.floor(user.traffic_used_bytes / 1048576)} MB
              {user.traffic_quota_bytes > 0 && ` (${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)`}
            </span>
            <span className="fl">到期时间</span>
            <span className="font-mono">
              {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
            </span>
          </div>
        </div>
      </div>

      {/* Granted nodes */}
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">已授权节点</h3>
          <Link to="/my/rules" className="btn-secondary text-xs ml-auto">前往「我的规则」</Link>
        </div>
        {nodes.length ? (
          <table className="tbl">
            <thead><tr><th>节点</th><th>类型</th><th>本节点上限</th></tr></thead>
            <tbody>
              {nodes.map((n, i) => (
                <tr key={n.id}>
                  <td className="font-semibold">{n.name}</td>
                  <td><NodeTypeBadge type={n.node_type} /></td>
                  <td className="font-mono">{grants[i]?.max_forwards ?? '--'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="管理员尚未为您授权任何节点" desc="请联系管理员。" />}
      </div>
    </Layout>
  )
}

function NodeTypeBadge({ type }) {
  if (type === 'composite') return <Badge color="violet">组合</Badge>
  if (type === 'self') return <Badge color="blue">自身</Badge>
  return <Badge color="green">单点</Badge>
}
