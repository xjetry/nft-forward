import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, useConfirm } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton, TableScroll } from '../../components/page'
import { RulesTable } from '../../components/RulesTable'
import { RuleFormModal, copyInitial, ruleToForm } from '../../components/RuleFormModal'

export default function RulesList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [createInitial, setCreateInitial] = useState(null)
  const [editRule, setEditRule] = useState(null)
  const [users, setUsers] = useState([])
  const [selectedOwners, setSelectedOwners] = useState(new Set())
  const [search, setSearch] = useState('')
  const toast = useToast()
  const blurred = useBlur()
  const navigate = useNavigate()
  const confirm = useConfirm()

  const load = (ownerIDs) => {
    const ids = ownerIDs ?? selectedOwners
    setLoading(true)
    const params = ids.size > 0 ? `?owner_ids=${[...ids].join(',')}` : ''
    api.get(`/rules${params}`)
      .then(d => {
        setData(d)
        if (d.users?.length) setUsers(d.users)
      })
      .catch(console.error)
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { rules: allRules = [], nodes = [] } = data || {}
  const nodeMap = {}
  nodes.forEach(n => { nodeMap[n.id] = n })
  // Rules on a node flagged hidden are omitted from this list (the node config
  // is the single switch); they keep forwarding and stay reachable by URL.
  const rules = allRules.filter(r => !nodeMap[r.node_id]?.hidden)

  const deleteRule = async (rule) => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }
  const openCreate = () => { setCreateInitial(null); setCreateOpen(true) }
  const copyRule = (rule) => { setCreateInitial(copyInitial(rule)); setCreateOpen(true) }

  const q = search.trim().toLowerCase()
  const filtered = !q ? rules : rules.filter(r => {
    const node = nodeMap[r.node_id]
    const exit = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : ''
    return [r.name, node?.name, r.entry, exit, r.owner_name].some(v => (v || '').toLowerCase().includes(q))
  })

  return (
    <Layout>
      <div className="h-full flex flex-col">
      <PageHeader title="转发规则" count={rules.length} />

      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索规则名称、节点、目标…" />
          <ToolbarButton onClick={openCreate}>＋ 创建规则</ToolbarButton>
        </PanelToolbar>

        {users.length > 0 && (
          <div className="flex flex-wrap gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
            <button
              onClick={() => { const next = new Set(); setSelectedOwners(next); load(next) }}
              className={`px-2 py-0.5 rounded text-xs border transition-colors ${
                selectedOwners.size === 0
                  ? 'bg-blue-500 text-white border-blue-500'
                  : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}
            >全部</button>
            {users.map(u => (
              <button
                key={u.id}
                onClick={() => {
                  const next = new Set(selectedOwners)
                  if (next.has(u.id)) next.delete(u.id)
                  else next.add(u.id)
                  setSelectedOwners(next)
                  load(next)
                }}
                className={`px-2 py-0.5 rounded text-xs border transition-colors ${
                  selectedOwners.has(u.id)
                    ? 'bg-blue-500 text-white border-blue-500'
                    : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
                }`}
              >{u.username}</button>
            ))}
          </div>
        )}

        {rules.length === 0 ? (
          <Empty title="暂无规则" desc="点击右上角「创建规则」添加。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配规则" desc="试试别的关键词。" />
        ) : (
          <TableScroll>
            <RulesTable variant="admin" rules={filtered} nodeMap={nodeMap} blurred={blurred}
              onDelete={deleteRule} onEdit={setEditRule} onCopy={copyRule}
              onRowClick={r => navigate(`/rules/${r.id}`)} />
          </TableScroll>
        )}
      </Panel>
      </div>

      <RuleFormModal
        open={createOpen} onClose={() => setCreateOpen(false)} title="创建规则" submitLabel="创建规则"
        nodes={nodes} initial={createInitial}
        onSubmit={async (form) => {
          await api.post('/rules', {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            exit: form.exit, comment: form.comment || undefined,
          })
          toast('规则已创建'); setCreateOpen(false); load()
        }} />

      <RuleFormModal
        open={!!editRule} onClose={() => setEditRule(null)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} initial={editRule ? ruleToForm(editRule) : null}
        onSubmit={async (form) => {
          await api.put(`/rules/${editRule.id}`, {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            exit: form.exit, comment: form.comment || undefined,
          })
          toast('已保存并重下发'); setEditRule(null); load()
        }} />
    </Layout>
  )
}
