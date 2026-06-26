import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, fmtTrafficGB, pct, fmtDate, fmtDateInput, isExpired, nullInt, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, NodeTypeBadge, useConfirm, Select, Modal } from '../../components/ui'
import { fetchNodeRoles, nodeRoleKey, applyNodeRole, applyNodeRoleBatch, saveNodeRoles } from '../../lib/landing'

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
  const nodeMap = Object.fromEntries(all_nodes.map(n => [n.id, n]))

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

  const expiresAt = user.expires_at && user.expires_at > 0 ? user.expires_at : null

  return (
    <Layout>
      {/* Info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-[15px] font-bold">基本信息</h3></div>
        <div className="px-6 py-[22px]">
          <div className="grid grid-cols-[120px_1fr] gap-4 items-center text-[13.5px]">
            <span className="fl">用户名</span><span className="font-semibold">{user.username}</span>
            <span className="fl">角色</span><span className="font-mono">{user.role}</span>
            {isRegularUser && (
              <>
                <span className="fl">规则配额</span><span className="font-mono">{rules.length} / {user.max_forwards}</span>
                <span className="fl">流量</span>
                <span className="font-mono">
                  {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
                  {user.traffic_quota_bytes > 0 && ` (${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)`}
                  {user.traffic_reset_days > 0 && <span className="text-ink-mut text-xs ml-1">每{user.traffic_reset_days}天重置</span>}
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
            {isRegularUser && <MaxForwardsForm userId={id} maxForwards={user.max_forwards} onDone={load} />}
            {isRegularUser && <QuotaForm userId={id} quotaBytes={user.traffic_quota_bytes} onDone={load} />}
            {isRegularUser && <ResetDaysForm userId={id} resetDays={user.traffic_reset_days} onDone={load} />}
            {isRegularUser && <button onClick={toggleUser} className="btn-secondary text-xs">{user.disabled ? '启用' : '禁用'}</button>}
            {isRegularUser && <button onClick={resetTraffic} className="btn-secondary text-xs">重置流量</button>}
            {/* Admin accounts can't be reset or deleted here. */}
            {isRegularUser && <button onClick={resetPassword} className="btn-secondary text-xs">重置密码</button>}
            {isRegularUser && <button onClick={deleteUser} className="btn-danger-sm text-xs">删除用户</button>}
          </div>
        </div>
      </div>

      {/* Node grants (regular users only) */}
      {isRegularUser && (
        <GrantedNodesCard userId={id} nodes={nodes} grants={grants} allNodes={all_nodes} onDone={load} />
      )}

      {/* Landing-node source (regular users only) */}
      {isRegularUser && (
        <LandingSourceForm userId={id} subURL={user.landing_sub_url} uris={user.landing_uris} nodes={landing_nodes} onDone={load} />
      )}

      {/* Rules */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-[15px] font-bold">该用户的规则</h3>
          <span className="text-[13px] text-ink-mut">{rules.length} 条</span>
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
                  <td className="font-mono text-ink-soft">{nodeMap[r.node_id]?.name || `#${r.node_id}`}</td>
                  <td><ProtoBadge proto={r.proto} /></td>
                  <td className="font-mono text-xs">{r.entry_listen_port ? `:${r.entry_listen_port}` : '--'}</td>
                  <td className="font-mono text-xs">{r.exit_host ? `${r.exit_host}:${r.exit_port}` : '--'}</td>
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

function MaxForwardsForm({ userId, maxForwards, onDone }) {
  const [val, setVal] = useState(String(maxForwards || 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const n = Math.max(0, Math.round(Number(val) || 0))
    try { await api.post(`/users/${userId}/max-forwards`, { max_forwards: n }); toast('已设置'); onDone() } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="1" value={val} onChange={e => setVal(e.target.value)} style={{ width: 80 }} title="0 = 不限" />
      <button type="submit" className="btn-secondary text-xs">设规则配额</button>
    </form>
  )
}

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

function ResetDaysForm({ userId, resetDays, onDone }) {
  const [val, setVal] = useState(String(resetDays || 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const days = Math.max(0, Math.round(Number(val) || 0))
    try {
      await api.post(`/users/${userId}/reset-days`, { traffic_reset_days: days })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="1" value={val}
        onChange={e => setVal(e.target.value)} style={{ width: 80 }} title="0 = 永不重置" />
      <span className="text-xs text-ink-mut">天</span>
      <button type="submit" className="btn-secondary text-xs">设周期</button>
    </form>
  )
}

function PerNodeQuotaForm({ userId, nodeId, quotaBytes, onDone }) {
  const [gb, setGb] = useState(String(Number(((quotaBytes || 0) / 1073741824).toFixed(2))))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.round((Number(gb) || 0) * 1073741824))
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/quota`, { traffic_quota_bytes: bytes })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="0.1" value={gb}
        onChange={e => setGb(e.target.value)} style={{ width: 80 }} title="0 = 不限" />
      <span className="text-xs text-ink-mut">GB</span>
      <button type="submit" className="btn-secondary text-xs">设配额</button>
    </form>
  )
}

function LandingSourceForm({ userId, subURL, uris, nodes, onDone }) {
  const [url, setUrl] = useState(subURL || '')
  const [text, setText] = useState(uris || '')
  const [preview, setPreview] = useState(nodes || [])
  const [roles, setRoles] = useState({})
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  useEffect(() => { fetchNodeRoles().then(setRoles) }, [])

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      const d = await api.post(`/users/${userId}/landing`, { landing_sub_url: url.trim(), landing_uris: text })
      setPreview(d?.landing_nodes || [])
      toast('已保存'); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  const handleSetRole = (n, role) => {
    const next = applyNodeRole(roles, n, role)
    setRoles(next); saveNodeRoles(next)
  }
  const handleMarkAll = (role) => {
    const next = applyNodeRoleBatch(roles, preview, role)
    setRoles(next); saveNodeRoles(next)
  }
  const roleOf = (n) => { const k = nodeRoleKey(n); return k && roles[k] ? roles[k] : 'none' }
  const landingCount = preview.filter(n => roleOf(n) === 'landing').length
  const directCount = preview.filter(n => roleOf(n) === 'direct').length
  const unconfiguredCount = preview.length - landingCount - directCount

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
            <div className="flex items-center justify-between mb-2">
              <div className="text-xs font-bold text-ink-mut uppercase tracking-wider">
                解析出的节点
                <span className="normal-case font-normal ml-2">{landingCount} 落地 · {directCount} 直连 · {unconfiguredCount} 未配置</span>
              </div>
              <div className="flex gap-1.5 text-[12px]">
                <button type="button" onClick={() => handleMarkAll('landing')} className="text-emerald-600 hover:underline">全部落地</button>
                <span className="text-ink-mut">|</span>
                <button type="button" onClick={() => handleMarkAll('direct')} className="text-blue-600 hover:underline">全部直连</button>
                <span className="text-ink-mut">|</span>
                <button type="button" onClick={() => handleMarkAll('none')} className="text-ink-mut hover:underline">全部未配置</button>
              </div>
            </div>
            <table className="tbl">
              <thead><tr><th>名称</th><th>协议</th><th>地址</th><th className="text-right">用途</th></tr></thead>
              <tbody>
                {preview.map((n, i) => {
                  const st = roleOf(n)
                  return (
                    <tr key={i}>
                      <td className="font-semibold">{n.name || '(未命名)'}</td>
                      <td className="font-mono text-xs text-ink-soft">{n.protocol}</td>
                      <td className="font-mono text-xs">{n.host}:{n.port}</td>
                      <td className="text-right">
                        <AdminTriToggle state={st} onChange={k => handleSetRole(n, k)} />
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}

function AdminTriToggle({ state, onChange }) {
  const opts = [
    ['landing', '落地', 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'],
    ['direct', '直连', 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700'],
    ['none', '未配置', 'bg-gray-50 text-gray-500 border-gray-200 dark:bg-gray-800/40 dark:text-gray-400 dark:border-gray-600'],
  ]
  return (
    <div className="inline-flex gap-px rounded-md overflow-hidden border border-line">
      {opts.map(([key, label, cls]) => (
        <button key={key} type="button" onClick={() => onChange(key)}
          className={`px-2 py-0.5 text-[11px] font-semibold transition-colors ${
            state === key ? cls : 'bg-transparent text-ink-mut/40 hover:text-ink-mut'
          }`}>
          {label}
        </button>
      ))}
    </div>
  )
}

function GrantedNodesCard({ userId, nodes, grants, allNodes, onDone }) {
  const [tab, setTab] = useState('single')
  const [selected, setSelected] = useState(new Set())
  const [revoking, setRevoking] = useState(false)
  const toast = useToast()
  const confirm = useConfirm()

  const singleNodes = nodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes
  const grantByNode = {}
  nodes.forEach((n, i) => { grantByNode[n.id] = grants[i] })

  const toggleOne = (id) => setSelected(s => {
    const next = new Set(s)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })
  const toggleAll = () => {
    const allIds = tabNodes.map(n => n.id)
    const allSelected = allIds.every(id => selected.has(id))
    if (allSelected) setSelected(s => { const next = new Set(s); allIds.forEach(id => next.delete(id)); return next })
    else setSelected(s => { const next = new Set(s); allIds.forEach(id => next.add(id)); return next })
  }

  const batchRevoke = async () => {
    const ids = [...selected]
    if (!ids.length) return
    if (!(await confirm({ title: '批量撤销', message: `确认撤销 ${ids.length} 个节点的授权？`, confirmText: '撤销', danger: true }))) return
    setRevoking(true)
    try {
      await api.post(`/users/${userId}/grants/batch-revoke`, { node_ids: ids })
      toast(`已撤销 ${ids.length} 个节点`)
      setSelected(new Set())
      onDone()
    } catch (err) { toast(err.message) } finally { setRevoking(false) }
  }

  const revokeOne = async (nodeId) => {
    try { await api.del(`/users/${userId}/grants/${nodeId}`); toast('已撤销'); onDone() } catch (err) { toast(err.message) }
  }

  const resetNodeTraffic = async (nodeId) => {
    if (!(await confirm({ title: '重置节点流量', message: '清零该用户在此节点上的已用流量？', confirmText: '清零', danger: true }))) return
    try { await api.post(`/users/${userId}/nodes/${nodeId}/reset-traffic`); toast('已重置'); onDone() } catch (err) { toast(err.message) }
  }

  return (
    <div className="card mb-5">
      <div className="card-header"><h3 className="text-sm font-bold">已授权节点</h3></div>
      {nodes.length > 0 && (
        <div className="flex items-center gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
          {[['single', '单点', singleNodes.length], ['composite', '组合', compositeNodes.length]].map(([key, label, n]) => (
            <button key={key} onClick={() => { setTab(key); setSelected(new Set()) }}
              className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                tab === key ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}>{label} {n}</button>
          ))}
          {selected.size > 0 && (
            <button onClick={batchRevoke} disabled={revoking} className="btn-danger-sm text-xs ml-auto">
              撤销选中 ({selected.size})
            </button>
          )}
        </div>
      )}
      {tabNodes.length > 0 ? (
        <table className="tbl">
          <thead><tr>
            <th className="w-8"><input type="checkbox" className="accent-blue-600"
              checked={tabNodes.length > 0 && tabNodes.every(n => selected.has(n.id))}
              onChange={toggleAll} /></th>
            <th>节点</th><th>类型</th><th>本用户上限</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">流量配额</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">已用</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft w-16"></th><th className="text-right">操作</th>
          </tr></thead>
          <tbody>
            {tabNodes.map(n => (
              <tr key={n.id}>
                <td><input type="checkbox" className="accent-blue-600" checked={selected.has(n.id)} onChange={() => toggleOne(n.id)} /></td>
                <td className="font-semibold">
                  <Link to={`/nodes/${n.id}`} className="text-blue-600 hover:underline">{n.name}</Link>
                </td>
                <td><NodeTypeBadge type={n.node_type} /></td>
                <td className="font-mono">{grantByNode[n.id]?.max_forwards ?? '--'}</td>
                <td className="px-3 py-2">
                  <PerNodeQuotaForm userId={userId} nodeId={n.id} quotaBytes={grantByNode[n.id]?.traffic_quota_bytes} onDone={onDone} />
                </td>
                <td className="px-3 py-2 font-mono text-sm">
                  {fmtTrafficGB(grantByNode[n.id]?.traffic_used_bytes, grantByNode[n.id]?.traffic_quota_bytes)}
                </td>
                <td className="px-3 py-2">
                  {grantByNode[n.id]?.traffic_quota_bytes > 0 && grantByNode[n.id]?.traffic_used_bytes > 0 && (
                    <button onClick={() => resetNodeTraffic(n.id)} className="btn-danger-sm text-xs">重置</button>
                  )}
                </td>
                <td className="text-right">
                  <button onClick={() => revokeOne(n.id)} className="btn-danger-sm text-xs">撤销</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : nodes.length > 0 ? (
        <Empty title={tab === 'composite' ? '暂无已授权的组合节点' : '暂无已授权的单点节点'} />
      ) : (
        <Empty title="尚未授权任何节点" />
      )}
      <div className="p-5 border-t border-line-soft">
        <GrantNodeForm userId={userId} allNodes={allNodes} grantedNodes={nodes} onDone={onDone} />
      </div>
    </div>
  )
}

function GrantNodeForm({ userId, allNodes, grantedNodes, onDone }) {
  const [nodeIds, setNodeIds] = useState([])
  const [max, setMax] = useState('10')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  if (!allNodes?.length) return <Empty desc={<Link to="/nodes" className="text-blue-600 text-xs font-semibold">请先创建节点</Link>} />

  const grantedIds = new Set((grantedNodes || []).map(n => n.id))
  const available = allNodes.filter(n => !grantedIds.has(n.id))
  if (!available.length) return <div className="text-xs text-ink-mut">所有节点均已授权</div>

  const submit = async (e) => {
    e.preventDefault()
    if (!nodeIds.length) { toast('请选择节点'); return }
    setLoading(true)
    try {
      await api.post(`/users/${userId}/grants`, { node_ids: nodeIds.map(Number), max_forwards: Number(max) })
      toast(`已授权 ${nodeIds.length} 个节点`); setNodeIds([]); onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-ink-mut uppercase tracking-wider mb-3">授权新节点</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点 <span className="text-ink-mut font-normal text-xs">(可多选)</span></label>
          <Select value={nodeIds} onChange={setNodeIds} placeholder="-- 选择 --" searchable multiple tabs
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
