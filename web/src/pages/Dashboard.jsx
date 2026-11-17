import { useState } from 'react'
import { api } from '../lib/api'
import { fmtBytes, fmtTime } from '../lib/fmt'
import { useIsMobile } from '../lib/useIsMobile'
import { useResource } from '../lib/useResource'
import { NodeStatus, NodeTypeTag } from '../components/net'
import { ListItem, Page, PageHeader, SortableTh, TableScroll, nextSort } from '../components/page'
import { useBlur } from '../components/prefs'
import { ProxyURIEditor } from '../components/ProxyURIEditor'
import { ResourceView } from '../components/ResourceView'
import { Button, Card, CardHeader, Empty, Meter, SensText, Stat, TextLink } from '../components/ui'

export default function Dashboard() {
  const dashboard = useResource(() => api.get('/dashboard'), [])
  return (
    // 两张卡片并排时（xl 起）整页占满视口高度，各自在内部滚动；上下堆叠时
    // 按内容自然增高。
    <Page className="xl:h-full xl:min-h-0">
      <PageHeader title="概览" />
      <ResourceView resource={dashboard}>
        {data => <Overview data={data} />}
      </ResourceView>
    </Page>
  )
}

/** 数值最大的那一项；全为 0 时没有“最多”可言，返回 null。 */
function topBy(rows, valueOf) {
  let best = null
  for (const row of rows) {
    const value = valueOf(row)
    if (value > 0 && (!best || value > best.value)) best = { row, value }
  }
  return best
}

function Overview({ data }) {
  const { nodes = [], node_raw_traffic = {}, rule_count = 0, rule_count_by_node = {}, total_bytes = 0, user_count = 0 } = data
  const rulesOf = n => Number(rule_count_by_node[n.id] || 0)
  const trafficOf = n => Number(node_raw_traffic[n.id] || 0)

  const online = nodes.filter(n => !n.disabled && n.online === 1)
  const down = nodes.filter(n => n.disabled || n.online !== 1)
  const topRules = topBy(nodes, rulesOf)
  const topTraffic = topBy(nodes, trafficOf)
  // fmtBytes 给出 “1.2 GB”；数值与单位分开排版。
  const [totalValue, totalUnit] = fmtBytes(total_bytes).split(' ')

  return (
    <>
      <Card className="grid flex-none grid-cols-2 gap-x-6 gap-y-5 p-5 lg:grid-cols-4">
        <Stat
          label="在线节点"
          value={online.length}
          unit={`/ ${nodes.length}`}
          hint={down.length
            ? `${down.slice(0, 2).map(n => n.name).join('、')}${down.length > 2 ? ` 等 ${down.length} 个` : ''}不在线`
            : nodes.length ? '全部在线' : undefined}
        />
        <Stat label="规则" value={rule_count} hint={topRules ? `最多：${topRules.row.name}（${topRules.value} 条）` : undefined} />
        <Stat label="总流量" value={totalValue} unit={totalUnit} hint={topTraffic ? `最多：${topTraffic.row.name}（${fmtBytes(topTraffic.value)}）` : undefined} />
        <Stat label="用户" value={user_count} />
      </Card>

      {/* 节点表是这一页的主体，拿走剩余宽度；右栏固定为窄屏卡片的宽度，
          “我添加的节点”在那个宽度下本来就要能用。并排从 xl 才开始：再窄
          的话节点表放不下它的列。 */}
      <div className="grid gap-5 xl:min-h-0 xl:flex-1 xl:grid-cols-[minmax(0,1fr)_22.5rem] xl:grid-rows-[minmax(0,1fr)]">
        <div className="flex min-h-0 flex-col">
          <NodesCard nodes={nodes} rulesOf={rulesOf} trafficOf={trafficOf} />
        </div>
        <div className="flex min-h-0 flex-col">
          <ProxyURIEditor fill />
        </div>
      </div>
    </>
  )
}

const UNSORTED = { col: null, dir: null }

/** 节点的入口地址；组合节点等没有地址的返回空串。 */
const entryOf = n => n.relay_host || n.address || ''

function NodesCard({ nodes, rulesOf, trafficOf }) {
  const blurred = useBlur()
  const isMobile = useIsMobile()
  const [sort, setSort] = useState(UNSORTED)
  const maxTraffic = Math.max(0, ...nodes.map(trafficOf))

  // 不排序时保持服务端给出的顺序。
  const sortValue = sort.col === 'rules' ? rulesOf : trafficOf
  const sign = sort.dir === 'asc' ? 1 : -1
  const rows = sort.col ? [...nodes].sort((a, b) => sign * (sortValue(a) - sortValue(b))) : nodes
  const onSort = col => setSort(s => nextSort(s, col))

  return (
    <Card fill>
      <CardHeader
        title="节点"
        description={nodes.length ? `共 ${nodes.length} 个` : undefined}
        actions={<Button size="sm" to="/nodes">管理节点</Button>}
      />
      {nodes.length === 0 ? (
        <Empty title="还没有节点" desc="添加节点后，这里会显示它们的在线状态、规则数与流量。" />
      ) : isMobile ? (
        <div>
          {rows.map(n => (
            <ListItem key={n.id} to={`/nodes/${n.id}`}>
              <div className="flex items-center justify-between gap-3">
                <span className="truncate font-medium text-ink">{n.name}</span>
                <NodeStatus node={n} />
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-small text-ink-soft">
                <NodeTypeTag type={n.node_type} />
                <span>{rulesOf(n)} 条规则</span>
                <span>{fmtBytes(trafficOf(n))}</span>
                <span className="text-ink-mut">{fmtTime(n.last_seen)}</span>
              </div>
            </ListItem>
          ))}
        </div>
      ) : (
        <TableScroll>
          <table className="tbl">
            <thead>
              <tr>
                <th>节点</th>
                <th>类型</th>
                <SortableTh col="rules" sort={sort} onSort={onSort} align="right">规则</SortableTh>
                <SortableTh col="traffic" sort={sort} onSort={onSort} align="right">流量</SortableTh>
                <th>状态</th>
                <th>心跳</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(n => (
                <tr key={n.id}>
                  {/* 入口地址作为名称下的第二行，而不是单独一列：流量格本来就是
                      “数值 + 条形”两行，行高不变，表格却能少一列的宽度。 */}
                  <td>
                    <TextLink to={`/nodes/${n.id}`} tone="ink" className="font-medium">{n.name}</TextLink>
                    {entryOf(n) && (
                      <div className="mt-0.5 font-mono text-caption text-ink-mut">
                        <SensText blurred={blurred}>{entryOf(n)}</SensText>
                      </div>
                    )}
                  </td>
                  <td><NodeTypeTag type={n.node_type} /></td>
                  <td className="num">{rulesOf(n)}</td>
                  <td className="num">
                    {/* 条形是相对值：以流量最大的节点为满格，用来比较节点之间的占比。 */}
                    <div className="ml-auto flex w-20 flex-col items-end gap-1">
                      <span>{fmtBytes(trafficOf(n))}</span>
                      <Meter value={trafficOf(n)} max={maxTraffic} label={`${n.name} 的流量占比`} />
                    </div>
                  </td>
                  <td><NodeStatus node={n} /></td>
                  <td className="whitespace-nowrap text-small text-ink-mut">{fmtTime(n.last_seen)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      )}
    </Card>
  )
}
