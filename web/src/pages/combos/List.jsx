import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, Modal, ProbeButton } from '../../components/ui'

export default function ComboList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [editCombo, setEditCombo] = useState(null)
  const toast = useToast()

  const load = () => {
    setLoading(true)
    api.get('/combos').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const deleteCombo = async (c) => {
    if (!confirm('删除该组合通道？')) return
    try { await api.del(`/combos/${c.combo.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { combos = [], tunnels = [], node_by_id = {} } = data || {}

  return (
    <Layout>
      {/* Combo table */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">所有组合通道</h3>
          <span className="text-xs text-gray-400">{combos.length} 条</span>
        </div>
        {combos.length ? (
          <table className="tbl">
            <thead><tr><th className="w-12">ID</th><th>名称</th><th>路径</th><th>跳数</th><th>测试</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {combos.map(c => (
                <tr key={c.combo.id}>
                  <td className="font-mono text-xs text-gray-400">{c.combo.id}</td>
                  <td className="font-semibold">{c.combo.name}</td>
                  <td className="font-mono text-xs text-gray-500">{c.path}</td>
                  <td className="font-mono">{c.hops?.length || 0}</td>
                  <td>
                    {c.node_hosts?.length ? (
                      <ProbeComboButton nodeHosts={c.node_hosts} />
                    ) : <span className="text-gray-300">--</span>}
                  </td>
                  <td className="text-right whitespace-nowrap">
                    <button onClick={() => setEditCombo(c)} className="btn-secondary text-xs mr-1.5">编辑</button>
                    <button onClick={() => deleteCombo(c)} className="btn-danger-sm text-xs">删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="尚无组合通道" />}
      </div>

      {/* Create form */}
      <CreateComboCard tunnels={tunnels} nodeById={node_by_id} onDone={load} />

      {/* Edit modal */}
      {editCombo && (
        <EditComboModal combo={editCombo} tunnels={tunnels} nodeById={node_by_id}
          onClose={() => setEditCombo(null)} onDone={() => { setEditCombo(null); load() }} />
      )}
    </Layout>
  )
}

function ProbeComboButton({ nodeHosts }) {
  const [state, setState] = useState('idle')
  const [result, setResult] = useState('')
  const probe = () => {
    setState('loading')
    Promise.all(nodeHosts.map(t => fetch(`/api/probe?target=${encodeURIComponent(t)}`).then(r => r.json())))
      .then(results => {
        const parts = results.map(d => d.ok ? d.latency_ms + 'ms' : 'x')
        const allOk = results.every(d => d.ok)
        setState(allOk ? 'ok' : 'fail')
        setResult(parts.join(' -> '))
      }).catch(() => { setState('fail'); setResult('请求失败') })
  }
  return (
    <span className="inline-flex items-center gap-2">
      <button onClick={probe} disabled={state === 'loading'}
        className="text-[11px] px-2 py-0.5 rounded border border-gray-200 bg-white text-gray-500 hover:border-blue-500 hover:text-blue-600 disabled:opacity-50">
        {state === 'loading' ? '测试中...' : '测通'}
      </button>
      {state === 'ok' && <span className="text-[11px] text-green-700 font-semibold">{result}</span>}
      {state === 'fail' && <span className="text-[11px] text-red-600">{result}</span>}
    </span>
  )
}

function ComboHopRow({ hop, tunnels, onSet, onMove, onRemove }) {
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
      <select className="input-field flex-1" value={hop.tunnel_id || ''} onChange={e => onSet('tunnel_id', e.target.value)} required>
        <option value="">-- 选择通道 --</option>
        {tunnels.map(t => <option key={t.id} value={t.id}>{t._label}</option>)}
      </select>
      <select className="input-field" value={hop.mode || 'userspace'} onChange={e => onSet('mode', e.target.value)} style={{ width: 200 }}>
        <option value="userspace">用户态(split-TCP)</option>
        <option value="kernel">内核态(零拷贝)</option>
      </select>
      <button type="button" onClick={onRemove} className="btn-danger-sm text-xs">删除</button>
    </div>
  )
}

function labeledTunnels(tunnels, nodeById) {
  return tunnels.map(t => {
    const n = nodeById?.[t.node_id]
    return { ...t, _label: `${t.name} @ ${n ? n.name : '#' + t.node_id}` }
  })
}

function CreateComboCard({ tunnels, nodeById, onDone }) {
  const [name, setName] = useState('')
  const [hops, setHops] = useState([{ tunnel_id: '', mode: 'userspace' }, { tunnel_id: '', mode: 'userspace' }])
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const lt = labeledTunnels(tunnels, nodeById)

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

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/combos', {
        name,
        hops: hops.map(h => ({ tunnel_id: Number(h.tunnel_id), mode: h.mode })),
      })
      toast('组合通道已创建')
      setName(''); setHops([{ tunnel_id: '', mode: 'userspace' }, { tunnel_id: '', mode: 'userspace' }])
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  if (!tunnels.length) return (
    <div className="card">
      <div className="card-header"><h3 className="text-sm font-bold">新建组合通道</h3></div>
      <Empty title="请先添加通道" />
    </div>
  )

  return (
    <div className="card">
      <div className="card-header"><h3 className="text-sm font-bold">新建组合通道</h3></div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-4">
          <div className="grid grid-cols-[150px_1fr] gap-4 items-center max-w-2xl">
            <label className="fl">名称</label>
            <input className="input-field" value={name} onChange={e => setName(e.target.value)} required placeholder="例如 jp-direct" />
          </div>

          <div>
            <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3 mt-2">通道序列（按顺序）</div>
            <div className="space-y-2.5 max-w-3xl">
              {hops.map((hop, i) => (
                <ComboHopRow key={i} hop={hop} tunnels={lt}
                  onSet={(k, v) => setHop(i, k, v)}
                  onMove={(dir) => moveHop(i, dir)}
                  onRemove={() => removeHop(i)} />
              ))}
            </div>
            <p className="text-xs text-gray-400 mt-2.5">至少添加 2 个通道，按中继顺序排列。租户选择该组合后将自动展开为多跳链路。</p>
          </div>

          <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
            <button type="button" onClick={addHop} className="btn-secondary text-xs">+ 添加通道</button>
            <button type="submit" disabled={loading} className="btn-primary">创建组合</button>
          </div>
        </form>
      </div>
    </div>
  )
}

function EditComboModal({ combo, tunnels, nodeById, onClose, onDone }) {
  const [name, setName] = useState(combo.combo.name)
  const [hops, setHops] = useState((combo.hops || []).map(h => ({ tunnel_id: String(h.tunnel_id), mode: h.mode })))
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const lt = labeledTunnels(tunnels, nodeById)

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

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/combos/${combo.combo.id}`, {
        name,
        hops: hops.map(h => ({ tunnel_id: Number(h.tunnel_id), mode: h.mode })),
      })
      toast('已保存')
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={true} onClose={onClose} title="编辑组合通道" wide>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">名称</label>
          <input className="input-field" value={name} onChange={e => setName(e.target.value)} required />
        </div>

        <div>
          <div className="text-xs font-bold text-gray-400 uppercase tracking-wider mb-3 mt-2">通道序列（按顺序）</div>
          <div className="space-y-2.5">
            {hops.map((hop, i) => (
              <ComboHopRow key={i} hop={hop} tunnels={lt}
                onSet={(k, v) => setHop(i, k, v)}
                onMove={(dir) => moveHop(i, dir)}
                onRemove={() => removeHop(i)} />
            ))}
          </div>
        </div>

        <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
          <button type="button" onClick={addHop} className="btn-secondary text-xs">+ 添加通道</button>
          <button type="submit" disabled={loading} className="btn-primary">保存</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
