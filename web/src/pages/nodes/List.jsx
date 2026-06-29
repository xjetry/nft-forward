import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, fmtBytes, nullStr } from '../../lib/fmt'
import { useSpeed, fmtSpeed } from '../../lib/useSpeed'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, Modal, Confirm, NodeTypeBadge, useConfirm, Select } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, TableScroll } from '../../components/page'

export default function NodeList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showAdd, setShowAdd] = useState(false)
  const [showComposite, setShowComposite] = useState(false)
  const [panelUrl, setPanelUrl] = useState('')
  const [panelName, setPanelName] = useState('')
  const [showRateToUser, setShowRateToUser] = useState(false)
  const [search, setSearch] = useState('')
  const [tab, setTab] = useState(() => localStorage.getItem('nodes.tab') || 'single')
  const [dragIndex, setDragIndex] = useState(null)
  // Default on, persisted per-browser: most of the time you want the working
  // set, but the preference is a local view choice, not server state.
  const [onlyVisible, setOnlyVisible] = useState(() => localStorage.getItem('nodes.onlyVisible') !== '0')
  const speeds = useSpeed()
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()
  const [sort, setSort] = useState({ col: null, dir: null })
  const [speedSnap, setSpeedSnap] = useState(null)

  useEffect(() => { localStorage.setItem('nodes.tab', tab) }, [tab])
  useEffect(() => { localStorage.setItem('nodes.onlyVisible', onlyVisible ? '1' : '0') }, [onlyVisible])

  const load = () => {
    setLoading(true)
    api.get('/nodes').then(d => {
      setData(d)
      setPanelUrl(d.panel_url || '')
      setPanelName(d.panel_name || '')
      setShowRateToUser(!!d.show_rate_to_user)
    }).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const savePanelUrl = async (e) => {
    e.preventDefault()
    try {
      await api.post('/settings', { panel_url: panelUrl })
      toast('面板地址已保存')
    } catch (err) { toast(err.message) }
  }

  const savePanelName = async (e) => {
    e.preventDefault()
    try {
      await api.post('/settings', { panel_url: panelUrl, panel_name: panelName })
      toast('面板名称已保存')
    } catch (err) { toast(err.message) }
  }

  const resyncAll = async () => {
    if (!(await confirm({ title: '同步所有节点', message: '向所有节点重新推送转发规则？', confirmText: '同步' }))) return
    try { await api.post('/nodes/resync-all'); toast('已发起同步'); load() } catch (err) { toast(err.message) }
  }

  const upgradeAll = async () => {
    if (!(await confirm({ title: '升级所有节点', message: '向所有版本不一致的节点推送升级？', confirmText: '升级' }))) return
    try { await api.post('/nodes/upgrade-all'); toast('已发起升级'); load() } catch (err) { toast(err.message) }
  }

  const deleteNode = async (node) => {
    if (!(await confirm({ title: '删除节点', message: `确认删除节点 ${node.name}？此操作会清空节点上的转发。`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/nodes/${node.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  const resyncNode = async (id) => {
    try { await api.post(`/nodes/${id}/resync`); toast('已发起同步') } catch (err) { toast(err.message) }
  }

  const toggleHidden = async (node) => {
    try { await api.post(`/nodes/${node.id}/hidden`); toast(node.hidden ? '已显示节点' : '已隐藏节点'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { nodes = [], latest_agent_version, node_traffic = {} } = data || {}
  const singleNodes = nodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes
  const q = search.trim().toLowerCase()
  const visibleNodes = onlyVisible ? tabNodes.filter(n => !n.hidden) : tabNodes
  const filtered0 = !q ? visibleNodes : visibleNodes.filter(n => (n.name || '').toLowerCase().includes(q))
  const hiddenCount = tabNodes.length - tabNodes.filter(n => !n.hidden).length

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
    if (sort.col === 'traffic') {
      d = (node_traffic[a.id] || 0) - (node_traffic[b.id] || 0)
    } else if (sort.col === 'speed') {
      const sa = speedSnap || speeds
      const va = sa[a.id] ? (sa[a.id].up + sa[a.id].down) : 0
      const vb = sa[b.id] ? (sa[b.id].up + sa[b.id].down) : 0
      d = va - vb
    }
    return sort.dir === 'asc' ? d : -d
  })
  const draggable = !sort.col && !q
  const onDrop = async (toIndex) => {
    if (dragIndex === null || dragIndex === toIndex) { setDragIndex(null); return }
    const list = [...filtered]
    const [moved] = list.splice(dragIndex, 1)
    list.splice(toIndex, 0, moved)
    setDragIndex(null)
    const hiddenIds = tabNodes.filter(n => n.hidden).map(n => n.id)
    const otherIds = (tab === 'composite' ? singleNodes : compositeNodes).map(n => n.id)
    const tabIds = [...list.map(n => n.id), ...hiddenIds]
    const allIds = tab === 'composite' ? [...otherIds, ...tabIds] : [...tabIds, ...otherIds]
    const byId = Object.fromEntries(nodes.map(n => [n.id, n]))
    setData(d => ({ ...d, nodes: allIds.map(id => byId[id]) }))
    try { await api.post('/nodes/reorder', { ids: allIds }); toast('顺序已保存') } catch (err) { toast(err.message); load() }
  }

  return (
    <Layout>
      <PageHeader title="节点" count={nodes.length} unit="个节点" />

      {/* Panel URL settings — desktop only */}
      <Panel className="mb-5 hidden md:block">
        <div className="flex items-center gap-3 px-[22px] py-4 border-b border-line-soft">
          <h3 className="text-sm font-bold text-ink">面板设置</h3>
        </div>
        <div className="p-5 space-y-4">
          <form onSubmit={savePanelName} className="flex items-center gap-3 max-w-xl">
            <label className="text-[13px] font-semibold text-ink-soft whitespace-nowrap w-[80px]">面板名称</label>
            <input className="input-field flex-1" value={panelName} onChange={e => setPanelName(e.target.value)} placeholder="nft-forward" />
            <button type="submit" className="btn-primary whitespace-nowrap">保存</button>
          </form>
          <form onSubmit={savePanelUrl} className="flex items-center gap-3 max-w-xl">
            <label className="text-[13px] font-semibold text-ink-soft whitespace-nowrap w-[80px]">面板地址</label>
            <input className="input-field font-mono flex-1" value={panelUrl} onChange={e => setPanelUrl(e.target.value)} placeholder="例如 https://panel.example.com" />
            <button type="submit" className="btn-primary whitespace-nowrap">保存</button>
          </form>
          <label className="inline-flex items-center gap-2 cursor-pointer select-none">
            <input type="checkbox" className="accent-blue-600" checked={showRateToUser} onChange={async e => {
              const v = e.target.checked
              setShowRateToUser(v)
              try { await api.post('/settings', { panel_url: panelUrl, show_rate_to_user: v }); toast(v ? '用户侧倍率已开启' : '用户侧倍率已关闭') } catch (err) { toast(err.message); setShowRateToUser(!v) }
            }} />
            <span className="text-[13px] font-semibold text-ink-soft">向用户展示倍率</span>
          </label>
          <p className="text-xs text-ink-mut">面板名称留空则默认为 nft-forward；面板地址留空则安装命令回退使用当前域名。</p>
        </div>
      </Panel>

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
        <div className="flex items-center gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
          {[['single', '单点', singleNodes.length], ['composite', '组合', compositeNodes.length]].map(([key, label, n]) => (
            <button key={key} onClick={() => setTab(key)}
              className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                tab === key ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}>{label} {n}</button>
          ))}
          <label className="ml-auto inline-flex items-center gap-1.5 text-xs text-ink-soft cursor-pointer select-none"
            title="仅展示未隐藏的节点。隐藏的节点及其规则不在列表显示，但转发照常运行。该偏好保存在本浏览器。">
            <input type="checkbox" className="accent-blue-600" checked={onlyVisible} onChange={e => setOnlyVisible(e.target.checked)} />
            仅展示可见节点{hiddenCount > 0 && <span className="text-ink-mut">（{hiddenCount} 个隐藏）</span>}
          </label>
        </div>
        <TableScroll>
        {nodes.length === 0 ? (
          <Empty title="尚未注册任何节点" desc="点击右上角「添加节点」创建。" />
        ) : tabNodes.length === 0 ? (
          <Empty title={tab === 'composite' ? '暂无组合节点' : '暂无单点节点'} desc={tab === 'composite' ? '点击右上角「组合节点」创建。' : '点击右上角「添加节点」创建。'} />
        ) : filtered.length === 0 ? (
          q
            ? <Empty title="无匹配节点" desc="试试别的关键词。" />
            : <Empty title="没有可见节点" desc="当前分类的节点都已隐藏，关闭右上「仅展示可见节点」即可查看。" />
        ) : (<>
          {/* Desktop table */}
          <table className="tbl hidden md:table">
            <thead><tr>
              <th className="w-14">ID</th><th>名称</th><th>类型</th><th>版本</th><th>最近同步</th><th>状态</th>
              <th className="cursor-pointer select-none" onClick={() => cycleSort('traffic')}>
                <span className="inline-flex items-center">流量<SortArrow col="traffic" sort={sort} /></span>
              </th>
              <th className="cursor-pointer select-none" onClick={() => cycleSort('speed')}>
                <span className="inline-flex items-center">速度<SortArrow col="speed" sort={sort} /></span>
              </th>
              <th className="text-right">操作</th>
            </tr></thead>
            <tbody>
              {filtered.map((n, i) => (
                <tr key={n.id}
                  draggable={draggable}
                  onDragStart={draggable ? () => setDragIndex(i) : undefined}
                  onDragOver={draggable ? e => e.preventDefault() : undefined}
                  onDrop={draggable ? () => onDrop(i) : undefined}
                  onClick={() => navigate(`/nodes/${n.id}`)}
                  className={`cursor-pointer ${draggable ? 'cursor-move' : ''} ${dragIndex === i ? 'opacity-50' : ''}`}>
                  <td className="font-mono text-xs text-ink-mut">
                    {draggable && <span className="text-ink-mut mr-1 select-none" title="拖拽排序">⠿</span>}#{n.id}
                  </td>
                  <td>
                    <span className="inline-flex items-center gap-2 font-semibold text-blue-600">
                      <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500 shadow-[0_0_0_3px_rgba(34,197,94,0.18)]' : 'bg-gray-400 shadow-[0_0_0_3px_rgba(154,163,176,0.16)]'}`} />
                      {n.name}
                      {n.hidden && <Badge color="gray">已隐藏</Badge>}
                    </span>
                  </td>
                  <td><NodeTypeBadge type={n.node_type} /></td>
                  <td className="font-mono text-xs">
                    {n.agent_version ? (
                      <span className={n.agent_version !== latest_agent_version ? 'text-red-600' : ''}>{n.agent_version}</span>
                    ) : <span className="text-ink-mut">--</span>}
                  </td>
                  <td className="font-mono text-xs text-ink-soft">
                    {fmtTime(n.last_apply_at?.Valid ? n.last_apply_at.Int64 : null)}
                  </td>
                  <td><NodeStatus node={n} /></td>
                  <td className="font-mono text-xs text-ink-mut">{fmtBytes(node_traffic[n.id] || 0)}</td>
                  <td className="font-mono text-xs whitespace-nowrap">
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
                      <button onClick={() => toggleHidden(n)} title={n.hidden ? '显示' : '隐藏'} className="icon-btn">
                        {n.hidden ? <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
                        : <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9.88 9.88a3 3 0 1 0 4.24 4.24"/><path d="M10.73 5.08A10.43 10.43 0 0 1 12 5c7 0 10 7 10 7a13.16 13.16 0 0 1-1.67 2.68"/><path d="M6.61 6.61A13.526 13.526 0 0 0 2 12s3 7 10 7a9.74 9.74 0 0 0 5.39-1.61"/><path d="m2 2 20 20"/></svg>}
                      </button>
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
          {/* Mobile cards */}
          <div className="md:hidden">
            {filtered.map(n => (
              <Link key={n.id} to={`/nodes/${n.id}`} className="mobile-card block no-underline text-ink">
                <div className="flex items-center justify-between mb-1">
                  <span className="inline-flex items-center gap-2 font-semibold text-blue-600">
                    <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500' : 'bg-gray-400'}`} />
                    {n.name}
                    {n.hidden && <Badge color="gray">已隐藏</Badge>}
                  </span>
                  <NodeStatus node={n} />
                </div>
                <div className="flex items-center gap-2 text-xs text-ink-soft flex-wrap">
                  <NodeTypeBadge type={n.node_type} />
                  <span className="text-ink-mut">·</span>
                  <span className="font-mono text-ink-mut">{fmtBytes(node_traffic[n.id] || 0)}</span>
                  {speeds[n.id] && <>
                    <span className="text-ink-mut">·</span>
                    <span className="font-mono text-emerald-600">↑{fmtSpeed(speeds[n.id].up)}</span>
                    <span className="font-mono text-blue-600">↓{fmtSpeed(speeds[n.id].down)}</span>
                  </>}
                </div>
              </Link>
            ))}
          </div>
        </>)}
        </TableScroll>
      </Panel>

      <AddNodeModal open={showAdd} onClose={() => setShowAdd(false)} onDone={() => { setShowAdd(false); load() }} />
      <CompositeNodeModal open={showComposite} onClose={() => setShowComposite(false)} nodes={nodes.filter(n => n.node_type !== 'composite')} onDone={() => { setShowComposite(false); load() }} />
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

function AddNodeModal({ open, onClose, onDone }) {
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [portStart, setPortStart] = useState('10001')
  const [portEnd, setPortEnd] = useState('20000')
  const [rateMult, setRateMult] = useState('1')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  const navigate = useNavigate()

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      const portRange = `${portStart || '10001'}-${portEnd || '20000'}`
      const rm = parseFloat(rateMult)
      const res = await api.post('/nodes', { name, secret: secret || undefined, port_range: portRange, rate_multiplier: rm >= 0 ? rm : 1 })
      toast('节点已添加')
      setName(''); setSecret(''); setPortStart('10001'); setPortEnd('20000'); setRateMult('1')
      if (res?.node?.id) navigate(`/nodes/${res.node.id}`)
      else onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
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
  // A disconnected agent is offline regardless of when it last synced; a stale
  // "已同步" on an offline node misrepresents its real state.
  if (node.online !== 1) return <Badge color="gray">离线</Badge>
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red" title={lastErr}>错误</Badge>
  if (node.last_apply_at?.Valid) return <Badge color="green">已同步</Badge>
  return <Badge color="amber">待同步</Badge>
}

function CompositeNodeModal({ open, onClose, nodes, onDone }) {
  const [name, setName] = useState('')
  const [hops, setHops] = useState([{ node_id: '', mode: 'userspace' }])
  const [loading, setLoading] = useState(false)
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
    const validHops = hops.filter(h => h.node_id)
    if (validHops.length < 2) {
      toast('组合节点至少需要 2 个子节点')
      return
    }
    setLoading(true)
    try {
      const res = await api.post('/nodes', {
        name,
        node_type: 'composite',
        hops: validHops.map(h => ({ node_id: Number(h.node_id), mode: h.mode })),
      })
      toast('组合节点已创建')
      setName('')
      setHops([{ node_id: '', mode: 'userspace' }])
      if (res?.node?.id) navigate(`/nodes/${res.node.id}`)
      else onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="创建组合节点">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="text-[13px] font-semibold text-ink-soft">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required placeholder="例如 hk-jp-chain" />
        </div>

        <div>
          <div className="flex items-center gap-2 mb-2">
            <span className="text-[13px] font-semibold text-ink-soft">跳序（从入口到出口）</span>
          </div>
          <div className="space-y-2">
            {hops.map((hop, i) => (
              <div key={i} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
                <span className="text-xs text-ink-mut w-5 text-center font-mono">{i + 1}</span>
                <Select className="flex-1" placeholder="-- 选择节点 --" searchable value={hop.node_id} onChange={v => setHop(i, 'node_id', v)}
                  options={nodes.filter(n => n.id === Number(hop.node_id) || !hops.some((h, j) => j !== i && Number(h.node_id) === n.id)).map(n => ({ value: n.id, label: n.name }))} />
                <Select value={hop.mode} onChange={v => setHop(i, 'mode', v)} style={{ width: 110 }}
                  options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
                <button type="button" onClick={() => moveHop(i, -1)} disabled={i === 0} className="btn-secondary text-xs px-1.5">↑</button>
                <button type="button" onClick={() => moveHop(i, 1)} disabled={i === hops.length - 1} className="btn-secondary text-xs px-1.5">↓</button>
                {hops.length > 1 && (
                  <button type="button" onClick={() => removeHop(i)} className="btn-danger-sm text-xs px-1.5">×</button>
                )}
              </div>
            ))}
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
