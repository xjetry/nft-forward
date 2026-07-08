import { useState, useEffect, useMemo } from 'react'
import { api } from '../lib/api'
import { Layout, useToast, useBlur, useUser, useCopyFmt } from '../components/Layout'
import { Loading, Empty, CopyText, SensText, Badge } from '../components/ui'
import { copyToClipboard } from '../lib/clipboard'
import { PageHeader, Panel, PanelToolbar, SearchInput, TableScroll } from '../components/page'
import {
  parseURIs, loadLocalURIs, loadSubCache, fetchNodeRoles, loadLocalRoles, nodeHasRole, ROLE_LANDING, ROLE_DIRECT,
  landingIndex, splitEndpoint, rewriteEndpoint, mergeLanding,
} from '../lib/landing'
import { uriToClashYaml } from '../lib/yaml-convert'

export default function Proxies() {
  const [rules, setRules] = useState(null)
  const [serverNodes, setServerNodes] = useState([])
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [tab, setTab] = useState('all')
  const { copyFmt } = useCopyFmt()
  const blurred = useBlur()
  const toast = useToast()
  const { user } = useUser()

  const isAdmin = user?.role === 'admin'

  const [roles, setRoles] = useState({})

  useEffect(() => {
    const rulesEndpoint = isAdmin ? `/rules?owner_ids=${user?.id}` : '/my/rules'
    const rulesP = api.get(rulesEndpoint).then(d => d?.rules || []).catch(() => [])
    const serverP = !isAdmin
      ? api.get('/my/landing-nodes').then(d => d?.nodes || []).catch(() => [])
      : Promise.resolve([])
    const rolesP = fetchNodeRoles()
    Promise.all([rulesP, serverP, rolesP]).then(([r, s, sr]) => {
      setRules(r); setServerNodes(s); setRoles({ ...sr, ...loadLocalRoles(user?.username) })
    }).finally(() => setLoading(false))
  }, [])

  const manualNodes = useMemo(() => parseURIs(loadLocalURIs(user?.username)), [user])
  const localSubNodes = useMemo(() => loadSubCache(user?.username), [user])

  const allSubNodes = useMemo(() => mergeLanding(localSubNodes, serverNodes), [localSubNodes, serverNodes])

  const directSub = useMemo(() => allSubNodes.filter(n => nodeHasRole(roles, n, ROLE_DIRECT)), [allSubNodes, roles])
  const landingSub = useMemo(() => allSubNodes.filter(n => nodeHasRole(roles, n, ROLE_LANDING)), [allSubNodes, roles])
  const directManual = useMemo(() => manualNodes.filter(n => nodeHasRole(roles, n, ROLE_DIRECT)), [manualNodes, roles])
  const landingManual = useMemo(() => manualNodes.filter(n => nodeHasRole(roles, n, ROLE_LANDING)), [manualNodes, roles])

  const allLanding = useMemo(() => {
    const manual = landingManual.map(n => ({ ...n, source: 'local' }))
    const sub = landingSub.map(n => ({ ...n, source: serverNodes.some(s => s.host === n.host && s.port === n.port) ? 'admin' : 'local' }))
    return mergeLanding(manual, sub)
  }, [landingManual, landingSub, serverNodes])
  const allLandingIdx = useMemo(() => landingIndex(allLanding), [allLanding])

  const relayProxies = useMemo(() => {
    if (!rules) return []
    const out = []
    for (const r of rules) {
      const key = r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : null
      if (!key || !allLandingIdx.has(key) || !r.entry) continue
      const ep = splitEndpoint(r.entry)
      const node = allLandingIdx.get(key)
      const relay = ep && rewriteEndpoint(node.uri, ep.host, ep.port)
      if (relay) out.push({ ...node, relay, ruleName: r.name })
    }
    return out
  }, [rules, allLandingIdx])

  if (loading) return <Layout><Loading /></Layout>

  const directProxies = [...directManual, ...directSub]
  const allProxies = [...directProxies.map(n => ({ ...n, kind: 'direct' })), ...relayProxies.map(n => ({ ...n, kind: 'relay' }))]

  const tabbed = tab === 'all' ? allProxies : allProxies.filter(n => n.kind === (tab === 'relay' ? 'relay' : 'direct'))
  const q = search.trim().toLowerCase()
  const filtered = !q ? tabbed : tabbed.filter(n =>
    [n.name, n.protocol, `${n.host}:${n.port}`, n.ruleName].some(v => (v || '').toLowerCase().includes(q)))

  const copyText = (n) => {
    const uri = n.kind === 'relay' ? n.relay : n.uri
    if (!uri) return null
    if (copyFmt === 'yaml') {
      const yaml = uriToClashYaml(uri)
      if (yaml) return yaml
    }
    return uri
  }

  return (
    <Layout>
      <div className="user-page h-full flex flex-col">
      <PageHeader title="我的代理" count={allProxies.length} unit="个" />
      <Panel fill className="user-panel">
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索名称、协议、地址…" />
        </PanelToolbar>
        <div className="user-tabs px-[22px] py-3 border-b border-line-soft">
          {[['all', '全部', allProxies.length], ['direct', '直连', directProxies.length], ['relay', '中转', relayProxies.length]].map(([key, label, n]) => (
            <button key={key} onClick={() => setTab(key)}
              className={`user-tab ${tab === key ? 'user-tab-active' : ''}`}>{label} {n}</button>
          ))}
          {filtered.length > 0 && (
            <button onClick={() => {
              const all = filtered.map(n => copyText(n)).filter(Boolean).join('\n')
              copyToClipboard(all).then(() => toast(`已复制 ${filtered.length} 条`)).catch(() => toast('复制失败', 'error'))
            }} className="user-tab ml-auto">
              复制全部
            </button>
          )}
        </div>

        <TableScroll>
        {allProxies.length === 0 ? (
          <Empty title="暂无可用代理" desc="在概览页添加代理 URI 或订阅地址，并标记为直连或落地。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配" desc="试试别的关键词。" />
        ) : (
          <table className="tbl">
            <thead><tr><th>名称</th><th>协议</th><th>地址</th><th>类型</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {filtered.map((n, i) => {
                const text = copyText(n)
                return (
                  <tr key={i}>
                    <td className="font-semibold">
                      <span className="route-name">{n.name || '(未命名)'}</span>
                      {n.kind === 'relay' && <span className="proxy-name-sub">← {n.ruleName}</span>}
                    </td>
                    <td className="font-mono text-xs text-ink-soft">{n.protocol}</td>
                    <td className="font-mono text-xs">
                      <SensText blurred={blurred}>
                        {n.kind === 'relay' ? `${splitEndpoint(n.relay)?.host || n.host}:${splitEndpoint(n.relay)?.port || n.port}` : `${n.host}:${n.port}`}
                      </SensText>
                    </td>
                    <td>
                      {n.kind === 'relay'
                        ? <Badge color="emerald">中转</Badge>
                        : <Badge color="blue">直连</Badge>}
                    </td>
                    <td className="text-right">
                      {text && (
                        <CopyText text={text}>
                          <span className="text-blue-600 font-sans text-xs font-semibold">
                            {copyFmt === 'yaml' && uriToClashYaml(n.kind === 'relay' ? n.relay : n.uri) ? '复制YAML' : '复制'}
                          </span>
                        </CopyText>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
        </TableScroll>
      </Panel>
      </div>
    </Layout>
  )
}
