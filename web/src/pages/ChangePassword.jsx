import { useId, useState } from 'react'
import { api } from '../lib/api'
import { Page, PageHeader } from '../components/page'
import { Alert, Button, Card, CardSection, Field, Input, useAction, useToast } from '../components/ui'

const EMPTY = { old_password: '', new_password: '', confirm: '' }

/**
 * 提交前就能判定的问题，按字段给出；没有问题的字段不出现在结果里。
 *
 * “至少 6 位”“与原密码相同”对应服务端 /change-password 的两条规则，提前在
 * 本地指出是为了把提示落在对应的字段上。长度这里按 UTF-16 码元数、服务端
 * 按 UTF-8 字节数，前者不大于后者，所以本地放行的密码服务端不会再因长度
 * 拒绝。服务端的拒绝原因（包括只有它能判定的“原密码不正确”）显示在表单顶部。
 */
function validate(form) {
  /** @type {{ old_password?: string, new_password?: string, confirm?: string }} */
  const errors = {}
  if (!form.old_password) errors.old_password = '请输入原密码'
  if (form.new_password.length < 6) errors.new_password = '新密码至少 6 位'
  else if (form.new_password === form.old_password) errors.new_password = '新密码与原密码相同'
  if (form.confirm !== form.new_password) errors.confirm = '两次输入的新密码不一致'
  return errors
}

export default function ChangePassword() {
  const toast = useToast()
  const formId = useId()
  const [form, setForm] = useState(EMPTY)
  // 字段提示在第一次提交之后才显示，之后随输入实时重算，不会留下过期的红字。
  const [attempted, setAttempted] = useState(false)
  const [rejection, setRejection] = useState('')

  const found = validate(form)
  const errors = attempted ? found : {}
  const set = (key, value) => setForm(f => ({ ...f, [key]: value }))

  const change = useAction(async () => {
    setAttempted(true)
    setRejection('')
    if (Object.keys(found).length > 0) return
    await api.post('/change-password', { old_password: form.old_password, new_password: form.new_password })
    toast('新密码已保存')
    setForm(EMPTY)
    setAttempted(false)
  }, {
    onError: err => setRejection(err instanceof Error ? err.message : '新密码没有保存'),
  })

  return (
    <Page width="form">
      <PageHeader
        title="修改密码"
        actions={<Button type="submit" form={formId} variant="primary" loading={change.pending}>保存新密码</Button>}
      />
      <Card>
        {/* 校验由上面的 validate 负责并显示在字段下方；noValidate 关掉浏览器
            自带的气泡提示，避免同一个问题出现两种样式的提示。 */}
        <form id={formId} noValidate onSubmit={e => { e.preventDefault(); void change.run() }}>
          <CardSection className="flex flex-col gap-4">
            {rejection && <Alert tone="danger">{rejection}</Alert>}
            <Field label="原密码" layout="row" error={errors.old_password}>
              <Input
                type="password"
                value={form.old_password}
                onChange={v => set('old_password', v)}
                autoComplete="current-password"
                autoFocus
              />
            </Field>
            <Field label="新密码" layout="row" hint="至少 6 位" error={errors.new_password}>
              <Input
                type="password"
                value={form.new_password}
                onChange={v => set('new_password', v)}
                autoComplete="new-password"
              />
            </Field>
            <Field label="确认新密码" layout="row" error={errors.confirm}>
              <Input
                type="password"
                value={form.confirm}
                onChange={v => set('confirm', v)}
                autoComplete="new-password"
              />
            </Field>
          </CardSection>
          <CardSection>
            <p className="text-small text-ink-mut">保存后，这个账号在其他设备和浏览器上的登录会失效；当前浏览器保持登录。</p>
          </CardSection>
        </form>
      </Card>
    </Page>
  )
}
