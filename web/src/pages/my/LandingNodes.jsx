import { useMemo, useState } from 'react'
import { fmtTrafficGB } from '../../lib/fmt'
import { hostPortKey } from '../../lib/landing'
import { useLandingCatalog } from '../../lib/useLandingCatalog'
import { Page, PageHeader, SearchInput, TableScroll, Toolbar } from '../../components/page'
import { useBlur } from '../../components/prefs'
import { ResourceView } from '../../components/ResourceView'
import { useUser } from '../../components/session'
import { Alert, Badge, Button, Card, CopyText, Empty, IconRefresh, SensText, Tag, useAction, useToast } from '../../components/ui'

/* 落地节点页：当前用户可以选作规则出口的节点 —— 目录里的 landing，即生效
   角色含“落地”的节点。来源可以是管理员分配，也可以是用户自己添加（手动
   填写或订阅）；同一个地址两边都有时，目录保留用户自己的那份。 */
export default function MyLandingNodes() {
  const { user } = useUser()
  const catalog = useLandingCatalog(user)
  const toast = useToast()
  const data = catalog.state.status === 'ready' ? catalog.state.data : null

  // 管理员分配的来源里有订阅时，让面板重新抓取一次。面板抓取失败时接口
  // 仍然成功返回，只是带 stale —— 那是上一次的结果，不能报成“已刷新”。
  const refresh = useAction(async () => {
    const { stale } = await catalog.refreshAssigned()
    if (stale) toast('管理员分配的订阅没有刷新成功，仍显示上次的结果', 'error')
    else toast('分配的节点已刷新')
  })

  return (
    <Page fill>
      <PageHeader
        title="落地节点"
        meta={<>
          {data && <span>共 {data.landing.length} 个</span>}
          <span>可以选作规则出口的节点：管理员分配给你的落地节点，以及你自己添加并标记为“落地”的节点。</span>
        </>}
        actions={data?.hasDynamic && (
          <Button icon={<IconRefresh />} loading={refresh.pending} onClick={() => refresh.run()}>刷新分配的节点</Button>
        )}
      />
      <ResourceView resource={catalog}>
        {ready => <LandingList data={ready} />}
      </ResourceView>
    </Page>
  )
}

function LandingList({ data }) {
  const blurred = useBlur()
  const [search, setSearch] = useState('')

  /* 配额账本按出口地址记，与这一行最终用的是谁的链接无关：用户自己也填了
     一份管理员分配的节点时，目录里留下的是用户那份（source 为 local），
     它的用量仍要按地址查到。键用 hostPortKey —— 目录去重用的就是它，手拼
     host:port 在 IPv6 地址上会与它对不上。 */
  const ledger = useMemo(
    () => new Map(data.serverNodes.map(n => [hostPortKey(n.host, n.port), n])),
    [data.serverNodes],
  )

  const q = search.trim().toLowerCase()
  const rows = data.landing.map(node => ({ node, address: hostPortKey(node.host, node.port) }))
  const shown = q
    ? rows.filter(({ node, address }) => [node.name, node.protocol, address].some(v => (v || '').toLowerCase().includes(q)))
    : rows

  return (
    <>
      {data.stale && (
        <Alert tone="warn" className="flex-none">
          管理员分配的订阅上次没有刷新成功，下面显示的是此前的结果。
        </Alert>
      )}
      <Card fill>
        <Toolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="搜索名称、协议、地址…" />
        </Toolbar>
        <TableScroll>
          {rows.length === 0 ? (
            <Empty
              title="还没有落地节点"
              desc="落地节点来自管理员为你分配的节点，或你在概览页“我添加的节点”里添加并标记为“落地”的节点。有了落地节点，创建规则时就可以把它选作出口。"
            >
              <Button to="/my">去概览页添加</Button>
            </Empty>
          ) : shown.length === 0 ? (
            <Empty title="没有匹配的落地节点" desc="换一个关键词试试。" />
          ) : (
            <table className="tbl">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>协议</th>
                  <th>地址</th>
                  <th>来源</th>
                  <th className="num">已用 / 配额</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {shown.map(({ node, address }) => {
                  const usage = ledger.get(address)
                  return (
                    <tr key={address}>
                      <td className="font-medium">{node.name || '未命名'}</td>
                      <td><Tag mono>{node.protocol}</Tag></td>
                      <td className="font-mono"><SensText blurred={blurred}>{address}</SensText></td>
                      <td><Tag>{node.source === 'local' ? '自己添加' : '管理员分配'}</Tag></td>
                      <td className="num">
                        {usage ? (
                          <span className="inline-flex items-center justify-end gap-2">
                            {usage.exceeded && <Badge tone="danger">已超额</Badge>}
                            {fmtTrafficGB(usage.used_bytes, usage.quota_bytes)}
                          </span>
                        ) : (
                          <span className="text-ink-mut">—</span>
                        )}
                      </td>
                      <td className="num"><CopyText text={node.uri}>复制链接</CopyText></td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </TableScroll>
      </Card>
    </>
  )
}
