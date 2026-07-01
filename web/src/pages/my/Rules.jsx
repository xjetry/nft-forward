import { useState, useEffect, useMemo } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur, useUser } from '../../components/Layout'
import { Loading, Empty, useConfirm } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton, TableScroll } from '../../components/page'
import { RulesTable } from '../../components/RulesTable'
import { RuleFormModal, copyInitial } from '../../components/RuleFormModal'
import { parseURIs, landingIndex, rewriteEndpoint, splitEndpoint, mergeLanding, loadLocalURIs, saveLocalURIs, loadSubCache, fetchNodeRoles, loadLocalRoles, nodeHasRole, ROLE_LANDING } from '../../lib/landing'

export default function MyRules() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [serverLanding, setServerLanding] = useState([])
  const [createOpen, setCreateOpen] = useState(false)
  const [createInitial, setCreateInitial] = useState(null)
  const [search, setSearch] = useState('')
  const navigate = useNavigate()
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()
  const { user } = useUser()

  // The user's own proxy URIs live only in localStorage (never sent to the
  // server). Parse them here to both feed the create picker and resolve a
  // client-side relay URI for rules whose exit matches one of them.
  const [localVer, setLocalVer] = useState(0)
  const [nodeRoles, setNodeRoles] = useState({})
  useEffect(() => {
    fetchNodeRoles().then(sr => setNodeRoles({ ...sr, ...loadLocalRoles(user?.username) }))
  }, [user])
  const localNodes = useMemo(() => {
    const isLanding = n => nodeHasRole(nodeRoles, n, ROLE_LANDING)
    const manual = parseURIs(loadLocalURIs(user?.username)).filter(isLanding)
    const sub = loadSubCache(user?.username).filter(isLanding)
    return mergeLanding(manual, sub)
  }, [user, localVer, nodeRoles])
  const localIdx = useMemo(() => landingIndex(localNodes), [localNodes])

  const addProxyURI = (uri) => {
    if (!user?.username) return
    const existing = loadLocalURIs(user.username)
    const lines = existing.split('\n').map(l => l.trim()).filter(Boolean)
    if (lines.includes(uri.trim())) return
    lines.push(uri.trim())
    saveLocalURIs(user.username, lines.join('\n'))
    setLocalVer(v => v + 1)
  }

  const load = () => {
    setLoading(true)
    api.get('/my/rules').then(setData).catch(console.error).finally(() => setLoading(false))
    api.get('/my/landing-nodes').then(d => setServerLanding(d?.nodes || [])).catch(console.error)
  }
  useEffect(load, [])

  if (loading) return <Layout><Loading /></Layout>

  const { rules = [], nodes = [], node_by_id = {}, show_rate } = data || {}

  // Filter server-assigned nodes by global role table — only landing-marked ones
  // appear in the exit picker (unconfigured/direct ones are excluded).
  const serverLandingFiltered = serverLanding.filter(n => nodeHasRole(nodeRoles, n, ROLE_LANDING))
  const landingNodes = mergeLanding(localNodes, serverLandingFiltered)

  const allLandingIdx = landingIndex(landingNodes)

  const enrich = (r) => {
    const key = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : null
    if (key && allLandingIdx.has(key) && r.entry) {
      const ep = splitEndpoint(r.entry)
      const node = allLandingIdx.get(key)
      const relay = ep && rewriteEndpoint(node.uri, ep.host, ep.port)
      if (relay) return { ...r, exit_kind: 'landing', landing_name: node.name, relay_uri: relay }
    }
    return r
  }

  const deleteRule = async (rule) => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/my/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message, 'error') }
  }
  const openCreate = () => { setCreateInitial(null); setCreateOpen(true) }
  const copyRule = (rule) => { setCreateInitial(copyInitial(rule)); setCreateOpen(true) }

  const q = search.trim().toLowerCase()
  const enriched = rules.map(enrich)
  const filtered = !q ? enriched : enriched.filter(r => {
    const node = node_by_id?.[r.node_id]
    const exit = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : ''
    return [r.name, node?.name, r.entry, exit].some(v => (v || '').toLowerCase().includes(q))
  })

  return (
    <Layout>
      <div className="h-full flex flex-col">
      <PageHeader title="我的规则" count={rules.length} />

      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索规则名称、节点、目标…" />
          <ToolbarButton onClick={openCreate}>＋ 创建规则</ToolbarButton>
        </PanelToolbar>

        {rules.length === 0 ? (
          <Empty title="暂无规则" desc="点击右上角「创建规则」开始。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配规则" desc="试试别的关键词。" />
        ) : (
          <TableScroll>
            <RulesTable variant="my" rules={filtered} nodeMap={node_by_id} blurred={blurred}
              onDelete={deleteRule} onCopy={copyRule} onRowClick={r => navigate(`/my/rules/${r.id}`)} />
          </TableScroll>
        )}
      </Panel>
      </div>

      <RuleFormModal
        open={createOpen} onClose={() => setCreateOpen(false)} title="创建规则" submitLabel="创建规则"
        nodes={nodes} landingNodes={landingNodes} initial={createInitial} onAddProxyURI={addProxyURI} showRate={show_rate}
        onSubmit={async (form) => {
          await api.post('/my/rules', {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
            comment: form.comment || undefined,
          })
          toast('规则已创建'); setCreateOpen(false); load()
        }} />

    </Layout>
  )
}
