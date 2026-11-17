import { useMemo, useState } from 'react'
import { fmtBytes } from '../lib/fmt'
import { enrichRule, hostPortKey, relayExportName, splitEndpoint } from '../lib/landing'
import { batchProgress, probeStateOf } from '../lib/probe'
import { combineResources } from '../lib/resource'
import { useIsMobile } from '../lib/useIsMobile'
import { useLandingCatalog } from '../lib/useLandingCatalog'
import { useResource } from '../lib/useResource'
import { uriToClashYaml } from '../lib/yaml-convert'
import { ExitKindTag, NodeTypeIcon, ProtoTag, SuppressedBadge } from './net'
import { ListItem, Page, PageHeader, SearchInput, SortableTh, TableScroll, Toolbar, nextSort } from './page'
import { useBlur, useCopyFmt } from './prefs'
import { ProbeButton, ProbeControl, useProbes } from './probe'
import { ResourceView } from './ResourceView'
import { RuleDialog, useDeleteRule } from './RuleFormModal'
import { useUser } from './session'
import {
  Button, Card, CardHeader, CardSection, CodeBlock, CopyText, Empty, IconActivity, IconPlus, Menu, SensText, Spinner,
  Tag, TextLink, Tooltip, cx,
} from './ui'

/* 规则页面的读取侧：列表页骨架 RulesListPage（页头 + 工具栏 + 宽屏表格 /
   窄屏列表），以及详情页用的入口卡片与出口展示。
   管理员的列表页与用户的列表页各自只提供：数据怎么取、自己独有的筛选；
   取数状态、落地节点目录的对照、创建 / 编辑 / 删除的接线、连通性测试都在
   这里，两侧因此是同一份。差别由 scope（RuleFormModal.jsx 的 RULE_SCOPES）
   决定。 */

/* ---------- 数据 ---------- */

/**
 * 规则页面的数据：页面自己的规则数据（fetcher）加上当前用户的落地节点目录，
 * 并成一个资源。
 *
 * 两者到齐才算就绪：出口是否显示成落地节点、有没有中转链接、编辑表单按
 * 哪种出口预填，都要对照目录才能得出。
 *
 * @param {{ id: number, role: string }} user
 * @param {() => Promise<any>} fetcher
 * @param {readonly unknown[]} deps
 * @returns {{
 *   resource: { state: import('../lib/resource').Resource<readonly [any, any]>, reload: () => Promise<void> },
 *   reload: () => Promise<void>,
 * }} `resource` 交给 <ResourceView>，它的 reload 重取两份数据（失败后的重试
 * 不需要知道失败的是哪一份）；外层的 `reload` 只重取规则数据，保存或删除
 * 之后用。
 */
export function useRulesWithCatalog(user, fetcher, deps) {
  const rules = useResource(fetcher, deps)
  const catalog = useLandingCatalog(user)
  const reloadAll = async () => { await Promise.all([rules.reload(), catalog.reload()]) }
  return {
    resource: { state: combineResources(rules.state, catalog.state), reload: reloadAll },
    reload: rules.reload,
  }
}

/**
 * 规则的入口地址。
 *
 * 服务端在规则还没有入口时把 entry 填成占位符 “—”（internal/server/shared.go
 * 的 buildRuleView），它不是地址：不能复制给客户端，也不该参与搜索。这里
 * 只认能解析成 host:port 的值，用的是 lib/landing.js 的 relayFor 判断“入口
 * 能否用来改写中转链接”的同一个 splitEndpoint，两处对“有没有入口”的结论
 * 因此一致。
 *
 * @returns {{ primary: string | null, v6: string | null }} v6 只在双栈规则上有值。
 */
function ruleEntries(rule) {
  const real = (s) => (s && splitEndpoint(s) ? s : null)
  return { primary: real(rule.entry), v6: real(rule.entry_v6) }
}

/** 规则出口的 host:port（IPv6 带方括号），与落地节点目录用同一种键。 */
function exitAddress(rule) {
  return rule.exit_host && rule.exit_port ? hostPortKey(rule.exit_host, rule.exit_port) : ''
}

/* 复制中转链接时写进剪贴板的文本：复制格式选了 YAML、且这条链接能转成
   Clash 配置时是 YAML，否则是链接本身。YAML 里的名称用 relayExportName，
   与“我的代理”页导出同一条中转时的名称一致。 */
function relayCopyText({ uri, name }, copyFmt) {
  return (copyFmt === 'yaml' && uriToClashYaml(uri, name)) || uri
}

/** 规则的中转链接（出口是落地节点且规则有入口时才有）；双栈规则有两条。 */
function relayLinks(rule) {
  const link = (uri, v6, family) => uri && { uri, family, name: relayExportName(rule.landing_name, rule.name, v6) }
  return [
    link(rule.relay_uri, false, rule.relay_uri_v6 ? 'v4' : null),
    link(rule.relay_uri_v6, true, 'v6'),
  ].filter(Boolean)
}

/* 出口的文字。用户侧的落地出口显示落地节点的名称；其余情况显示地址，地址
   受脱敏开关控制。 */
function exitText(rule, scope, blurred) {
  if (!scope.admin && rule.exit_kind === 'landing' && rule.landing_name) return rule.landing_name
  return <span className="font-mono"><SensText blurred={blurred}>{exitAddress(rule)}</SensText></span>
}

/**
 * 详情页里的出口：种类标签 + 出口文字。管理员侧在地址之后补上落地节点的名称。
 * @param {{ rule: object, scope: { admin: boolean } }} props
 */
export function RuleExit({ rule, scope }) {
  const blurred = useBlur()
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <ExitKindTag kind={rule.exit_kind} protocol={rule.landing_protocol} />
      {exitText(rule, scope, blurred)}
      {scope.admin && rule.exit_kind === 'landing' && rule.landing_name && <span className="text-ink-mut">{rule.landing_name}</span>}
    </span>
  )
}

/* ---------- 连通性测试 ---------- */

const probeKey = (rule) => String(rule.id)

/* 列表的探测状态。状态表在列表卡片上，不在行里：行因为排序、筛选、宽窄屏
   切换而重新挂载时，结果不丢，也不会重新发起测试。

   batch 是正在进行的“全部测试”所含的 key，没有批次在跑时为 null。批次是
   显式记录的，不从“有没有行正在测”推断 —— 单独测一行不算开始了一个批次。 */
function useRuleProbes() {
  const { table, probe, cancel } = useProbes()
  const [batch, setBatch] = useState(/** @type {string[] | null} */ (null))

  const probeOne = (rule) => probe(probeKey(rule), { ruleId: rule.id })
  const probeAll = (rules) => {
    const keys = rules.map(probeKey)
    setBatch(keys)
    // probe 返回的 Promise 在该次探测落定（或被取消、被新的一次取代）后完成。
    // 停止之后已经发出的请求还会迟一会儿才落定；那时 batch 可能已经是 null
    // 或者是新的一批，所以只在它仍是这一批时才清掉。
    Promise.all(rules.map(probeOne)).then(() => setBatch(current => (current === keys ? null : current)))
  }
  const stop = () => {
    if (batch) cancel(batch)
    setBatch(null)
  }

  return {
    stateOf: (rule) => probeStateOf(table, probeKey(rule)),
    probeOne,
    probeAll,
    stop,
    progress: batch ? batchProgress(table, batch) : null,
  }
}

/* ---------- 列表 ---------- */

const UNSORTED = { col: null, dir: null }

const SORT_VALUE = {
  node: (row) => row.nodeName,
  owner: (row) => row.rule.owner_name || '',
  traffic: (row) => row.rule.total_bytes || 0,
}

function toRow(rule, nodeById) {
  const node = nodeById.get(rule.node_id)
  const nodeName = node?.name || `#${rule.node_id}`
  return {
    rule,
    node,
    nodeName,
    entries: ruleEntries(rule),
    // 入口显示成“节点名:端口”而不是真实地址；复制出去的仍是真实的 host:port。
    entryLabel: rule.entry_listen_port ? `${nodeName}:${rule.entry_listen_port}` : nodeName,
  }
}

function matchesSearch(row, q) {
  const { rule, entries } = row
  return [rule.name, row.nodeName, entries.primary, entries.v6, exitAddress(rule), rule.landing_name, rule.owner_name]
    .some(v => v && v.toLowerCase().includes(q))
}

function sortRows(rows, sort) {
  if (!sort.col) return rows
  const valueOf = SORT_VALUE[sort.col]
  const sign = sort.dir === 'asc' ? 1 : -1
  return [...rows].sort((a, b) => {
    const va = valueOf(a)
    const vb = valueOf(b)
    return sign * (typeof va === 'number' ? va - vb : va.localeCompare(vb))
  })
}

/**
 * 规则列表页的骨架：页头（数量 + “创建规则”）、列表卡片、创建 / 复制 / 编辑
 * 弹窗与删除。
 *
 * 页面提供：
 * - `load`：取回 `{ rules, nodes, bindings, show_rate? }`（可以带页面自己的
 *   其它字段，原样传给 filters / narrow）。nodes 与 bindings 供表单使用；
 *   show_rate 明确为 false 时表单里不显示倍率。
 * - 搜索词由页面持有（`search` / `onSearch`）：放 URL 还是放组件状态是页面
 *   自己的决定。`onReset` 清空搜索以及页面自己的筛选。
 * - `filters(data)`：页面独有的筛选控件，放进工具栏；`narrow(rules, data)`：
 *   按这些筛选条件收窄规则。搜索词的匹配不在 narrow 里，由列表卡片统一做。
 * - `linkState`：随“进入详情页”的链接带过去的路由 state。
 *
 * @param {{
 *   scope: { admin: boolean, api: string, path: string, createdId: (res: any) => number },
 *   title: string,
 *   load: () => Promise<{ rules: object[], nodes: object[], bindings: object[], show_rate?: boolean }>,
 *   search: string,
 *   onSearch: (value: string) => void,
 *   onReset: () => void,
 *   filters?: (data: any) => import('react').ReactNode,
 *   narrow?: (rules: object[], data: any) => object[],
 *   linkState?: unknown,
 * }} props
 */
export function RulesListPage({ scope, title, load, search, onSearch, onReset, filters, narrow, linkState }) {
  const { user } = useUser()
  const { resource, reload } = useRulesWithCatalog(user, load, [])
  // 互斥的弹窗状态：null | { kind: 'create' } | { kind: 'copy', rule } | { kind: 'edit', rule }
  const [dialog, setDialog] = useState(null)
  const loaded = resource.state.status === 'ready' ? resource.state.data[0] : null

  return (
    <Page fill>
      <PageHeader
        title={title}
        meta={loaded ? `共 ${loaded.rules.length} 条` : null}
        actions={
          // 表单要用节点、绑定关系与落地节点目录：数据就绪之前不能打开。弹窗
          // 本身也只渲染在下面的就绪分支里。
          <Button variant="primary" icon={<IconPlus />} disabled={!loaded} onClick={() => setDialog({ kind: 'create' })}>
            创建规则
          </Button>
        }
      />
      <ResourceView resource={resource}>
        {([data, catalog]) => (
          <RulesListBody
            scope={scope}
            data={data}
            catalog={catalog}
            userId={user.id}
            reload={reload}
            dialog={dialog}
            onDialog={setDialog}
            search={search}
            onSearch={onSearch}
            onReset={onReset}
            filters={filters}
            narrow={narrow}
            linkState={linkState}
          />
        )}
      </ResourceView>
    </Page>
  )
}

function RulesListBody({ scope, data, catalog, userId, reload, dialog, onDialog, search, onSearch, onReset, filters, narrow, linkState }) {
  const rules = useMemo(
    () => data.rules.map(rule => enrichRule(rule, catalog.landingByKey)),
    [data.rules, catalog.landingByKey],
  )
  const deletion = useDeleteRule(scope, reload)

  return (
    <>
      <RulesCard
        scope={scope}
        rules={narrow ? narrow(rules, data) : rules}
        total={rules.length}
        nodes={data.nodes}
        search={search}
        onSearch={onSearch}
        onReset={onReset}
        filters={filters?.(data)}
        linkState={linkState}
        deletingId={deletion.pendingId}
        onEdit={rule => onDialog({ kind: 'edit', rule })}
        onCopy={rule => onDialog({ kind: 'copy', rule })}
        onDelete={deletion.request}
      />
      {dialog && (
        <RuleDialog
          dialog={dialog}
          scope={scope}
          nodes={data.nodes}
          bindings={data.bindings}
          catalog={catalog}
          userId={userId}
          showRate={data.show_rate}
          onSaved={reload}
          onClose={() => onDialog(null)}
        />
      )}
    </>
  )
}

/* 规则列表卡片：工具栏（搜索、筛选、全部测试）+ 规则行。宽屏是表格，窄屏是
   列表；排序与探测状态放在这一层，两种布局之间切换、筛选到没有结果再回来
   时都还在。

   `rules` 是页面按自己的筛选条件收窄后的规则，搜索词的匹配在这里做；
   `total` 是收窄之前的总数，用来区分“还没有规则”与“没有匹配的规则”。 */
function RulesCard({ scope, rules, total, nodes, search, onSearch, onReset, filters, linkState, deletingId, onEdit, onCopy, onDelete }) {
  const isMobile = useIsMobile()
  const blurred = useBlur()
  const copyFmt = useCopyFmt()
  const probes = useRuleProbes()
  const [sort, setSort] = useState(UNSORTED)
  const onSort = (col) => setSort(s => nextSort(s, col))

  const nodeById = useMemo(() => new Map(nodes.map(n => [n.id, n])), [nodes])
  const rows = useMemo(() => {
    const q = search.trim().toLowerCase()
    const all = rules.map(rule => toRow(rule, nodeById))
    return sortRows(q ? all.filter(row => matchesSearch(row, q)) : all, sort)
  }, [rules, nodeById, search, sort])

  const actionsOf = (rule) => [
    { label: '编辑', onSelect: () => onEdit(rule) },
    { label: '复制为新规则', onSelect: () => onCopy(rule) },
    { label: '删除', danger: true, onSelect: () => onDelete(rule) },
  ]
  const probeOf = (rule) => <ProbeControl state={probes.stateOf(rule)} onProbe={() => probes.probeOne(rule)} />
  const detailOf = (rule) => `${scope.path}/${rule.id}`

  return (
    <Card fill>
      <Toolbar>
        <SearchInput value={search} onChange={onSearch} placeholder="搜索名称、节点、入口或出口…" />
        {filters}
        <div className="ml-auto">
          {probes.progress ? (
            <Button icon={<Spinner size={14} />} onClick={probes.stop}>
              停止测试（{probes.progress.total - probes.progress.pending}/{probes.progress.total}）
            </Button>
          ) : (
            <Button icon={<IconActivity />} disabled={rows.length === 0} onClick={() => probes.probeAll(rows.map(row => row.rule))}>
              全部测试
            </Button>
          )}
        </div>
      </Toolbar>

      {total === 0 ? (
        <Empty title="还没有规则" desc="创建一条规则，把入口节点上的一个端口转发到你的出口地址或落地节点。" />
      ) : rows.length === 0 ? (
        <Empty title="没有匹配的规则" desc="换一个关键词，或者清除当前的搜索与筛选条件。">
          <Button onClick={onReset}>{filters ? '清除搜索与筛选' : '清除搜索'}</Button>
        </Empty>
      ) : isMobile ? (
        <div className="min-h-0 flex-1 overflow-auto">
          {rows.map(row => (
            <ListItem key={row.rule.id} className={cx('relative', deletingId === row.rule.id && 'opacity-50')}>
              <div className="flex items-center gap-3">
                {/* after 伪元素把这个链接的可点区域铺满整项（ListItem 是它的
                    定位祖先）。行里的按钮不是链接的后代，而是排在它之后、自带
                    定位的兄弟元素：它们盖在这层之上，点它们不会经过链接。 */}
                <TextLink to={detailOf(row.rule)} state={linkState} tone="ink" className="min-w-0 flex-1 font-medium after:absolute after:inset-0">
                  <span className="min-w-0 truncate">{row.rule.name}</span>
                </TextLink>
                <span className="relative flex-none"><Menu size="sm" items={actionsOf(row.rule)} /></span>
              </div>
              {row.rule.suppressed && <div className="mt-1.5"><SuppressedBadge reason={row.rule.suppressed} detailed /></div>}
              <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-small text-ink-soft">
                <ProtoTag proto={row.rule.proto} />
                <NodeName row={row} />
                {scope.admin && row.rule.owner_name && <span>{row.rule.owner_name}</span>}
                <span>{fmtBytes(row.rule.total_bytes || 0)}</span>
              </div>
              <div className="mt-1 flex items-center gap-1.5 text-small text-ink-mut">
                <span className="min-w-0 truncate">{row.entries.primary ? row.entryLabel : '入口未分配'}</span>
                <span className="flex-none">→</span>
                <span className="min-w-0 truncate text-ink-soft">{exitText(row.rule, scope, blurred)}</span>
              </div>
              <div className="relative mt-2 w-fit">{probeOf(row.rule)}</div>
            </ListItem>
          ))}
        </div>
      ) : (
        <TableScroll>
          <table className="tbl">
            <thead>
              <tr>
                {scope.admin && <th>ID</th>}
                <th>名称</th>
                <SortableTh col="node" sort={sort} onSort={onSort}>入口节点</SortableTh>
                <th>入口 / 出口</th>
                <th>协议</th>
                {scope.admin && <SortableTh col="owner" sort={sort} onSort={onSort}>所有者</SortableTh>}
                <th>备注</th>
                <SortableTh col="traffic" sort={sort} onSort={onSort} align="right">流量</SortableTh>
                <th>连通性</th>
                <th className="num">操作</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(row => (
                <tr key={row.rule.id} className={deletingId === row.rule.id ? 'opacity-50' : undefined}>
                  {scope.admin && <td className="font-mono text-ink-mut">#{row.rule.id}</td>}
                  <td>
                    {/* 徽标放在名称下面而不是右边：并排会把这一列撑宽，挤窄右边的入口/出口列。 */}
                    <span className="inline-flex flex-col items-start gap-1">
                      <TextLink to={detailOf(row.rule)} state={linkState} tone="ink" className="font-medium">{row.rule.name}</TextLink>
                      <SuppressedBadge reason={row.rule.suppressed} />
                    </span>
                  </td>
                  <td className="text-ink-soft"><NodeName row={row} /></td>
                  <td>
                    <div className="flex flex-col gap-1">
                      <EntryLines row={row} />
                      <ExitLines rule={row.rule} scope={scope} blurred={blurred} copyFmt={copyFmt} />
                    </div>
                  </td>
                  <td><ProtoTag proto={row.rule.proto} /></td>
                  {scope.admin && <td className="text-ink-soft">{row.rule.owner_name || '--'}</td>}
                  <td className="text-ink-soft"><Comment text={row.rule.comment} /></td>
                  <td className="num text-ink-soft">{fmtBytes(row.rule.total_bytes || 0)}</td>
                  <td>{probeOf(row.rule)}</td>
                  <td className="num"><Menu size="sm" items={actionsOf(row.rule)} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableScroll>
      )}
    </Card>
  )
}

function NodeName({ row }) {
  const layers = row.rule.via_node_ids?.length || 0
  return (
    <span className="inline-flex items-center gap-1.5">
      <NodeTypeIcon type={row.node?.node_type} />
      {row.nodeName}
      {layers > 0 && <span className="text-caption text-ink-mut">+{layers} 层</span>}
    </span>
  )
}

function EntryLines({ row }) {
  const { primary, v6 } = row.entries
  if (!primary) {
    return (
      <div className="flex items-center gap-1.5">
        <Tag>入口</Tag>
        <span className="text-ink-mut">未分配</span>
      </div>
    )
  }
  return (
    <>
      <div className="flex items-center gap-1.5">
        <Tag>{v6 ? '入口 v4' : '入口'}</Tag>
        <CopyText text={primary}>{row.entryLabel}</CopyText>
      </div>
      {v6 && (
        <div className="flex items-center gap-1.5">
          <Tag>入口 v6</Tag>
          <CopyText text={v6}>{row.entryLabel}</CopyText>
        </div>
      )}
    </>
  )
}

/* 出口行。规则有中转链接时，点出口文字复制的是中转链接（双栈规则 v4 / v6
   各一行）；没有时只显示出口。 */
function ExitLines({ rule, scope, blurred, copyFmt }) {
  const tag = <ExitKindTag kind={rule.exit_kind} protocol={rule.landing_protocol} />
  const text = exitText(rule, scope, blurred)
  const links = relayLinks(rule)
  if (links.length === 0) {
    return <div className="flex items-center gap-1.5 text-ink-soft">{tag}{text}</div>
  }
  return links.map(link => (
    <div key={link.family ?? 'single'} className="flex items-center gap-1.5 text-ink-soft">
      {tag}
      {link.family && <span className="text-caption text-ink-mut">{link.family}</span>}
      <CopyText text={relayCopyText(link, copyFmt)}>{text}</CopyText>
    </div>
  ))
}

/* 备注列按固定宽度截断；长到会被截断的备注可以悬停看全文，完整内容在详情页。 */
function Comment({ text }) {
  if (!text) return <span className="text-ink-mut">--</span>
  if (text.length <= 12) return text
  return <Tooltip content={text} className="block max-w-40 truncate">{text}</Tooltip>
}

/* ---------- 详情页的入口卡片 ---------- */

function Endpoint({ label, tag, copyText, note, children }) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center gap-2 text-small text-ink-mut">
        {label}
        {tag}
      </div>
      <CodeBlock text={copyText}>{children}</CodeBlock>
      {note && <p className="text-caption text-ink-mut">{note}</p>}
    </div>
  )
}

/**
 * 详情页的“入口”卡片：客户端要填的入口地址，以及出口是落地节点时的中转
 * 链接。规则还没有入口时显示空状态，不显示任何可复制的东西。
 *
 * @param {{ rule: object, entryNodeName?: string }} props `entryNodeName` 是
 * 实际承接入口的那个节点（入口是组合节点时为它的第一个成员），页面认得出它
 * 时才传。
 */
export function RuleEntryCard({ rule, entryNodeName }) {
  const blurred = useBlur()
  const copyFmt = useCopyFmt()
  const { primary, v6 } = ruleEntries(rule)
  const nodeTag = entryNodeName ? <Tag>{entryNodeName}</Tag> : null

  return (
    <Card>
      <CardHeader title="入口" description="填进客户端的地址" actions={<ProbeButton request={{ ruleId: rule.id }} />} />
      {primary ? (
        <CardSection className="flex flex-col gap-4">
          <Endpoint label={v6 ? '入口地址 v4' : '入口地址'} tag={nodeTag} copyText={primary}>
            <SensText blurred={blurred}>{primary}</SensText>
          </Endpoint>
          {v6 && (
            <Endpoint label="入口地址 v6" tag={nodeTag} copyText={v6}>
              <SensText blurred={blurred}>{v6}</SensText>
            </Endpoint>
          )}
          {relayLinks(rule).map((link) => {
            const { uri, family } = link
            const copyText = relayCopyText(link, copyFmt)
            return (
              <Endpoint
                key={family ?? 'single'}
                label={family ? `中转链接 ${family}` : '中转链接'}
                copyText={copyText}
                note={copyText !== uri ? '复制得到的是这条链接对应的 Clash YAML 配置。' : undefined}
              >
                <SensText blurred={blurred}>{uri}</SensText>
              </Endpoint>
            )
          })}
        </CardSection>
      ) : (
        <Empty title="还没有入口地址" desc="入口节点分配了端口并配置好中转地址之后，这里会显示可以填进客户端的地址。" />
      )}
    </Card>
  )
}
