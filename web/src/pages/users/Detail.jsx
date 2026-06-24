import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, fmtTrafficGB, pct, fmtDate, fmtDateInput, isExpired, nullInt, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, NodeTypeBadge, useConfirm, Select, Modal } from '../../components/ui'

export default function UserDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const toast = useToast()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [newPassword, setNewPassword] = useState(null)
  const confirm = useConfirm()

  const load = () => {
    setLoading(true)
    api.get(`/users/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="用户不存在" /></Layout>

  const { user, nodes = [], grants = [], all_nodes = [], rules = [], landing_nodes = [] } = data

  const isRegularUser = user.role === 'user'

  const toggleUser = async () => {
    try { await api.post(`/users/${id}/toggle`); toast(user.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }
  const resetTraffic = async () => {
    if (!(await confirm({ title: '清零流量', message: '清零该用户的已用流量？', confirmText: '清零', danger: true }))) return
    try { await api.post(`/users/${id}/reset-traffic`); toast('已重置'); load() } catch (err) { toast(err.message) }
  }
  const deleteUser = async () => {
    if (!(await confirm({ title: '删除用户', message: '删除用户？关联的规则将被一并清除。', confirmText: '删除', danger: true }))) return
    try { await api.del(`/users/${id}`); toast('已删除'); navigate('/users') } catch (err) { toast(err.message) }
  }
  const resetPassword = async () => {
    if (!(await confirm({ title: '重置密码', message: '重置该用户密码？新密码只显示一次，请及时复制保存。', confirmText: '重置', danger: true }))) return
    try {
      const d = await api.post(`/users/${id}/reset-password`)
      if (d?.new_password) setNewPassword(d.new_password)
      else toast('已重置')
    } catch (err) { toast(err.message) }
  }

  const expiresAt = user.expires_at?.Valid && user.expires_at.Int64 > 0 ? user.expires_at.Int64 : null

  return (
    <Layout>
      {/* Info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">基本信息</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="fl">用户名</span><span className="font-semibold">{user.username}</span>
            <span className="fl">角色</span><span className="font-mono">{user.role}</span>
            {isRegularUser && (
              <>
                <span className="fl">规则配额</span><span className="font-mono">{rules.length} / {user.max_forwards}</span>
                <span className="fl">流量</span>
                <span className="font-mono">
                  {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
                  {user.traffic_quota_bytes > 0 && ` (${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)`}
                </span>
                <span className="fl">到期时间</span>
                <span className="font-mono">
                  {expiresAt ? <>{fmtDate(expiresAt)} {isExpired(expiresAt) && <Badge color="red">已过期</Badge>}</> : '永不过期'}
                </span>
                <span className="fl">状态</span>
                <span>
                  {user.disabled ? (
                    <><Badge color="amber">已禁用</Badge> <span className="text-ink-soft text-xs ml-1">原因：{nullStr(user.disable_reason)}</span></>
                  ) : <Badge color="green">正常</Badge>}
                </span>
              </>
            )}
          </div>

          <div className="flex items-center gap-2 mt-5 flex-wrap">
            {isRegularUser && <ExpiryForm userId={id} expiresAt={expiresAt} onDone={load} />}
            {isRegularUser && <QuotaForm userId={id} quotaBytes={user.traffic_quota_bytes} onDone={load} />}
            {isRegularUser && <button onClick={toggleUser} className="btn-secondary text-xs">{user.disabled ? '启用' : '禁用'}</button>}
            {isRegularUser && <button onClick={resetTraffic} className="btn-secondary text-xs">重置流量</button>}
            <button onClick={resetPassword} className="btn-secondary text-xs">重置密码</button>
            <button onClick={deleteUser} className="btn-danger-sm text-xs">删除用户</button>
          </div>
        </div>
      </div>

      {/* Node grants (regular users only) */}
      {isRegularUser && (
        <div className="card mb-5">
          <div className="card-header"><h3 className="text-sm font-bold">已授权节点</h3></div>
          {nodes.length ? (
            <table className="tbl">
              <thead><tr><th>节点</th><th>类型</th><th>本用户上限</th><th className="text-right">操作</th></tr></thead>
              <tbody>
                {nodes.map((n, i) => (
                  <tr key={n.id}>
                    <td className="font-semibold">
                      <Link to={`/nodes/${n.id}`} className="text-blue-600 hover:underline">{n.name}</Link>
                    </td>
                    <td><NodeTypeBadge type={n.node_type} /></td>
                    <td className="font-mono">{grants[i]?.max_forwards ?? '--'}</td>
                    <td className="text-right">
                      <RevokeBtn url={`/users/${id}/grants/${n.id}`} onDone={load} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <Empty title="尚未授权任何节点" />}
          <div className="p-5 border-t border-line-soft">
            <GrantNodeForm userId={id} allNodes={all_nodes} grantedNodes={nodes} onDone={load} />
          </div>
        </div>
      )}

      {/* Landing-node source (regular users only) */}
      {isRegularUser && (
        <LandingSourceForm userId={id} subURL={user.landing_sub_url} uris={user.landing_uris} nodes={landing_nodes} onDone={load} />
      )}

      {/* Rules */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">该用户的规则</h3>
          <span className="text-xs text-ink-mut">{rules.length} 条</span>
        </div>
        {rules.length ? (
          <table className="tbl">
            <thead><tr><th>ID</th><th>名称</th><th>节点</th><th>协议</th><th>入口</th><th>出口</th><th className="text-right">流量</th></tr></thead>
            <tbody>
              {rules.map(r => (
                <tr key={r.id}>
                  <td className="font-mono text-xs text-ink-mut">{r.id}</td>
                  <td className="font-semibold">
                    <Link to={`/rules/${r.id}`} className="text-blue-600 hover:underline">{r.name}</Link>
                  </td>
                  <td className="font-mono text-ink-soft">{r.node_name || `#${r.node_id}`}</td>
                  <td><ProtoBadge proto={r.proto} /></td>
                  <td className="font-mono text-xs">{r.entry || '--'}</td>
                  <td className="font-mono text-xs">{r.exit || '--'}</td>
                  <td className="text-right font-mono">{fmtBytes(r.total_bytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="该用户尚无规则" />}
      </div>

      <Link to="/users" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回用户列表
      </Link>

      <Modal open={!!newPassword} onClose={() => setNewPassword(null)} title="新密码">
        <p className="text-sm text-ink-soft mb-3">新密码只显示这一次，请复制并妥善保存。关闭后将无法再次查看。</p>
        <div className="flex items-center gap-2">
          <code className="flex-1 font-mono text-sm bg-raised border border-line rounded-lg px-3 py-2.5 break-all select-all">{newPassword}</code>
          <CopyButton text={newPassword} />
        </div>
        <div className="flex justify-end mt-5">
          <button onClick={() => setNewPassword(null)} className="btn-secondary">关闭</button>
        </div>
      </Modal>
    </Layout>
  )
}

function CopyButton({ text }) {
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <button onClick={copy} className="btn-primary flex-none px-4">{copied ? '已复制' : '复制'}</button>
  )
}

function ExpiryForm({ userId, expiresAt, onDone }) {
  const [val, setVal] = useState(expiresAt ? fmtDateInput(expiresAt) : '')
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    try { await api.post(`/users/${userId}/expiry`, { expires_at: val || undefined }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="date" value={val} onChange={e => setVal(e.target.value)} style={{ width: 160 }} />
      <button type="submit" className="btn-secondary text-xs">设到期</button>
    </form>
  )
}

// Quota is stored in bytes server-side but edited in GB here, matching the
// read-only display above. Empty/0 means unlimited.
function QuotaForm({ userId, quotaBytes, onDone }) {
  const [gb, setGb] = useState(String(Number(((quotaBytes || 0) / 1073741824).toFixed(2))))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.round((Number(gb) || 0) * 1073741824))
    try { await api.post(`/users/${userId}/quota`, { traffic_quota_bytes: bytes }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="0.1" value={gb} onChange={e => setGb(e.target.value)} style={{ width: 120 }} title="0 = 不限" />
      <span className="text-xs text-ink-mut">GB</span>
      <button type="submit" className="btn-secondary text-xs">设配额</button>
    </form>
  )
}

// LandingSourceForm edits a user's landing-node source: a subscription URL
// and/or a list of manual proxy URIs (one per line); the two combine. Saving
// returns a fresh preview of the resolved nodes.
function LandingSourceForm({ userId, subURL, uris, nodes, onDone }) {
  const [url, setUrl] = useState(subURL || '')
  const [text, setText] = useState(uris || '')
  const [preview, setPreview] = useState(nodes || [])
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      const d = await api.post(`/users/${userId}/landing`, { landing_sub_url: url.trim(), landing_uris: text })
      setPreview(d?.landing_nodes || [])
      toast('已保存'); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <div className="card mb-5">
      <div className="card-header">
        <h3 className="text-sm font-bold">落地节点来源</h3>
        <span className="text-xs text-ink-mut">{preview.length} 个节点</span>
      </div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-3">
          <div>
            <label className="fl block mb-1.5">订阅地址 <span className="text-ink-mut font-normal text-xs">(可选，支持 Remnawave 等面板的订阅链接)</span></label>
            <input className="input-field font-mono w-full" value={url} onChange={e => setUrl(e.target.value)}
              placeholder="https://example.com/api/sub/xxxx" />
          </div>
          <div>
            <label className="fl block mb-1.5">手动节点 URI <span className="text-ink-mut font-normal text-xs">(可选，每行一条，可与订阅组合)</span></label>
            <textarea className="input-field font-mono w-full" rows={10} value={text} onChange={e => setText(e.target.value)}
              placeholder={'vless://…\ntrojan://…'} />
          </div>
          <button type="submit" disabled={loading} className="btn-primary text-xs">保存</button>
        </form>

        {preview.length > 0 && (
          <div className="mt-4 border-t border-line-soft pt-4">
            <div className="text-xs font-bold text-ink-mut uppercase tracking-wider mb-2">解析出的落地节点</div>
            <table className="tbl">
              <thead><tr><th>名称</th><th>协议</th><th>地址</th></tr></thead>
              <tbody>
                {preview.map((n, i) => (
                  <tr key={i}>
                    <td className="font-semibold">{n.name || '(未命名)'}</td>
                    <td className="font-mono text-xs text-ink-soft">{n.protocol}</td>
                    <td className="font-mono text-xs">{n.host}:{n.port}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}

function RevokeBtn({ url, onDone }) {
  const toast = useToast()
  const revoke = async () => {
    try { await api.del(url); toast('已撤销'); onDone() } catch (err) { toast(err.message) }
  }
  return <button onClick={revoke} className="btn-danger-sm text-xs">撤销</button>
}

function GrantNodeForm({ userId, allNodes, grantedNodes, onDone }) {
  const [nodeId, setNodeId] = useState('')
  const [max, setMax] = useState('10')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  if (!allNodes?.length) return <Empty desc={<Link to="/nodes" className="text-blue-600 text-xs font-semibold">请先创建节点</Link>} />

  const grantedIds = new Set((grantedNodes || []).map(n => n.id))
  const available = allNodes.filter(n => !grantedIds.has(n.id))
  if (!available.length) return <div className="text-xs text-ink-mut">所有节点均已授权</div>

  const submit = async (e) => {
    e.preventDefault()
    if (!nodeId) { toast('请选择节点'); return }
    setLoading(true)
    try {
      await api.post(`/users/${userId}/grants`, { node_id: Number(nodeId), max_forwards: Number(max) })
      toast('已授权'); setNodeId(''); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-ink-mut uppercase tracking-wider mb-3">授权新节点</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点</label>
          <Select value={nodeId} onChange={v => setNodeId(v)} placeholder="-- 选择 --" searchable
            groups={[
              { label: '单点', options: available.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: n.name })) },
              { label: '组合', options: available.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: n.name })) },
            ]} />
          <label className="fl">本用户上限</label>
          <input className="input-field font-mono" type="number" min="1" value={max} onChange={e => setMax(e.target.value)} style={{ maxWidth: 160 }} />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">授权</button>
      </form>
    </>
  )
}
