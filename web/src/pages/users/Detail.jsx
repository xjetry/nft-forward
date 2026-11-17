import { useId, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { copyToClipboard } from '../../lib/clipboard'
import { GB, bytesToGBText, fmtBytes, fmtDate, fmtDateInput, fmtGrantTraffic, fmtTrafficGB, pct } from '../../lib/fmt'
import { formatGrantLine } from '../../lib/grantText'
import {
  ROLE_DIRECT, ROLE_LANDING, applyNodeRoleBatch, fetchNodeRoles, hostPortKey, mergeLanding, setNodeRoleBit,
  nodeRoleKey, rolesFirstOrder,
} from '../../lib/landing'
import { combineResources } from '../../lib/resource'
import { useResource } from '../../lib/useResource'
import { NodeTypeTag, ProtoTag } from '../../components/net'
import { Page, PageHeader, TableBox, Toolbar } from '../../components/page'
import { useBlur } from '../../components/prefs'
import { ResourceView } from '../../components/ResourceView'
import {
  Alert, Badge, Button, Card, CardHeader, CardSection, Checkbox, Empty, Field, IconButton, IconEdit, Input, Menu,
  Meter, Modal, ModalActions, Segmented, Select, SensText, Spinner, Stat, Tag, TextLink, Textarea, ToggleChip,
  useAction, useConfirm, useToast,
} from '../../components/ui'
import { ExpiryField, NewPasswordModal, expiryAlert, parseNumber, userConfirms } from './common'
import PasteGrantsModal from './PasteGrantsModal'

/* 授权上的用途：0 = 沿用节点自己的用途，其余是只对这个用户生效的
   入口(1) / 中转(2) 组合。 */
const GRANT_ROLE_OPTIONS = [
  { value: 0, label: '跟随节点' },
  { value: 1, label: '仅入口' },
  { value: 2, label: '仅中转' },
  { value: 3, label: '入口 + 中转' },
]

/* ==========================================================================
   用户详情页
   ========================================================================== */

const USERS = { to: '/users', label: '用户' }

function Dash() {
  return <span className="text-ink-mut">--</span>
}

/** 行内动作进行中时，占住“更多操作”按钮位置的转圈。 */
function RowBusy() {
  return (
    <span role="status" aria-label="处理中" className="inline-flex size-control-sm items-center justify-center text-ink-mut">
      <Spinner size={14} />
    </span>
  )
}

export default function UserDetail() {
  const { id } = useParams()
  const resource = useResource(() => api.get(`/users/${id}`), [id])
  return (
    <Page>
      {resource.state.status !== 'ready' && <PageHeader title="用户详情" back={USERS} />}
      <ResourceView resource={resource} notFound="用户不存在">
        {data => <UserDetailBody userId={id} data={data} reload={resource.reload} patch={resource.patch} />}
      </ResourceView>
    </Page>
  )
}

function UserDetailBody({ userId, data, reload, patch }) {
  const { user, nodes, grants, all_nodes: allNodes, rules, landing_nodes: landingNodes } = data
  const navigate = useNavigate()
  const toast = useToast()
  const confirm = useConfirm()
  const [newPassword, setNewPassword] = useState(null)
  const isRegular = user.role === 'user'
  const disableReason = user.disable_reason

  // 接口是“翻转”语义：服务端按它当时的状态取反。结果以响应为准，而不是
  // 按页面上（可能已过期）的状态去猜。
  const toggle = useAction(async () => {
    const d = await api.post(`/users/${userId}/toggle`)
    toast(d?.disabled ? `已禁用 ${user.username}` : `已启用 ${user.username}`)
    await reload()
  })
  const resetPassword = useAction(async () => {
    const d = await api.post(`/users/${userId}/reset-password`)
    setNewPassword(d.new_password)
  })
  const remove = useAction(async () => {
    await api.del(`/users/${userId}`)
    toast(`已删除用户 ${user.username}`)
    navigate('/users')
  })

  const askResetPassword = async () => {
    if (await confirm(userConfirms.resetPassword(user.username))) resetPassword.run()
  }
  const askRemove = async () => {
    if (await confirm(userConfirms.remove(user.username, rules.length))) remove.run()
  }

  return (
    <>
      <PageHeader
        title={user.username}
        back={USERS}
        meta={<>
          <Tag>{user.role === 'admin' ? '管理员' : '用户'}</Tag>
          {user.disabled
            ? <><Badge tone="warn" dot>已禁用</Badge>{disableReason && <span>{disableReason}</span>}</>
            : user.over_quota
              ? <><Badge tone="danger" dot>已超额</Badge><span>全局流量配额已用尽，规则已暂停转发；调高配额或重置流量后恢复</span></>
              : <Badge tone="ok" dot>正常</Badge>}
        </>}
        actions={isRegular && <>
          <Button loading={toggle.pending} onClick={toggle.run}>{user.disabled ? '启用' : '禁用'}</Button>
          <Button loading={resetPassword.pending} onClick={askResetPassword}>重置密码</Button>
          <Button danger loading={remove.pending} onClick={askRemove}>删除用户</Button>
        </>}
      />

      {isRegular && (
        <RegularUserCards
          userId={userId}
          user={user}
          nodes={nodes}
          grants={grants}
          allNodes={allNodes}
          rules={rules}
          landingNodes={landingNodes}
          reload={reload}
          onLandingNodes={list => patch(d => ({ ...d, landing_nodes: list }))}
        />
      )}

      <RulesCard rules={rules} allNodes={allNodes} />

      {newPassword != null && (
        <NewPasswordModal username={user.username} password={newPassword} onClose={() => setNewPassword(null)} />
      )}
    </>
  )
}

/* 只对普通用户存在的几块：用量、账号设置、节点授权、落地节点。 */
function RegularUserCards({ userId, user, nodes, grants, allNodes, rules, landingNodes, reload, onLandingNodes }) {
  // 落地节点出口的用量台账。放在这一层而不是落地节点卡片里：账号级的
  // “重置流量”会把台账一并清零，重置后要连它一起刷新。
  const exits = useResource(async () => (await api.get(`/users/${userId}/landing-exits`))?.exits || [], [userId])
  return (
    <>
      <UsageCard
        userId={userId}
        user={user}
        ruleCount={rules.length}
        onReset={() => Promise.all([reload(), exits.reload()])}
      />
      <SettingsCard userId={userId} user={user} reload={reload} />
      <GrantsCard userId={userId} user={user} nodes={nodes} grants={grants} allNodes={allNodes} rules={rules} reload={reload} />
      <LandingCard userId={userId} user={user} rawNodes={landingNodes} exits={exits} onNodes={onLandingNodes} />
    </>
  )
}

/* ---------- 用量 ---------- */

function UsageCard({ userId, user, ruleCount, onReset }) {
  const toast = useToast()
  const confirm = useConfirm()
  const used = user.traffic_used_bytes || 0
  const quota = user.traffic_quota_bytes || 0
  // fmtBytes 给出 “1.2 GB”；数值与单位分开排版。
  const [usedValue, usedUnit] = fmtBytes(used).split(' ')
  const expiresAt = user.expires_at
  const expiry = expiryAlert(expiresAt)

  const reset = useAction(async () => {
    await api.post(`/users/${userId}/reset-traffic`)
    toast('流量已重置')
    await onReset()
  })
  const askReset = async () => {
    const ok = await confirm({
      title: '重置流量',
      message: `把 ${user.username} 的已用流量全部清零？各节点、各落地节点出口与各规则上的用量会一起清零；因配额用尽而暂停的转发会恢复。清零后无法找回。`,
      confirmText: '重置流量',
    })
    if (ok) reset.run()
  }

  return (
    <Card>
      <CardHeader title="用量" actions={<Button size="sm" loading={reset.pending} onClick={askReset}>重置流量</Button>} />
      <CardSection className="grid grid-cols-1 gap-x-6 gap-y-5 sm:grid-cols-3">
        <Stat
          label="规则"
          value={ruleCount}
          unit={`/ ${user.max_forwards}`}
          hint={user.max_forwards === 0 ? '规则配额为 0，该用户不能创建规则' : undefined}
        >
          <Meter value={ruleCount} max={user.max_forwards} tone={ruleCount >= user.max_forwards ? 'warn' : 'accent'} label="规则配额用量" />
        </Stat>
        <Stat
          label="流量"
          value={usedValue}
          unit={`${usedUnit} / ${quota > 0 ? fmtBytes(quota) : '不限'}`}
          hint={`${quota > 0 ? `已用 ${pct(used, quota)}% · ` : ''}${user.traffic_reset_days > 0 ? `每 ${user.traffic_reset_days} 天重置` : '不自动重置'}`}
        >
          {quota > 0 && <Meter value={used} max={quota} tone={used >= quota ? 'danger' : 'accent'} label="流量配额用量" />}
        </Stat>
        <Stat label="到期" value={expiresAt ? fmtDate(expiresAt) : '永不到期'}>
          {expiry && <div><Badge tone={expiry.tone}>{expiry.label}</Badge></div>}
        </Stat>
      </CardSection>
    </Card>
  )
}

/* ---------- 账号设置 ---------- */

function SettingsCard({ userId, user, reload }) {
  const toast = useToast()
  const savedExpiry = user.expires_at ? fmtDateInput(user.expires_at) : ''
  // 表单只在挂载时取一次服务端的值：之后页面数据刷新（别的卡片保存后都会
  // 刷新）不会冲掉这里输入到一半的内容。
  const [form, setForm] = useState(() => ({
    expiresAt: savedExpiry,
    maxForwards: String(user.max_forwards || 0),
    quotaGB: bytesToGBText(user.traffic_quota_bytes),
    resetDays: String(user.traffic_reset_days || 0),
    billingRate: String(user.billing_rate ?? 1),
    adminNote: user.admin_note || '',
  }))
  const [errors, setErrors] = useState({})
  const set = key => value => setForm(f => ({ ...f, [key]: value }))

  const save = useAction(async () => {
    const maxForwards = parseNumber(form.maxForwards, { integer: true })
    const quotaGB = parseNumber(form.quotaGB)
    const resetDays = parseNumber(form.resetDays, { integer: true })
    const billingRate = parseNumber(form.billingRate)
    const found = {}
    if (maxForwards == null) found.maxForwards = '填写不小于 0 的整数'
    if (quotaGB == null) found.quotaGB = '填写不小于 0 的数'
    if (resetDays == null) found.resetDays = '填写不小于 0 的整数'
    if (!billingRate) found.billingRate = '填写大于 0 的数'
    setErrors(found)
    if (Object.keys(found).length) return
    await api.patch(`/users/${userId}/profile`, {
      expires_at: form.expiresAt,
      max_forwards: maxForwards,
      traffic_quota_gb: quotaGB,
      traffic_reset_days: resetDays,
      admin_note: form.adminNote,
      billing_rate: billingRate,
    })
    toast('账号设置已保存')
    await reload()
  })

  return (
    <Card>
      <CardHeader title="账号设置" />
      <CardSection>
        <form noValidate className="flex max-w-2xl flex-col gap-4" onSubmit={e => { e.preventDefault(); save.run() }}>
          <ExpiryField layout="row" value={form.expiresAt} onChange={set('expiresAt')} saved={savedExpiry} />
          <Field label="规则配额" layout="row" error={errors.maxForwards} hint="该用户最多可创建的规则数。0 = 不允许创建规则。">
            <Input type="number" min="0" step="1" className="w-44" value={form.maxForwards} onChange={set('maxForwards')} suffix="条" />
          </Field>
          <Field label="流量配额" layout="row" error={errors.quotaGB} hint="0 = 不限。">
            <Input type="number" min="0" step="0.1" className="w-44" value={form.quotaGB} onChange={set('quotaGB')} suffix="GB" />
          </Field>
          <Field label="流量重置周期" layout="row" error={errors.resetDays} hint="从开户日起每隔这么多天把已用流量清零。0 = 不自动重置。">
            <Input type="number" min="0" step="1" className="w-44" value={form.resetDays} onChange={set('resetDays')} suffix="天" />
          </Field>
          <Field label="计费倍率" layout="row" error={errors.billingRate} hint="计入总流量时乘以这个倍率：1 = 按实际流量，小于 1 打折，大于 1 加价。">
            <Input type="number" min="0" step="0.1" className="w-44" value={form.billingRate} onChange={set('billingRate')} suffix="倍" />
          </Field>
          <Field label="管理备注" layout="row" optional hint="只有管理员能看到。">
            <Input value={form.adminNote} onChange={set('adminNote')} />
          </Field>
          <div>
            <Button type="submit" loading={save.pending}>保存修改</Button>
          </div>
        </form>
      </CardSection>
    </Card>
  )
}

/* ---------- 节点授权 ---------- */

function GrantRoles({ roles }) {
  if (!roles) return <span className="text-ink-mut">跟随节点</span>
  return (
    <span className="inline-flex items-center gap-1">
      {(roles & 1) !== 0 && <Tag>入口</Tag>}
      {(roles & 2) !== 0 && <Tag>中转</Tag>}
    </span>
  )
}

function GrantsCard({ userId, user, nodes, grants, allNodes, rules, reload }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [tab, setTab] = useState('single')
  const [selected, setSelected] = useState(() => new Set())
  // null | { kind: 'paste' } | { kind: 'grant' } | { kind: 'edit', nodeId }
  const [dialog, setDialog] = useState(null)

  // 服务端的 nodes 与 grants 出自同一次查询、逐行成对，所以每个已授权节点
  // 一定能按 node_id 找到它的授权。
  const grantByNode = new Map(grants.map(g => [g.node_id, g]))
  const singles = nodes.filter(n => n.node_type !== 'composite')
  const composites = nodes.filter(n => n.node_type === 'composite')
  const tabNodes = tab === 'composite' ? composites : singles
  // 选择集里可能留着刚被撤销的节点；一切判断都以当前页签里实际还在的节点为准。
  const chosen = tabNodes.filter(n => selected.has(n.id))
  // 节点规则配额按“以该节点为入口的规则数”计，与服务端创建规则时的口径相同。
  const entryRuleCount = new Map()
  for (const r of rules) entryRuleCount.set(r.node_id, (entryRuleCount.get(r.node_id) || 0) + 1)

  const switchTab = (next) => {
    setTab(next)
    setSelected(new Set())
  }
  const toggleOne = (id, on) => setSelected(s => {
    const next = new Set(s)
    if (on) next.add(id); else next.delete(id)
    return next
  })

  const revoke = useAction(async (nodeId) => {
    await api.del(`/users/${userId}/grants/${nodeId}`)
    toast('授权已撤销')
    await reload()
  })
  const revokeChosen = useAction(async (ids) => {
    await api.post(`/users/${userId}/grants/batch-revoke`, { node_ids: ids })
    toast(`已撤销 ${ids.length} 个节点的授权`)
    setSelected(new Set())
    await reload()
  })
  const resetTraffic = useAction(async (nodeId) => {
    await api.post(`/users/${userId}/nodes/${nodeId}/reset-traffic`)
    toast('节点流量已重置')
    await reload()
  })

  const askRevoke = async (node) => {
    const ok = await confirm({
      title: '撤销授权',
      message: `撤销 ${user.username} 对节点 ${node.name} 的授权？该用户经过这个节点的规则会一并删除并停止转发，无法恢复。`,
      confirmText: '撤销授权',
      danger: true,
    })
    if (ok) revoke.run(node.id)
  }
  const askRevokeChosen = async () => {
    const ok = await confirm({
      title: '撤销授权',
      message: `撤销 ${user.username} 对这 ${chosen.length} 个节点的授权？该用户经过这些节点的规则会一并删除并停止转发，无法恢复。\n${chosen.map(n => n.name).join('、')}`,
      confirmText: `撤销 ${chosen.length} 个授权`,
      danger: true,
    })
    if (ok) revokeChosen.run(chosen.map(n => n.id))
  }
  const askResetTraffic = async (node) => {
    const ok = await confirm({
      title: '重置节点流量',
      message: `把 ${user.username} 在节点 ${node.name} 上的已用流量清零？清零后无法找回。`,
      confirmText: '重置流量',
    })
    if (ok) resetTraffic.run(node.id)
  }

  const copyGrants = () => {
    const text = nodes.map(n => formatGrantLine(n.name, grantByNode.get(n.id))).join('\n')
    copyToClipboard(text).then(
      () => toast(`已复制 ${nodes.length} 个节点的授权`),
      () => toast('没能写入剪贴板，请检查浏览器的剪贴板权限', 'error'),
    )
  }

  const editing = dialog?.kind === 'edit' ? nodes.find(n => n.id === dialog.nodeId) : null
  const close = () => setDialog(null)

  return (
    <Card>
      <CardHeader
        title="节点授权"
        description={nodes.length ? `${nodes.length} 个节点` : undefined}
        actions={<>
          {nodes.length > 0 && <Button size="sm" onClick={copyGrants}>复制授权</Button>}
          <Button size="sm" onClick={() => setDialog({ kind: 'paste' })}>粘贴授权</Button>
          <Button size="sm" onClick={() => setDialog({ kind: 'grant' })}>授权节点</Button>
        </>}
      />
      {nodes.length === 0 ? (
        <Empty title="还没有授权节点" desc="授权节点后，该用户才能在这些节点上创建规则。">
          <Button onClick={() => setDialog({ kind: 'grant' })}>授权节点</Button>
        </Empty>
      ) : (
        <>
          <Toolbar>
            <Segmented
              size="sm"
              label="节点类型"
              value={tab}
              onChange={switchTab}
              options={[
                { value: 'single', label: '单点', count: singles.length },
                { value: 'composite', label: '组合', count: composites.length },
              ]}
            />
            {chosen.length > 0 && (
              <Button size="sm" danger className="ml-auto" loading={revokeChosen.pending} onClick={askRevokeChosen}>
                撤销选中的 {chosen.length} 个
              </Button>
            )}
          </Toolbar>
          {tabNodes.length === 0 ? (
            <Empty title={tab === 'composite' ? '没有已授权的组合节点' : '没有已授权的单点节点'} />
          ) : (
            <TableBox>
              <table className="tbl">
                <thead>
                  <tr>
                    <th>
                      <div className="flex items-center gap-2.5">
                        <Checkbox
                          aria-label="全选"
                          checked={chosen.length === tabNodes.length}
                          onChange={on => setSelected(on ? new Set(tabNodes.map(n => n.id)) : new Set())}
                        />
                        节点
                      </div>
                    </th>
                    <th>类型</th>
                    <th className="num">规则配额</th>
                    <th className="num">流量</th>
                    <th className="num">限速</th>
                    <th>用途</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {tabNodes.map((n) => {
                    const g = grantByNode.get(n.id)
                    const exceeded = g.traffic_quota_bytes > 0 && g.traffic_used_bytes >= g.traffic_quota_bytes
                    const busy = revoke.pendingKey === n.id || resetTraffic.pendingKey === n.id
                    return (
                      <tr key={n.id}>
                        <td>
                          <div className="flex items-center gap-2.5">
                            <Checkbox aria-label={`选择 ${n.name}`} checked={selected.has(n.id)} onChange={on => toggleOne(n.id, on)} />
                            <TextLink to={`/nodes/${n.id}`} tone="ink" className="font-medium">{n.name}</TextLink>
                          </div>
                        </td>
                        <td><NodeTypeTag type={n.node_type} /></td>
                        <td className="num">{entryRuleCount.get(n.id) || 0} / {g.max_forwards}</td>
                        <td className="num">
                          <span className="inline-flex items-center gap-1.5">
                            {exceeded && <Badge tone="danger">已超额</Badge>}
                            {fmtGrantTraffic(g.traffic_used_bytes, g.traffic_quota_bytes, user.traffic_quota_bytes)}
                          </span>
                        </td>
                        <td className="num">{g.rate_limit_mbytes > 0 ? `${g.rate_limit_mbytes} MB/s` : '不限'}</td>
                        <td><GrantRoles roles={g.roles} /></td>
                        <td className="num">
                          <div className="flex items-center justify-end gap-1">
                            <Button size="sm" onClick={() => setDialog({ kind: 'edit', nodeId: n.id })}>编辑</Button>
                            {busy ? <RowBusy /> : (
                              <Menu
                                size="sm"
                                label={`${n.name} 的更多操作`}
                                items={[
                                  // 节点流量只在设了节点配额时才参与限制，没设配额或还没用量时重置没有意义。
                                  { label: '重置流量', disabled: !(g.traffic_quota_bytes > 0 && g.traffic_used_bytes > 0), onSelect: () => askResetTraffic(n) },
                                  { label: '撤销授权', danger: true, onSelect: () => askRevoke(n) },
                                ]}
                              />
                            )}
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </TableBox>
          )}
        </>
      )}

      {dialog?.kind === 'paste' && <PasteGrantsModal initialUserIds={[user.id]} onClose={close} onDone={reload} />}
      {dialog?.kind === 'grant' && (
        <GrantNodesModal userId={userId} allNodes={allNodes} granted={grantByNode} onClose={close} reload={reload} />
      )}
      {editing && <GrantEditModal userId={userId} node={editing} grant={grantByNode.get(editing.id)} onClose={close} reload={reload} />}
    </Card>
  )
}

function GrantNodesModal({ userId, allNodes, granted, onClose, reload }) {
  const toast = useToast()
  const formId = useId()
  const [nodeIds, setNodeIds] = useState([])
  const [maxForwards, setMaxForwards] = useState('10')
  const [errors, setErrors] = useState({})
  const available = allNodes.filter(n => !granted.has(n.id))
  const optionsOf = composite => available
    .filter(n => (n.node_type === 'composite') === composite)
    .map(n => ({ value: n.id, label: n.name }))

  const grant = useAction(async () => {
    const max = parseNumber(maxForwards, { min: 1, integer: true })
    const found = {}
    if (nodeIds.length === 0) found.nodes = '至少选择一个节点'
    if (max == null) found.maxForwards = '填写不小于 1 的整数'
    setErrors(found)
    if (Object.keys(found).length) return
    await api.post(`/users/${userId}/grants`, { node_ids: nodeIds, max_forwards: max })
    toast(`已授权 ${nodeIds.length} 个节点`)
    await reload()
    onClose()
  })

  if (available.length === 0) {
    return (
      <Modal title="授权节点" size="sm" onClose={onClose} footer={<ModalActions><Button onClick={onClose}>关闭</Button></ModalActions>}>
        {allNodes.length === 0
          ? <Empty title="还没有节点" desc="先添加节点，再把它授权给用户。"><Button to="/nodes">去添加节点</Button></Empty>
          : <Empty title="所有节点都已授权给该用户" />}
      </Modal>
    )
  }
  return (
    <Modal
      title="授权节点"
      onClose={onClose}
      busy={grant.pending}
      footer={
        <ModalActions>
          <Button onClick={onClose} disabled={grant.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={grant.pending}>授权</Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); grant.run() }}>
        <Field label="节点" error={errors.nodes}>
          <Select
            multiple
            searchable
            tabs
            value={nodeIds}
            onChange={setNodeIds}
            placeholder="选择节点"
            groups={[{ label: '单点', options: optionsOf(false) }, { label: '组合', options: optionsOf(true) }]}
          />
        </Field>
        <Field label="规则配额" error={errors.maxForwards} hint="该用户以每个所选节点为入口最多可创建的规则数，授权后可以逐个调整。">
          <Input type="number" min="1" step="1" className="w-44" value={maxForwards} onChange={setMaxForwards} suffix="条" />
        </Field>
      </form>
    </Modal>
  )
}

function grantFormOf(grant) {
  return {
    maxForwards: String(grant.max_forwards),
    quotaGB: bytesToGBText(grant.traffic_quota_bytes),
    rate: String(grant.rate_limit_mbytes || 0),
    roles: grant.roles || 0,
  }
}

function GrantEditModal({ userId, node, grant, onClose, reload }) {
  const toast = useToast()
  const formId = useId()
  const [form, setForm] = useState(() => grantFormOf(grant))
  const [errors, setErrors] = useState({})
  const set = key => value => setForm(f => ({ ...f, [key]: value }))
  // “改过没有”以输入框里的文字与当前授权换算出的文字比较，而不是比较换算
  // 回去的数值：配额按两位小数的 GB 显示，原值若不是它的整数倍，按数值比较
  // 会把没碰过的配额也判成改过并写回一个被舍入的值。
  const current = grantFormOf(grant)
  const changed = key => form[key] !== current[key]
  const dirty = Object.keys(current).some(changed)

  const save = useAction(async () => {
    const maxForwards = parseNumber(form.maxForwards, { min: 1, integer: true })
    const quotaGB = parseNumber(form.quotaGB)
    const rate = parseNumber(form.rate, { integer: true })
    const found = {}
    if (maxForwards == null) found.maxForwards = '填写不小于 1 的整数'
    if (quotaGB == null) found.quotaGB = '填写不小于 0 的数'
    if (rate == null) found.rate = '填写不小于 0 的整数'
    setErrors(found)
    if (Object.keys(found).length) return
    // 四项设置各有自己的接口，只提交改过的项。
    const requests = [
      changed('maxForwards') && ['max-forwards', { max_forwards: maxForwards }],
      changed('quotaGB') && ['quota', { traffic_quota_bytes: Math.round(quotaGB * GB) }],
      changed('rate') && ['rate-limit', { rate_limit_mbytes: rate }],
      changed('roles') && ['roles', { roles: form.roles }],
    ].filter(Boolean)
    if (requests.length === 0) {
      onClose()
      return
    }
    try {
      for (const [path, body] of requests) await api.post(`/users/${userId}/nodes/${node.id}/${path}`, body)
    } finally {
      // 中途失败时排在前面的几项已经生效。无论成败都刷新，“改过的项”随之
      // 按服务端的现值重新计算，再点保存只会提交还没生效的那些。
      await reload()
    }
    toast('授权已更新')
    onClose()
  })

  return (
    <Modal
      title={`编辑授权：${node.name}`}
      onClose={onClose}
      busy={save.pending}
      footer={
        <ModalActions>
          <Button onClick={onClose} disabled={save.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={save.pending} disabled={!dirty}>保存修改</Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); save.run() }}>
        <Field label="规则配额" error={errors.maxForwards} hint="该用户以这个节点为入口最多可创建的规则数，至少为 1。">
          <Input type="number" min="1" step="1" className="w-44" value={form.maxForwards} onChange={set('maxForwards')} suffix="条" />
        </Field>
        <Field label="流量配额" error={errors.quotaGB} hint="0 = 不单独设限，只受该用户的总流量配额约束。">
          <Input type="number" min="0" step="0.1" className="w-44" value={form.quotaGB} onChange={set('quotaGB')} suffix="GB" />
        </Field>
        <Field label="限速" error={errors.rate} hint="该用户在这个节点上的所有规则共享这一限速。0 = 不限速。">
          <Input type="number" min="0" step="1" className="w-44" value={form.rate} onChange={set('rate')} suffix="MB/s" />
        </Field>
        <Field label="用途" hint="该用户可以把这个节点用作入口、中转，还是两者都可以。“跟随节点”沿用节点自己的用途。">
          <Select className="w-44" value={form.roles} onChange={set('roles')} options={GRANT_ROLE_OPTIONS} />
        </Field>
      </form>
    </Modal>
  )
}

/* ---------- 落地节点 ---------- */

const MARKS = [[ROLE_LANDING, '落地'], [ROLE_DIRECT, '直连']]

function LandingCard({ userId, user, rawNodes, exits, onNodes }) {
  const blurred = useBlur()
  const toast = useToast()
  // 落地 / 直连标记表。读取失败就是失败：把它当成空表的话，所有节点都会
  // 显示成未标记，而在一张读不出来的表上也不应该允许编辑。
  const roles = useResource(fetchNodeRoles, [])
  const [url, setUrl] = useState(user.landing_sub_url || '')
  const [uris, setUris] = useState(user.landing_uris || '')
  const [urlError, setUrlError] = useState(null)
  // 最近一次保存时订阅没抓到的原因；null 表示抓到了，或来源里没有订阅。
  // 来源此时已经保存，所以这不是保存失败，而是一条要一直留在表单旁的提醒：
  // 下面的节点表还是上一次抓到的结果。
  const [subError, setSubError] = useState(null)

  // 落地节点以 host:port 为身份：出口台账、用户侧的节点目录都按它去重并
  // 保留先出现的那个。这里用同一口径，表格行与行内状态才有唯一而稳定的 key。
  const nodes = useMemo(() => mergeLanding(rawNodes), [rawNodes])

  const save = useAction(async () => {
    const subURL = url.trim()
    if (subURL && !/^https?:\/\//.test(subURL)) {
      setUrlError('订阅地址要以 http:// 或 https:// 开头')
      return
    }
    setUrlError(null)
    const d = await api.post(`/users/${userId}/landing`, { landing_sub_url: subURL, landing_uris: uris })
    onNodes(d?.landing_nodes || [])
    setSubError(d?.sub_error || null)
    if (d?.sub_error) toast('来源已保存，但订阅地址没有抓到', 'error')
    else toast('落地节点来源已保存')
    await exits.reload()
  })

  // 节点表同时需要出口台账与标记表，缺一个就不渲染。
  const table = {
    state: combineResources(exits.state, roles.state),
    reload: () => Promise.all([exits.reload(), roles.reload()]),
  }

  return (
    <Card>
      <CardHeader title="落地节点" description={nodes.length ? `${nodes.length} 个节点` : undefined} />
      <CardSection>
        <form noValidate className="flex max-w-2xl flex-col gap-4" onSubmit={e => { e.preventDefault(); save.run() }}>
          <Field label="订阅地址" optional error={urlError} hint="订阅内容是代理 URI 列表，base64 编码或纯文本均可。">
            <Input mono className={blurred ? 'blur-sm' : undefined} value={url} onChange={setUrl} placeholder="https://example.com/sub/xxxx" />
          </Field>
          <Field label="节点 URI" optional hint="每行一条，可以与订阅地址同时使用。">
            <Textarea mono rows={8} className={blurred ? 'blur-sm' : undefined} value={uris} onChange={setUris} placeholder={'vless://…\ntrojan://…'} />
          </Field>
          {subError && (
            <Alert tone="warn">
              <p className="break-all">来源已保存，但订阅地址没有抓到：{subError}</p>
              <p>订阅里的落地节点仍是上一次抓到的结果，面板会定期重新抓取；地址有误的话请改正后再保存。</p>
            </Alert>
          )}
          <div>
            <Button type="submit" loading={save.pending}>保存来源</Button>
          </div>
        </form>
      </CardSection>
      <div className="border-t border-line-soft">
        <ResourceView resource={table}>
          {([exitList, roleTable]) => (
            <LandingNodes
              userId={userId}
              username={user.username}
              nodes={nodes}
              exits={exitList}
              roles={roleTable}
              onRoles={next => roles.patch(() => next)}
              reloadRoles={roles.reload}
              reloadExits={exits.reload}
            />
          )}
        </ResourceView>
      </div>
    </Card>
  )
}

function LandingNodes({ userId, username, nodes, exits, roles, onRoles, reloadRoles, reloadExits }) {
  const blurred = useBlur()
  const toast = useToast()
  const confirm = useConfirm()
  const [selected, setSelected] = useState(() => new Set())

  /* 标记的写入。

     标记表是全局的一行设置。这里提交的是一次改动（打开 / 关闭某一位），由
     服务端应用到它当时的表上（见 lib/landing.js 的 setNodeRoleBit），同一改动
     重复提交结果不变；写入口自己按点击顺序排队，所以连续点击不需要互相
     等待，每一次都入队。

     显示 = 服务端的表 + 尚未落定的改动（pendingOps）：
     - 点击后立即生效在显示上；
     - 写入成功，用返回的整表替换服务端的表，同时把这次改动移出 pendingOps；
     - 写入失败，改动移出 pendingOps，显示随之回到服务端的表。 */
  const [pendingOps, setPendingOps] = useState([])
  const writesInFlight = useRef(0)
  const shownRoles = useMemo(
    () => pendingOps.reduce((table, op) => applyNodeRoleBatch(table, op.nodes, op.bit, op.on), roles),
    [roles, pendingOps],
  )
  const setMark = (targets, bit, on) => {
    const op = { nodes: targets, bit, on }
    writesInFlight.current += 1
    setPendingOps(ops => [...ops, op])
    setNodeRoleBit(targets, bit, on).then(
      onRoles,
      (err) => {
        toast(`标记没有保存：${err instanceof Error ? err.message : '请求失败'}`, 'error')
        // 失败时服务端的表有没有变并不确定（响应可能丢在半路）。后面还有写入
        // 在排队时，它们各自会带回写入后的整表；只有这是最后一笔时才需要主动
        // 重读，而且此时没有别的写入会与这次重读交错。
        if (writesInFlight.current === 1) void reloadRoles()
      },
    ).finally(() => {
      writesInFlight.current -= 1
      setPendingOps(ops => ops.filter(o => o !== op))
    })
  }

  const markOf = (n) => {
    const key = nodeRoleKey(n)
    return (key && shownRoles[key]) || 0
  }
  // 有标记的排在前面、未标记的沉底，组内保持来源里的顺序。
  const order = useMemo(() => rolesFirstOrder(nodes, markOf), [nodes, shownRoles])
  const exitByKey = new Map(exits.map(e => [hostPortKey(e.host, e.port), e]))
  const residual = exits.filter(e => !e.present)
  const chosen = nodes.filter(n => selected.has(hostPortKey(n.host, n.port)))
  const countWith = bit => nodes.filter(n => markOf(n) & bit).length

  /* 没有落地标记的节点，只要它的台账上有配额或已用量，仍然显示配额与用量：
     出口配额在服务端是否生效只看台账、不看标记，一个正在起作用的配额不能
     因为取消了标记就从界面上消失。 */
  const ledgerOf = (n, mark) => {
    const exit = exitByKey.get(hostPortKey(n.host, n.port))
    if (!exit) return null
    return (mark & ROLE_LANDING) || exit.quota_bytes > 0 || exit.used_bytes > 0 ? exit : null
  }

  const toggleOne = (key, on) => setSelected(s => {
    const next = new Set(s)
    if (on) next.add(key); else next.delete(key)
    return next
  })

  const resetExit = useAction(async (key, exit) => {
    await api.post(`/users/${userId}/landing-exits/reset`, { host: exit.host, port: exit.port })
    toast('出口流量已重置')
    await reloadExits()
  })
  const deleteExit = useAction(async (key, exit) => {
    await api.post(`/users/${userId}/landing-exits/delete`, { host: exit.host, port: exit.port })
    toast('用量记录已删除')
    await reloadExits()
  })
  const exitLabel = exit => `${exit.name_override || exit.name || '（未命名）'}（${hostPortKey(exit.host, exit.port)}）`
  const askResetExit = async (key, exit) => {
    const ok = await confirm({
      title: '重置出口流量',
      message: `把 ${username} 在出口 ${exitLabel(exit)} 上的已用流量清零？清零后无法找回。`,
      confirmText: '重置流量',
    })
    if (ok) resetExit.run(key, exit)
  }
  const askDeleteExit = async (key, exit) => {
    const ok = await confirm({
      title: '删除用量记录',
      message: `删除出口 ${exitLabel(exit)} 的用量记录？它已经不在来源里，记录着 ${fmtTrafficGB(exit.used_bytes, exit.quota_bytes)} 的已用量与配额；删除后无法恢复，这个出口以后再出现时从 0 开始计。`,
      confirmText: '删除记录',
      danger: true,
    })
    if (ok) deleteExit.run(key, exit)
  }

  const usageCell = (key, exit) => (
    <span className="inline-flex items-center gap-1.5">
      {fmtTrafficGB(exit.used_bytes, exit.quota_bytes)}
      {exit.quota_bytes > 0 && exit.used_bytes >= exit.quota_bytes && <Badge tone="danger">已超额</Badge>}
      <Button size="sm" variant="ghost" loading={resetExit.pendingKey === key} onClick={() => askResetExit(key, exit)}>重置</Button>
    </span>
  )

  if (nodes.length === 0 && residual.length === 0) {
    return <Empty title="还没有落地节点" desc="保存订阅地址或节点 URI 后，这里会列出解析出的节点，再把它们标记为落地或直连。" />
  }
  return (
    <>
      <Toolbar>
        <span className="text-small text-ink-soft">
          {countWith(ROLE_LANDING)} 落地 · {countWith(ROLE_DIRECT)} 直连 · {nodes.filter(n => !markOf(n)).length} 未标记
        </span>
        <span className="text-caption text-ink-mut">
          落地：可作该用户规则的出口；直连：出现在该用户的“我的代理”里。标记记在节点上（协议 + 地址），同一节点出现在别的用户的来源里时共用同一标记。
        </span>
        {chosen.length > 0 && (
          <div className="ml-auto flex items-center gap-1.5">
            <span className="text-small text-ink-mut">选中的 {chosen.length} 个</span>
            {MARKS.map(([bit, label]) => (
              <ToggleChip key={bit} label={`选中的 ${chosen.length} 个节点：${label}`} pressed={chosen.every(n => markOf(n) & bit)} onChange={on => setMark(chosen, bit, on)}>{label}</ToggleChip>
            ))}
          </div>
        )}
      </Toolbar>
      <TableBox>
        <table className="tbl">
          <thead>
            <tr>
              <th>
                <div className="flex items-center gap-2.5">
                  <Checkbox
                    aria-label="全选"
                    disabled={nodes.length === 0}
                    checked={nodes.length > 0 && chosen.length === nodes.length}
                    onChange={on => setSelected(on ? new Set(nodes.map(n => hostPortKey(n.host, n.port))) : new Set())}
                  />
                  节点
                </div>
              </th>
              <th>协议</th>
              <th>地址</th>
              <th>流量配额（0 = 不限）</th>
              <th>已用</th>
              <th className="num">标记</th>
            </tr>
          </thead>
          <tbody>
            {order.map((i) => {
              const n = nodes[i]
              const key = hostPortKey(n.host, n.port)
              const mark = markOf(n)
              const ledger = ledgerOf(n, mark)
              return (
                <tr key={`node:${key}`}>
                  <td>
                    <div className="flex items-center gap-2.5">
                      <Checkbox aria-label={`选择 ${n.name || key}`} checked={selected.has(key)} onChange={on => toggleOne(key, on)} />
                      <ExitName userId={userId} name={n.name} exit={exitByKey.get(key)} onSaved={reloadExits} />
                    </div>
                  </td>
                  <td><Tag mono>{n.protocol}</Tag></td>
                  <td className="font-mono"><SensText blurred={blurred}>{key}</SensText></td>
                  <td>{ledger ? <ExitQuotaForm userId={userId} exit={ledger} onSaved={reloadExits} /> : <Dash />}</td>
                  <td>{ledger ? usageCell(key, ledger) : <Dash />}</td>
                  <td className="num">
                    <span className="inline-flex gap-1.5">
                      {MARKS.map(([bit, label]) => (
                        <ToggleChip key={bit} label={`${n.name || hostPortKey(n.host, n.port)}：${label}`} pressed={(mark & bit) !== 0} onChange={on => setMark([n], bit, on)}>{label}</ToggleChip>
                      ))}
                    </span>
                  </td>
                </tr>
              )
            })}
            {residual.map((exit) => {
              const key = hostPortKey(exit.host, exit.port)
              return (
                <tr key={`residual:${key}`}>
                  <td>
                    <div className="flex items-center gap-2.5">
                      <ExitName userId={userId} name={exit.name} exit={exit} onSaved={reloadExits} />
                      <Badge>已不在来源</Badge>
                    </div>
                  </td>
                  <td><Tag mono>{exit.protocol}</Tag></td>
                  <td className="font-mono"><SensText blurred={blurred}>{key}</SensText></td>
                  <td><ExitQuotaForm userId={userId} exit={exit} onSaved={reloadExits} /></td>
                  <td>{usageCell(key, exit)}</td>
                  <td className="num">
                    <Button size="sm" variant="ghost" danger loading={deleteExit.pendingKey === key} onClick={() => askDeleteExit(key, exit)}>删除记录</Button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </TableBox>
    </>
  )
}

function ExitQuotaForm({ userId, exit, onSaved }) {
  const toast = useToast()
  const current = bytesToGBText(exit.quota_bytes)
  const [text, setText] = useState(current)
  const gb = parseNumber(text)
  const save = useAction(async () => {
    await api.post(`/users/${userId}/landing-exits/quota`, { host: exit.host, port: exit.port, quota_bytes: Math.round(gb * GB) })
    toast(gb > 0 ? '出口配额已保存' : '出口配额已取消')
    await onSaved()
  })
  // 表格单元格里放不下字段错误文字：非法输入用输入框的错误边框表示，并且
  // 不允许提交。
  const canSave = gb != null && text !== current
  return (
    <form noValidate className="inline-flex items-center gap-1.5" onSubmit={e => { e.preventDefault(); if (canSave) save.run() }}>
      <Input
        size="sm"
        type="number"
        min="0"
        step="0.1"
        className="w-28"
        aria-label="流量配额（GB）"
        invalid={gb == null}
        value={text}
        onChange={setText}
        suffix="GB"
      />
      <Button size="sm" type="submit" loading={save.pending} disabled={!canSave}>保存</Button>
    </form>
  )
}

/* 落地节点的显示名。改名记在出口台账上，所以订阅刷新后仍然保留；还没有
   台账的节点（尚未同步）只读。保存空名称即恢复来源里的原名。 */
function ExitName({ userId, name, exit, onSaved }) {
  const toast = useToast()
  // null = 没在编辑；字符串 = 编辑中的草稿。
  const [draft, setDraft] = useState(null)
  const original = name || '（未命名）'
  const save = useAction(async () => {
    const next = draft.trim()
    await api.post(`/users/${userId}/landing-exits/rename`, { host: exit.host, port: exit.port, name: next })
    toast(next ? '已改名' : '已恢复原名')
    setDraft(null)
    await onSaved()
  })

  if (!exit) return <span className="font-medium">{original}</span>
  if (draft == null) {
    return (
      <span className="inline-flex items-center gap-1">
        <span className="font-medium">{exit.name_override || original}</span>
        <IconButton size="sm" label="改名" onClick={() => setDraft(exit.name_override || name || '')}><IconEdit size={14} /></IconButton>
        {exit.name_override && <span className="text-ink-mut">原名 {original}</span>}
      </span>
    )
  }
  return (
    <form noValidate className="inline-flex items-center gap-1.5" onSubmit={e => { e.preventDefault(); save.run() }}>
      <Input
        size="sm"
        className="w-40"
        aria-label="节点名称"
        autoFocus
        value={draft}
        onChange={setDraft}
        onKeyDown={e => { if (e.key === 'Escape') setDraft(null) }}
        placeholder="留空恢复原名"
      />
      <Button size="sm" type="submit" loading={save.pending}>保存</Button>
      <Button size="sm" variant="ghost" disabled={save.pending} onClick={() => setDraft(null)}>取消</Button>
    </form>
  )
}

/* ---------- 规则 ---------- */

function RulesCard({ rules, allNodes }) {
  const blurred = useBlur()
  const nodeName = new Map(allNodes.map(n => [n.id, n.name]))
  return (
    <Card>
      <CardHeader title="规则" description={rules.length ? `${rules.length} 条` : undefined} />
      {rules.length === 0 ? (
        <Empty title="该用户还没有规则" desc="用户创建规则后，这里会列出它们的入口、出口与流量。" />
      ) : (
        <TableBox>
          <table className="tbl">
            <thead>
              <tr>
                <th>名称</th>
                <th>ID</th>
                <th>入口节点</th>
                <th>协议</th>
                <th>入口端口</th>
                <th>出口</th>
                <th className="num">流量</th>
              </tr>
            </thead>
            <tbody>
              {rules.map(r => (
                <tr key={r.id}>
                  <td><TextLink to={`/rules/${r.id}`} tone="ink" className="font-medium">{r.name}</TextLink></td>
                  <td className="text-ink-mut">{r.id}</td>
                  <td>{nodeName.get(r.node_id) || `#${r.node_id}`}</td>
                  <td><ProtoTag proto={r.proto} /></td>
                  <td className="font-mono">
                    {r.entry_listen_port ? <SensText blurred={blurred}>:{r.entry_listen_port}</SensText> : <Dash />}
                  </td>
                  <td className="font-mono">
                    {r.exit_host ? <SensText blurred={blurred}>{hostPortKey(r.exit_host, r.exit_port)}</SensText> : <Dash />}
                  </td>
                  <td className="num">{fmtBytes(r.total_bytes || 0)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableBox>
      )}
    </Card>
  )
}
