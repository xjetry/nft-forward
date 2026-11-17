import { useMemo, useState } from 'react'
import { api } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { hostPortKey, relayFor } from '../lib/landing'
import { combineResources, mapResource } from '../lib/resource'
import { useLandingCatalog } from '../lib/useLandingCatalog'
import { useResource } from '../lib/useResource'
import { uriToClashYaml } from '../lib/yaml-convert'
import { Page, PageHeader, SearchInput, TableScroll, Toolbar } from '../components/page'
import { useBlur, useCopyFmt } from '../components/prefs'
import { ResourceView } from '../components/ResourceView'
import { useUser } from '../components/session'
import { Button, Card, CopyText, Empty, IconCopy, Segmented, SensText, Tag, TextLink, useAction, useToast } from '../components/ui'

/* 把规则与目录推导成“可以导入客户端的代理”。
   - 直连：目录的 direct（生效角色含“直连”的节点，目录已按地址去重），
     客户端连的就是节点自己的地址。
   - 中转：出口是落地节点的规则。客户端连的是规则的入口，所以地址取规则
     入口，链接取 relayFor 把落地节点链接的端点换成入口之后的结果。双栈
     规则有两个入口，各算一条。 */
function listProxies(rules, catalog) {
  const direct = catalog.direct.map((node) => {
    const address = hostPortKey(node.host, node.port)
    return { key: `direct:${address}`, kind: 'direct', name: node.name, protocol: node.protocol, address, uri: node.uri, rule: null }
  })
  const relay = []
  for (const rule of rules) {
    const hit = relayFor(rule, catalog.landingByKey)
    if (!hit) continue
    const entries = [
      { slot: 'entry', address: rule.entry, uri: hit.relay_uri },
      { slot: 'entry_v6', address: rule.entry_v6, uri: hit.relay_uri_v6 },
    ]
    for (const { slot, address, uri } of entries) {
      // relayFor 在入口不是可解析的地址（规则还没有入口）或链接无法改写时
      // 给出 null：这个入口没有可导入的链接。
      if (!uri) continue
      relay.push({ key: `relay:${rule.id}:${slot}`, kind: 'relay', name: hit.node.name, protocol: hit.node.protocol, address, uri, rule })
    }
  }
  return [...direct, ...relay]
}

/* 复制出去的内容：偏好是 YAML 且这条链接能转成 Clash 配置时给 YAML，否则
   给链接本身。按钮文字跟着实际给出的内容走，转不了的那几行不会写着
   “复制 YAML”却复制出链接。 */
function copyPayload(proxy, copyFmt) {
  const yaml = copyFmt === 'yaml' ? uriToClashYaml(proxy.uri) : null
  return yaml ? { text: yaml, label: '复制 YAML' } : { text: proxy.uri, label: '复制链接' }
}

export default function Proxies() {
  const { user } = useUser()
  const isAdmin = user.role === 'admin'
  const copyFmt = useCopyFmt()
  const blurred = useBlur()
  const toast = useToast()
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState('all')

  // /my/rules 只对普通用户开放；管理员从全部规则里取属于自己的。
  const rules = useResource(
    async () => (await api.get(isAdmin ? `/rules?owner_ids=${user.id}` : '/my/rules'))?.rules || [],
    [isAdmin, user.id],
  )
  const catalog = useLandingCatalog(user)
  // 规则与目录缺一不可：少了任何一份，推导出的列表都不完整，不能当成
  // “没有代理”显示，所以两者并成一个资源，任一失败即失败。
  const state = useMemo(
    () => mapResource(combineResources(rules.state, catalog.state), ([ruleList, cat]) => listProxies(ruleList, cat)),
    [rules.state, catalog.state],
  )
  const reload = async () => { await Promise.all([rules.reload(), catalog.reload()]) }

  const ready = state.status === 'ready'
  const proxies = ready ? state.data : []
  const countOf = k => proxies.filter(p => p.kind === k).length
  const q = search.trim().toLowerCase()
  const shown = proxies.filter(p =>
    (kind === 'all' || p.kind === kind)
    && (!q || [p.name, p.protocol, p.address, p.rule?.name].some(v => (v || '').toLowerCase().includes(q))))
  const narrowed = kind !== 'all' || q !== ''
  const overviewPath = isAdmin ? '/' : '/my'

  const copyAll = useAction(async () => {
    await copyToClipboard(shown.map(p => copyPayload(p, copyFmt).text).join('\n'))
    toast(`已复制 ${shown.length} 条`)
  }, { onError: () => toast('没能复制到剪贴板，请重试', 'error') })

  const emptyState = () => {
    if (proxies.length === 0) {
      return (
        <Empty
          title="还没有可以导入客户端的代理"
          desc="在概览页“我添加的节点”里添加节点：标记为“直连”的会直接出现在这里；标记为“落地”的，被某条规则选作出口后，这里会多出一条经该规则中转的代理。"
        >
          <Button to={overviewPath}>去概览页添加</Button>
        </Empty>
      )
    }
    if (q) return <Empty title="没有匹配的代理" desc="换一个关键词，或切换连接方式。" />
    if (kind === 'relay') return <Empty title="还没有中转代理" desc="把一条规则的出口设为落地节点后，这里会出现经该规则入口中转的代理。" />
    return <Empty title="还没有直连代理" desc="在概览页“我添加的节点”里把节点标记为“直连”，它就会出现在这里。" />
  }

  return (
    <Page fill>
      <PageHeader
        title="我的代理"
        meta={<>
          {ready && <span>共 {proxies.length} 个</span>}
          <span>可以直接导入客户端的代理：标记为“直连”的节点，以及出口是落地节点的规则所提供的中转入口。</span>
        </>}
        actions={ready && proxies.length > 0 && (
          <Button
            variant="primary"
            icon={<IconCopy />}
            loading={copyAll.pending}
            disabled={shown.length === 0}
            onClick={() => copyAll.run()}
          >
            {narrowed ? `复制筛选出的 ${shown.length} 条` : '复制全部'}
          </Button>
        )}
      />
      <ResourceView resource={{ state, reload }}>
        {() => (
          <Card fill>
            <Toolbar>
              <SearchInput value={search} onChange={setSearch} placeholder="搜索名称、协议、地址、规则…" />
              <Segmented
                label="连接方式"
                value={kind}
                onChange={setKind}
                options={[
                  { value: 'all', label: '全部', count: proxies.length },
                  { value: 'direct', label: '直连', count: countOf('direct') },
                  { value: 'relay', label: '中转', count: countOf('relay') },
                ]}
              />
            </Toolbar>
            <TableScroll>
              {shown.length === 0 ? emptyState() : (
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>名称</th>
                      <th>连接方式</th>
                      <th>协议</th>
                      <th>地址</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {shown.map((p) => {
                      const { text, label } = copyPayload(p, copyFmt)
                      return (
                        <tr key={p.key}>
                          <td className="font-medium">{p.name || '未命名'}</td>
                          <td>
                            <span className="inline-flex items-center gap-2">
                              <Tag>{p.kind === 'relay' ? '中转' : '直连'}</Tag>
                              {p.rule && (
                                <span className="text-ink-soft">
                                  经规则{' '}
                                  <TextLink tone="ink" to={isAdmin ? `/rules/${p.rule.id}` : `/my/rules/${p.rule.id}`}>{p.rule.name}</TextLink>
                                </span>
                              )}
                            </span>
                          </td>
                          <td><Tag mono>{p.protocol}</Tag></td>
                          <td className="font-mono"><SensText blurred={blurred}>{p.address}</SensText></td>
                          <td className="num"><CopyText text={text}>{label}</CopyText></td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              )}
            </TableScroll>
          </Card>
        )}
      </ResourceView>
    </Page>
  )
}
