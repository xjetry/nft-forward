import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Modal, ProtoBadge, SensText, CopyText, ProbeChainButton } from '../../components/ui'

export default function ChainList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showAdd, setShowAdd] = useState(false)
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get('/chains').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const deleteChain = async (c) => {
    if (!confirm('删除该链路？会删除其所有跳的转发。')) return
    try { await api.del(`/chains/${c.chain.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { chains = [], nodes = [] } = data || {}

  return (
    <Layout>
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">所有链路</h3>
          <span className="text-xs text-gray-400">{chains.length} 条中继链路</span>
          {nodes.length > 0 && (
            <button onClick={() => setShowAdd(true)} className="btn-primary text-xs ml-auto">+ 新建链路</button>
          )}
        </div>
        {chains.length ? (
          <table className="tbl">
            <thead><tr><th className="w-12">ID</th><th>名称</th><th>用户</th><th>协议</th><th>路径</th><th>入口（可复制）</th><th>测试</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {chains.map(c => (
                <tr key={c.chain.id}>
                  <td className="font-mono text-xs text-gray-400">#{c.chain.id}</td>
                  <td className="font-semibold">{c.chain.name}</td>
                  <td className="text-gray-500">{c.owner_name || '--'}</td>
                  <td><ProtoBadge proto={c.chain.proto} /></td>
                  <td className="font-mono text-xs text-gray-500"><SensText blurred={blurred}>{c.path}</SensText></td>
                  <td className="font-mono font-semibold">
                    {c.entry && c.entry !== '--' ? (
                      <SensText blurred={blurred}>
                        <CopyText text={c.entry}>{c.entry}</CopyText>
                      </SensText>
                    ) : <span className="text-gray-300">--</span>}
                  </td>
                  <td>{c.entry && c.entry !== '--' ? <ProbeChainButton chainId={c.chain.id} /> : <span className="text-gray-300">--</span>}</td>
                  <td className="text-right whitespace-nowrap">
                    <Link to={`/chains/${c.chain.id}`} className="btn-secondary text-xs mr-1.5">详情</Link>
                    <button onClick={() => deleteChain(c)} className="btn-danger-sm text-xs">删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚无链路" desc="点击上方「新建链路」创建。" />}
      </div>

      {nodes.length > 0 && (
        <NewChainModal open={showAdd} onClose={() => setShowAdd(false)} nodes={nodes} onDone={() => { setShowAdd(false); load() }} />
      )}
    </Layout>
  )
}

function NewChainModal({ open, onClose, nodes, onDone }) {
  const [name, setName] = useState('')
  const [proto, setProto] = useState('tcp')
  const [exit, setExit] = useState('')
  const [entryPort, setEntryPort] = useState('')
  const [hops, setHops] = useState([{ node_id: '', mode: 'userspace' }])
  const [loading, setLoading] = useState(false)
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
    setLoading(true)
    try {
      await api.post('/chains', {
        name, proto, exit,
        entry_port: entryPort ? Number(entryPort) : undefined,
        hops: hops.map(h => ({ node_id: Number(h.node_id), mode: h.mode })),
      })
      toast('链路已创建')
      setName(''); setProto('tcp'); setExit(''); setEntryPort(''); setHops([{ node_id: '', mode: 'userspace' }])
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="新建中继链路" wide>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required placeholder="如 vless-seednet" />
          <label className="fl">协议</label>
          <select className="input-field" value={proto} onChange={e => setProto(e.target.value)} style={{ maxWidth: 200 }}>
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </select>
          <label className="fl">出口</label>
          <input className="input-field font-mono" value={exit} onChange={e => setExit(e.target.value)} required placeholder="目标 host:port" />
          <label className="fl">入口端口</label>
          <input className="input-field font-mono" type="number" min="1" max="65535" value={entryPort} onChange={e => setEntryPort(e.target.value)} placeholder="留空自动分配" style={{ maxWidth: 200 }} />
        </div>

        <div>
          <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3 mt-2">中继节点（按顺序）</div>
          <div className="space-y-2.5">
            {hops.map((hop, i) => (
              <HopRow key={i} hop={hop} nodes={nodes} proto={proto}
                onSet={(k, v) => setHop(i, k, v)}
                onMove={(dir) => moveHop(i, dir)}
                onRemove={() => removeHop(i)} />
            ))}
          </div>
        </div>

        <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
          <button type="button" onClick={addHop} className="btn-secondary text-xs">+ 添加一跳</button>
          <button type="submit" disabled={loading} className="btn-primary">创建链路</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}

export function HopRow({ hop, nodes, tunnels, proto, onSet, onMove, onRemove, useTunnel }) {
  const isUdp = proto === 'udp'
  return (
    <div className="flex items-center gap-3">
      <div className="flex gap-1">
        <button type="button" onClick={() => onMove(-1)} className="w-7 h-7 border border-gray-200 rounded grid place-items-center text-gray-400 hover:text-gray-600 hover:bg-gray-50">
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
        </button>
        <button type="button" onClick={() => onMove(1)} className="w-7 h-7 border border-gray-200 rounded grid place-items-center text-gray-400 hover:text-gray-600 hover:bg-gray-50">
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 5v14M5 12l7 7 7-7"/></svg>
        </button>
      </div>
      {useTunnel ? (
        <select className="input-field flex-1" value={hop.tunnel_id || ''} onChange={e => onSet('tunnel_id', e.target.value)} required>
          <option value="">-- 选择通道 --</option>
          {(tunnels || []).map(t => (
            <option key={t.id} value={t.id} disabled={t._disabled}>{t._label || t.name}</option>
          ))}
        </select>
      ) : (
        <select className="input-field flex-1" value={hop.node_id || ''} onChange={e => onSet('node_id', e.target.value)} required>
          <option value="">-- 选择节点 --</option>
          {(nodes || []).map(n => (
            <option key={n.id} value={n.id} disabled={!n.relay_host}>
              {n.name}{!n.relay_host ? '（未设中继地址）' : ` (${n.relay_host})`}
            </option>
          ))}
        </select>
      )}
      <select className="input-field" value={hop.mode || 'userspace'} onChange={e => onSet('mode', e.target.value)} style={{ width: 200 }}>
        <option value="userspace" disabled={isUdp}>用户态(split-TCP)</option>
        <option value="kernel">内核态(零拷贝)</option>
      </select>
      <button type="button" onClick={onRemove} className="btn-danger-sm text-xs">删除</button>
    </div>
  )
}
