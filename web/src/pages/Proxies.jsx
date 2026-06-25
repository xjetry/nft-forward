import { useState, useEffect, useMemo } from 'react'
import { api } from '../lib/api'
import { Layout, useToast, useBlur, useUser } from '../components/Layout'
import { Loading, Empty, CopyText, SensText, Badge } from '../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput } from '../components/page'
import {
  parseURIs, loadLocalURIs, loadSubCache, loadLandingMarks, loadDirectMarks,
  nodeKey, landingIndex, splitEndpoint, rewriteEndpoint, mergeLanding,
} from '../lib/landing'
import { uriToClashYaml } from '../lib/yaml-convert'

export default function Proxies() {
  const [rules, setRules] = useState(null)
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')
  const [tab, setTab] = useState('all')
  const [copyFmt, setCopyFmt] = useState(() => localStorage.getItem('nf-copy-fmt') || 'uri')
  const toast = useToast()
  const blurred = useBlur()
  const { user } = useUser()

  const isAdmin = user?.role === 'admin'

  useEffect(() => {
    const endpoint = isAdmin ? `/rules?owner_ids=${user?.id}` : '/my/rules'
    api.get(endpoint)
      .then(d => setRules(d?.rules || []))
      .catch(console.error)
      .finally(() => setLoading(false))
  }, [])

  const manualNodes = useMemo(() => parseURIs(loadLocalURIs(user?.username)), [user])
  const subNodes = useMemo(() => loadSubCache(user?.username), [user])
  const landingMarks = useMemo(() => loadLandingMarks(user?.username), [user])
  const directMarks = useMemo(() => loadDirectMarks(user?.username), [user])

  const directSub = useMemo(() => subNodes.filter(n => directMarks.has(nodeKey(n))), [subNodes, directMarks])
  const landingSub = useMemo(() => subNodes.filter(n => landingMarks.has(nodeKey(n))), [subNodes, landingMarks])

  const allLanding = useMemo(() => mergeLanding(manualNodes, landingSub), [manualNodes, landingSub])
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

  const directProxies = [...manualNodes, ...directSub]
  const allProxies = [...directProxies.map(n => ({ ...n, kind: 'direct' })), ...relayProxies.map(n => ({ ...n, kind: 'relay' }))]

  const tabbed = tab === 'all' ? allProxies : allProxies.filter(n => n.kind === (tab === 'relay' ? 'relay' : 'direct'))
  const q = search.trim().toLowerCase()
  const filtered = !q ? tabbed : tabbed.filter(n =>
    [n.name, n.protocol, `${n.host}:${n.port}`, n.ruleName].some(v => (v || '').toLowerCase().includes(q)))

  const toggleFmt = () => {
    setCopyFmt(f => {
      const next = f === 'uri' ? 'yaml' : 'uri'
      localStorage.setItem('nf-copy-fmt', next)
      return next
    })
  }

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
      <PageHeader title="我的代理" count={allProxies.length} unit="个" />
      <Panel>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索名称、协议、地址…" />
          <button type="button" onClick={toggleFmt}
            className="ml-auto text-[11px] font-mono px-2 py-1 rounded border border-line bg-surface text-ink-mut hover:text-ink hover:border-ink-mut transition-colors"
            title="切换复制格式">{copyFmt.toUpperCase()}</button>
        </PanelToolbar>
        <div className="flex items-center gap-1.5 px-[22px] py-2.5 border-b border-line-soft">
          {[['all', '全部', allProxies.length], ['direct', '直连', directProxies.length], ['relay', '中转', relayProxies.length]].map(([key, label, n]) => (
            <button key={key} onClick={() => setTab(key)}
              className={`px-3 py-0.5 rounded text-xs border transition-colors ${
                tab === key ? 'bg-blue-500 text-white border-blue-500' : 'bg-surface text-ink-soft border-line hover:border-ink-mut'
              }`}>{label} {n}</button>
          ))}
        </div>

        {allProxies.length === 0 ? (
          <Empty title="暂无可用代理" desc="在概览页添加代理 URI 或订阅地址。" />
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
                      {n.name || '(未命名)'}
                      {n.kind === 'relay' && <span className="ml-1.5 text-[11px] text-ink-mut font-normal">← {n.ruleName}</span>}
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
      </Panel>
    </Layout>
  )
}
