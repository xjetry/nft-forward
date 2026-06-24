import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, useConfirm } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton } from '../../components/page'
import { RulesTable } from '../../components/RulesTable'
import { RuleFormModal, copyInitial, ruleToForm } from '../../components/RuleFormModal'

export default function MyRules() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [createInitial, setCreateInitial] = useState(null)
  const [editRule, setEditRule] = useState(null)
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
  const openCreate = () => { setCreateInitial(null); setCreateOpen(true) }
  const copyRule = (rule) => { setCreateInitial(copyInitial(rule)); setCreateOpen(true) }

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
          <ToolbarButton onClick={openCreate}>＋ 创建规则</ToolbarButton>
        </PanelToolbar>

        {rules.length === 0 ? (
          <Empty title="暂无规则" desc="点击右上角「创建规则」开始。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配规则" desc="试试别的关键词。" />
        ) : (
          <RulesTable variant="my" rules={filtered} nodeMap={node_by_id} blurred={blurred}
            onDelete={deleteRule} onEdit={setEditRule} onCopy={copyRule} />
        )}
      </Panel>

      <RuleFormModal
        open={createOpen} onClose={() => setCreateOpen(false)} title="创建规则" submitLabel="创建规则"
        nodes={nodes} initial={createInitial}
        onSubmit={async (form) => {
          await api.post('/my/rules', {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            exit: form.exit, comment: form.comment || undefined,
          })
          toast('规则已创建'); setCreateOpen(false); load()
        }} />

      <RuleFormModal
        open={!!editRule} onClose={() => setEditRule(null)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} initial={editRule ? ruleToForm(editRule) : null}
        onSubmit={async (form) => {
          await api.put(`/my/rules/${editRule.id}`, {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            exit: form.exit, comment: form.comment || undefined,
          })
          toast('已保存并重下发'); setEditRule(null); load()
        }} />
    </Layout>
  )
}
