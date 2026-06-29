import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTrafficGB, nullStr, nullInt } from '../../lib/fmt'
import { Layout, useToast, useUser } from '../../components/Layout'
import { Loading, Empty, Badge, Modal, useConfirm, Select } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton, TableScroll } from '../../components/page'

export default function UserList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [search, setSearch] = useState('')
  const [sortBy, setSortBy] = useState(null) // null | 'expires_asc' | 'expires_desc'
  const { user: currentUser } = useUser()
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()

  const load = () => {
    setLoading(true)
    api.get('/users').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { users = [] } = data || {}

  const toggleUser = async (u) => {
    try { await api.post(`/users/${u.id}/toggle`); toast(u.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }
  const resetPassword = async (u) => {
    if (!(await confirm({ title: '重置密码', message: '重置该用户密码？新密码会一次性显示。', confirmText: '重置', danger: true }))) return
    try {
      const d = await api.post(`/users/${u.id}/reset-password`)
      toast(d?.new_password ? `新密码：${d.new_password}` : '已重置')
    } catch (err) { toast(err.message) }
  }
  const deleteUser = async (u) => {
    if (!(await confirm({ title: '删除用户', message: '删除该用户？关联的转发将被一并清除。', confirmText: '删除', danger: true }))) return
    try { await api.del(`/users/${u.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  const q = search.trim().toLowerCase()
  const searched = !q ? users : users.filter(u => (u.username || '').toLowerCase().includes(q))

  const toggleExpirySort = () => setSortBy(s => s === 'expires_asc' ? 'expires_desc' : s === 'expires_desc' ? null : 'expires_asc')
  const filtered = !sortBy ? searched : [...searched].sort((a, b) => {
    const ea = nullInt(a.expires_at) || 0
    const eb = nullInt(b.expires_at) || 0
    if (!ea && !eb) return 0
    if (!ea) return 1  // no expiry → push to end
    if (!eb) return -1
    return sortBy === 'expires_asc' ? ea - eb : eb - ea
  })

  return (
    <Layout>
      <div className="h-full flex flex-col">
      <PageHeader title="用户列表" count={users.length} unit="个用户" />

      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索用户名…" />
          <div className="hidden md:block ml-auto"><ToolbarButton onClick={() => setShowCreate(true)}>＋ 新建用户</ToolbarButton></div>
        </PanelToolbar>

        {users.length === 0 ? (
          <Empty title="暂无用户" desc="点击右上角「新建用户」创建。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配用户" desc="试试别的关键词。" />
        ) : (
          <TableScroll>
          {/* Desktop table */}
          <table className="tbl hidden md:table">
            <thead><tr><th className="w-12">ID</th><th>用户名</th><th>角色</th><th>规则配额</th><th>流量</th><th>状态</th><th className="cursor-pointer select-none whitespace-nowrap" onClick={toggleExpirySort}>到期{sortBy === 'expires_asc' ? ' ↑' : sortBy === 'expires_desc' ? ' ↓' : ''}</th><th>备注</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {filtered.map(u => {
                const isSelf = u.id === currentUser?.id
                return (
                  <tr key={u.id} className="cursor-pointer" onClick={() => navigate(`/users/${u.id}`)}>
                    <td className="font-mono text-xs text-ink-mut">{u.id}</td>
                    <td className="text-blue-600 font-semibold">{u.username}</td>
                    <td><span className="inline-flex items-center font-mono text-xs bg-raised text-ink-soft px-1.5 py-0.5 rounded">{u.role}</span></td>
                    <td className="font-mono">{u.role === 'user' ? `${u.rule_count || 0} / ${u.max_forwards}` : '--'}</td>
                    <td className="font-mono">{u.role === 'user' ? fmtTrafficGB(u.traffic_used_bytes, u.traffic_quota_bytes) : '--'}</td>
                    <td>
                      {u.disabled ? (
                        <Badge color="amber" title={nullStr(u.disable_reason)}>已禁用</Badge>
                      ) : <Badge color="green">正常</Badge>}
                    </td>
                    <ExpiryCell unix={nullInt(u.expires_at)} />
                    <td className="text-ink-soft text-xs max-w-[200px] truncate" title={u.admin_note}>{u.admin_note}</td>
                    <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                      {isSelf ? (
                        <span className="text-xs text-ink-mut">(当前用户)</span>
                      ) : (
                        <div className="flex gap-2 justify-end">
                          <button onClick={() => toggleUser(u)} title={u.disabled ? '启用' : '禁用'}
                            className={u.disabled ? 'icon-btn !text-green-600 !border-green-500/30' : 'icon-btn'}>
                            {u.disabled
                              ? <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5"/></svg>
                              : <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="9"/><path d="m5.6 5.6 12.8 12.8"/></svg>}
                          </button>
                          {u.role !== 'admin' && <button onClick={() => resetPassword(u)} title="重置密码" className="icon-btn">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="7.5" cy="15.5" r="4.5"/><path d="m10.7 12.3 9.6-9.6"/><path d="m15.5 7.5 3 3"/><path d="m18 5 2.5 2.5"/></svg>
                          </button>}
                          {u.role !== 'admin' && <button onClick={() => deleteUser(u)} title="删除" className="icon-btn-danger">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/></svg>
                          </button>}
                        </div>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          {/* Mobile cards */}
          <div className="md:hidden">
            {filtered.map(u => (
              <Link key={u.id} to={`/users/${u.id}`} className="mobile-card block no-underline text-ink">
                <div className="flex items-center justify-between mb-1">
                  <span className="font-semibold text-blue-600">{u.username}</span>
                  {u.disabled
                    ? <Badge color="amber">已禁用</Badge>
                    : <Badge color="green">正常</Badge>}
                </div>
                <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                  <span className="font-mono bg-raised px-1.5 py-0.5 rounded">{u.role}</span>
                  {u.role === 'user' && <>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono">{u.rule_count || 0}/{u.max_forwards} 规则</span>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono">{fmtTrafficGB(u.traffic_used_bytes, u.traffic_quota_bytes)}</span>
                    {nullInt(u.expires_at) ? <>
                      <span className="text-ink-mut">·</span>
                      <ExpiryInline unix={nullInt(u.expires_at)} />
                    </> : null}
                  </>}
                </div>
              </Link>
            ))}
          </div>
          </TableScroll>
        )}
      </Panel>
      </div>

      <CreateUserModal open={showCreate} onClose={() => setShowCreate(false)} onDone={() => { setShowCreate(false); load() }} />
    </Layout>
  )
}

function fmtExpiry(unix) {
  if (!unix) return '--'
  const d = new Date(unix * 1000)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

function expiryDaysLeft(unix) {
  if (!unix) return null
  return Math.ceil((unix - Date.now() / 1000) / 86400)
}

function ExpiryCell({ unix }) {
  if (!unix) return <td className="text-xs text-ink-mut">--</td>
  const days = expiryDaysLeft(unix)
  const expired = days <= 0
  const soon = !expired && days <= 7
  const cls = expired ? 'text-red-500' : soon ? 'text-amber-500' : 'text-ink-soft'
  return (
    <td className={`text-xs font-mono whitespace-nowrap ${cls}`}>
      {fmtExpiry(unix)}
      {expired ? <span className="ml-1 text-[10px]">已过期</span>
       : soon ? <span className="ml-1 text-[10px]">{days}天</span> : null}
    </td>
  )
}

function ExpiryInline({ unix }) {
  const days = expiryDaysLeft(unix)
  const expired = days <= 0
  const soon = !expired && days <= 7
  const cls = expired ? 'text-red-500' : soon ? 'text-amber-500' : 'text-ink-soft'
  return (
    <span className={`font-mono ${cls}`}>
      {expired ? '已过期' : soon ? `${days}天到期` : fmtExpiry(unix)}
    </span>
  )
}

function todayStr() {
  const d = new Date()
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
function toDateStr(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
// Unambiguous alphabet (no O/0/I/l/1) for a copy-pasteable random password.
function genPassword(len = 16) {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
  const arr = new Uint32Array(len)
  crypto.getRandomValues(arr)
  return [...arr].map(n => chars[n % chars.length]).join('')
}
const emptyForm = () => ({ username: '', password: '', role: 'user', max_forwards: '100', traffic_quota_gb: '0', expires_at: todayStr(), landing_sub_url: '', admin_note: '' })

function CreateUserModal({ open, onClose, onDone }) {
  const [form, setForm] = useState(emptyForm)
  const [loading, setLoading] = useState(false)
  const [panelURL, setPanelURL] = useState('')
  const toast = useToast()

  // Fetch the configured panel address so "创建并复制信息" can include the login
  // URL; fall back to the current origin when unset.
  useEffect(() => {
    if (!open) return
    setForm(emptyForm())
    api.get('/settings').then(d => setPanelURL((d?.panel_url || '').trim() || window.location.origin)).catch(() => setPanelURL(window.location.origin))
  }, [open])

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const addToExpiry = (kind) => {
    const base = form.expires_at ? new Date(form.expires_at + 'T00:00:00') : new Date()
    if (kind === '1d') base.setDate(base.getDate() + 1)
    if (kind === '1m') base.setMonth(base.getMonth() + 1)
    if (kind === '3m') base.setMonth(base.getMonth() + 3)
    if (kind === '1y') base.setFullYear(base.getFullYear() + 1)
    set('expires_at', toDateStr(base))
  }

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    const isUser = form.role === 'user'
    try {
      await api.post('/users', {
        username: form.username,
        password: form.password,
        role: form.role,
        ...(isUser ? {
          max_forwards: Number(form.max_forwards),
          traffic_quota_bytes: Math.max(0, Math.round((Number(form.traffic_quota_gb) || 0) * 1073741824)),
          expires_at: form.expires_at || undefined,
          landing_sub_url: form.landing_sub_url.trim() || undefined,
          admin_note: form.admin_note.trim() || undefined,
        } : {}),
      })
      // Copy login info before resetting the form (the password is only here in
      // plaintext at creation time).
      const info = `面板地址：${panelURL}\n用户名：${form.username}\n密码：${form.password}`
      try { await navigator.clipboard.writeText(info); toast('用户已创建，登录信息已复制') } catch { toast('用户已创建（复制失败，请手动记录密码）') }
      setForm(emptyForm())
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="新建用户">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[150px_1fr] gap-4 items-center">
          <label className="fl">用户名</label>
          <input className="input-field" value={form.username} onChange={e => set('username', e.target.value)} required placeholder="登录用户名" />
          <label className="fl">密码</label>
          <div className="flex items-center gap-2">
            <input className="input-field font-mono flex-1" type="text" value={form.password} onChange={e => set('password', e.target.value)} required placeholder="密码" />
            <button type="button" onClick={() => set('password', genPassword())} className="btn-secondary text-xs flex-none">随机生成</button>
          </div>
          <label className="fl">角色</label>
          <Select value={form.role} onChange={v => set('role', v)} options={[{ value: 'user', label: 'user (普通用户)' }, { value: 'admin', label: 'admin (管理员)' }]} style={{ maxWidth: 200 }} />
          {form.role === 'user' && (
            <>
              <label className="fl">最大转发数</label>
              <input className="input-field font-mono" type="number" min="1" value={form.max_forwards} onChange={e => set('max_forwards', e.target.value)} style={{ maxWidth: 160 }} />
              <label className="fl">流量配额 <span className="text-ink-mut font-normal text-xs">(GB，0 = 不限)</span></label>
              <input className="input-field font-mono" type="number" min="0" step="0.1" value={form.traffic_quota_gb} onChange={e => set('traffic_quota_gb', e.target.value)} style={{ maxWidth: 160 }} />
              <label className="fl">到期时间</label>
              <div className="flex items-center gap-2 flex-wrap">
                <input className="input-field font-mono" type="date" value={form.expires_at} onChange={e => set('expires_at', e.target.value)} style={{ maxWidth: 200 }} />
                <button type="button" onClick={() => set('expires_at', todayStr())} title="重置为当天"
                  className="btn-secondary flex-none px-2" style={{ height: 38 }}>
                  <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>
                </button>
                <div className="inline-flex gap-1.5">
                  {[['1d', '+1天'], ['1m', '+1月'], ['3m', '+3月'], ['1y', '+1年']].map(([k, lbl]) => (
                    <button key={k} type="button" onClick={() => addToExpiry(k)} className="btn-secondary text-xs">{lbl}</button>
                  ))}
                </div>
              </div>
              <label className="fl">订阅地址 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
              <input className="input-field font-mono" value={form.landing_sub_url} onChange={e => set('landing_sub_url', e.target.value)} placeholder="https://example.com/api/sub/xxxx" />
              <label className="fl">管理备注 <span className="text-ink-mut font-normal text-xs">(可选，仅管理员可见)</span></label>
              <input className="input-field" value={form.admin_note} onChange={e => set('admin_note', e.target.value)} placeholder="仅管理员可见的备注" />
            </>
          )}
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-line-soft">
          <button type="submit" disabled={loading} className="btn-primary">创建并复制信息</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
