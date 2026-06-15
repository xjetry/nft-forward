import { useState, useEffect } from 'react'
import { useParams, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, CopyText } from '../../components/ui'
import { HopRow } from './List'

export default function ChainDetail() {
  const { id } = useParams()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get(`/chains/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="链路不存在" /></Layout>

  const { chain, view, hops = [], nodes = [], fw_by_node = {}, node_by_id = {} } = data

  return (
    <Layout>
      {/* Entry */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">入口</h3><span className="text-xs text-gray-400">复制给客户端</span></div>
        <div className="p-5">
          <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
            <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">ENTRY</span>
            <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1"><SensText blurred={blurred}>{view?.entry}</SensText></span>
            <button onClick={() => { navigator.clipboard.writeText(view?.entry); toast('入口地址已复制') }}
              className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
              <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
              复制
            </button>
          </div>
          <div className="grid grid-cols-[90px_1fr] gap-4 items-center mt-5 text-sm">
            <span className="text-gray-500 font-semibold">协议</span>
            <span><ProtoBadge proto={chain.proto} /></span>
            <span className="text-gray-500 font-semibold">路径</span>
            <span className="font-mono text-gray-500"><SensText blurred={blurred}>{view?.path}</SensText></span>
            <span className="text-gray-500 font-semibold">出口</span>
            <span className="font-mono"><SensText blurred={blurred}>{chain.exit_host}:{chain.exit_port}</SensText></span>
          </div>
        </div>
      </div>

      {/* Hop status */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">各跳状态</h3></div>
        <table className="tbl">
          <thead><tr><th className="w-10">#</th><th>节点</th><th>模式</th><th>监听</th><th>目标</th><th>状态</th><th className="text-right">操作</th></tr></thead>
          <tbody>
            {hops.map(h => {
              const node = node_by_id?.[h.node_id]
              const fw = fw_by_node?.[h.node_id]
              return (
                <tr key={h.position}>
                  <td className="font-mono text-xs text-gray-400">{h.position + 1}</td>
                  <td className="font-semibold">{node?.name || `#${h.node_id}`}</td>
                  <td><ModeBadge mode={h.mode} /></td>
                  <td className="font-mono">:{h.listen_port}</td>
                  <td className="font-mono">{fw ? `${fw.target_ip}:${fw.target_port}` : ''}</td>
                  <td><HopStatus node={node} /></td>
                  <td className="text-right">
                    <ReallocateForm chainId={chain.id} position={h.position} onDone={load} />
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {/* Edit chain */}
      <EditChainCard chain={chain} hops={hops} nodes={nodes} onDone={load} />

      <Link to="/chains" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline mt-5">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回链路列表
      </Link>
    </Layout>
  )
}

function HopStatus({ node }) {
  if (!node) return null
  const err = node.last_error?.Valid ? node.last_error.String : null
  if (err) return <Badge color="red">{err}</Badge>
  if (node.online === 1) return <Badge color="green">在线</Badge>
  return <Badge color="amber">离线/待同步</Badge>
}

function ReallocateForm({ chainId, position, onDone }) {
  const [port, setPort] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/chains/${chainId}/hops/${position}/reallocate`, { port: port ? Number(port) : undefined })
      toast('端口已重分配')
      setPort('')
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="1" max="65535" value={port} onChange={e => setPort(e.target.value)} placeholder="随机" style={{ width: 90, height: 28, fontSize: 12, padding: '0 6px' }} />
      <button type="submit" disabled={loading} className="btn-secondary text-xs">换端口</button>
    </form>
  )
}

function EditChainCard({ chain, hops: initHops, nodes, onDone }) {
  const [name, setName] = useState(chain.name)
  const [proto, setProto] = useState(chain.proto)
  const [exit, setExit] = useState(`${chain.exit_host}:${chain.exit_port}`)
  const [entryPort, setEntryPort] = useState(chain.entry_listen_port > 0 ? String(chain.entry_listen_port) : '')
  const [hops, setHops] = useState(initHops.map(h => ({ node_id: String(h.node_id), mode: h.mode })))
  const [saving, setSaving] = useState(false)
  const toast = useToast()

  const addHop = () => setHops(h => [...h, { node_id: '', mode: 'userspace' }])
  const removeHop = (i) => setHops(h => h.filter((_, j) => j !== i))
  const setHop = (i, k, v) => setHops(h => h.map((hop, j) => j === i ? { ...hop, [k]: v } : hop))
  const moveHop = (i, dir) => {
    setHops(h => {
      const arr = [...h]
      const j = i + dir
      if (j < 0 || j >= arr.length) return arr;
      [arr[i], arr[j]] = [arr[j], arr[i]]
      return arr
    })
  }

  const submit = async (e) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.post(`/chains/${chain.id}`, {
        name, proto, exit,
        entry_port: entryPort ? Number(entryPort) : undefined,
        hops: hops.map(h => ({ node_id: Number(h.node_id), mode: h.mode })),
      })
      toast('已保存并重下发')
      onDone()
    } catch (err) { toast(err.message) } finally { setSaving(false) }
  }

  return (
    <div className="card mb-5">
      <div className="card-header"><h3 className="text-sm font-bold">编辑链路</h3></div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-4">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center max-w-2xl">
            <label className="fl">名称</label>
            <input className="input-field" value={name} onChange={e => setName(e.target.value)} required />
            <label className="fl">协议</label>
            <select className="input-field" value={proto} onChange={e => setProto(e.target.value)} style={{ maxWidth: 200 }}>
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
              <option value="tcp+udp">TCP+UDP</option>
            </select>
            <label className="fl">出口</label>
            <input className="input-field font-mono" value={exit} onChange={e => setExit(e.target.value)} required />
            <label className="fl">入口端口</label>
            <input className="input-field font-mono" type="number" min="1" max="65535" value={entryPort} onChange={e => setEntryPort(e.target.value)} placeholder="留空自动分配" style={{ maxWidth: 200 }} />
          </div>

          <div>
            <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3 mt-3">中继节点（按顺序）</div>
            <div className="space-y-2.5 max-w-3xl">
              {hops.map((hop, i) => (
                <HopRow key={i} hop={hop} nodes={nodes} proto={proto}
                  onSet={(k, v) => setHop(i, k, v)}
                  onMove={(dir) => moveHop(i, dir)}
                  onRemove={() => removeHop(i)} />
              ))}
            </div>
          </div>

          <div className="flex items-center gap-3 pt-4 border-t border-gray-100 max-w-3xl">
            <button type="button" onClick={addHop} className="btn-secondary text-xs">+ 添加一跳</button>
            <button type="submit" disabled={saving} className="btn-primary">保存并重下发</button>
          </div>
        </form>
      </div>
    </div>
  )
}
