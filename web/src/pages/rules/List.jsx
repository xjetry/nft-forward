import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, Modal, SensText, CopyText } from '../../components/ui'

export default function RulesList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const toast = useToast()
  const blurred = useBlur()
  const navigate = useNavigate()

  const load = () => {
    setLoading(true)
    api.get('/rules').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { rules = [], nodes = [] } = data || {}
  const nodeMap = {}
  nodes.forEach(n => { nodeMap[n.id] = n })

  const deleteRule = async (rule) => {
    if (!confirm(`确认删除规则「${rule.name}」？`)) return
    try { await api.del(`/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  return (
    <Layout>
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">转发规则</h3>
          <span className="text-xs text-gray-400">{rules.length} 条</span>
          <button onClick={() => setShowCreate(true)} className="btn-primary text-xs ml-auto">+ 创建规则</button>
        </div>
        {rules.length ? (
          <table className="tbl">
            <thead>
              <tr>
                <th className="w-12">ID</th>
                <th>名称</th>
                <th>节点</th>
                <th>协议</th>
                <th>路径</th>
                <th>入口</th>
                <th>所有者</th>
                <th className="text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {rules.map(r => {
                const node = nodeMap[r.node_id]
                return (
                  <tr key={r.id} className="cursor-pointer" onClick={() => navigate(`/rules/${r.id}`)}>
                    <td className="font-mono text-xs text-gray-400">#{r.id}</td>
                    <td className="font-semibold">{r.name}</td>
                    <td>
                      <span className="font-mono text-gray-600">{node?.name || `#${r.node_id}`}</span>
                    </td>
                    <td><ProtoBadge proto={r.proto} /></td>
                    <td className="font-mono text-xs text-gray-500 max-w-[200px] truncate"><SensText blurred={blurred}>{r.path || '--'}</SensText></td>
                    <td className="font-mono text-xs" onClick={e => e.stopPropagation()}>
                      {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
                    </td>
                    <td className="text-gray-500">{r.owner_name || '--'}</td>
                    <td className="text-right whitespace-nowrap" onClick={e => e.stopPropagation()}>
                      <Link to={`/rules/${r.id}`} className="btn-secondary text-xs mr-1.5">详情</Link>
                      <button onClick={() => deleteRule(r)} className="btn-danger-sm text-xs">删除</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        ) : <Empty title="暂无规则" desc="点击上方「创建规则」添加。" />}
      </div>

      <CreateRuleModal open={showCreate} onClose={() => setShowCreate(false)} nodes={nodes} onDone={() => { setShowCreate(false); load() }} />
    </Layout>
  )
}

export function NodeTypeBadge({ type }) {
  if (type === 'composite') return <Badge color="violet">组合</Badge>
  if (type === 'self') return <Badge color="blue">自身</Badge>
  return <Badge color="green">单点</Badge>
}

function CreateRuleModal({ open, onClose, nodes, onDone }) {
  const [form, setForm] = useState({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const selectedNode = nodes.find(n => String(n.id) === form.node_id)
  const isComposite = selectedNode?.node_type === 'composite'

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/rules', {
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
            <option value="">-- 选择节点 --</option>
            {nodes.map(n => (
              <option key={n.id} value={n.id}>{n.name}</option>
            ))}
          </select>
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          <select className="input-field" value={form.proto} onChange={e => set('proto', e.target.value)} style={{ maxWidth: 200 }}>
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
            {!isComposite && <option value="tcp+udp">TCP+UDP</option>}
          </select>
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
