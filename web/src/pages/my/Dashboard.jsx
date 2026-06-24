import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { pct, fmtTrafficGB, fmtDate, isExpired, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, NodeTypeBadge } from '../../components/ui'
import { loadLocalURIs, saveLocalURIs, parseURIs } from '../../lib/landing'

export default function MyDashboard() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    api.get('/my').then(setData).catch(console.error).finally(() => setLoading(false))
  }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="无法加载数据" /></Layout>

  const { user, nodes = [], grants = [], rules = [] } = data

  const expiresAt = user.expires_at?.Valid && user.expires_at.Int64 > 0 ? user.expires_at.Int64 : null

  return (
    <Layout>
      {user.disabled && (
        <div className="mb-4 px-4 py-3 bg-red-50 border border-red-200 rounded-lg text-red-600 text-sm font-medium">
          您的账号已被禁用：{nullStr(user.disable_reason)}。请联系管理员。
        </div>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-[1.15fr_1fr] gap-[18px] mb-[22px]">
        {/* Quota */}
        <div className="card flex flex-col">
          <div className="px-6 py-[22px] flex-1 flex flex-col">
            <h3 className="text-[16px] font-bold mb-5">我的配额</h3>
            <div className="flex items-center gap-4 py-3 border-b border-line-soft">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">规则配额</div>
              <div className="text-[14.5px]"><span className="font-mono">{rules.length}</span> <span className="text-ink-mut">/</span> <span className="font-mono">{user.max_forwards}</span></div>
            </div>
            <div className="flex items-center gap-4 py-3 border-b border-line-soft">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">流量</div>
              <div className="text-[14.5px] font-mono">
                {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
                {user.traffic_quota_bytes > 0 && <span className="text-green-600 dark:text-green-400"> ({pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)</span>}
              </div>
            </div>
            <div className="flex items-center gap-4 py-3">
              <div className="w-[130px] flex-shrink-0 text-[14px] text-ink-soft">到期时间</div>
              <div className="text-[14.5px]">
                {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
              </div>
            </div>
          </div>
        </div>

        {/* My proxy URIs (browser-local) */}
        <MyProxyURIs username={user.username} />
      </div>

      {/* Granted nodes */}
      <div className="card">
        <div className="card-header">
          <h3 className="text-[15px] font-bold">已授权节点</h3>
        </div>
        {nodes.length ? (
          <table className="tbl">
            <thead><tr><th>节点</th><th>类型</th><th>状态</th><th>本节点上限</th></tr></thead>
            <tbody>
              {nodes.map((n, i) => (
                <tr key={n.id}>
                  <td className="font-semibold">{n.name}</td>
                  <td><NodeTypeBadge type={n.node_type} /></td>
                  <td><NodeOnline node={n} /></td>
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

// Online/offline (or disabled) status for a granted node. The server resolves
// composite nodes' online state from their children before sending.
function NodeOnline({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
}

// MyProxyURIs edits the user's own proxy URIs, kept only in this browser
// (localStorage) and never uploaded. They feed the create-rule landing picker
// and the relay-URI copy on the rules page.
function MyProxyURIs({ username }) {
  const [text, setText] = useState(() => loadLocalURIs(username))
  const toast = useToast()
  const count = parseURIs(text).length

  const save = () => {
    saveLocalURIs(username, text)
    toast('已保存到本浏览器')
  }

  return (
    <div className="card flex flex-col">
      <div className="px-6 py-[22px] flex-1 flex flex-col">
        <div className="flex items-baseline gap-2.5 mb-3.5">
          <h3 className="text-[16px] font-bold">我的代理 URI</h3>
          <span className="text-[13px] text-ink-mut">{count} 个节点</span>
        </div>
        <p className="text-[13px] leading-[1.7] text-ink-soft mb-3.5">
          每行一条（vless:// / trojan:// / ss:// / vmess:// 等）。
          <span className="text-amber-500 font-semibold">仅保存在本浏览器，不会上传服务器。</span>
          创建规则时可从中选择落地出口；规则出口与某条 URI 的 host:port 一致时，规则页可一键复制中转代理 URI。
        </p>
        <textarea className="input-field font-mono w-full flex-1 min-h-[100px] resize-y !py-3 !px-3.5" value={text} onChange={e => setText(e.target.value)}
          placeholder={'vless://…\ntrojan://…'} />
        <button onClick={save} className="btn-primary mt-3.5">保存</button>
      </div>
    </div>
  )
}
