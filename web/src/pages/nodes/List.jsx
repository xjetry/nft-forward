import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, nullStr } from '../../lib/fmt'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Badge, Modal, Confirm, NodeTypeBadge, useConfirm, Select } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, TableScroll } from '../../components/page'

export default function NodeList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showAdd, setShowAdd] = useState(false)
  const [showComposite, setShowComposite] = useState(false)
  const [panelUrl, setPanelUrl] = useState('')
  const [search, setSearch] = useState('')
  const [tab, setTab] = useState('single')
  const [dragIndex, setDragIndex] = useState(null)
  // Default on, persisted per-browser: most of the time you want the working
  // set, but the preference is a local view choice, not server state.
  const [onlyVisible, setOnlyVisible] = useState(() => localStorage.getItem('nodes.onlyVisible') !== '0')
  const toast = useToast()
  const confirm = useConfirm()

  useEffect(() => { localStorage.setItem('nodes.onlyVisible', onlyVisible ? '1' : '0') }, [onlyVisible])

  const load = () => {
    setLoading(true)
    api.get('/nodes').then(d => {
      setData(d)
      setPanelUrl(d.panel_url || '')
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

  const { nodes = [], server_version } = data || {}
  const singleNodes = nodes.filter(n => n.node_type !== 'composite')
  const compositeNodes = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? compositeNodes : singleNodes
  const q = search.trim().toLowerCase()
  const visibleNodes = onlyVisible ? tabNodes.filter(n => !n.hidden) : tabNodes
  const filtered = !q ? visibleNodes : visibleNodes.filter(n => (n.name || '').toLowerCase().includes(q))
  const hiddenCount = tabNodes.length - tabNodes.filter(n => !n.hidden).length
  // Drag-reorder is only offered when the shown rows are exactly tabNodes in
  // order — no search and no hidden rows filtered out — so a dropped row's
  // index maps straight onto tabNodes. Persisting sends the full id order (the
  // reordered tab plus the other tab in its current order) so sort_order stays
  // a clean sequence.
  const draggable = filtered.length === tabNodes.length
  const onDrop = async (toIndex) => {
    if (dragIndex === null || dragIndex === toIndex) { setDragIndex(null); return }
    const list = [...tabNodes]
    const [moved] = list.splice(dragIndex, 1)
    list.splice(toIndex, 0, moved)
    setDragIndex(null)
    const otherIds = (tab === 'composite' ? singleNodes : compositeNodes).map(n => n.id)
    const tabIds = list.map(n => n.id)
    const allIds = tab === 'composite' ? [...otherIds, ...tabIds] : [...tabIds, ...otherIds]
    const byId = Object.fromEntries(nodes.map(n => [n.id, n]))
    setData(d => ({ ...d, nodes: allIds.map(id => byId[id]) }))
    try { await api.post('/nodes/reorder', { ids: allIds }); toast('顺序已保存') } catch (err) { toast(err.message); load() }
  }

  return (
    <Layout>
      <PageHeader title="节点" count={nodes.length} unit="个节点" />

      {/* Panel URL settings */}
      <Panel className="mb-5">
        <div className="flex items-center gap-3 px-[22px] py-4 border-b border-line-soft">
          <h3 className="text-sm font-bold text-ink">面板地址</h3>
          <span className="text-xs text-ink-mut">agent 反向连接面板用的公网地址，会写进各节点的安装命令</span>
        </div>
        <div className="p-5">
          <form onSubmit={savePanelUrl} className="flex items-center gap-3 max-w-xl">
            <label className="text-[13px] font-semibold text-ink-soft whitespace-nowrap">Panel 地址</label>
            <input className="input-field font-mono flex-1" value={panelUrl} onChange={e => setPanelUrl(e.target.value)} placeholder="例如 https://panel.example.com" />
            <button type="submit" className="btn-primary whitespace-nowrap">保存</button>
          </form>
          <p className="text-xs text-ink-mut mt-2">留空则安装命令回退使用你当前访问的域名。</p>
        </div>
      </Panel>

      {/* Node list */}
      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索节点名称…" />
          {server_version && <span className="text-xs text-ink-mut whitespace-nowrap">server {server_version}</span>}
          <div className="ml-auto flex gap-2">
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
        ) : (
          <table className="tbl">
            <thead><tr><th className="w-14">ID</th><th>名称</th><th>类型</th><th>版本</th><th>最近同步</th><th>状态</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {filtered.map((n, i) => (
                <tr key={n.id}
                  draggable={draggable}
                  onDragStart={draggable ? () => setDragIndex(i) : undefined}
                  onDragOver={draggable ? e => e.preventDefault() : undefined}
                  onDrop={draggable ? () => onDrop(i) : undefined}
                  className={`${draggable ? 'cursor-move' : ''} ${dragIndex === i ? 'opacity-50' : ''}`}>
                  <td className="font-mono text-xs text-ink-mut">
                    {draggable && <span className="text-ink-mut mr-1 select-none" title="拖拽排序">⠿</span>}#{n.id}
                  </td>
                  <td>
                    <span className="inline-flex items-center gap-2 font-semibold">
                      <span className={`w-1.5 h-1.5 rounded-full flex-none ${!n.disabled && n.online === 1 ? 'bg-green-500 shadow-[0_0_0_3px_rgba(34,197,94,0.18)]' : 'bg-gray-400 shadow-[0_0_0_3px_rgba(154,163,176,0.16)]'}`} />
                      <Link to={`/nodes/${n.id}`} className="text-blue-600 font-semibold hover:underline">{n.name}</Link>
                      {n.hidden && <Badge color="gray">已隐藏</Badge>}
                    </span>
                  </td>
                  <td><NodeTypeBadge type={n.node_type} /></td>
                  <td className="font-mono text-xs">
                    {n.agent_version ? (
                      <span className={n.agent_version !== server_version ? 'text-red-600' : ''}>{n.agent_version}</span>
                    ) : <span className="text-ink-mut">--</span>}
                  </td>
                  <td className="font-mono text-xs text-ink-soft">
                    {fmtTime(n.last_apply_at?.Valid ? n.last_apply_at.Int64 : null)}
                  </td>
                  <td><NodeStatus node={n} /></td>
                  <td className="text-right whitespace-nowrap">
                    <button onClick={() => toggleHidden(n)} title={n.hidden ? '在节点列表与规则列表中显示' : '从节点列表与规则列表中隐藏（不影响转发）'} className="btn-secondary text-xs mr-1.5">{n.hidden ? '显示' : '隐藏'}</button>
                    {n.node_type !== 'composite' && <button onClick={() => resyncNode(n.id)} className="btn-secondary text-xs mr-1.5">重新同步</button>}
                    <button onClick={() => deleteNode(n)} className="btn-danger-sm text-xs">删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        </TableScroll>
      </Panel>

      <AddNodeModal open={showAdd} onClose={() => setShowAdd(false)} onDone={() => { setShowAdd(false); load() }} />
      <CompositeNodeModal open={showComposite} onClose={() => setShowComposite(false)} nodes={nodes.filter(n => n.node_type !== 'composite')} onDone={() => { setShowComposite(false); load() }} />
    </Layout>
  )
}

function AddNodeModal({ open, onClose, onDone }) {
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()
  const navigate = useNavigate()

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      const res = await api.post('/nodes', { name, secret: secret || undefined })
      toast('节点已添加')
      setName(''); setSecret('')
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
