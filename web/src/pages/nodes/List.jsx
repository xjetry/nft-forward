import { useEffect, useId, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, fmtTime } from '../../lib/fmt'
import { NODE_TYPE_COMPOSITE, isComposite } from '../../lib/nodeType'
import { SEAT_ENTRY, SEAT_VIA } from '../../lib/seats'
import { upgradeAllReport } from '../../lib/upgrade'
import { useIsMobile } from '../../lib/useIsMobile'
import { useResource } from '../../lib/useResource'
import { fmtSpeed, useSpeed } from '../../lib/useSpeed'
import { NodeStackTags, NodeStatus } from '../../components/net'
import { ListItem, Page, PageHeader, ReorderHandle, SearchInput, SortableTh, TableScroll, Toolbar, nextSort } from '../../components/page'
import { ResourceView } from '../../components/ResourceView'
import {
  Alert, Badge, Button, Card, Empty, Field, IconButton, IconPlus, IconRefresh, Input, Menu, Modal,
  ModalActions, Segmented, Tag, TextLink, ToggleChip, cx, useAction, useConfirm, useToast,
} from '../../components/ui'
import {
  GrantUserSelect, HopListEditor, hopRowsError, hopRowsPayload, newHopRow, removeNode, useGrantableUsers,
} from './common'

/**
 * 存在 Web Storage 里的界面状态。读不出、或读出的值不合法时用 fallback。
 * @template T
 * @param {Storage} storage
 * @param {string} key
 * @param {T} fallback
 * @param {(value: unknown) => boolean} valid
 * @returns {[T, (next: T | ((prev: T) => T)) => void]}
 */
function useStoredState(storage, key, fallback, valid) {
  const [value, setValue] = useState(() => {
    try {
      const parsed = JSON.parse(storage.getItem(key) ?? 'null')
      return valid(parsed) ? parsed : fallback
    } catch {
      return fallback
    }
  })
  useEffect(() => {
    try { storage.setItem(key, JSON.stringify(value)) } catch { /* 存不进去不影响当前页面 */ }
  }, [storage, key, value])
  return [value, setValue]
}

const UNSORTED = { col: null, dir: null }
const isSort = v => !!v && typeof v === 'object' && (v.col === null || v.col === 'traffic' || v.col === 'speed')

/** 节点的用途位；没有这个字段的节点按“入口”处理，与服务端的缺省一致。 */
const seatsOf = node => node.roles ?? SEAT_ENTRY

export default function NodeList() {
  const nodes = useResource(() => api.get('/nodes'), [])
  // null | 'add' | 'composite'
  const [dialog, setDialog] = useState(null)
  const toast = useToast()
  const confirm = useConfirm()
  const ready = nodes.state.status === 'ready' ? nodes.state.data : null
  const latest = ready?.latest_agent_version

  const resyncAll = useAction(async () => {
    try {
      const d = await api.post('/nodes/resync-all')
      if (d?.failed) toast(`已同步 ${d.synced} 个节点，${d.failed} 个失败（失败原因见各节点的状态）`, 'error')
      else toast(`已向 ${d?.synced ?? 0} 个节点重新推送规则`)
    } finally {
      await nodes.reload()
    }
  })
  const upgradeAll = useAction(async () => {
    const agreed = await confirm({
      title: '升级版本落后的节点',
      message: `向所有已启用、Agent 版本低于 ${latest} 的节点推送 ${latest}？节点替换程序后会重启 Agent，转发会短暂中断。`,
      confirmText: '升级',
    })
    if (!agreed) return
    try {
      const report = upgradeAllReport(await api.post('/nodes/upgrade-all'))
      if (report.incomplete) toast(report.text, 'error')
      else toast(report.text)
    } finally {
      await nodes.reload()
    }
  })

  return (
    <Page fill>
      <PageHeader
        title="节点"
        meta={ready && <>
          <span>共 {(ready.nodes || []).length} 个</span>
          {latest && <span>面板版本 <span className="font-mono">{latest}</span></span>}
        </>}
        actions={<>
          <Menu
            label="批量操作"
            items={[
              { label: '向所有节点重新推送规则', disabled: resyncAll.pending, onSelect: () => resyncAll.run() },
              { label: '升级版本落后的节点', disabled: !latest || upgradeAll.pending, onSelect: () => upgradeAll.run() },
            ]}
          />
          <Button onClick={() => setDialog('composite')}>创建组合节点</Button>
          <Button variant="primary" icon={<IconPlus />} onClick={() => setDialog('add')}>添加节点</Button>
        </>}
      />
      <ResourceView resource={nodes}>
        {data => <NodeTable data={data} resource={nodes} onAdd={() => setDialog('add')} onComposite={() => setDialog('composite')} />}
      </ResourceView>

      {dialog === 'add' && <AddNodeModal onClose={() => setDialog(null)} />}
      {dialog === 'composite' && ready && <CompositeNodeModal nodes={ready.nodes || []} onClose={() => setDialog(null)} />}
    </Page>
  )
}

function Speed({ speed }) {
  if (!speed) return <span className="text-ink-mut">--</span>
  return <span className="whitespace-nowrap">↑ {fmtSpeed(speed.up)}　↓ {fmtSpeed(speed.down)}</span>
}

function SeatTags({ node }) {
  const seats = seatsOf(node)
  return (
    <>
      {(seats & SEAT_ENTRY) !== 0 && <Tag>入口</Tag>}
      {(seats & SEAT_VIA) !== 0 && <Tag>中转</Tag>}
    </>
  )
}

function NodeTable({ data, resource, onAdd, onComposite }) {
  const { nodes = [], node_raw_traffic: rawTraffic = {} } = data
  // 哪些节点的 Agent 版本低于面板，由服务端判定（与批量升级选对象用的是同一个
  // 判定），这里只负责标出来。
  const outdated = useMemo(() => new Set(data.outdated_node_ids || []), [data.outdated_node_ids])
  const { reload, patch } = resource
  const navigate = useNavigate()
  const toast = useToast()
  const confirm = useConfirm()
  const isMobile = useIsMobile()
  const speeds = useSpeed()

  // 页签跨会话记住；搜索词、用途筛选与排序只在本次会话内保留：进详情再返回时
  // 还在，但隔天打开不会带着旧的筛选让列表看起来缺节点。
  const [tab, setTab] = useStoredState(localStorage, 'nodes.tab', 'single', v => v === 'single' || v === 'composite')
  const [search, setSearch] = useStoredState(sessionStorage, 'nodes.search', '', v => typeof v === 'string')
  // 用途筛选的位掩码，与节点的 roles 同构；0 = 不筛选，两个都选 = 要求兼具。
  const [seatFilter, setSeatFilter] = useStoredState(sessionStorage, 'nodes.seatFilter', 0, v => v === 0 || v === 1 || v === 2 || v === 3)
  const [sort, setSort] = useStoredState(sessionStorage, 'nodes.sort', UNSORTED, isSort)
  // 按速度排序时用点击那一刻的速度：速度每秒都在变，按实时值排序行会不停换位。
  const [speedSnap, setSpeedSnap] = useState(null)
  const [dragId, setDragId] = useState(null)

  const single = nodes.filter(n => !isComposite(n))
  const composite = nodes.filter(n => isComposite(n))
  const inTab = tab === 'composite' ? composite : single
  const q = search.trim().toLowerCase()
  const matched = inTab
    .filter(n => !seatFilter || (seatsOf(n) & seatFilter) === seatFilter)
    .filter(n => !q || (n.name || '').toLowerCase().includes(q))

  const trafficOf = n => Number(rawTraffic[n.id] || 0)
  const speedOf = (n) => {
    const s = (speedSnap || speeds)[n.id]
    return s ? s.up + s.down : 0
  }
  const sortValue = sort.col === 'speed' ? speedOf : trafficOf
  const sign = sort.dir === 'asc' ? 1 : -1
  const rows = sort.col ? [...matched].sort((a, b) => sign * (sortValue(a) - sortValue(b))) : matched
  const onSort = (col) => {
    if (col === 'speed') setSpeedSnap({ ...speeds })
    setSort(s => nextSort(s, col))
  }

  /* 调整顺序。保存接口收的是全部节点的完整顺序，这里用“当前可见的行”拼出它，
     所以只有在可见行就是这个页签的全部节点、且按保存的顺序排列时才允许调序：
     有筛选时被筛掉的节点会从顺序里丢失，有排序时会把排序结果存成顺序。 */
  const reorder = useAction(async (list) => {
    const other = tab === 'composite' ? single : composite
    const ordered = tab === 'composite' ? [...other, ...list] : [...list, ...other]
    patch(d => ({ ...d, nodes: ordered }))
    try {
      await api.post('/nodes/reorder', { ids: ordered.map(n => n.id) })
      toast('顺序已保存')
    } catch (err) {
      // 屏幕上已经是新顺序；没存上就换回服务端的顺序。
      await reload()
      throw err
    }
  })
  // 排序、搜索或筛选生效时，屏幕上的顺序不是保存的顺序，调序没有意义。
  const canReorder = !sort.col && !q && !seatFilter
  const moveTo = (id, to) => {
    const from = rows.findIndex(n => n.id === id)
    if (from < 0 || from === to) return
    const list = [...rows]
    list.splice(to, 0, ...list.splice(from, 1))
    reorder.run(list)
  }

  const resync = useAction(async (id) => {
    try {
      await api.post(`/nodes/${id}/resync`)
      toast('已重新同步')
    } finally {
      // 下发失败时服务端会把原因记到节点上，所以成败都刷新。
      await reload()
    }
  })
  const remove = useAction(async (id) => {
    if (!(await removeNode(id, confirm))) return
    toast('节点已删除')
    await reload()
  })

  const actionsOf = (n, i) => [
    ...(canReorder && rows.length > 1 ? [
      { label: '移到最前', disabled: i === 0, onSelect: () => moveTo(n.id, 0) },
      { label: '移到最后', disabled: i === rows.length - 1, onSelect: () => moveTo(n.id, rows.length - 1) },
    ] : []),
    { label: '删除节点', danger: true, onSelect: () => remove.run(n.id) },
  ]

  let body
  if (nodes.length === 0) {
    body = (
      <Empty title="还没有节点" desc="添加节点后会得到一条安装命令，在目标机器上执行，它就会连到面板。">
        <Button icon={<IconPlus />} onClick={onAdd}>添加节点</Button>
      </Empty>
    )
  } else if (inTab.length === 0) {
    body = tab === 'composite' ? (
      <Empty title="还没有组合节点" desc="组合节点把几个节点串成一条固定的链路，规则可以像用一个节点那样用它。">
        <Button onClick={onComposite}>创建组合节点</Button>
      </Empty>
    ) : (
      <Empty title="还没有单点节点" desc="单点节点对应一台运行 Agent 的机器。">
        <Button icon={<IconPlus />} onClick={onAdd}>添加节点</Button>
      </Empty>
    )
  } else if (rows.length === 0) {
    body = <Empty title="没有匹配的节点" desc={seatFilter ? '换一个关键词，或取消上面的用途筛选。' : '换一个关键词试试。'} />
  } else if (isMobile) {
    body = (
      <div className="min-h-0 flex-1 overflow-y-auto">
        {rows.map(n => (
          <ListItem key={n.id} to={`/nodes/${n.id}`}>
            <div className="flex items-center justify-between gap-3">
              <span className="truncate font-medium text-ink">{n.name}</span>
              <NodeStatus node={n} />
            </div>
            <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-small text-ink-soft">
              <span className="inline-flex gap-1"><SeatTags node={n} /></span>
              <span>{fmtBytes(trafficOf(n))}</span>
              {speeds[n.id] && <Speed speed={speeds[n.id]} />}
            </div>
          </ListItem>
        ))}
      </div>
    )
  } else {
    body = (
      <TableScroll>
        <table className="tbl">
          <thead>
            <tr>
              <th>ID</th>
              <th>名称</th>
              <th>IP 栈</th>
              <th>Agent 版本</th>
              <th>最近同步</th>
              <th>状态</th>
              <SortableTh
                col="traffic"
                sort={sort}
                onSort={onSort}
                align="right"
                title="节点实际转发的累计字节（上行 + 下行），不乘倍率、不随重置清零；组合节点取它入口那个子节点的数值"
              >
                原始流量
              </SortableTh>
              <SortableTh col="speed" sort={sort} onSort={onSort} align="right">速度</SortableTh>
              <th className="num">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((n, i) => (
              <tr
                key={n.id}
                onDragOver={dragId != null ? e => e.preventDefault() : undefined}
                onDrop={dragId != null ? () => { moveTo(dragId, i); setDragId(null) } : undefined}
                onClick={() => navigate(`/nodes/${n.id}`)}
                className={cx('cursor-pointer', dragId === n.id && 'opacity-50')}
              >
                <td className="font-mono text-small text-ink-mut">
                  <span className="inline-flex items-center gap-1.5">
                    {canReorder && (
                      <ReorderHandle
                        name={n.name}
                        index={i}
                        count={rows.length}
                        busy={reorder.pending}
                        onMove={to => moveTo(n.id, to)}
                        onDragStart={() => setDragId(n.id)}
                        onDragEnd={() => setDragId(null)}
                      />
                    )}
                    {n.id}
                  </span>
                </td>
                <td>
                  <span className="inline-flex items-center gap-2">
                    <TextLink to={`/nodes/${n.id}`} tone="ink" className="font-medium" onClick={e => e.stopPropagation()}>{n.name}</TextLink>
                    <SeatTags node={n} />
                  </span>
                </td>
                <td><span className="inline-flex flex-wrap items-center gap-1.5"><NodeStackTags node={n} /></span></td>
                <td>
                  {n.agent_version ? (
                    <span className="inline-flex items-center gap-1.5">
                      <span className="font-mono text-small">{n.agent_version}</span>
                      {outdated.has(n.id) && <Badge tone="warn">可升级</Badge>}
                    </span>
                  ) : <span className="text-ink-mut">--</span>}
                </td>
                <td className="whitespace-nowrap text-small text-ink-soft">{fmtTime(n.last_apply_at?.Valid ? n.last_apply_at.Int64 : null)}</td>
                <td><NodeStatus node={n} /></td>
                <td className="num text-ink-soft">{fmtBytes(trafficOf(n))}</td>
                <td className="num text-small text-ink-soft"><Speed speed={speeds[n.id]} /></td>
                <td className="num" onClick={e => e.stopPropagation()}>
                  <span className="inline-flex items-center gap-1">
                    {!isComposite(n) && (
                      <IconButton size="sm" label="重新同步" loading={resync.pendingKey === n.id} onClick={() => resync.run(n.id)}>
                        <IconRefresh />
                      </IconButton>
                    )}
                    <Menu size="sm" label={`${n.name} 的更多操作`} items={actionsOf(n, i)} />
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </TableScroll>
    )
  }

  return (
    <Card fill>
      <Toolbar>
        <SearchInput value={search} onChange={setSearch} placeholder="搜索节点名称" />
        <Segmented
          size="sm"
          label="节点类型"
          value={tab}
          onChange={setTab}
          options={[{ value: 'single', label: '单点', count: single.length }, { value: 'composite', label: '组合', count: composite.length }]}
        />
        <span className="flex items-center gap-1.5">
          <span className="text-small text-ink-mut">用途</span>
          <ToggleChip pressed={(seatFilter & SEAT_ENTRY) !== 0} onChange={() => setSeatFilter(m => m ^ SEAT_ENTRY)} title="只看有入口用途的节点">入口</ToggleChip>
          <ToggleChip pressed={(seatFilter & SEAT_VIA) !== 0} onChange={() => setSeatFilter(m => m ^ SEAT_VIA)} title="只看有中转用途的节点">中转</ToggleChip>
        </span>
      </Toolbar>
      {body}
    </Card>
  )
}

/* ---------- 添加节点 ---------- */

/** 倍率输入对应的数值；不是大于等于 0 的数字时返回 null。 */
function parseRate(text) {
  const t = text.trim()
  const n = Number(t)
  return t !== '' && Number.isFinite(n) && n >= 0 ? n : null
}

function parsePort(text) {
  const n = Number(text.trim())
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : null
}

const BILLING_OPTIONS = [{ value: 'both', label: '双向' }, { value: 'larger', label: '单向' }]

function AddNodeModal({ onClose }) {
  const formId = useId()
  const navigate = useNavigate()
  const toast = useToast()
  const users = useGrantableUsers()
  const [form, setForm] = useState({ name: '', secret: '', portStart: '10001', portEnd: '20000', rate: '1', billing: 'both', userIds: [] })
  const [errors, setErrors] = useState({})
  const set = key => value => setForm(f => ({ ...f, [key]: value }))

  const create = useAction(async () => {
    const name = form.name.trim()
    const rate = parseRate(form.rate)
    const start = parsePort(form.portStart)
    const end = parsePort(form.portEnd)
    const found = {}
    if (!name) found.name = '填写节点名称'
    if (rate == null) found.rate = '倍率必须是大于等于 0 的数字'
    if (start == null || end == null) found.ports = '端口需是 1–65535 之间的整数'
    else if (start > end) found.ports = '起始端口不能大于结束端口'
    setErrors(found)
    if (Object.keys(found).length) return

    const res = await api.post('/nodes', {
      name,
      secret: form.secret.trim() || undefined,
      port_range: `${start}-${end}`,
      rate_multiplier: rate,
      unidirectional: form.billing === 'larger',
      user_ids: form.userIds.length ? form.userIds : undefined,
    })
    toast('节点已添加')
    // 详情页上有这个节点的安装命令，是添加之后紧接着要用的东西。
    navigate(`/nodes/${res.node.id}`)
  })

  return (
    <Modal
      title="添加节点"
      onClose={onClose}
      busy={create.pending}
      footer={
        <ModalActions note="添加后会打开节点详情，那里有安装命令。">
          <Button onClick={onClose} disabled={create.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.pending}>添加节点</Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); create.run() }}>
        <Field label="名称" error={errors.name}>
          <Input value={form.name} onChange={set('name')} placeholder="例如 hk-1" data-autofocus />
        </Field>
        <Field label="Token" optional hint="Agent 用它向面板证明身份。留空则自动生成。">
          <Input mono value={form.secret} onChange={set('secret')} />
        </Field>
        <Field label="端口范围" error={errors.ports} hint="规则自动分配监听端口时从这个范围里选。">
          <div className="flex items-center gap-2">
            <Input mono type="number" min="1" max="65535" className="w-28" value={form.portStart} onChange={set('portStart')} aria-label="起始端口" />
            <span className="text-ink-mut">–</span>
            <Input mono type="number" min="1" max="65535" className="w-28" value={form.portEnd} onChange={set('portEnd')} aria-label="结束端口" />
          </div>
        </Field>
        <Field label="倍率" error={errors.rate} hint="影响本节点承担流量的计费。">
          <Input mono type="number" min="0" step="0.1" className="w-28" value={form.rate} onChange={set('rate')} />
        </Field>
        <Field label="计费方向" hint="双向按出站 + 入站计费，单向取出站与入站中较大的一方。">
          <div className="flex">
            <Segmented value={form.billing} onChange={set('billing')} options={BILLING_OPTIONS} />
          </div>
        </Field>
        <Field label="授权用户" optional hint="被授权的用户可以用这个节点创建规则；之后也可以在节点详情里修改。">
          <GrantUserSelect users={users} value={form.userIds} onChange={set('userIds')} placeholder="暂不授权" />
        </Field>
      </form>
    </Modal>
  )
}

/* ---------- 创建组合节点 ---------- */

function CompositeNodeModal({ nodes, onClose }) {
  const formId = useId()
  const navigate = useNavigate()
  const toast = useToast()
  const users = useGrantableUsers()
  const [form, setForm] = useState({ name: '', rate: '1', userIds: [] })
  // 组合至少 2 跳，所以一开始就给出两行。
  const [hops, setHops] = useState(() => [newHopRow(), newHopRow()])
  const [errors, setErrors] = useState({})
  const set = key => value => setForm(f => ({ ...f, [key]: value }))

  const create = useAction(async () => {
    const name = form.name.trim()
    const rate = parseRate(form.rate)
    const found = {}
    if (!name) found.name = '填写节点名称'
    if (rate == null) found.rate = '倍率必须是大于等于 0 的数字'
    const hopsProblem = hopRowsError(hops)
    if (hopsProblem) found.hops = hopsProblem
    setErrors(found)
    if (Object.keys(found).length) return

    const res = await api.post('/nodes', {
      name,
      node_type: NODE_TYPE_COMPOSITE,
      rate_multiplier: rate,
      hops: hopRowsPayload(hops),
      user_ids: form.userIds.length ? form.userIds : undefined,
    })
    toast('组合节点已创建')
    navigate(`/nodes/${res.node.id}`)
  })

  return (
    <Modal
      title="创建组合节点"
      size="lg"
      onClose={onClose}
      busy={create.pending}
      footer={
        <ModalActions>
          <Button onClick={onClose} disabled={create.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.pending}>创建组合节点</Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); create.run() }}>
        <Field label="名称" error={errors.name}>
          <Input value={form.name} onChange={set('name')} placeholder="例如 hk-jp-chain" data-autofocus />
        </Field>
        <Field label="倍率" error={errors.rate} hint="组合节点自身的计费倍率，作用于整条链路承担的流量。">
          <Input mono type="number" min="0" step="0.1" className="w-28" value={form.rate} onChange={set('rate')} />
        </Field>
        <Field label="授权用户" optional hint="被授权的用户可以用这个节点创建规则；之后也可以在节点详情里修改。">
          <GrantUserSelect users={users} value={form.userIds} onChange={set('userIds')} placeholder="暂不授权" />
        </Field>
        <div className="flex flex-col gap-2">
          <div className="text-small font-medium text-ink-soft">跳序（从入口到出口）</div>
          {errors.hops && <Alert tone="danger">{errors.hops}</Alert>}
          <HopListEditor rows={hops} candidates={nodes} onChange={(next) => { setHops(next); setErrors(e => ({ ...e, hops: undefined })) }} />
        </div>
      </form>
    </Modal>
  )
}
