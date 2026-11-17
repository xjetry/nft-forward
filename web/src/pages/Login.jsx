import { useState } from 'react'
import { Navigate } from 'react-router-dom'
import { api } from '../lib/api'
import { useSession } from '../components/session'
import { Alert, Button, Card, CardSection, Field, Input, TextLink, useAction } from '../components/ui'

/** 提交前就能判定的缺项，按字段给出；没有问题的字段不出现在结果里。 */
function missingFields(username, password) {
  /** @type {{ username?: string, password?: string }} */
  const errors = {}
  if (!username.trim()) errors.username = '请输入用户名'
  if (!password) errors.password = '请输入密码'
  return errors
}

/**
 * 表单上方的提示：为什么现在还停在登录页。
 *
 * 按顺序取第一条成立的原因：
 * - 本次提交失败：服务端拒绝（密码错误、账号被禁用、尝试过于频繁），或请求
 *   没能发出去；
 * - 没能确认会话（session 为 error：/me 没有得到答复），给出重试；
 * - 服务端拒绝了浏览器里现有的登录（anonymous 且带 reason，如账号被禁用）。
 *   重试不会有别的结果，所以只说明原因，表单照常可以换一个账号登录；
 * - 提交已被接受，但随后 /me 仍然回答未登录。服务端在请求没带会话 Cookie、
 *   或按 Cookie 查不到有效会话时这样回答（requireAPIAuth），所以提示用户
 *   检查 Cookie。不提示的话，这种情况在用户眼里就是“点了登录没有任何反应”。
 *
 * @param {{
 *   outcome: null | { kind: 'rejected', message: string } | { kind: 'accepted' },
 *   session: import('../components/session').Session,
 *   retry: { run: () => Promise<void>, pending: boolean },
 * }} props
 */
function LoginNotice({ outcome, session, retry }) {
  if (outcome?.kind === 'rejected') {
    return <Alert tone="danger">{outcome.message}</Alert>
  }
  if (session.status === 'error') {
    return (
      <Alert tone="danger">
        无法确认登录状态：{session.message}{' '}
        <TextLink disabled={retry.pending} onClick={() => retry.run()}>重试</TextLink>
      </Alert>
    )
  }
  if (session.status === 'anonymous' && session.reason) {
    return <Alert tone="danger">当前登录的账号不可用：{session.reason}。可以换一个账号登录。</Alert>
  }
  if (outcome?.kind === 'accepted' && session.status === 'anonymous') {
    return <Alert tone="danger">用户名和密码正确，但登录状态没有保存下来。请确认浏览器允许本站保存 Cookie，然后重新登录。</Alert>
  }
  return null
}

/**
 * 登录页。位于应用外壳之外，自己负责整屏布局。
 *
 * 不变式：离开这一页只有一条路 —— 会话变成 authenticated 时渲染的
 * <Navigate>。“已登录的人打开 /login”与“刚刚登录成功”因此是同一条路径：
 * 登录成功后只让 SessionProvider 重新确认会话（refresh），这里不再另行跳转。
 * 目的地固定为 "/"，由根路由上的 RequireAuth 按角色把普通用户转到 /my。
 *
 * 会话还在确认中（loading）时也直接显示表单：未登录的访客不必等 /me 返回
 * 才能开始输入；确认到已登录时再离开。
 */
export default function Login() {
  const { session, refresh, panelName } = useSession()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  // 缺项提示在第一次提交之后才显示，之后随输入实时重算，不会留下过期的红字。
  const [attempted, setAttempted] = useState(false)
  const [outcome, setOutcome] = useState(/** @type {null | { kind: 'rejected', message: string } | { kind: 'accepted' }} */ (null))

  const missing = missingFields(username, password)
  const errors = attempted ? missing : {}

  const login = useAction(async () => {
    setAttempted(true)
    setOutcome(null)
    if (missing.username || missing.password) return
    // 本页路径就是 /login，api 在这里把 401 当作普通失败抛出（见 lib/api.js
    // 的 request），所以“用户名或密码错误”会走到下面的 onError。
    await api.post('/login', { username, password })
    await refresh()
    setOutcome({ kind: 'accepted' })
  }, {
    onError: err => setOutcome({ kind: 'rejected', message: err instanceof Error ? err.message : '登录失败' }),
  })
  const retry = useAction(refresh)

  if (session.status === 'authenticated') return <Navigate to="/" replace />

  return (
    <main className="grid min-h-dvh place-items-center bg-app px-4 py-10">
      <Card className="w-full max-w-sm">
        <CardSection>
          {/* 校验由上面的 missingFields 负责并显示在字段下方；noValidate 关掉
              浏览器自带的气泡提示，避免同一个问题出现两种样式的提示。 */}
          <form noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); void login.run() }}>
            <h1 className="break-words text-heading text-ink">{panelName}</h1>
            <LoginNotice outcome={outcome} session={session} retry={retry} />
            <Field label="用户名" error={errors.username}>
              <Input
                value={username}
                onChange={setUsername}
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                autoFocus
              />
            </Field>
            <Field label="密码" error={errors.password}>
              <Input type="password" value={password} onChange={setPassword} autoComplete="current-password" />
            </Field>
            <Button type="submit" variant="primary" loading={login.pending} className="w-full">登录</Button>
          </form>
        </CardSection>
      </Card>
    </main>
  )
}
