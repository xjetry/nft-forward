import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { fmtGB } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, ProtoBadge, Modal, SensText, CopyText } from '../../components/ui'

export default function MyRules() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [search, setSearch] = useState('')
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get('/my/rules').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { rules = [], nodes = [], node_by_id = {} } = data || {}

  const deleteRule = async (rule) => {
    if (!confirm(`确认删除规则「${rule.name}」？`)) return
    try { await api.del(`/my/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  const q = search.trim().toLowerCase()
  const filtered = !q ? rules : rules.filter(r => {
    const node = node_by_id?.[r.node_id]
    return [r.name, node?.name, r.path, r.entry].some(v => (v || '').toLowerCase().includes(q))
  })

  return (
    <Layout>
      {/* Page header */}
      <div className="flex items-baseline gap-2.5 mb-4">
        <h1 className="m-0 text-lg font-bold tracking-[-0.01em]">我的规则</h1>
        <span className="text-[13px] text-[#9aa4b2]">共 {rules.length} 条</span>
      </div>

      <section className="max-w-[1320px] bg-white border border-[#e6e9ee] rounded-2xl shadow-[0_1px_2px_rgba(16,24,40,0.04)] overflow-hidden">
        {/* Toolbar */}
        <div className="flex items-center gap-3.5 px-[22px] py-4 border-b border-[#eef0f3]">
          <div className="relative flex-1 max-w-[360px]">
            <svg className="w-4 h-4 absolute left-3 top-1/2 -translate-y-1/2 text-[#9aa4b2] pointer-events-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="11" cy="11" r="7" /><path d="m21 21-4.3-4.3" /></svg>
            <input value={search} onChange={e => setSearch(e.target.value)} placeholder="搜索规则名称、节点、目标…"
              className="w-full text-[13.5px] pl-9 pr-3 py-[9px] border border-[#d7dce3] rounded-[10px] outline-none text-[#1f2733] focus:border-blue-600 focus:ring-3 focus:ring-blue-600/10 transition-colors" />
          </div>
          <button onClick={() => setShowCreate(true)} className="ml-auto inline-flex items-center gap-1.5 text-[13.5px] font-semibold text-white bg-blue-600 hover:bg-blue-700 border-0 px-[18px] py-[9px] rounded-[10px] cursor-pointer transition-colors">＋ 创建规则</button>
        </div>

        {/* Table */}
        {rules.length === 0 ? (
          <Empty title="暂无规则" desc="点击右上角「创建规则」开始。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配规则" desc="试试别的关键词。" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th>名称</th>
                <th>节点</th>
                <th>协议</th>
                <th>路径</th>
                <th>入口</th>
                <th className="text-right">流量</th>
                <th className="text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map(r => {
                const node = node_by_id?.[r.node_id]
                return (
                  <tr key={r.id}>
                    <td className="font-bold text-[#1f2733]">{r.name}</td>
                    <td className="text-[#39424f]">{node?.name || `#${r.node_id}`}</td>
                    <td><ProtoBadge proto={r.proto} /></td>
                    <td className="font-mono text-xs text-[#39424f] max-w-[280px] truncate"><SensText blurred={blurred}>{r.path || '--'}</SensText></td>
                    <td className="font-mono text-xs">
                      {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
                    </td>
                    <td className="text-right font-mono text-xs text-gray-400">{fmtGB(r.total_bytes)}</td>
                    <td className="text-right whitespace-nowrap">
                      <button onClick={() => deleteRule(r)} className="btn-danger-sm text-xs">删除</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </section>

      <CreateMyRuleModal open={showCreate} onClose={() => setShowCreate(false)} nodes={nodes} onDone={() => { setShowCreate(false); load() }} />
    </Layout>
  )
}

function CreateMyRuleModal({ open, onClose, nodes, onDone }) {
  const [form, setForm] = useState({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => {
    setForm(f => {
      const next = { ...f, [k]: v }
      if (k === 'node_id') {
        const n = (nodes || []).find(nd => String(nd.id) === v)
        if (n?.node_type === 'composite' && next.proto !== 'tcp') {
          next.proto = 'tcp'
        }
      }
      return next
    })
  }

  const selectedNode = nodes?.find(n => String(n.id) === form.node_id)
  const isComposite = selectedNode?.node_type === 'composite'

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/my/rules', {
        node_id: Number(form.node_id),
        name: form.name,
        proto: form.proto,
        exit: form.exit,
        comment: form.comment || undefined,
      })
      toast('规则已创建')
      setForm({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '' })
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="创建规则">
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点</label>
          <select className="input-field" value={form.node_id} onChange={e => set('node_id', e.target.value)} required>
            <option value="">-- 选择已授权节点 --</option>
            {(nodes || []).map(n => (
              <option key={n.id} value={n.id}>{n.name}</option>
            ))}
          </select>
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          {isComposite ? (
            <input className="input-field" value="TCP" disabled style={{ maxWidth: 200 }} />
          ) : (
            <select className="input-field" value={form.proto} onChange={e => set('proto', e.target.value)} style={{ maxWidth: 200 }}>
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
              <option value="tcp+udp">TCP+UDP</option>
            </select>
          )}
          <label className="fl">出口</label>
          <input className="input-field font-mono" value={form.exit} onChange={e => set('exit', e.target.value)} required placeholder="host:port" />
          <label className="fl">备注 <span className="text-gray-400 font-normal text-xs">(可选)</span></label>
          <input className="input-field" value={form.comment} onChange={e => set('comment', e.target.value)} placeholder="备注" />
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
          <button type="submit" disabled={loading} className="btn-primary">创建规则</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
