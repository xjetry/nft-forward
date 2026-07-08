import { useState, useEffect, useMemo } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, fmtTrafficGB, pct, fmtDate, fmtDateInput, isExpired, nullInt, nullStr } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, NodeTypeBadge, useConfirm, Select, Modal, SensText } from '../../components/ui'
import { TableBox } from '../../components/page'
import { copyToClipboard } from '../../lib/clipboard'
import { fetchNodeRoles, nodeRoleKey, applyNodeRole, applyNodeRoleBatch, saveNodeRoles, ROLE_LANDING, ROLE_DIRECT, rolesFirstOrder } from '../../lib/landing'
import PasteGrantsModal from './PasteGrantsModal'

export default function UserDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const toast = useToast()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [newPassword, setNewPassword] = useState(null)
  const confirm = useConfirm()
  const blurred = useBlur()

  const [allUsers, setAllUsers] = useState([])

  const load = () => {
    setLoading(true)
    api.get(`/users/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])
  useEffect(() => { api.get('/users').then(d => setAllUsers(d?.users || [])) }, [])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="用户不存在" /></Layout>

  const { user, nodes = [], grants = [], all_nodes = [], rules = [], landing_nodes = [] } = data
  const nodeMap = Object.fromEntries(all_nodes.map(n => [n.id, n]))

  const isRegularUser = user.role === 'user'

  const toggleUser = async () => {
    try { await api.post(`/users/${id}/toggle`); toast(user.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message, 'error') }
  }
  const resetTraffic = async () => {
    if (!(await confirm({ title: '清零流量', message: '清零该用户的已用流量？', confirmText: '清零', danger: true }))) return
    try { await api.post(`/users/${id}/reset-traffic`); toast('已重置'); load() } catch (err) { toast(err.message, 'error') }
  }
  const deleteUser = async () => {
    if (!(await confirm({ title: '删除用户', message: '删除用户？关联的规则将被一并清除。', confirmText: '删除', danger: true }))) return
    try { await api.del(`/users/${id}`); toast('已删除'); navigate('/users') } catch (err) { toast(err.message, 'error') }
  }
  const resetPassword = async () => {
    if (!(await confirm({ title: '重置密码', message: '重置该用户密码？新密码只显示一次，请及时复制保存。', confirmText: '重置', danger: true }))) return
    try {
      const d = await api.post(`/users/${id}/reset-password`)
      if (d?.new_password) setNewPassword(d.new_password)
      else toast('已重置')
    } catch (err) { toast(err.message, 'error') }
  }

  const expiresAt = user.expires_at && user.expires_at > 0 ? user.expires_at : null

  return (
    <Layout>
      <div className="admin-detail-page admin-user-detail-page">
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
                <span className="fl">计费倍率</span>
                <span className="font-mono">×{user.billing_rate ?? 1}</span>
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
                {user.admin_note && (
                  <>
                    <span className="fl">管理备注</span>
                    <span className="text-ink-soft text-[13px]">{user.admin_note}</span>
                  </>
                )}
              </>
            )}
          </div>

          {isRegularUser && (
            <UserProfileForm
              userId={id}
              expiresAt={expiresAt}
              maxForwards={user.max_forwards}
              quotaBytes={user.traffic_quota_bytes}
              resetDays={user.traffic_reset_days}
              adminNote={user.admin_note || ''}
              billingRate={user.billing_rate}
              onDone={load}
            />
          )}

          <div className="flex items-center gap-2 mt-5 flex-wrap">
            {isRegularUser && <button onClick={toggleUser} className="btn-secondary text-xs">{user.disabled ? '启用' : '禁用'}</button>}
            {isRegularUser && <button onClick={resetTraffic} className="btn-secondary text-xs">重置流量</button>}
            {isRegularUser && <button onClick={resetPassword} className="btn-secondary text-xs">重置密码</button>}
            {isRegularUser && <button onClick={deleteUser} className="btn-secondary text-xs">删除用户</button>}
          </div>
        </div>
      </div>

      {/* Node grants (regular users only) */}
      {isRegularUser && (
        <GrantedNodesCard userId={id} nodes={nodes} grants={grants} allNodes={all_nodes} allUsers={allUsers} onDone={load} />
      )}

      {/* Landing-node source (regular users only) */}
      {isRegularUser && (
        <LandingSourceForm userId={id} subURL={user.landing_sub_url} uris={user.landing_uris} nodes={landing_nodes} blurred={blurred} />
      )}

      {/* Rules */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-[15px] font-bold">该用户的规则</h3>
          <span className="text-[13px] text-ink-mut">{rules.length} 条</span>
        </div>
        {rules.length ? (
          <TableBox>
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
                  <td className="font-mono text-xs"><SensText blurred={blurred}>{r.entry_listen_port ? `:${r.entry_listen_port}` : '--'}</SensText></td>
                  <td className="font-mono text-xs"><SensText blurred={blurred}>{r.exit_host ? `${r.exit_host}:${r.exit_port}` : '--'}</SensText></td>
                  <td className="text-right font-mono text-xs text-ink-mut">{fmtBytes(r.total_bytes || 0)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          </TableBox>
        ) : <Empty title="该用户尚无规则" />}
      </div>

      <Link to="/users" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回用户列表
      </Link>
      </div>

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
    copyToClipboard(text).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <button onClick={copy} className="btn-primary flex-none px-4">{copied ? '已复制' : '复制'}</button>
  )
}

function UserProfileForm({ userId, expiresAt, maxForwards, quotaBytes, resetDays, adminNote, billingRate, onDone }) {
  const [form, setForm] = useState({
    expiresAt: expiresAt ? fmtDateInput(expiresAt) : '',
    maxForwards: String(maxForwards || 0),
    quotaGB: String(Number(((quotaBytes || 0) / 1073741824).toFixed(2))),
    resetDays: String(resetDays || 0),
    adminNote: adminNote,
    billingRate: String(billingRate ?? 1),
  })
  const [saving, setSaving] = useState(false)
  const toast = useToast()

  const set = (key) => (e) => setForm(f => ({ ...f, [key]: e.target.value }))
  const initExpiry = expiresAt ? fmtDateInput(expiresAt) : ''

  const addDays = (days) => {
    const base = form.expiresAt ? new Date(form.expiresAt + 'T00:00:00') : new Date()
    base.setDate(base.getDate() + days)
    const y = base.getFullYear()
    const m = String(base.getMonth() + 1).padStart(2, '0')
    const d = String(base.getDate()).padStart(2, '0')
    setForm(f => ({ ...f, expiresAt: `${y}-${m}-${d}` }))
  }

  const submit = async (e) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.patch(`/users/${userId}/profile`, {
        expires_at: form.expiresAt || '',
        max_forwards: Math.max(0, Math.round(Number(form.maxForwards) || 0)),
        traffic_quota_gb: Math.max(0, Number(form.quotaGB) || 0),
        traffic_reset_days: Math.max(0, Math.round(Number(form.resetDays) || 0)),
        admin_note: form.adminNote,
        billing_rate: Math.max(0, Number(form.billingRate) || 1),
      })
      toast('已保存')
      onDone()
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false) }
  }

  return (
    <form onSubmit={submit} className="mt-5">
      <div className="grid grid-cols-[100px_1fr] gap-x-4 gap-y-3 items-center max-w-lg">
        <label className="fl">到期时间</label>
        <div>
          <input className="input-field font-mono" type="date" value={form.expiresAt} onChange={set('expiresAt')} />
          <div className="flex items-center gap-1.5 mt-1.5 flex-wrap">
            {[[1,'1天'],[7,'7天'],[30,'30天'],[365,'1年']].map(([d, l]) => (
              <button key={d} type="button" onClick={() => addDays(d)}
                className="text-[11px] px-2 py-0.5 rounded border border-line bg-surface text-ink-soft hover:border-blue-500 hover:text-blue-600 transition-colors cursor-pointer">+{l}</button>
            ))}
            {form.expiresAt !== initExpiry && (
              <button type="button" onClick={() => setForm(f => ({ ...f, expiresAt: initExpiry }))}
                className="text-[11px] px-2 py-0.5 rounded border border-line bg-surface text-ink-mut hover:text-ink-soft transition-colors cursor-pointer">还原</button>
            )}
          </div>
        </div>

        <label className="fl">规则配额</label>
        <input className="input-field font-mono" type="number" min="0" step="1" value={form.maxForwards} onChange={set('maxForwards')} title="0 = 不限" />

        <label className="fl">流量配额</label>
        <div className="flex items-center gap-1.5">
          <input className="input-field font-mono flex-1" type="number" min="0" step="0.1" value={form.quotaGB} onChange={set('quotaGB')} title="0 = 不限" />
          <span className="text-xs text-ink-mut">GB</span>
        </div>

        <label className="fl">重置周期</label>
        <div className="flex items-center gap-1.5">
          <input className="input-field font-mono flex-1" type="number" min="0" step="1" value={form.resetDays} onChange={set('resetDays')} title="0 = 永不重置" />
          <span className="text-xs text-ink-mut">天</span>
        </div>

        <label className="fl">计费倍率</label>
        <div className="flex items-center gap-1.5">
          <input className="input-field font-mono flex-1" type="number" min="0" step="0.1" value={form.billingRate} onChange={set('billingRate')} title="1.0 = 原价，<1 折扣，>1 加价" />
          <span className="text-xs text-ink-mut">×</span>
        </div>

        <label className="fl">管理备注</label>
        <input className="input-field" value={form.adminNote} onChange={set('adminNote')} placeholder="管理备注" />
      </div>
      <button type="submit" disabled={saving} className="btn-primary text-xs mt-4">{saving ? '保存中…' : '保存'}</button>
    </form>
  )
}

function PerNodeMaxForwardsForm({ userId, nodeId, maxForwards, onDone }) {
  const [val, setVal] = useState(String(maxForwards ?? 10))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const n = Math.max(1, Number(val) || 1)
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/max-forwards`, { max_forwards: n })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="1" value={val}
        onChange={e => setVal(e.target.value)} style={{ width: 64 }} />
      <button type="submit" className="btn-secondary text-xs">设上限</button>
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
    } catch (err) { toast(err.message, 'error') }
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

function PerNodeRateForm({ userId, nodeId, rateMBytes, onDone }) {
  const [mb, setMb] = useState(String(rateMBytes || 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const n = Math.max(0, Math.round(Number(mb) || 0))
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/rate-limit`, { rate_limit_mbytes: n })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" value={mb}
        onChange={e => setMb(e.target.value)} style={{ width: 64 }} title="0 = 不限，同节点所有规则共享" />
      <span className="text-xs text-ink-mut">MB/s</span>
      <button type="submit" className="btn-secondary text-xs">设限速</button>
    </form>
  )
}

// Shared with the node detail page (nodes/Detail.jsx) so per-grant role
// editing behaves identically from both directions.
export function PerNodeRolesForm({ userId, nodeId, roles, onDone }) {
  // 0 = 继承节点掩码；其余是覆盖值（入口=1 / 中转=2 的组合）。
  const [val, setVal] = useState(String(roles ?? 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/roles`, { roles: Number(val) })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <select className="input-field" value={val} onChange={e => setVal(e.target.value)} style={{ width: 108 }}>
        <option value="0">跟随节点</option>
        <option value="1">仅入口</option>
        <option value="2">仅中转</option>
        <option value="3">入口+中转</option>
      </select>
      <button type="submit" className="btn-secondary text-xs">设用途</button>
    </form>
  )
}

function LandingSourceForm({ userId, subURL, uris, nodes, blurred }) {
  const [url, setUrl] = useState(subURL || '')
  const [text, setText] = useState(uris || '')
  const [preview, setPreview] = useState(nodes || [])
  const [roles, setRoles] = useState({})
  const [exits, setExits] = useState([])
  const [loading, setLoading] = useState(false)
  const [sel, setSel] = useState(new Set())
  const toast = useToast()

  const loadExits = () => {
    api.get(`/users/${userId}/landing-exits`)
      .then(d => setExits(d?.exits || []))
      .catch(err => toast(err.message, 'error'))
  }
  useEffect(() => { fetchNodeRoles().then(setRoles); loadExits() }, [userId])
  useEffect(() => { setSel(new Set()) }, [preview])

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      const d = await api.post(`/users/${userId}/landing`, { landing_sub_url: url.trim(), landing_uris: text })
      setPreview(d?.landing_nodes || [])
      toast('已保存'); loadExits()
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }

  const resetExit = async (ex) => {
    try {
      await api.post(`/users/${userId}/landing-exits/reset`, { host: ex.host, port: ex.port })
      toast('已重置'); loadExits()
    } catch (err) { toast(err.message, 'error') }
  }
  const deleteExit = async (ex) => {
    try {
      await api.post(`/users/${userId}/landing-exits/delete`, { host: ex.host, port: ex.port })
      toast('已删除'); loadExits()
    } catch (err) { toast(err.message, 'error') }
  }

  const handleSetRole = (n, bit) => {
    const next = applyNodeRole(roles, n, bit)
    setRoles(next); saveNodeRoles(next)
  }
  const handleBulkRole = (nodesList, bit, on) => {
    const next = applyNodeRoleBatch(roles, nodesList, bit, on)
    setRoles(next); saveNodeRoles(next)
  }
  const toggleSel = (i) => setSel(s => {
    const next = new Set(s)
    if (next.has(i)) next.delete(i); else next.add(i)
    return next
  })
  const toggleSelAll = () => setSel(s =>
    s.size === preview.length ? new Set() : new Set(preview.map((_, i) => i)))
  const roleOf = (n) => { const k = nodeRoleKey(n); return (k && roles[k]) || 0 }
  const landingCount = preview.filter(n => roleOf(n) & ROLE_LANDING).length
  const directCount = preview.filter(n => roleOf(n) & ROLE_DIRECT).length
  const unconfiguredCount = preview.filter(n => !roleOf(n)).length
  // Unconfigured nodes sink below configured ones; selection stays keyed to
  // the original index so re-sorting never re-targets a checked row.
  const previewOrder = useMemo(() => rolesFirstOrder(preview, roleOf), [preview, roles])
  const exitByAddr = Object.fromEntries(exits.map(e => [`${e.host}:${e.port}`, e]))
  const residualExits = exits.filter(e => !e.present)

  // Quota cells stay visible on rows with an active ledger even without the
  // landing mark: the backend enforces per-exit quotas regardless of role
  // marks, so an enforcing quota must never be hidden by unmarking a node.
  const showQuotaFor = (n, st) => {
    const ex = exitByAddr[`${n.host}:${n.port}`]
    if (!ex) return null
    return (st & ROLE_LANDING) || ex.quota_bytes > 0 || ex.used_bytes > 0 ? ex : null
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
            <input className={`input-field font-mono w-full ${blurred ? 'blur-[5px]' : ''}`} value={url} onChange={e => setUrl(e.target.value)}
              placeholder="https://example.com/api/sub/xxxx" />
          </div>
          <div>
            <label className="fl block mb-1.5">手动节点 URI <span className="text-ink-mut font-normal text-xs">(可选，每行一条，可与订阅组合)</span></label>
            <textarea className={`input-field font-mono w-full ${blurred ? 'blur-[5px]' : ''}`} rows={10} value={text} onChange={e => setText(e.target.value)}
              placeholder={'vless://…\ntrojan://…'} />
          </div>
          <button type="submit" disabled={loading} className="btn-primary text-xs">保存</button>
        </form>

        {(preview.length > 0 || residualExits.length > 0) && (
          <div className="mt-4 border-t border-line-soft pt-4">
            <div className="flex items-center justify-between mb-2">
              <div className="text-xs font-bold text-ink-mut uppercase tracking-wider">
                解析出的节点
                <span className="normal-case font-normal ml-2">{landingCount} 落地 · {directCount} 直连 · {unconfiguredCount} 未配置</span>
              </div>
              <AdminRoleBulkToggle nodes={preview.filter((_, i) => sel.has(i))} roleOf={roleOf}
                onToggle={(bit, on) => handleBulkRole(preview.filter((_, i) => sel.has(i)), bit, on)} />
            </div>
            <TableBox>
            <table className="tbl">
              <thead><tr>
                <th className="w-8"><input type="checkbox" className="accent-blue-600"
                  checked={preview.length > 0 && sel.size === preview.length} onChange={toggleSelAll} /></th>
                <th>名称</th><th>协议</th><th>地址</th><th>限额</th><th>已用</th><th className="text-right">用途</th></tr></thead>
              <tbody>
                {previewOrder.map((i) => {
                  const n = preview[i]
                  const st = roleOf(n)
                  const ex = showQuotaFor(n, st)
                  const exceeded = ex && ex.quota_bytes > 0 && ex.used_bytes >= ex.quota_bytes
                  return (
                    <tr key={i}>
                      <td><input type="checkbox" className="accent-blue-600" checked={sel.has(i)} onChange={() => toggleSel(i)} /></td>
                      <td><ExitNameCell userId={userId} name={n.name}
                        exit={exitByAddr[`${n.host}:${n.port}`]} onDone={loadExits} /></td>
                      <td className="font-mono text-xs text-ink-soft">{n.protocol}</td>
                      <td className="font-mono text-xs"><SensText blurred={blurred}>{n.host}:{n.port}</SensText></td>
                      <td>{ex ? <ExitQuotaForm userId={userId} exit={ex} onDone={loadExits} /> : <span className="text-xs text-ink-mut">—</span>}</td>
                      <td className="font-mono text-xs">
                        {ex ? (
                          <>
                            {fmtTrafficGB(ex.used_bytes, ex.quota_bytes)}
                            {exceeded && <Badge color="red">已超额</Badge>}
                            <button onClick={() => resetExit(ex)}
                              className="ml-2 px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700">
                              重置
                            </button>
                          </>
                        ) : <span className="text-ink-mut">—</span>}
                      </td>
                      <td className="text-right">
                        <AdminRoleToggle state={st} onChange={bit => handleSetRole(n, bit)} />
                      </td>
                    </tr>
                  )
                })}
                {residualExits.map((ex, i) => {
                  const exceeded = ex.quota_bytes > 0 && ex.used_bytes >= ex.quota_bytes
                  return (
                    <tr key={`residual-${i}`} className="opacity-50">
                      <td></td>
                      <td>
                        <ExitNameCell userId={userId} name={ex.name} exit={ex} onDone={loadExits} />
                        <Badge color="gray">已不在来源</Badge>
                      </td>
                      <td className="font-mono text-xs text-ink-soft">{ex.protocol}</td>
                      <td className="font-mono text-xs"><SensText blurred={blurred}>{ex.host}:{ex.port}</SensText></td>
                      <td><ExitQuotaForm userId={userId} exit={ex} onDone={loadExits} /></td>
                      <td className="font-mono text-xs">
                        {fmtTrafficGB(ex.used_bytes, ex.quota_bytes)}
                        {exceeded && <Badge color="red">已超额</Badge>}
                        <button onClick={() => resetExit(ex)}
                              className="ml-2 px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700">
                              重置
                            </button>
                      </td>
                      <td className="text-right">
                        <button onClick={() => deleteExit(ex)} className="text-red-600 text-xs font-semibold">删除</button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
            </TableBox>
          </div>
        )}
      </div>
    </div>
  )
}

function ExitQuotaForm({ userId, exit, onDone }) {
  const [gb, setGb] = useState(String(Number(((exit.quota_bytes || 0) / 1073741824).toFixed(2))))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.round((Number(gb) || 0) * 1073741824))
    try {
      await api.post(`/users/${userId}/landing-exits/quota`, { host: exit.host, port: exit.port, quota_bytes: bytes })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="0.1" value={gb}
        onChange={e => setGb(e.target.value)} style={{ width: 80 }} title="0 = 不限" />
      <span className="text-xs text-ink-mut">GB</span>
      <button type="submit" className="btn-secondary text-xs">设限额</button>
    </form>
  )
}

/* Inline-editable display name for a parsed node. The override lives on the
   exit-ledger row so it survives subscription refreshes; nodes without a
   ledger row (not yet synced) stay read-only. Saving an empty value restores
   the parsed name. */
function ExitNameCell({ userId, name, exit, onDone }) {
  const [editing, setEditing] = useState(false)
  const [val, setVal] = useState('')
  const toast = useToast()
  const effective = (exit?.name_override || name) || '(未命名)'
  if (!exit) return <span className="font-semibold">{effective}</span>
  const start = () => { setVal(exit.name_override || name || ''); setEditing(true) }
  const save = async () => {
    try {
      await api.post(`/users/${userId}/landing-exits/rename`,
        { host: exit.host, port: exit.port, name: val.trim() })
      toast(val.trim() ? '已改名' : '已恢复原名')
      setEditing(false)
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  if (!editing) return (
    <button type="button" onClick={start}
      title={exit.name_override ? `原名称: ${name || '(未命名)'}` : '点击改名'}
      className="font-semibold text-left hover:text-blue-600 transition-colors">
      {effective}
      {exit.name_override && <span className="text-blue-500 ml-1">*</span>}
    </button>
  )
  return (
    <form onSubmit={e => { e.preventDefault(); save() }} className="inline-flex items-center gap-1.5">
      <input autoFocus className="input-field" value={val} onChange={e => setVal(e.target.value)}
        onKeyDown={e => { if (e.key === 'Escape') setEditing(false) }}
        placeholder="留空恢复原名" style={{ width: 140 }} />
      <button type="submit" className="btn-secondary text-xs">保存</button>
    </form>
  )
}

const ADMIN_ROLE_OPTS = [
  [ROLE_LANDING, '落地', 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'],
  [ROLE_DIRECT, '直连', 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700'],
]

// Two independent per-node switches — landing and direct can both be on at once.
function AdminRoleToggle({ state, onChange }) {
  return (
    <div className="inline-flex gap-1.5">
      {ADMIN_ROLE_OPTS.map(([bit, label, cls]) => (
        <button key={bit} type="button" onClick={() => onChange(bit)}
          className={`px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors ${
            state & bit ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
          }`}>
          {label}
        </button>
      ))}
    </div>
  )
}

// Same switches, scoped to the multi-selected rows: highlighted when every
// selected node already has the bit, click flips it for all of them.
function AdminRoleBulkToggle({ nodes, roleOf, onToggle }) {
  if (!nodes.length) return null
  return (
    <div className="flex gap-1.5 text-[12px]">
      {ADMIN_ROLE_OPTS.map(([bit, label, cls]) => {
        const allOn = nodes.every(n => roleOf(n) & bit)
        return (
          <button key={bit} type="button" onClick={() => onToggle(bit, !allOn)}
            className={`px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors ${
              allOn ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
            }`}>
            {label}
          </button>
        )
      })}
    </div>
  )
}

function GrantedNodesCard({ userId, nodes, grants, allNodes, allUsers, onDone }) {
  const [tab, setTab] = useState('single')
  const [selected, setSelected] = useState(new Set())
  const [revoking, setRevoking] = useState(false)
  const [showPaste, setShowPaste] = useState(false)
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
    } catch (err) { toast(err.message, 'error') } finally { setRevoking(false) }
  }

  const revokeOne = async (nodeId) => {
    try { await api.del(`/users/${userId}/grants/${nodeId}`); toast('已撤销'); onDone() } catch (err) { toast(err.message, 'error') }
  }

  const resetNodeTraffic = async (nodeId) => {
    if (!(await confirm({ title: '重置节点流量', message: '清零该用户在此节点上的已用流量？', confirmText: '清零', danger: true }))) return
    try { await api.post(`/users/${userId}/nodes/${nodeId}/reset-traffic`); toast('已重置'); onDone() } catch (err) { toast(err.message, 'error') }
  }

  const copyGrants = () => {
    const lines = nodes.map(n => {
      const g = grantByNode[n.id]
      const parts = [n.name]
      parts.push(`max=${g?.max_forwards ?? 10}`)
      const gb = g?.traffic_quota_bytes ? Number((g.traffic_quota_bytes / 1073741824).toFixed(2)) : 0
      parts.push(`quota=${gb}GB`)
      parts.push(`rate=${g?.rate_limit_mbytes || 0}`)
      return parts.join(' | ')
    })
    const text = lines.join('\n')
    copyToClipboard(text).then(() => toast(`已复制 ${nodes.length} 个节点授权`)).catch(() => toast('复制失败', 'error'))
  }

  return (
    <div className="card mb-5">
      <div className="card-header">
        <h3 className="text-sm font-bold">已授权节点</h3>
        <div className="flex items-center gap-1.5 ml-auto">
          {nodes.length > 0 && <button onClick={copyGrants} className="btn-secondary text-xs">复制授权</button>}
          <button onClick={() => setShowPaste(true)} className="btn-secondary text-xs">粘贴授权</button>
        </div>
      </div>
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
        <TableBox>
        <table className="tbl">
          <thead><tr>
            <th className="w-8"><input type="checkbox" className="accent-blue-600"
              checked={tabNodes.length > 0 && tabNodes.every(n => selected.has(n.id))}
              onChange={toggleAll} /></th>
            <th>节点</th><th>类型</th><th>节点规则数上限</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">流量配额</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">限速</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">用途</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">已用</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft w-16"></th><th className="text-right">操作</th>
          </tr></thead>
          <tbody>
            {tabNodes.map(n => (
              <tr key={n.id}>
                <td><input type="checkbox" className="accent-blue-600" checked={selected.has(n.id)} onChange={() => toggleOne(n.id)} /></td>
                <td className="font-semibold">
                  <Link to={`/nodes/${n.id}`} className="text-blue-600 hover:underline">{n.name}</Link>
                </td>
                <td><NodeTypeBadge type={n.node_type} /></td>
                <td className="px-3 py-2">
                  <PerNodeMaxForwardsForm userId={userId} nodeId={n.id} maxForwards={grantByNode[n.id]?.max_forwards} onDone={onDone} />
                </td>
                <td className="px-3 py-2">
                  <PerNodeQuotaForm userId={userId} nodeId={n.id} quotaBytes={grantByNode[n.id]?.traffic_quota_bytes} onDone={onDone} />
                </td>
                <td className="px-3 py-2">
                  <PerNodeRateForm userId={userId} nodeId={n.id} rateMBytes={grantByNode[n.id]?.rate_limit_mbytes} onDone={onDone} />
                </td>
                <td className="px-3 py-2">
                  <PerNodeRolesForm userId={userId} nodeId={n.id} roles={grantByNode[n.id]?.roles} onDone={onDone} />
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
        </TableBox>
      ) : nodes.length > 0 ? (
        <Empty title={tab === 'composite' ? '暂无已授权的组合节点' : '暂无已授权的单点节点'} />
      ) : (
        <Empty title="尚未授权任何节点" />
      )}
      <div className="p-5 border-t border-line-soft">
        <GrantNodeForm userId={userId} allNodes={allNodes} grantedNodes={nodes} onDone={onDone} />
      </div>
      <PasteGrantsModal open={showPaste} onClose={() => setShowPaste(false)} onDone={onDone}
        allNodes={allNodes} allUsers={allUsers} preSelectedUserIds={[Number(userId)]} />
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
    if (!nodeIds.length) { toast('请选择节点', 'error'); return }
    setLoading(true)
    try {
      await api.post(`/users/${userId}/grants`, { node_ids: nodeIds.map(Number), max_forwards: Number(max) })
      toast(`已授权 ${nodeIds.length} 个节点`); setNodeIds([]); onDone()
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }
  return (
    <>
      <div className="text-xs font-bold text-ink-mut uppercase tracking-wider mb-3">授权新节点</div>
      <form onSubmit={submit} className="space-y-3 max-w-xl">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点规则数上限</label>
          <input className="input-field font-mono" type="number" min="1" value={max} onChange={e => setMax(e.target.value)} style={{ maxWidth: 160 }} />
          <label className="fl">节点 <span className="text-ink-mut font-normal text-xs">(可多选)</span></label>
          <Select value={nodeIds} onChange={setNodeIds} placeholder="-- 选择 --" searchable multiple tabs
            groups={[
              { label: '单点', options: available.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: n.name })) },
              { label: '组合', options: available.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: n.name })) },
            ]} />
        </div>
        <button type="submit" disabled={loading} className="btn-primary text-xs">授权</button>
      </form>
    </>
  )
}
