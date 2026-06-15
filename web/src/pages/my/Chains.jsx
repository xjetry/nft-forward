import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Modal, ProtoBadge, SensText, CopyText, ProbeButton } from '../../components/ui'

export default function MyChains() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showAdd, setShowAdd] = useState(false)
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get('/my/chains').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const deleteChain = async (c) => {
    if (!confirm('删除该链路？')) return
    try { await api.post(`/my/chains/${c.chain.id}/delete`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { chains = [], tunnels = [], node_by_id = {} } = data || {}

  return (
    <Layout>
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">链路列表</h3>
          {tunnels?.length > 0 && (
            <button onClick={() => setShowAdd(true)} className="btn-primary text-xs ml-auto">+ 新建链路</button>
          )}
        </div>
        {chains.length ? (
          <table className="tbl">
            <thead><tr><th>名称</th><th>协议</th><th>路径</th><th>入口（可复制）</th><th>测试</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {chains.map(c => (
                <tr key={c.chain.id}>
                  <td className="font-semibold">{c.chain.name}</td>
                  <td><ProtoBadge proto={c.chain.proto} /></td>
                  <td className="font-mono text-xs text-gray-500">{c.path}</td>
                  <td className="font-mono font-semibold">
                    {c.entry && c.entry !== '--' ? (
                      <CopyText text={c.entry}>{c.entry}</CopyText>
                    ) : '--'}
                  </td>
                  <td>
                    {c.entry && c.entry !== '--' ? <ProbeButton target={c.entry} /> : <span className="text-gray-300">--</span>}
                  </td>
                  <td className="text-right">
                    <button onClick={() => deleteChain(c)} className="btn-danger-sm text-xs">删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚无链路" desc="点击上方「新建链路」创建。" />}
      </div>

      {tunnels?.length > 0 && (
        <NewMyChainModal open={showAdd} onClose={() => setShowAdd(false)} tunnels={tunnels} nodeById={node_by_id} onDone={() => { setShowAdd(false); load() }} />
      )}
    </Layout>
  )
}

function NewMyChainModal({ open, onClose, tunnels, nodeById, onDone }) {
  const [name, setName] = useState('')
  const [proto, setProto] = useState('tcp')
  const [exit, setExit] = useState('')
  const [entryPort, setEntryPort] = useState('')
  const [hops, setHops] = useState([{ tunnel_id: '', mode: 'userspace' }])
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const labeledTunnels = tunnels.map(t => {
    const n = nodeById?.[t.node_id]
    const disabled = n && !n.relay_host
    return {
      ...t,
      _label: `${t.name} @ ${n ? n.name : '--'}${disabled ? '（节点未设中继地址）' : ''}（${t.port_start}-${t.port_end}）`,
      _disabled: disabled,
    }
  })

  const addHop = () => setHops(h => [...h, { tunnel_id: '', mode: 'userspace' }])
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

  const isUdp = proto === 'udp'

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/my/chains', {
        name, proto, exit,
        entry_port: entryPort ? Number(entryPort) : undefined,
        hops: hops.map(h => ({ tunnel_id: Number(h.tunnel_id), mode: h.mode })),
      })
      toast('链路已创建')
      setName(''); setProto('tcp'); setExit(''); setEntryPort(''); setHops([{ tunnel_id: '', mode: 'userspace' }])
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="新建中继链路" wide>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required />
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
          <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3 mt-2">中继通道（按顺序）</div>
          <div className="space-y-2.5">
            {hops.map((hop, i) => (
              <div key={i} className="flex items-center gap-3">
                <div className="flex gap-1">
                  <button type="button" onClick={() => moveHop(i, -1)} className="w-7 h-7 border border-gray-200 rounded grid place-items-center text-gray-400 hover:text-gray-600 hover:bg-gray-50">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
                  </button>
                  <button type="button" onClick={() => moveHop(i, 1)} className="w-7 h-7 border border-gray-200 rounded grid place-items-center text-gray-400 hover:text-gray-600 hover:bg-gray-50">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 5v14M5 12l7 7 7-7"/></svg>
                  </button>
                </div>
                <select className="input-field flex-1" value={hop.tunnel_id} onChange={e => setHop(i, 'tunnel_id', e.target.value)} required>
                  <option value="">-- 选择通道 --</option>
                  {labeledTunnels.map(t => (
                    <option key={t.id} value={t.id} disabled={t._disabled}>{t._label}</option>
                  ))}
                </select>
                <select className="input-field" value={hop.mode} onChange={e => setHop(i, 'mode', e.target.value)} style={{ width: 200 }}>
                  <option value="userspace" disabled={isUdp}>用户态(split-TCP)</option>
                  <option value="kernel">内核态(零拷贝)</option>
                </select>
                <button type="button" onClick={() => removeHop(i)} className="btn-danger-sm text-xs">删除</button>
              </div>
            ))}
          </div>
          <p className="text-xs text-gray-400 mt-2.5">入口 IP:端口由系统自动生成。每一跳消耗对应通道一条转发配额。</p>
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
