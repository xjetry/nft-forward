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

  const { tenant, tunnels = [], grants = [], node_by_id = {} } = data

  const expiresAt = tenant.expires_at?.Valid && tenant.expires_at.Int64 > 0 ? tenant.expires_at.Int64 : null

  return (
    <Layout>
      {tenant.disabled && (
        <div className="mb-4 px-4 py-3 bg-red-50 border border-red-200 rounded-lg text-red-600 text-sm font-medium">
          您的账号已被禁用：{nullStr(tenant.disable_reason)}。请联系管理员。
        </div>
      )}

      {/* Quota */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">我的配额</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="fl">最大转发数</span><span className="font-mono">{tenant.max_forwards}</span>
            <span className="fl">流量配额</span><span className="font-mono">{tenant.traffic_quota_bytes === 0 ? <span className="text-xl">&#x221e;（不限）</span> : `${Math.floor(tenant.traffic_quota_bytes / 1048576)} MB`}</span>
            <span className="fl">已用流量</span>
            <span className="font-mono">
              {Math.floor(tenant.traffic_used_bytes / 1048576)} MB
              {tenant.traffic_quota_bytes > 0 && ` (${pct(tenant.traffic_used_bytes, tenant.traffic_quota_bytes)}%)`}
            </span>
            <span className="fl">到期时间</span>
            <span className="font-mono">
              {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
            </span>
          </div>
        </div>
      </div>

      {/* Available tunnels */}
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">可用通道</h3>
          <Link to="/my/forwards" className="btn-secondary text-xs ml-auto">前往「我的转发」</Link>
        </div>
        {tunnels.length ? (
          <table className="tbl">
            <thead><tr><th>通道</th><th>节点</th><th>协议</th><th>端口段</th><th>允许目标 CIDR</th><th>本通道上限</th></tr></thead>
            <tbody>
              {tunnels.map((t, i) => {
                const node = node_by_id?.[t.node_id]
                return (
                  <tr key={t.id}>
                    <td className="font-semibold">{t.name}</td>
                    <td className="font-mono text-gray-500">{node ? node.name : '--'}</td>
                    <td>{t.proto_mask}</td>
                    <td className="font-mono">{t.port_start}-{t.port_end}</td>
                    <td className="font-mono text-gray-500">{t.target_cidr_allow}</td>
                    <td className="font-mono">{grants[i]?.max_forwards}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        ) : <Empty title="管理员尚未为您授权任何通道" desc="请联系管理员。" />}
      </div>
    </Layout>
  )
}
