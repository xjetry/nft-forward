import { useState, useEffect, useRef, useMemo } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur, useUser } from '../../components/Layout'
import { Loading, Empty, useConfirm } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput, ToolbarButton, TableScroll } from '../../components/page'
import { RulesTable } from '../../components/RulesTable'
import { RuleFormModal, copyInitial, ruleToForm } from '../../components/RuleFormModal'
import { parseURIs, mergeLanding, landingIndex, rewriteEndpoint, splitEndpoint, loadLocalURIs, saveLocalURIs, loadSubCache, fetchNodeRoles, nodeHasRole, ROLE_LANDING } from '../../lib/landing'

export default function RulesList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [createInitial, setCreateInitial] = useState(null)
  const [editRule, setEditRule] = useState(null)
  const [users, setUsers] = useState([])
  // Filters live in the URL (not local state) so they survive navigating into a
  // rule's detail page and back — a plain useState resets on remount.
  const [searchParams, setSearchParams] = useSearchParams()
  const search = searchParams.get('q') || ''
  const selectedOwners = useMemo(() => new Set((searchParams.get('owners') || '').split(',').filter(Boolean).map(Number)), [searchParams])
  const selectedNodes = useMemo(() => new Set((searchParams.get('nodes') || '').split(',').filter(Boolean).map(Number)), [searchParams])
  const updateParams = (patch) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      for (const [k, v] of Object.entries(patch)) {
        if (v) next.set(k, v); else next.delete(k)
      }
      return next
    }, { replace: true })
  }
  const setSearch = (v) => updateParams({ q: v })
  const setSelectedOwners = (next) => updateParams({ owners: next.size ? [...next].join(',') : '' })
  const setSelectedNodes = (next) => updateParams({ nodes: next.size ? [...next].join(',') : '' })
  const toast = useToast()
  const blurred = useBlur()
  const navigate = useNavigate()
  const confirm = useConfirm()
  const { user } = useUser()

  const [localVer, setLocalVer] = useState(0)
  const [nodeRoles, setNodeRoles] = useState({})
  useEffect(() => { fetchNodeRoles().then(setNodeRoles) }, [])
  const localNodes = useMemo(() => {
    const isLanding = n => nodeHasRole(nodeRoles, n, ROLE_LANDING)
    const manual = parseURIs(loadLocalURIs(user?.username)).filter(isLanding)
    const sub = loadSubCache(user?.username).filter(isLanding)
    return mergeLanding(manual, sub)
  }, [user, localVer, nodeRoles])

  const landingNodes = localNodes
  const localIdx = useMemo(() => landingIndex(localNodes), [localNodes])

  const enrich = (r) => {
    const key = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : null
    if (key && localIdx.has(key) && r.entry) {
      const ep = splitEndpoint(r.entry)
      const node = localIdx.get(key)
      const relay = ep && rewriteEndpoint(node.uri, ep.host, ep.port)
      if (relay) return { ...r, exit_kind: 'landing', landing_name: node.name, landing_protocol: node.protocol, relay_uri: relay }
    }
    return r
  }

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
    setError('')
    api.get(`/rules`)
      .then(d => {
        setData(d)
        if (d.users?.length) setUsers(d.users)
      })
      .catch(err => setError(err?.message || '加载失败'))
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  // Only blank the page on the first load; later reloads (delete/edit) keep the
  // current list on screen instead of flashing a full-page spinner.
  if (loading && !data) return <Layout><Loading /></Layout>
  // A load failure is distinct from an empty list — don't disguise an error as
  // "暂无规则".
  if (!data && error) return <Layout><Empty title="加载失败" desc={error}><button onClick={load} className="btn-secondary text-xs mt-3">重试</button></Empty></Layout>

  const { rules: allRulesRaw = [], nodes = [] } = data || {}
  const nodeMap = {}
  nodes.forEach(n => { nodeMap[n.id] = n })
  const allRules = allRulesRaw.map(enrich)
  const rules = allRules.filter(r => !nodeMap[r.node_id]?.hidden)

  const deleteRule = async (rule) => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/rules/${rule.id}`); toast('已删除'); load() } catch (err) { toast(err.message, 'error') }
  }
  const openCreate = () => { setCreateInitial(null); setCreateOpen(true) }
  const copyRule = (rule) => { setCreateInitial(copyInitial(rule)); setCreateOpen(true) }

  const q = search.trim().toLowerCase()
  let filtered = rules
  // Owner and node filters are both applied client-side: the initial load
  // already fetched every rule, so re-requesting a subset per filter change was
  // pure waste.
  if (selectedOwners.size > 0) filtered = filtered.filter(r => r.owner_id?.Valid && selectedOwners.has(r.owner_id.Int64))
  if (selectedNodes.size > 0) filtered = filtered.filter(r => selectedNodes.has(r.node_id))
  if (q) filtered = filtered.filter(r => {
    const node = nodeMap[r.node_id]
    const exit = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : ''
    return [r.name, node?.name, r.entry, exit, r.owner_name].some(v => (v || '').toLowerCase().includes(q))
  })

  const filterActive = selectedOwners.size > 0 || selectedNodes.size > 0
  // A single updateParams call: setSearchParams's updater closes over the
  // searchParams from this render, not an accumulating prev, so two separate
  // setSelectedOwners/setSelectedNodes calls here would each compute from the
  // same stale params and the second navigate would clobber the first's clear.
  const clearFilters = () => updateParams({ owners: '', nodes: '' })
  // Handed to the detail page via route state so its "返回规则列表" link can
  // restore this exact filter/search combo instead of landing on a bare list.
  const rulesQuery = searchParams.toString()

  const userOptions = users.map(u => ({ value: u.id, label: u.username }))
  const nodeOptions = nodes.filter(n => !n.hidden).map(n => ({ value: n.id, label: n.name }))

  return (
    <Layout>
      <div className="h-full flex flex-col">
      <PageHeader title="转发规则" count={rules.length} />

      <Panel fill>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索规则名称、节点、目标…" />
          <div className="hidden md:block ml-auto"><ToolbarButton onClick={openCreate}>＋ 创建规则</ToolbarButton></div>
        </PanelToolbar>

        {(users.length > 0 || nodes.length > 0) && (
          <div className="relative flex items-center gap-2.5 px-[22px] py-2.5 border-b border-line-soft flex-wrap z-10">
            <span className="text-xs text-ink-mut">筛选</span>
            <FilterDropdown
              label="用户"
              icon={<svg className="w-[15px] h-[15px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M16 21v-2a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v2"/><circle cx="9.5" cy="7" r="3.5"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/></svg>}
              options={userOptions}
              selected={selectedOwners}
              onChange={setSelectedOwners}
              searchPlaceholder="搜索用户…"
            />
            <FilterDropdown
              label="节点"
              icon={<svg className="w-[15px] h-[15px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="5" width="18" height="6" rx="1.5"/><rect x="3" y="13" width="18" height="6" rx="1.5"/><path d="M7 8h.01M7 16h.01"/></svg>}
              options={nodeOptions}
              selected={selectedNodes}
              onChange={setSelectedNodes}
              searchPlaceholder="搜索节点…"
            />
            {filterActive && (
              <button onClick={clearFilters}
                className="inline-flex items-center gap-1 text-xs text-ink-mut hover:text-ink transition-colors cursor-pointer bg-transparent border-0 p-0">
                <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
                清除筛选
              </button>
            )}
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
              onRowClick={r => navigate(`/rules/${r.id}`, rulesQuery ? { state: { rulesQuery: `?${rulesQuery}` } } : undefined)} />
          </TableScroll>
        )}
      </Panel>
      </div>

      <RuleFormModal
        open={createOpen} onClose={() => setCreateOpen(false)} title="创建规则" submitLabel="创建规则"
        nodes={nodes} landingNodes={landingNodes} initial={createInitial} onAddProxyURI={addProxyURI}
        onSubmit={async (form) => {
          const res = await api.post('/rules', {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            mode: form.mode || undefined,
            exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
            comment: form.comment || undefined,
          })
          toast('规则已创建'); setCreateOpen(false)
          if (res?.rule?.id) navigate(`/rules/${res.rule.id}`)
        }} />

      <RuleFormModal
        open={!!editRule} onClose={() => setEditRule(null)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} landingNodes={landingNodes} initial={editRule ? ruleToForm(editRule) : null} onAddProxyURI={addProxyURI}
        onSubmit={async (form) => {
          await api.put(`/rules/${editRule.id}`, {
            node_id: Number(form.node_id), name: form.name, proto: form.proto,
            mode: form.mode || undefined,
            exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
            comment: form.comment || undefined,
          })
          toast('已保存并重下发'); setEditRule(null); load()
        }} />
    </Layout>
  )
}

function FilterDropdown({ label, icon, options, selected, onChange, searchPlaceholder }) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const ref = useRef(null)

  useEffect(() => {
    if (!open) { setQuery(''); return }
    const onDoc = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false) }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  const q = query.trim().toLowerCase()
  const shown = q ? options.filter(o => String(o.label).toLowerCase().includes(q)) : options

  const toggle = (val) => {
    const next = new Set(selected)
    if (next.has(val)) next.delete(val); else next.add(val)
    onChange(next)
  }

  return (
    <div ref={ref} className="relative">
      <button onClick={() => setOpen(o => !o)}
        className="inline-flex items-center gap-2 h-[34px] px-3.5 rounded-[9px] text-[13.5px] cursor-pointer transition-colors bg-surface border border-line text-ink-soft hover:border-ink-mut">
        {icon}
        {label}
        {selected.size > 0 && (
          <span className="min-w-[18px] h-[18px] px-1 rounded-full bg-blue-600 text-white text-[11px] font-bold inline-flex items-center justify-center">
            {selected.size}
          </span>
        )}
        <svg className="w-3.5 h-3.5 text-ink-mut" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      {open && (
        <div className="absolute top-[calc(100%+8px)] left-0 w-[260px] z-50 bg-surface border border-line rounded-[11px] shadow-[0_20px_50px_-16px_rgba(0,0,0,0.7)] overflow-hidden">
          <div className="p-2.5 border-b border-line-soft">
            <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder={searchPlaceholder}
              onKeyDown={e => { if (e.key === 'Enter') e.preventDefault() }}
              className="input-field w-full text-[13px]" style={{ height: 32 }} />
          </div>
          <div className="max-h-[230px] overflow-y-auto py-1.5 px-1.5">
            {shown.length === 0 ? (
              <div className="px-3 py-2 text-[13px] text-ink-mut">无匹配</div>
            ) : shown.map(o => {
              const checked = selected.has(o.value)
              return (
                <div key={o.value} onClick={() => toggle(o.value)}
                  className="flex items-center gap-2.5 px-2.5 py-[7px] rounded-lg cursor-pointer text-[13px] text-ink hover:bg-raised transition-colors">
                  <span className={`w-4 h-4 flex-none rounded border flex items-center justify-center ${checked ? 'bg-blue-600 border-blue-600' : 'border-line'}`}>
                    {checked && <svg className="w-2.5 h-2.5 text-white" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5"/></svg>}
                  </span>
                  <span className="truncate">{o.label}</span>
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}
