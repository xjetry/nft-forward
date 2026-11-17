import { useId, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { copyToClipboard } from '../../lib/clipboard'
import { GB, fmtTrafficGB, nullInt, nullStr } from '../../lib/fmt'
import { useIsMobile } from '../../lib/useIsMobile'
import { useResource } from '../../lib/useResource'
import { ListItem, Page, PageHeader, SearchInput, SortableTh, TableScroll, Toolbar, nextSort } from '../../components/page'
import { ResourceView } from '../../components/ResourceView'
import { useUser } from '../../components/session'
import {
  Alert, Button, Card, Empty, Field, IconPlus, Input, Menu, Modal, ModalActions, Segmented, Spinner, StatusDot, Tag,
  TextLink, Tooltip, useAction, useConfirm, useToast,
} from '../../components/ui'
import { Expiry, ExpiryField, NewPasswordModal, OneTimeSecretModal, parseNumber, userConfirms } from './common'
import PasteGrantsModal from './PasteGrantsModal'

export default function UserList() {
  const navigate = useNavigate()
  const toast = useToast()
  const users = useResource(async () => (await api.get('/users'))?.users || [], [])
  // null | { kind: 'create' } | { kind: 'paste' }
  //      | { kind: 'password', username, password }
  //      | { kind: 'credentials', username, info, userId }
  const [dialog, setDialog] = useState(null)
  const close = () => setDialog(null)
  const count = users.state.status === 'ready' ? users.state.data.length : null

  const onCreated = ({ userId, username, info, copied }) => {
    if (copied) {
      toast(`已创建用户 ${username}，登录信息已复制`)
      navigate(`/users/${userId}`)
    } else {
      // 没复制成功时密码只存在于刚才的表单里，关掉就再也拿不到；改为把登录
      // 信息留在屏幕上，由管理员手动复制后再离开。
      setDialog({ kind: 'credentials', username, info, userId })
    }
  }

  return (
    <Page fill>
      <PageHeader
        title="用户"
        meta={count != null ? `共 ${count} 个` : undefined}
        actions={<>
          <Button onClick={() => setDialog({ kind: 'paste' })}>粘贴授权</Button>
          <Button variant="primary" icon={<IconPlus />} onClick={() => setDialog({ kind: 'create' })}>新建用户</Button>
        </>}
      />
      <ResourceView resource={users}>
        {list => (
          <UserTable
            users={list}
            reload={users.reload}
            onNewPassword={(username, password) => setDialog({ kind: 'password', username, password })}
          />
        )}
      </ResourceView>

      {dialog?.kind === 'create' && <CreateUserModal onClose={close} onCreated={onCreated} />}
      {dialog?.kind === 'paste' && <PasteGrantsModal onClose={close} onDone={users.reload} />}
      {dialog?.kind === 'password' && <NewPasswordModal username={dialog.username} password={dialog.password} onClose={close} />}
      {dialog?.kind === 'credentials' && (
        <OneTimeSecretModal
          title="登录信息"
          intro={`已创建用户 ${dialog.username}，但登录信息没能复制到剪贴板。手动复制并交给用户后再关闭。`}
          text={dialog.info}
          onClose={() => navigate(`/users/${dialog.userId}`)}
        />
      )}
    </Page>
  )
}

const UNSORTED = { col: null, dir: null }

function RoleTag({ role }) {
  return <Tag>{role === 'admin' ? '管理员' : '用户'}</Tag>
}

function UserState({ user }) {
  if (!user.disabled) return <StatusDot tone="ok">正常</StatusDot>
  const reason = nullStr(user.disable_reason)
  return (
    <span className="inline-flex items-center gap-2">
      <StatusDot tone="warn">已禁用</StatusDot>
      {reason && <span className="text-ink-mut">{reason}</span>}
    </span>
  )
}

function UserTable({ users, reload, onNewPassword }) {
  const { user: me } = useUser()
  const isMobile = useIsMobile()
  const toast = useToast()
  const confirm = useConfirm()
  const [search, setSearch] = useState('')
  const [sort, setSort] = useState(UNSORTED)

  // 接口是“翻转”语义：服务端按它当时的状态取反。结果以响应为准，而不是
  // 按列表里（可能已过期）的状态去猜。
  const toggle = useAction(async (id, username) => {
    const d = await api.post(`/users/${id}/toggle`)
    toast(d?.disabled ? `已禁用 ${username}` : `已启用 ${username}`)
    await reload()
  })
  const resetPassword = useAction(async (id, username) => {
    const d = await api.post(`/users/${id}/reset-password`)
    onNewPassword(username, d.new_password)
  })
  const remove = useAction(async (id, username) => {
    await api.del(`/users/${id}`)
    toast(`已删除用户 ${username}`)
    await reload()
  })
  const busy = id => [toggle, resetPassword, remove].some(action => action.pendingKey === id)

  const askResetPassword = async (u) => {
    if (await confirm(userConfirms.resetPassword(u.username))) resetPassword.run(u.id, u.username)
  }
  const askRemove = async (u) => {
    if (await confirm(userConfirms.remove(u.username, u.rule_count || 0))) remove.run(u.id, u.username)
  }
  const actionsOf = u => [
    { label: u.disabled ? '启用' : '禁用', onSelect: () => toggle.run(u.id, u.username) },
    // 管理员账号不支持重置密码与删除。
    ...(u.role === 'admin' ? [] : [
      { label: '重置密码', onSelect: () => askResetPassword(u) },
      { label: '删除用户', danger: true, onSelect: () => askRemove(u) },
    ]),
  ]

  const q = search.trim().toLowerCase()
  const matched = q ? users.filter(u => (u.username || '').toLowerCase().includes(q)) : users
  const expiryOf = u => nullInt(u.expires_at) || 0
  const rows = sort.col ? [...matched].sort((a, b) => {
    const ea = expiryOf(a)
    const eb = expiryOf(b)
    // 没有到期日的用户排在最后，与排序方向无关。
    if (!ea || !eb) return (ea ? -1 : 0) + (eb ? 1 : 0)
    return sort.dir === 'asc' ? ea - eb : eb - ea
  }) : matched

  return (
    <Card fill>
      <Toolbar>
        <SearchInput value={search} onChange={setSearch} placeholder="搜索用户名" />
      </Toolbar>
      {users.length === 0 ? (
        <Empty title="还没有用户" desc="新建用户并给他授权节点后，他就可以登录面板创建规则。" />
      ) : rows.length === 0 ? (
        <Empty title="没有匹配的用户" desc="换一个关键词试试。" />
      ) : isMobile ? (
        <div className="min-h-0 flex-1 overflow-y-auto">
          {rows.map(u => (
            <ListItem key={u.id} to={`/users/${u.id}`}>
              <div className="flex items-center justify-between gap-3">
                <span className="truncate font-medium text-ink">{u.username}</span>
                <UserState user={u} />
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-small text-ink-soft">
                <RoleTag role={u.role} />
                {u.role === 'user' && (
                  <>
                    <span>{u.rule_count || 0} / {u.max_forwards} 条规则</span>
                    <span>{fmtTrafficGB(u.traffic_used_bytes, u.traffic_quota_bytes)}</span>
                    {expiryOf(u) > 0 && <Expiry unix={expiryOf(u)} />}
                  </>
                )}
              </div>
            </ListItem>
          ))}
        </div>
      ) : (
        <TableScroll>
          <table className="tbl">
            <thead>
              <tr>
                <th>用户</th>
                <th>ID</th>
                <th>角色</th>
                <th className="num">规则</th>
                <th className="num">流量</th>
                <th>状态</th>
                <SortableTh col="expires" sort={sort} onSort={col => setSort(s => nextSort(s, col))}>到期</SortableTh>
                <th>备注</th>
                <th className="num">操作</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => {
                const isUser = u.role === 'user'
                return (
                  <tr key={u.id}>
                    <td><TextLink to={`/users/${u.id}`} tone="ink" className="font-medium">{u.username}</TextLink></td>
                    <td className="text-ink-mut">{u.id}</td>
                    <td><RoleTag role={u.role} /></td>
                    <td className="num">{isUser ? `${u.rule_count || 0} / ${u.max_forwards}` : <span className="text-ink-mut">--</span>}</td>
                    <td className="num">{isUser ? fmtTrafficGB(u.traffic_used_bytes, u.traffic_quota_bytes) : <span className="text-ink-mut">--</span>}</td>
                    <td><UserState user={u} /></td>
                    <td><Expiry unix={expiryOf(u)} /></td>
                    <td>
                      {u.admin_note && <Tooltip content={u.admin_note} className="block max-w-48 truncate text-ink-soft">{u.admin_note}</Tooltip>}
                    </td>
                    <td className="num">
                      {u.id === me.id ? (
                        <span className="text-ink-mut">当前账号</span>
                      ) : busy(u.id) ? (
                        <span role="status" aria-label="处理中" className="inline-flex size-control-sm items-center justify-center text-ink-mut">
                          <Spinner size={14} />
                        </span>
                      ) : (
                        <Menu size="sm" label={`${u.username} 的操作`} items={actionsOf(u)} />
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </TableScroll>
      )}
    </Card>
  )
}

/* 随机密码用的字符集去掉了容易看混的 O/0、I/l/1，抄写或口述时不会出错。 */
function genPassword(len = 16) {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
  const arr = new Uint32Array(len)
  crypto.getRandomValues(arr)
  return [...arr].map(n => chars[n % chars.length]).join('')
}

// 到期日默认留空（永不到期）：服务端把日期按 UTC 0 点换算成到期时刻，填当天
// 等于创建出来就已到期。
const NEW_USER = { username: '', password: '', role: 'user', maxForwards: '100', quotaGB: '0', expiresAt: '', subURL: '', adminNote: '' }

function CreateUserModal({ onClose, onCreated }) {
  const formId = useId()
  const [form, setForm] = useState(NEW_USER)
  const [errors, setErrors] = useState({})
  const set = key => value => setForm(f => ({ ...f, [key]: value }))
  const isUser = form.role === 'user'

  // 系统设置里的面板地址要写进复制给用户的登录信息。没设置时用当前访问的
  // 地址；读取失败时也用它，但要让管理员看见这一点并且能重试。
  const settings = useResource(() => api.get('/settings'), [])
  const origin = window.location.origin
  const panelURL = settings.state.status === 'ready' ? ((settings.state.data?.panel_url || '').trim() || origin) : origin

  const create = useAction(async () => {
    // 面板地址还没读回来时不提交：此刻复制出去的登录信息会带上错的地址。
    if (settings.state.status === 'loading') return
    const username = form.username.trim()
    const maxForwards = parseNumber(form.maxForwards, { min: 1, integer: true })
    const quotaGB = parseNumber(form.quotaGB)
    const subURL = form.subURL.trim()
    const found = {}
    if (!username) found.username = '填写用户名'
    if (!form.password) found.password = '填写密码，或点“随机生成”'
    if (isUser) {
      if (maxForwards == null) found.maxForwards = '填写不小于 1 的整数'
      if (quotaGB == null) found.quotaGB = '填写不小于 0 的数'
      if (subURL && !/^https?:\/\//.test(subURL)) found.subURL = '订阅地址要以 http:// 或 https:// 开头'
    }
    setErrors(found)
    if (Object.keys(found).length) return

    // 复制放在创建请求之前、仍在点击手势之内：非 HTTPS 的面板只能用
    // execCommand 复制（见 lib/clipboard），浏览器只在用户手势还有效时放行，
    // 等一次网络往返就过期了。登录信息在提交前已经全部确定，所以可以先复制；
    // 复制结果留到创建成功之后才告知，创建失败时不会声称凭据已复制。
    const info = `面板地址：${panelURL}\n用户名：${username}\n密码：${form.password}`
    let copied = true
    try { await copyToClipboard(info) } catch { copied = false }
    const res = await api.post('/users', {
      username,
      password: form.password,
      role: form.role,
      ...(isUser ? {
        max_forwards: maxForwards,
        traffic_quota_bytes: Math.round(quotaGB * GB),
        expires_at: form.expiresAt || undefined,
        landing_sub_url: subURL || undefined,
        admin_note: form.adminNote.trim() || undefined,
      } : {}),
    })
    onCreated({ userId: res.user.id, username, info, copied })
  })

  return (
    <Modal
      title="新建用户"
      onClose={onClose}
      busy={create.pending}
      footer={
        <ModalActions note={settings.state.status === 'loading' ? '正在读取面板地址…' : `登录信息里的面板地址：${panelURL}`}>
          <Button onClick={onClose} disabled={create.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.pending} disabled={settings.state.status === 'loading'}>
            创建并复制登录信息
          </Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); create.run() }}>
        {settings.state.status === 'error' && (
          <Alert tone="warn">
            没能读取系统设置里的面板地址（{settings.state.message}），登录信息里会写当前访问的地址 {origin}。{' '}
            <TextLink onClick={settings.reload}>重试</TextLink>
          </Alert>
        )}
        <Field label="用户名" error={errors.username}>
          <Input value={form.username} onChange={set('username')} placeholder="登录用的用户名" data-autofocus />
        </Field>
        <Field label="密码" error={errors.password}>
          <div className="flex items-center gap-2">
            <Input mono className="min-w-0 flex-1" value={form.password} onChange={set('password')} />
            <Button onClick={() => set('password')(genPassword())}>随机生成</Button>
          </div>
        </Field>
        <Field label="角色" hint={isUser ? undefined : '管理员可以管理全部节点、规则与用户，没有配额与到期日。'}>
          <Segmented
            value={form.role}
            onChange={set('role')}
            options={[{ value: 'user', label: '用户' }, { value: 'admin', label: '管理员' }]}
          />
        </Field>
        {isUser && (
          <>
            <Field label="规则配额" error={errors.maxForwards} hint="该用户最多可创建的规则数。">
              <Input type="number" min="1" step="1" className="w-44" value={form.maxForwards} onChange={set('maxForwards')} suffix="条" />
            </Field>
            <Field label="流量配额" error={errors.quotaGB} hint="0 = 不限。">
              <Input type="number" min="0" step="0.1" className="w-44" value={form.quotaGB} onChange={set('quotaGB')} suffix="GB" />
            </Field>
            <ExpiryField value={form.expiresAt} onChange={set('expiresAt')} />
            <Field label="订阅地址" optional error={errors.subURL} hint="落地节点的订阅链接，之后可以在用户详情页修改。">
              <Input mono value={form.subURL} onChange={set('subURL')} placeholder="https://example.com/api/sub/xxxx" />
            </Field>
            <Field label="管理备注" optional hint="只有管理员能看到。">
              <Input value={form.adminNote} onChange={set('adminNote')} />
            </Field>
          </>
        )}
      </form>
    </Modal>
  )
}
