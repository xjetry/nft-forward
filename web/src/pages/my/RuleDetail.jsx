import { useState, useEffect, useMemo } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { Layout, useToast, useBlur, useUser, useCopyFmt } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, SensText, useConfirm, ExitKindBadge } from '../../components/ui'
import { copyToClipboard } from '../../lib/clipboard'
import { RuleFormModal, ruleToForm, ruleFormToPayload } from '../../components/RuleFormModal'
import { uriToClashYaml } from '../../lib/yaml-convert'
import { parseURIs, landingIndex, mergeLanding, splitEndpoint, rewriteEndpoint, loadLocalURIs, loadSubCache, fetchNodeRoles, loadLocalRoles, nodeHasRole, ROLE_LANDING } from '../../lib/landing'

export default function MyRuleDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showEdit, setShowEdit] = useState(false)
  // The single-rule endpoint doesn't carry the binding graph (only the list
  // endpoint computes the granted-intersection edges) — fetch it alongside
  // so the edit modal's middle-layer cascade has candidates to offer.
  const [bindings, setBindings] = useState([])
  // Admin-assigned landing nodes live on the server (unlike the user's own
  // browser-local URIs) — without this fetch the edit modal's exit picker
  // would only offer local nodes and silently fall back to a custom exit.
  const [serverLanding, setServerLanding] = useState([])
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()
  const { user } = useUser()
  const { copyFmt } = useCopyFmt()

  const [nodeRoles, setNodeRoles] = useState({})
  useEffect(() => {
    fetchNodeRoles().then(sr => setNodeRoles({ ...sr, ...loadLocalRoles(user?.username) }))
  }, [user])
  const localNodes = useMemo(() => {
    const isLanding = n => nodeHasRole(nodeRoles, n, ROLE_LANDING)
    const manual = parseURIs(loadLocalURIs(user?.username)).filter(isLanding)
    const sub = loadSubCache(user?.username).filter(isLanding)
    return mergeLanding(manual, sub)
  }, [user, nodeRoles])

  const load = () => {
    setLoading(true)
    api.get(`/my/rules/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
    api.get('/my/rules').then(d => setBindings(d?.bindings || [])).catch(console.error)
    api.get('/my/landing-nodes').then(d => setServerLanding(d?.nodes || [])).catch(console.error)
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="规则不存在" /></Layout>

  const { rule: serverRule, nodes = [], node_by_id = {}, show_rate } = data

  // Landing nodes = the user's browser-local proxy URIs plus admin-assigned
  // ones, both filtered to the landing role. Used for the exit picker and to
  // enrich this rule below.
  const serverLandingFiltered = serverLanding.filter(n => nodeHasRole(nodeRoles, n, ROLE_LANDING))
  const landingNodes = mergeLanding(localNodes, serverLandingFiltered)
  const allLandingIdx = landingIndex(landingNodes)

  // A rule whose exit is one of the user's browser-local proxy URIs gets no
  // relay URI from the server (the URI never leaves the browser), so rewrite the
  // rule's entry into it client-side here — the same enrichment the rules list
  // does — so the detail page can offer a copyable relay URI too.
  const rule = (() => {
    const key = serverRule.exit_host && serverRule.exit_port ? `${serverRule.exit_host}:${serverRule.exit_port}` : null
    if (key && allLandingIdx.has(key) && serverRule.entry) {
      const lnode = allLandingIdx.get(key)
      const ep = splitEndpoint(serverRule.entry)
      const relay = ep && rewriteEndpoint(lnode.uri, ep.host, ep.port)
      if (relay) {
        const out = { ...serverRule, exit_kind: 'landing', landing_name: lnode.name, landing_protocol: lnode.protocol, relay_uri: relay }
        // Dual-stack rule: also rewrite the v6 entry into the URI so the detail
        // page can offer a v6 relay URI beside the v4 one.
        if (serverRule.entry_v6) {
          const ep6 = splitEndpoint(serverRule.entry_v6)
          const relay6 = ep6 && rewriteEndpoint(lnode.uri, ep6.host, ep6.port)
          if (relay6) out.relay_uri_v6 = relay6
        }
        return out
      }
    }
    return serverRule
  })()
  const node = node_by_id[rule.node_id]
  // Names resolve only through node_by_id — the granted-node map the page
  // already has in scope — so an unresolvable via (rare: node revoked after
  // the rule was built) silently drops from the chain instead of showing a
  // bare id the user has no way to recognize.
  const entryName = node?.name || `#${rule.node_id}`
  // The backend chain is the deployed physical path (composites already
  // flattened into their members); prefer it so display matches what actually
  // runs. Fall back to entry + logical via names before the rule's first
  // regeneration, when no physical hops exist yet.
  const flatNames = (rule.chain || []).map(c => c.name).filter(Boolean)
  const viaNames = (rule.via_node_ids || []).map(id => node_by_id[id]?.name).filter(Boolean)
  const nodeChain = flatNames.length
    ? flatNames.join(' → ')
    : (viaNames.length ? [entryName, ...viaNames].join(' → ') : entryName)

  const exitOf = (r) => (r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : '')

  const saveEdit = async (form) => {
    await api.put(`/my/rules/${rule.id}`, ruleFormToPayload(form))
    toast('已保存并重下发'); setShowEdit(false); load()
  }

  const deleteRule = async () => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/my/rules/${rule.id}`); toast('已删除'); navigate('/my/rules') } catch (err) { toast(err.message, 'error') }
  }

  return (
    <Layout>
      <div className="user-page h-full flex flex-col">
      <div className="flex flex-col sm:flex-row sm:items-end gap-2 sm:gap-3">
        <Link to="/my/rules" className="text-blue-600 text-[13px] font-semibold hover:underline inline-flex items-center gap-1 self-start">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
          我的规则
        </Link>
        <h1 className="m-0 text-2xl font-bold text-ink min-w-0 break-words">{rule.name}</h1>
      </div>

      <section className="user-section">
        <div className="user-section-head">
          <div>
            <h3 className="user-section-title">入口</h3>
            <div className="user-section-sub">复制给客户端</div>
          </div>
        </div>
        <div className="p-5">
          {rule.entry ? (
            <div className="space-y-2">
              <div className="copy-panel">
                <span className="copy-panel-label">ENTRY</span>
                {rule.entry_node_id && node_by_id[rule.entry_node_id] && <span className="copy-panel-chip">{node_by_id[rule.entry_node_id].name}</span>}
                <span className="copy-panel-value"><SensText blurred={blurred}>{rule.entry}</SensText></span>
                <button onClick={() => copyToClipboard(rule.entry).then(() => toast('入口地址已复制')).catch(() => toast('复制失败', 'error'))}
                  className="copy-panel-button">
                  <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                  复制
                </button>
              </div>
              {rule.entry_v6 && (
                <div className="copy-panel">
                  <span className="copy-panel-label">ENTRY (v6)</span>
                  <span className="copy-panel-value"><SensText blurred={blurred}>{rule.entry_v6}</SensText></span>
                  <button onClick={() => copyToClipboard(rule.entry_v6).then(() => toast('入口地址已复制')).catch(() => toast('复制失败', 'error'))}
                    className="copy-panel-button">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                    复制
                  </button>
                </div>
              )}
              {/* A dual-stack rule carries a v6 relay URI too, so each entry
                  family gets its own copyable proxy row. */}
              {[
                rule.relay_uri && { uri: rule.relay_uri, label: rule.relay_uri_v6 ? 'PROXY (v4)' : 'PROXY' },
                rule.relay_uri_v6 && { uri: rule.relay_uri_v6, label: 'PROXY (v6)' },
              ].filter(Boolean).map(({ uri, label }) => (
                <div key={label} className="copy-panel">
                  <span className="copy-panel-label">{label}</span>
                  <span className="copy-panel-value"><SensText blurred={blurred}>{uri}</SensText></span>
                  <button onClick={() => {
                    const yaml = copyFmt === 'yaml' ? uriToClashYaml(uri) : null
                    copyToClipboard(yaml || uri).then(() => toast('代理 URI 已复制')).catch(() => toast('复制失败', 'error'))
                  }}
                    className="copy-panel-button">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                    复制
                  </button>
                </div>
              ))}
            </div>
          ) : <span className="text-ink-mut text-sm">尚未分配入口</span>}
        </div>
      </section>

      <section className="user-section">
        <div className="user-section-head"><h3 className="user-section-title">规则信息</h3></div>
        <div className="p-5">
          <div className="detail-grid">
            <div className="detail-item">
              <span className="detail-label">名称</span>
              <span className="detail-value">{rule.name}</span>
            </div>
            <div className="detail-item">
              <span className="detail-label">节点</span>
              <span className="detail-value font-mono break-words">{nodeChain}</span>
            </div>
            <div className="detail-item">
              <span className="detail-label">协议</span>
              <span className="detail-value"><ProtoBadge proto={rule.proto} /></span>
            </div>
            <div className="detail-item">
              <span className="detail-label">出口</span>
              <span className="detail-value font-mono inline-flex items-center gap-2 flex-wrap">
                <ExitKindBadge kind={rule.exit_kind} protocol={rule.landing_protocol} />
                {rule.exit_kind === 'landing' && rule.landing_name
                  ? <span className="font-sans">{rule.landing_name}</span>
                  : <SensText blurred={blurred}>{exitOf(rule) || '--'}</SensText>}
              </span>
            </div>
            {show_rate && (
              <div className="detail-item">
                <span className="detail-label">倍率</span>
                <span className="detail-value"><Badge color="blue">×{rule.rate_multiplier ?? 1}</Badge></span>
              </div>
            )}
            <div className="detail-item">
              <span className="detail-label">流量</span>
              <span className="detail-value font-mono text-ink-mut">{fmtBytes(rule.total_bytes || 0)}</span>
            </div>
            {rule.comment && (
              <div className="detail-item md:col-span-2">
                <span className="detail-label">备注</span>
                <span className="detail-value text-ink-soft font-normal">{rule.comment}</span>
              </div>
            )}
          </div>
        </div>
      </section>

      <div className="flex items-center gap-3 flex-wrap">
        <button onClick={() => setShowEdit(true)} className="btn-primary text-xs">编辑规则</button>
        <button onClick={deleteRule} className="btn-secondary text-xs">删除规则</button>
      </div>
      </div>

      <RuleFormModal
        open={showEdit} onClose={() => setShowEdit(false)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} landingNodes={landingNodes} bindings={bindings} initial={showEdit ? ruleToForm(rule) : null}
        onSubmit={saveEdit} showRate={show_rate} />
    </Layout>
  )
}
