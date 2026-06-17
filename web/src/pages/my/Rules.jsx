import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Modal, useConfirm, Select } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton } from '../../components/page'
import { RulesTable } from '../../components/RulesTable'

export default function MyRules() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [search, setSearch] = useState('')
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()

  const load = () => {
    setLoading(true)
    api.get('/my/rules').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { rules = [], nodes = [], node_by_id = {} } = data || {}

  const deleteRule = async (rule) => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/my/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  const q = search.trim().toLowerCase()
  const filtered = !q ? rules : rules.filter(r => {
    const node = node_by_id?.[r.node_id]
    const exit = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : ''
    return [r.name, node?.name, r.entry, exit].some(v => (v || '').toLowerCase().includes(q))
  })

  return (
    <Layout>
      <PageHeader title="我的规则" count={rules.length} />

      <Panel>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索规则名称、节点、目标…" />
          <ToolbarButton onClick={() => setShowCreate(true)}>＋ 创建规则</ToolbarButton>
        </PanelToolbar>

        {rules.length === 0 ? (
          <Empty title="暂无规则" desc="点击右上角「创建规则」开始。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配规则" desc="试试别的关键词。" />
        ) : (
          <RulesTable variant="my" rules={filtered} nodeMap={node_by_id} blurred={blurred} onDelete={deleteRule} />
        )}
      </Panel>

      <CreateMyRuleModal open={showCreate} onClose={() => setShowCreate(false)} nodes={nodes} onDone={() => { setShowCreate(false); load() }} />
    </Layout>
  )
}

function CreateMyRuleModal({ open, onClose, nodes, onDone }) {
  const [form, setForm] = useState({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

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
          <Select value={form.node_id} onChange={v => set('node_id', v)} placeholder="-- 选择已授权节点 --"
            options={(nodes || []).map(n => ({ value: n.id, label: n.name }))} />
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
          <label className="fl">出口</label>
          <input className="input-field font-mono" value={form.exit} onChange={e => set('exit', e.target.value)} required placeholder="host:port" />
          <label className="fl">备注 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field" value={form.comment} onChange={e => set('comment', e.target.value)} placeholder="备注" />
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-line-soft">
          <button type="submit" disabled={loading} className="btn-primary">创建规则</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
