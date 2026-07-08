import { useState, useEffect, useRef } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, fmtBytes, nullStr } from '../../lib/fmt'
import { useSpeed, fmtSpeed } from '../../lib/useSpeed'
import { useIsMobile } from '../../lib/useIsMobile'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, Modal, Confirm, NodeStackBadge, useConfirm, Select } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, TableScroll } from '../../components/page'

export default function NodeList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showAdd, setShowAdd] = useState(false)
  const [showComposite, setShowComposite] = useState(false)
  // 搜索词与角色过滤存 sessionStorage：进详情返回后过滤还在，但不像 tab 那样
  // 跨会话记住——隔天打开还带着旧搜索词会让列表看起来莫名缺节点。
  const [search, setSearch] = useState(() => sessionStorage.getItem('nodes.search') || '')
  // 角色过滤位掩码，与 nodes.roles 同构（bit0 入口 / bit1 中转）；0 表示不过滤，
  // 选中多个时要求同时具备（用于收窄，而非并集）。
  const [roleMask, setRoleMask] = useState(() => Number(sessionStorage.getItem('nodes.roleMask')) || 0)
  const [tab, setTab] = useState(() => localStorage.getItem('nodes.tab') || 'single')
  const [dragIndex, setDragIndex] = useState(null)
  const speeds = useSpeed()
  const isMobile = useIsMobile()
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()
  // 排序与搜索/角色过滤同样按会话持久化：进详情返回后仍然生效，但不跨会话。
  const [sort, setSort] = useState(() => {
    try { return JSON.parse(sessionStorage.getItem('nodes.sort')) || { col: null, dir: null } }
    catch { return { col: null, dir: null } }
  })
  const [speedSnap, setSpeedSnap] = useState(null)
  const [pinMode, setPinMode] = useState(null)
  const pinRef = useRef(null)
  const pinClickGuard = useRef(false)
  const listRef = useRef(null)

  useEffect(() => { localStorage.setItem('nodes.tab', tab) }, [tab])
  useEffect(() => { sessionStorage.setItem('nodes.search', search) }, [search])
  useEffect(() => { sessionStorage.setItem('nodes.roleMask', String(roleMask)) }, [roleMask])
  useEffect(() => { sessionStorage.setItem('nodes.sort', JSON.stringify(sort)) }, [sort])

  const load = () => {
    setLoading(true)
    setError('')
    api.get('/nodes').then(setData).catch(err => setError(err?.message || '加载失败')).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const resyncAll = async () => {
    if (!(await confirm({ title: '同步所有节点', message: '向所有节点重新推送转发规则？', confirmText: '同步' }))) return
    try { await api.post('/nodes/resync-all'); toast('已发起同步'); load() } catch (err) { toast(err.message, 'error') }
  }

  const upgradeAll = async () => {
    if (!(await confirm({ title: '升级所有节点', message: '向所有版本不一致的节点推送升级？', confirmText: '升级' }))) return
    try { await api.post('/nodes/upgrade-all'); toast('已发起升级'); load() } catch (err) { toast(err.message, 'error') }
  }

  const deleteNode = async (node) => {
    if (!(await confirm({ title: '删除节点', message: `确认删除节点 ${node.name}？此操作会清空节点上的转发。`, confirmText: '删除', danger: true }))) return
    try {
      const res = await api.del(`/nodes/${node.id}`)
      if (res?.needs_confirm) {
        const names = (res.affected_composites || []).map(c => c.name).join('、')
        if (!(await confirm({ title: '该节点被组合引用', message: `节点「${node.name}」是组合 ${names} 的成员，删除会将它从这些组合中移除、改变其定义。是否继续？`, confirmText: '仍然删除', danger: true }))) return
        await api.del(`/nodes/${node.id}?confirm=1`)
      }
      toast('已删除'); load()
    } catch (err) { toast(err.message, 'error') }
  }

  const resyncNode = async (id) => {
    try { await api.post(`/nodes/${id}/resync`); toast('已发起同步') } catch (err) { toast(err.message, 'error') }
  }

  if (loading && !data) return <Layout><Loading /></Layout>
  if (!data && error) return <Layout><Empty title="加载失败" desc={error}><button onClick={load} className="btn-secondary text-xs mt-3">重试</button></Empty></Layout>

  const { nodes = [], latest_agent_version, node_raw_traffic = {} } = data || {}
  const singleNodes = nodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes
  const q = search.trim().toLowerCase()
  const roleFiltered = !roleMask ? tabNodes : tabNodes.filter(n => ((n.roles ?? 1) & roleMask) === roleMask)
  const filtered0 = !q ? roleFiltered : roleFiltered.filter(n => (n.name || '').toLowerCase().includes(q))

  const switchTab = (key) => setTab(key)
  const cycleSort = (col) => {
    setSort(s => {
      if (col === 'speed') setSpeedSnap({ ...speeds })
      if (s.col !== col) return { col, dir: 'desc' }
      if (s.dir === 'desc') return { col, dir: 'asc' }
      return { col: null, dir: null }
    })
  }
  const filtered = !sort.col ? filtered0 : [...filtered0].sort((a, b) => {
    let d = 0
    if (sort.col === 'rawtraffic') {
      d = (node_raw_traffic[a.id] || 0) - (node_raw_traffic[b.id] || 0)
    } else if (sort.col === 'speed') {
      const sa = speedSnap || speeds
      const va = sa[a.id] ? (sa[a.id].up + sa[a.id].down) : 0
      const vb = sa[b.id] ? (sa[b.id].up + sa[b.id].down) : 0
      d = va - vb
    }
    return sort.dir === 'asc' ? d : -d
  })
  // 任何过滤/排序生效时都不能拖拽调序：saveOrder 以可见列表为全量重建顺序，
  // 子集视图下会把被过滤掉的节点从顺序里丢掉。
  const draggable = !sort.col && !q && !roleMask
  const saveOrder = async (visibleList) => {
    const otherIds = (tab === 'composite' ? singleNodes : compositeNodes).map(n => n.id)
    const tabIds = visibleList.map(n => n.id)
    const allIds = tab === 'composite' ? [...otherIds, ...tabIds] : [...tabIds, ...otherIds]
    const byId = Object.fromEntries(nodes.map(n => [n.id, n]))
    setData(d => ({ ...d, nodes: allIds.map(id => byId[id]) }))
    try { await api.post('/nodes/reorder', { ids: allIds }); toast('顺序已保存') } catch (err) { toast(err.message, 'error'); load() }
  }
  const onDrop = async (toIndex) => {
    if (dragIndex === null || dragIndex === toIndex) { setDragIndex(null); return }
    const list = [...filtered]
    const [moved] = list.splice(dragIndex, 1)
    list.splice(toIndex, 0, moved)
    setDragIndex(null)
    saveOrder(list)
  }
  const moveToEdge = (idx, edge) => {
    const list = [...filtered]
    const [moved] = list.splice(idx, 1)
    if (edge === 'top') list.unshift(moved); else list.push(moved)
    saveOrder(list)
  }
  const onPinDown = (e, idx) => {
    if (e.button !== 0 || e.target.closest('[draggable], button, a')) return
    pinRef.current = { idx, x0: e.clientX, y0: e.clientY, entered: false }
    e.currentTarget.setPointerCapture(e.pointerId)
  }
  const onPinMove = (e) => {
    const p = pinRef.current
    if (!p) return
    const dx = e.clientX - p.x0, dy = e.clientY - p.y0
    if (!p.entered) {
      if (Math.abs(dx) < 10 && Math.abs(dy) < 10) return
      if (dx < -40 && Math.abs(dx) > Math.abs(dy) * 1.5) { p.entered = true } else if (Math.abs(dy) > 20 || dx > 10) { pinRef.current = null; return }
      if (!p.entered) return
    }
    e.preventDefault()
    const zone = (e.clientY - p.y0) < -20 ? 'top' : (e.clientY - p.y0) > 20 ? 'bottom' : null
    setPinMode({ idx: p.idx, zone, cx: e.clientX, cy: e.clientY })
  }
  const onPinUp = () => {
    const p = pinRef.current
    pinRef.current = null
    if (!p?.entered || !pinMode) { setPinMode(null); return }
    pinClickGuard.current = true
    setTimeout(() => { pinClickGuard.current = false }, 100)
    const { idx, zone } = pinMode
    setPinMode(null)
    if (zone) moveToEdge(idx, zone)
  }

  return (
    <Layout>
      <div className="h-full flex flex-col admin-list-page admin-node-list-page">
      <PageHeader title="节点" count={nodes.length} unit="个节点" />

      {/* Node list */}
      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索节点名称…" />
          {latest_agent_version && <span className="text-xs text-ink-mut whitespace-nowrap">agent {latest_agent_version}</span>}
          <div className="ml-auto hidden md:flex gap-2">
            <button onClick={() => setShowAdd(true)} className="btn-primary text-xs">+ 添加节点</button>
            <button onClick={() => setShowComposite(true)} className="btn-primary text-xs">+ 组合节点</button>
            <button onClick={resyncAll} className="btn-secondary text-xs">同步所有</button>
            <button onClick={upgradeAll} className="btn-secondary text-xs">一键升级全部</button>
          </div>
        </PanelToolbar>
        <div className="panel-filterbar flex items-center flex-wrap gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
          {[['single', '单点', singleNodes.length], ['composite', '组合', compositeNodes.length]].map(([key, label, n]) => (
            <button key={key} onClick={() => switchTab(key)}
              className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                tab === key ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}>{label} {n}</button>
          ))}
          <span className="ml-3 text-xs text-ink-mut select-none">角色</span>
          {[[1, '入口'], [2, '中转']].map(([bit, label]) => (
            <button key={bit} onClick={() => setRoleMask(m => m ^ bit)}
              title="按节点角色筛选，可叠加（同时选中表示需兼具两种角色）；不选则显示全部"
              className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                (roleMask & bit) !== 0 ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}>{label}</button>
          ))}
        </div>
        <TableScroll>
        {nodes.length === 0 ? (
          <Empty title="尚未注册任何节点" desc="点击右上角「添加节点」创建。" />
        ) : tabNodes.length === 0 ? (
          <Empty title={tab === 'composite' ? '暂无组合节点' : '暂无单点节点'} desc={tab === 'composite' ? '点击右上角「组合节点」创建。' : '点击右上角「添加节点」创建。'} />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配节点" desc={roleMask ? '试试别的关键词，或取消上方的角色筛选。' : '试试别的关键词。'} />
        ) : (<>
          {/* Desktop table */}
          {!isMobile && <div ref={listRef} className="relative">
          <table className="tbl">
            <thead><tr>
              <th className="w-14">ID</th><th>名称</th><th>IP 栈</th><th>版本</th><th>最近同步</th><th>状态</th>
              <th className="cursor-pointer select-none" onClick={() => cycleSort('rawtraffic')}
                title="节点实际转发的累计字节（上行+下行），不乘倍率、不随重置清零；组合节点取其入口物理子节点的原始流量">
                <span className="inline-flex items-center">原始流量<SortArrow col="rawtraffic" sort={sort} /></span>
              </th>
              <th className="cursor-pointer select-none min-w-[170px]" onClick={() => cycleSort('speed')}>
                <span className="inline-flex items-center">速度<SortArrow col="speed" sort={sort} /></span>
              </th>
              <th className="text-right">操作</th>
            </tr></thead>
            <tbody>
              {filtered.map((n, i) => (
                <tr key={n.id}
                  onDragOver={draggable ? e => e.preventDefault() : undefined}
                  onDrop={draggable ? () => onDrop(i) : undefined}
                  onPointerDown={e => onPinDown(e, i)}
                  onPointerMove={onPinMove}
                  onPointerUp={onPinUp}
                  onClick={() => { if (!pinClickGuard.current) navigate(`/nodes/${n.id}`) }}
                  className={`cursor-pointer ${dragIndex === i ? 'opacity-50' : ''} ${pinMode?.idx === i ? 'opacity-40' : ''}`}>
                  <td className="font-mono text-xs text-ink-mut">
                    {draggable && <span className="text-ink-mut mr-1 select-none cursor-move" title="拖拽排序"
                      draggable onDragStart={() => setDragIndex(i)}>⠿</span>}#{n.id}
                  </td>
                  <td>
                    <span className="inline-flex items-center gap-2 font-semibold text-blue-600">
                      <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500 shadow-[0_0_0_3px_rgba(34,197,94,0.18)]' : 'bg-gray-400 shadow-[0_0_0_3px_rgba(154,163,176,0.16)]'}`} />
                      {n.name}
                      {((n.roles ?? 1) & 1) !== 0 && <Badge color="green">入口</Badge>}
                      {((n.roles ?? 1) & 2) !== 0 && <Badge color="blue">中转</Badge>}
                    </span>
                  </td>
                  <td>
                    <div className="flex items-center gap-1.5 flex-wrap">
                      <NodeStackBadge node={n} />
                    </div>
                  </td>
                  <td className="font-mono text-xs">
                    {n.agent_version ? (
                      <span className={n.agent_version !== latest_agent_version ? 'text-red-600' : ''}>{n.agent_version}</span>
                    ) : <span className="text-ink-mut">--</span>}
                  </td>
                  <td className="font-mono text-xs text-ink-soft">
                    {fmtTime(n.last_apply_at?.Valid ? n.last_apply_at.Int64 : null)}
                  </td>
                  <td><NodeStatus node={n} /></td>
                  <td className="font-mono text-xs text-ink-mut">{fmtBytes(node_raw_traffic[n.id] || 0)}</td>
                  <td className="font-mono text-xs whitespace-nowrap min-w-[170px]">
                    {speeds[n.id] ? (
                      <>
                        <span className="text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                        {' '}
                        <span className="text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                      </>
                    ) : (
                      <span className="text-ink-mut">--</span>
                    )}
                  </td>
                  <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                    <div className="flex gap-2 justify-end">
                      {n.node_type !== 'composite' && <button onClick={() => resyncNode(n.id)} title="重新同步" className="icon-btn">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M3 21v-5h5"/></svg>
                      </button>}
                      <button onClick={() => deleteNode(n)} title="删除" className="icon-btn-danger">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/></svg>
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {pinMode && (
            <div className="fixed z-50 pointer-events-none flex flex-col items-end gap-1"
              style={{ left: pinMode.cx - 16, top: pinMode.cy - 18, transform: 'translateX(-100%)' }}>
              <div className={`rounded-md px-3.5 py-1.5 text-[12px] font-bold shadow transition-colors ${
                pinMode.zone === 'top' ? 'bg-blue-500 text-white' : 'bg-white/90 text-blue-400 border border-blue-200'}`}>↑ 置顶</div>
              <div className={`rounded-md px-3.5 py-1.5 text-[12px] font-bold shadow transition-colors ${
                pinMode.zone === 'bottom' ? 'bg-amber-500 text-white' : 'bg-white/90 text-amber-400 border border-amber-200'}`}>置底 ↓</div>
            </div>
          )}
          </div>}
          {/* Mobile cards */}
          {isMobile && <div>
            {filtered.map(n => (
              <Link key={n.id} to={`/nodes/${n.id}`} className="mobile-card block no-underline text-ink">
                <div className="flex items-center justify-between mb-1">
                  <span className="inline-flex items-center gap-2 font-semibold text-blue-600">
                    <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500' : 'bg-gray-400'}`} />
                    {n.name}
                    {((n.roles ?? 1) & 1) !== 0 && <Badge color="green">入口</Badge>}
                    {((n.roles ?? 1) & 2) !== 0 && <Badge color="blue">中转</Badge>}
                  </span>
                  <NodeStatus node={n} />
                </div>
                <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                  <span className="font-mono text-ink-mut">原始 {fmtBytes(node_raw_traffic[n.id] || 0)}</span>
                  {speeds[n.id] && <>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                    <span className="font-mono text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                  </>}
                </div>
              </Link>
            ))}
          </div>}
        </>)}
        </TableScroll>
      </Panel>
      </div>

      <AddNodeModal open={showAdd} onClose={() => setShowAdd(false)} onDone={() => { setShowAdd(false); load() }} />
      <CompositeNodeModal open={showComposite} onClose={() => setShowComposite(false)} nodes={nodes} onDone={() => { setShowComposite(false); load() }} />
    </Layout>
  )
}

function SortArrow({ col, sort }) {
  const active = sort.col === col
  return (
    <span className="inline-flex flex-col leading-[0.55] text-[9px] ml-1">
      <span className={active && sort.dir === 'asc' ? 'text-blue-600' : 'text-ink-mut opacity-50'}>▲</span>
      <span className={active && sort.dir === 'desc' ? 'text-blue-600' : 'text-ink-mut opacity-50'}>▼</span>
    </span>
  )
}

// 打开弹窗时才拉一次用户列表；授权只对普通用户有意义，管理员天然全量可见。
function useGrantableUsers(open) {
  const [users, setUsers] = useState(null)
  useEffect(() => {
    if (!open || users) return
    api.get('/users').then(d => setUsers((d.users || []).filter(u => u.role === 'user'))).catch(() => setUsers([]))
  }, [open, users])
  return users || []
}

function GrantUsersField({ users, value, onChange }) {
  return (
    <>
      <label className="text-[13px] font-semibold text-ink-soft">授权用户 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
      <Select multiple searchable placeholder="不授权，可稍后在用户详情添加" value={value} onChange={onChange}
        options={users.map(u => ({ value: u.id, label: u.username }))} />
    </>
  )
}

function AddNodeModal({ open, onClose, onDone }) {
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [portStart, setPortStart] = useState('10001')
  const [portEnd, setPortEnd] = useState('20000')
  const [rateMult, setRateMult] = useState('1')
  const [unidirectional, setUnidirectional] = useState(false)
  const [userIds, setUserIds] = useState([])
  const [loading, setLoading] = useState(false)
  const users = useGrantableUsers(open)
  const toast = useToast()
  const navigate = useNavigate()

  const submit = async (e) => {
    e.preventDefault()
    // 显式校验而不是把非法值静默替换成 1：用户必须知道输入没有被按原样保存。
    const rm = Number(rateMult)
    if (rateMult.trim() === '' || !Number.isFinite(rm) || rm < 0) { toast('倍率必须是大于等于 0 的数字', 'error'); return }
    setLoading(true)
    try {
      const portRange = `${portStart || '10001'}-${portEnd || '20000'}`
      const res = await api.post('/nodes', {
        name, secret: secret || undefined, port_range: portRange, rate_multiplier: rm, unidirectional,
        user_ids: userIds.length ? userIds.map(Number) : undefined,
      })
      toast('节点已添加')
      setName(''); setSecret(''); setPortStart('10001'); setPortEnd('20000'); setRateMult('1'); setUnidirectional(false); setUserIds([])
      if (res?.node?.id) navigate(`/nodes/${res.node.id}`)
      else onDone()
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="添加节点">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="text-[13px] font-semibold text-ink-soft">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required placeholder="例如 hk-1" />
          <label className="text-[13px] font-semibold text-ink-soft">Token <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field font-mono" value={secret} onChange={e => setSecret(e.target.value)} placeholder="留空则随机生成 64 位 hex" />
          <label className="text-[13px] font-semibold text-ink-soft">倍率</label>
          <input className="input-field font-mono" type="number" min="0" step="0.1" value={rateMult} onChange={e => setRateMult(e.target.value)} style={{ width: 100 }} />
          <label className="text-[13px] font-semibold text-ink-soft">计费方向</label>
          <div className="flex items-center gap-2">
            <button type="button" onClick={() => setUnidirectional(u => !u)}
              className={`inline-flex items-center gap-1.5 px-3.5 py-[7px] rounded-[8px] text-[13px] font-semibold border cursor-pointer transition-colors ${unidirectional ? 'bg-amber-50 text-amber-700 border-amber-200 hover:bg-amber-100' : 'bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100'}`}>
              {unidirectional ? '单向计费（取较大方向）' : '双向计费（出站+入站）'}
            </button>
            <span className="text-xs text-ink-mut">当前：{unidirectional ? '单向' : '双向'}</span>
          </div>
          <GrantUsersField users={users} value={userIds} onChange={setUserIds} />
        </div>
        <div className="flex gap-2">
          <div className="flex-1">
            <label className="text-[13px] font-semibold text-ink-soft block mb-1">起始端口</label>
            <input className="input-field w-full font-mono" type="number" min="1" max="65535"
              value={portStart} onChange={e => setPortStart(e.target.value)} placeholder="10001" />
          </div>
          <div className="flex-1">
            <label className="text-[13px] font-semibold text-ink-soft block mb-1">结束端口</label>
            <input className="input-field w-full font-mono" type="number" min="1" max="65535"
              value={portEnd} onChange={e => setPortEnd(e.target.value)} placeholder="20000" />
          </div>
        </div>
        <div className="flex gap-3 pt-4 border-t border-line-soft">
          <button type="submit" disabled={loading} className="btn-primary">添加节点</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
          <span className="text-xs text-ink-mut ml-auto">添加后会生成 token 与安装命令。</span>
        </div>
      </form>
    </Modal>
  )
}

function NodeStatus({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  // A composite node has no agent of its own to sync; its health is the
  // aggregate of its child hops, so show online/offline rather than a sync
  // state that would always read as "pending" or surface a spurious error.
  if (node.node_type === 'composite') {
    return node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="gray">离线</Badge>
  }
  // Error/warning outrank connectivity: they explain why the node needs
  // attention, so they stay visible while offline instead of being hidden
  // behind a silent "离线" — matches the detail page header's priority
  // (disabled > error > warning > online > offline).
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red" title={lastErr}>错误</Badge>
  if (node.last_warning) return <Badge color="amber" title={node.last_warning}>警告</Badge>
  // A disconnected agent is offline regardless of when it last synced; a stale
  // "已同步" on an offline node misrepresents its real state.
  if (node.online !== 1) return <Badge color="gray">离线</Badge>
  if (node.last_apply_at?.Valid) return <Badge color="green">已同步</Badge>
  return <Badge color="amber">待同步</Badge>
}

function CompositeNodeModal({ open, onClose, nodes, onDone }) {
  const [name, setName] = useState('')
  const [rateMult, setRateMult] = useState('1')
  const [hops, setHops] = useState([{ node_id: '', mode: 'userspace' }])
  const [userIds, setUserIds] = useState([])
  const [loading, setLoading] = useState(false)
  const users = useGrantableUsers(open)
  const toast = useToast()
  const navigate = useNavigate()

  const addHop = () => setHops(h => [...h, { node_id: '', mode: 'userspace' }])
  const removeHop = (i) => setHops(h => h.filter((_, j) => j !== i))
  const setHop = (i, k, v) => setHops(h => h.map((hop, j) => j === i ? { ...hop, [k]: v } : hop))
  const moveHop = (i, dir) => {
    setHops(h => {
      const arr = [...h]
      const j = i + dir
      if (j < 0 || j >= arr.length) return arr
      ;[arr[i], arr[j]] = [arr[j], arr[i]]
      return arr
    })
  }

  const submit = async (e) => {
    e.preventDefault()
    // 空行必须显式处理而不是静默过滤：过滤会让末跳与界面按行数标出的末跳
    // 错位，末跳模式（中转生效、出口被覆盖）会落到错误的行上
    if (hops.some(h => !h.node_id)) {
      toast('请为每一跳选择节点，或删除空行', 'error')
      return
    }
    if (hops.length < 2) {
      toast('组合节点至少需要 2 个子节点', 'error')
      return
    }
    const rm = Number(rateMult)
    if (rateMult.trim() === '' || !Number.isFinite(rm) || rm < 0) { toast('倍率必须是大于等于 0 的数字', 'error'); return }
    setLoading(true)
    try {
      const res = await api.post('/nodes', {
        name,
        node_type: 'composite',
        rate_multiplier: rm,
        hops: hops.map(h => ({ node_id: Number(h.node_id), mode: h.mode })),
        user_ids: userIds.length ? userIds.map(Number) : undefined,
      })
      toast('组合节点已创建')
      setName('')
      setRateMult('1')
      setHops([{ node_id: '', mode: 'userspace' }])
      setUserIds([])
      if (res?.node?.id) navigate(`/nodes/${res.node.id}`)
      else onDone()
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="创建组合节点">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="text-[13px] font-semibold text-ink-soft">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required placeholder="例如 hk-jp-chain" />
          <label className="text-[13px] font-semibold text-ink-soft">倍率</label>
          <input className="input-field font-mono" type="number" min="0" step="0.1" value={rateMult} onChange={e => setRateMult(e.target.value)} style={{ width: 100 }} />
          <GrantUsersField users={users} value={userIds} onChange={setUserIds} />
        </div>

        <div>
          <div className="flex items-center gap-2 mb-2">
            <span className="text-[13px] font-semibold text-ink-soft">跳序（从入口到出口）</span>
          </div>
          <div className="space-y-2">
            {hops.map((hop, i) => {
              // A composite member is a black box: its own hops decide how it
              // forwards, so no per-row mode applies to it here.
              const memberIsComposite = nodes.find(n => n.id === Number(hop.node_id))?.node_type === 'composite'
              return (
              <div key={i} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
                <span className="text-xs text-ink-mut w-5 text-center font-mono">{i + 1}</span>
                <Select className="flex-1" placeholder="-- 选择节点 --" searchable value={hop.node_id} onChange={v => setHop(i, 'node_id', v)}
                  options={nodes.filter(n => n.id === Number(hop.node_id) || !hops.some((h, j) => j !== i && Number(h.node_id) === n.id)).map(n => ({ value: n.id, label: n.name }))} />
                {memberIsComposite ? (
                  <span className="text-[11px] text-ink-mut shrink-0 text-center cursor-help" style={{ width: 110 }} title="组合成员：转发模式由其内部各跳决定，不在此处配置">组合</span>
                ) : (<>
                  {/* 每一跳都可配模式，包含末跳：末跳模式在该组合被用作中转时生效；
                      被用作规则出口时由规则的出口模式覆盖 */}
                  <Select value={hop.mode} onChange={v => setHop(i, 'mode', v)} style={{ width: 110 }}
                    title={i === hops.length - 1 ? '末跳模式：作为中转时生效；作为规则出口时由规则的出口模式覆盖' : undefined}
                    options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
                  {i === hops.length - 1 && (
                    <span className="text-[11px] text-ink-mut shrink-0 cursor-help" title="末跳模式：作为中转时生效；作为规则出口时由规则的出口模式覆盖">末</span>
                  )}
                </>)}
                <button type="button" onClick={() => moveHop(i, -1)} disabled={i === 0} className="btn-secondary text-xs px-1.5">↑</button>
                <button type="button" onClick={() => moveHop(i, 1)} disabled={i === hops.length - 1} className="btn-secondary text-xs px-1.5">↓</button>
                {hops.length > 1 && (
                  <button type="button" onClick={() => removeHop(i)} className="btn-danger-sm text-xs px-1.5">×</button>
                )}
              </div>
              )
            })}
          </div>
          <button type="button" onClick={addHop} className="btn-secondary text-xs mt-2">+ 添加一跳</button>
        </div>

        <div className="flex gap-3 pt-4 border-t border-line-soft">
          <button type="submit" disabled={loading} className="btn-primary">创建组合节点</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
