import { useId, useState } from 'react'
import { api } from '../../lib/api'
import { fmtBytes, fmtDate, fmtTrafficGB, isExpired, nullStr, pct } from '../../lib/fmt'
import { useIsMobile } from '../../lib/useIsMobile'
import { useResource } from '../../lib/useResource'
import { fmtSpeed, useSpeed } from '../../lib/useSpeed'
import { ApiTokenSection } from '../../components/ApiTokenSection'
import { NodeStatus, NodeTypeTag } from '../../components/net'
import { ListItem, Page, PageHeader, TableBox } from '../../components/page'
import { ProxyURIEditor } from '../../components/ProxyURIEditor'
import { ResourceView } from '../../components/ResourceView'
import { useUser } from '../../components/session'
import {
  Alert, Badge, Button, Card, CardHeader, CardSection, Empty, Field, IconGrip, Input, Meter, Modal, ModalActions,
  Segmented, Stat, Tag, useAction, useToast,
} from '../../components/ui'

export default function MyDashboard() {
  const my = useResource(() => api.get('/my'), [])
  const { refresh: refreshSession } = useUser()
  // 用户名同时出现在会话里（外壳、按用户名存放的本地数据都从那里取）和本页
  // 的数据里，改名之后两处都要重新取。
  const onRenamed = async () => { await Promise.all([refreshSession(), my.reload()]) }

  return (
    <Page>
      <PageHeader title="概览" />
      <ResourceView resource={my}>
        {data => <Overview data={data} onRenamed={onRenamed} />}
      </ResourceView>
      {/* 这两张卡片各有自己的数据来源，不依赖上面的账户数据：账户数据加载
          失败时它们照常可用。 */}
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-2 lg:items-start">
        <ProxyURIEditor />
        <ApiTokenSection />
      </div>
    </Page>
  )
}

/* 用量占比所处的状态：达到上限是 danger，到九成是 warn。 */
function usageTone(used, max) {
  if (!(max > 0)) return 'accent'
  const ratio = used / max
  return ratio >= 1 ? 'danger' : ratio >= 0.9 ? 'warn' : 'accent'
}

function Overview({ data, onRenamed }) {
  const { user, nodes = [], grants = [], rules = [], show_rate: showRate } = data
  const [renaming, setRenaming] = useState(false)

  const expiresAt = user.expires_at > 0 ? user.expires_at : null
  const disableReason = nullStr(user.disable_reason)
  const used = user.traffic_used_bytes || 0
  const quota = user.traffic_quota_bytes || 0
  // fmtBytes 给出 “1.2 GB”；数值与单位分开排版。
  const [usedValue, usedUnit] = fmtBytes(used).split(' ')

  return (
    <>
      {user.disabled && (
        <Alert tone="warn">
          你的账号已被禁用{disableReason ? `：${disableReason}` : ''}。请联系管理员。
        </Alert>
      )}

      <Card>
        <CardHeader
          title={user.username}
          description={<Badge tone={user.disabled ? 'warn' : 'ok'} dot>{user.disabled ? '已禁用' : '可用'}</Badge>}
          actions={<Button size="sm" onClick={() => setRenaming(true)}>修改用户名</Button>}
        />
        <CardSection className="grid grid-cols-1 gap-x-6 gap-y-5 sm:grid-cols-2 xl:grid-cols-4">
          <Stat label="规则" value={rules.length} unit={`/ ${user.max_forwards}`} hint="已创建的规则数 / 上限">
            <Meter
              value={rules.length}
              max={user.max_forwards}
              tone={usageTone(rules.length, user.max_forwards)}
              label="规则数占上限的比例"
            />
          </Stat>
          <Stat
            label="流量"
            value={usedValue}
            unit={usedUnit}
            hint={quota > 0 ? `配额 ${fmtBytes(quota)}，已用 ${pct(used, quota)}%` : '不限流量'}
          >
            {quota > 0 && <Meter value={used} max={quota} tone={usageTone(used, quota)} label="流量占配额的比例" />}
          </Stat>
          <Stat label="计费倍率" value={`×${user.billing_rate ?? 1}`} hint="与节点倍率相乘后计入流量" />
          <Stat
            label="到期"
            value={expiresAt ? fmtDate(expiresAt) : '永不过期'}
            hint={expiresAt && isExpired(expiresAt) ? <Badge tone="danger">已过期</Badge> : undefined}
          />
        </CardSection>
      </Card>

      <GrantedNodes userId={user.id} nodes={nodes} grants={grants} showRate={showRate} />

      {renaming && <RenameModal current={user.username} onClose={() => setRenaming(false)} onRenamed={onRenamed} />}
    </>
  )
}

function RenameModal({ current, onClose, onRenamed }) {
  const toast = useToast()
  const formId = useId()
  const [name, setName] = useState(current)
  const [error, setError] = useState('')

  const save = useAction(async () => {
    const next = name.trim()
    if (!next) {
      setError('用户名不能为空')
      return
    }
    if (next !== current) {
      await api.post('/my/username', { username: next })
      await onRenamed()
      toast('用户名已修改')
    }
    onClose()
  }, { onError: err => setError(err instanceof Error ? err.message : '修改失败') })

  return (
    <Modal
      title="修改用户名"
      size="sm"
      onClose={onClose}
      busy={save.pending}
      footer={(
        <ModalActions>
          <Button onClick={onClose} disabled={save.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={save.pending}>保存用户名</Button>
        </ModalActions>
      )}
    >
      <form id={formId} onSubmit={(e) => { e.preventDefault(); save.run() }}>
        <Field label="用户名" error={error || undefined}>
          <Input
            value={name}
            onChange={(v) => { setName(v); setError('') }}
            autoComplete="off"
            data-autofocus
          />
        </Field>
      </form>
    </Modal>
  )
}

/* 节点用途位里的“中转”，取值与服务端 db.NodeRoleVia 一致。 */
const NODE_ROLE_VIA = 2

const orderKey = userId => `my.nodeOrder.${userId}`

function readNodeOrder(userId) {
  try {
    const ids = JSON.parse(localStorage.getItem(orderKey(userId)) || '[]')
    return Array.isArray(ids) ? ids : []
  } catch {
    return []
  }
}

/**
 * 已授权节点：状态、实时速率、用量与限额。
 * @param {{ userId: number, nodes: any[], grants: any[], showRate: boolean }} props
 * `nodes` 与 `grants` 按下标一一对应（服务端 ListNodesForUser 同序返回）。
 */
function GrantedNodes({ userId, nodes, grants, showRate }) {
  const isMobile = useIsMobile()
  const speeds = useSpeed()
  const grantOf = new Map(nodes.map((n, i) => [n.id, grants[i]]))

  // 展示顺序是个人偏好，只存在本浏览器；键按用户 id 区分，同一浏览器换账号
  // 互不影响。
  const [order, setOrder] = useState(() => readNodeOrder(userId))
  const [dragId, setDragId] = useState(/** @type {number | null} */ (null))

  const rank = new Map(order.map((id, i) => [id, i]))
  // 不在名单里的节点（新授权的）排在后面；sort 是稳定的，它们之间保持服务端
  // 给出的顺序。
  const ordered = [...nodes].sort((a, b) => {
    const ra = rank.get(a.id) ?? Infinity
    const rb = rank.get(b.id) ?? Infinity
    return ra === rb ? 0 : ra - rb
  })
  const singles = ordered.filter(n => n.node_type !== 'composite')
  const composites = ordered.filter(n => n.node_type === 'composite')
  // 只有组合节点时直接显示组合，不让用户先看到一张空的单点表。
  const [tab, setTab] = useState(() => (singles.length === 0 && composites.length > 0 ? 'composite' : 'single'))
  const visible = tab === 'composite' ? composites : singles

  const dropOn = (targetId) => {
    setDragId(null)
    if (dragId === null || dragId === targetId) return
    const ids = visible.map(n => n.id)
    const from = ids.indexOf(dragId)
    const to = ids.indexOf(targetId)
    if (from < 0 || to < 0) return
    ids.splice(from, 1)
    ids.splice(to, 0, dragId)
    // 保存的名单总是单点在前、组合在后：在一类里调整顺序不改变另一类的顺序。
    const next = tab === 'composite'
      ? [...singles.map(n => n.id), ...ids]
      : [...ids, ...composites.map(n => n.id)]
    setOrder(next)
    try { localStorage.setItem(orderKey(userId), JSON.stringify(next)) } catch { /* 存不下时顺序只在本次访问内有效 */ }
  }

  const speedText = (n) => {
    const s = speeds[n.id]
    return s ? `↑ ${fmtSpeed(s.up)} · ↓ ${fmtSpeed(s.down)}` : null
  }
  const isVia = n => (n.roles & NODE_ROLE_VIA) !== 0

  return (
    <Card>
      <CardHeader
        title="已授权节点"
        description={nodes.length === 0
          ? undefined
          : isMobile ? `共 ${nodes.length} 个` : `共 ${nodes.length} 个，拖动行首的手柄可以调整顺序（只记在本浏览器）`}
        actions={nodes.length > 0 && (
          <Segmented
            size="sm"
            label="节点类型"
            value={tab}
            onChange={setTab}
            options={[
              { value: 'single', label: '单点', count: singles.length },
              { value: 'composite', label: '组合', count: composites.length },
            ]}
          />
        )}
      />
      {nodes.length === 0 ? (
        <Empty title="还没有被授权的节点" desc="管理员为你授权节点后，这里会显示它们的状态、速率与用量；创建规则也需要先有授权节点。请联系管理员。" />
      ) : visible.length === 0 ? (
        <Empty title={tab === 'composite' ? '没有已授权的组合节点' : '没有已授权的单点节点'} desc="切换上面的类型查看另一类节点。" />
      ) : isMobile ? (
        <div>
          {visible.map((n) => {
            const g = grantOf.get(n.id)
            const speed = speedText(n)
            return (
              <ListItem key={n.id}>
                <div className="flex items-center justify-between gap-3">
                  <span className="flex min-w-0 items-center gap-2">
                    <span className="truncate font-medium text-ink">{n.name}</span>
                    {isVia(n) && <Tag>中转</Tag>}
                  </span>
                  <NodeStatus node={n} detailed={false} />
                </div>
                <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-small text-ink-soft">
                  <NodeTypeTag type={n.node_type} />
                  {showRate && <span>×{n.rate_multiplier ?? 1}</span>}
                  {showRate && n.unidirectional && <Tag>单向</Tag>}
                  {speed && <span className="font-mono">{speed}</span>}
                  <span>{fmtTrafficGB(g?.traffic_used_bytes, g?.traffic_quota_bytes)}</span>
                  {g?.rate_limit_mbytes > 0 && <span>限速 {g.rate_limit_mbytes} MB/s</span>}
                  {g?.max_forwards != null && <span>规则上限 {g.max_forwards}</span>}
                </div>
              </ListItem>
            )
          })}
        </div>
      ) : (
        <TableBox>
          <table className="tbl">
            <thead>
              <tr>
                <th>节点</th>
                <th>类型</th>
                {showRate && <th className="num">倍率</th>}
                <th>状态</th>
                <th>实时速率</th>
                <th className="num">已用 / 配额</th>
                <th className="num">限速</th>
                <th className="num">规则上限</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((n) => {
                const g = grantOf.get(n.id)
                const speed = speedText(n)
                return (
                  <tr
                    key={n.id}
                    onDragOver={e => e.preventDefault()}
                    onDrop={() => dropOn(n.id)}
                    className={dragId === n.id ? 'opacity-50' : undefined}
                  >
                    <td>
                      <span className="inline-flex items-center gap-2">
                        <span
                          draggable
                          onDragStart={() => setDragId(n.id)}
                          onDragEnd={() => setDragId(null)}
                          className="inline-flex cursor-grab text-ink-mut"
                        >
                          <IconGrip size={14} />
                        </span>
                        <span className="font-medium">{n.name}</span>
                        {isVia(n) && <Tag>中转</Tag>}
                      </span>
                    </td>
                    <td><NodeTypeTag type={n.node_type} /></td>
                    {showRate && (
                      <td className="num">
                        <span className="inline-flex items-center justify-end gap-2">
                          {n.unidirectional && <Tag>单向</Tag>}
                          ×{n.rate_multiplier ?? 1}
                        </span>
                      </td>
                    )}
                    <td><NodeStatus node={n} detailed={false} /></td>
                    <td className="font-mono">{speed ?? <span className="text-ink-mut">--</span>}</td>
                    <td className="num">{fmtTrafficGB(g?.traffic_used_bytes, g?.traffic_quota_bytes)}</td>
                    <td className="num">{g?.rate_limit_mbytes > 0 ? `${g.rate_limit_mbytes} MB/s` : '不限'}</td>
                    <td className="num">{g?.max_forwards ?? '--'}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </TableBox>
      )}
    </Card>
  )
}
