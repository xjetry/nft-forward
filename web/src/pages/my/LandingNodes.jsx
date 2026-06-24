import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, CopyText, SensText } from '../../components/ui'
import { PageHeader, Panel, PanelToolbar, SearchInput } from '../../components/page'

/* Landing-nodes nav: lists the nodes resolved from the user's subscription
   and/or manual URIs, each with a one-click copy of its original (direct)
   proxy URI. The refresh button appears only for a dynamic source (a
   subscription URL) — a manual-only source is static and needs no refetch. */
export default function MyLandingNodes() {
  const [nodes, setNodes] = useState(null)
  const [hasDynamic, setHasDynamic] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [search, setSearch] = useState('')
  const toast = useToast()
  const blurred = useBlur()

  const load = (refresh = false) => {
    if (refresh) setRefreshing(true)
    api.get(`/my/landing-nodes${refresh ? '?refresh=1' : ''}`)
      .then(d => { setNodes(d?.nodes || []); setHasDynamic(!!d?.has_dynamic) })
      .catch(console.error)
      .finally(() => setRefreshing(false))
  }
  useEffect(() => load(false), [])

  if (nodes === null) return <Layout><Loading /></Layout>

  const refresh = () => { load(true); toast('已刷新订阅') }

  const q = search.trim().toLowerCase()
  const filtered = !q ? nodes : nodes.filter(n =>
    [n.name, n.protocol, `${n.host}:${n.port}`].some(v => (v || '').toLowerCase().includes(q)))

  return (
    <Layout>
      <PageHeader title="落地节点" count={nodes.length} unit="个" />
      <Panel>
        <PanelToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索名称、协议、地址…" />
          {hasDynamic && (
            <button onClick={refresh} disabled={refreshing}
              className="ml-auto inline-flex items-center gap-1.5 text-[13.5px] font-semibold text-ink-soft bg-surface border border-line hover:border-blue-500 hover:text-blue-600 px-[18px] py-[9px] rounded-[10px] transition-colors disabled:opacity-50">
              {refreshing ? '刷新中…' : '刷新订阅'}
            </button>
          )}
        </PanelToolbar>

        {nodes.length === 0 ? (
          <Empty title="暂无落地节点" desc="尚未配置订阅地址或节点 URI，请联系管理员。" />
        ) : filtered.length === 0 ? (
          <Empty title="无匹配节点" desc="试试别的关键词。" />
        ) : (
          <table className="tbl">
            <thead><tr><th>名称</th><th>协议</th><th>地址</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {filtered.map((n, i) => (
                <tr key={i}>
                  <td className="font-semibold">{n.name || '(未命名)'}</td>
                  <td className="font-mono text-xs text-ink-soft">{n.protocol}</td>
                  <td className="font-mono text-xs"><SensText blurred={blurred}>{n.host}:{n.port}</SensText></td>
                  <td className="text-right">
                    <CopyText text={n.uri}><span className="text-blue-600 font-sans text-xs font-semibold">复制节点</span></CopyText>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </Layout>
  )
}
