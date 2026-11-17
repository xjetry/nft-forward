import { useId, useState } from 'react'
import { api } from '../lib/api'
import { POOL_SIZE_MAX, parsePoolSize, sameForm, toForm, toSettings } from '../lib/settingsForm'
import { useDraft } from '../lib/useDraft'
import { useResource } from '../lib/useResource'
import { ApiTokenSection } from '../components/ApiTokenSection'
import { Page, PageHeader } from '../components/page'
import { ResourceView } from '../components/ResourceView'
import { useSession } from '../components/session'
import { Button, Card, CardHeader, CardSection, Checkbox, Field, Input, useAction, useToast } from '../components/ui'

/**
 * 设置表单。它不持有表单值：值由调用方给出（form），修改经 onChange 交回去，
 * 所以页头的保存按钮与这里看到的是同一份值、同一个“有没有改动”。
 *
 * 保存在途时（busy）控件不接受修改：这期间的改动不在这次保存里，锁住输入，
 * 保存落定时“设置已保存”说的就是屏幕上的这一份。输入框用只读而不是禁用 ——
 * 禁用会让正在输入的那个框失去焦点。
 *
 * 提交按钮在页头（PageHeader 的 actions），通过 form 属性指向这里的 <form>，
 * 所以点按钮与在输入框里按回车走的是同一个 onSubmit。没有改动时按钮是禁用
 * 的；onSubmit 自己也不提交没有改动的表单，不依赖浏览器在默认按钮被禁用时
 * 不触发回车提交。
 *
 * @param {{
 *   id: string,
 *   form: import('../lib/settingsForm').SettingsForm,
 *   dirty: boolean,
 *   busy: boolean,
 *   onChange: (form: import('../lib/settingsForm').SettingsForm) => void,
 *   onSave: (settings: import('../lib/settingsForm').Settings) => void,
 * }} props
 */
function SettingsForm({ id, form, dirty, busy, onChange, onSave }) {
  // 校验提示在第一次提交之后才显示，之后随输入实时重算，不会留下过期的红字。
  // 表单回到没有改动的状态（保存成功，或用户把输入改回了原样）时，这一轮
  // 提交结束，下一次修改重新从“还没提交过”开始。
  const [attempted, setAttempted] = useState(false)
  if (!dirty && attempted) setAttempted(false)
  const set = (key, value) => onChange({ ...form, [key]: value })

  const poolSizeError = attempted && parsePoolSize(form.pool_size) === null
    ? `请输入 0 到 ${POOL_SIZE_MAX} 之间的整数`
    : undefined

  const submit = (e) => {
    e.preventDefault()
    if (!dirty || busy) return
    setAttempted(true)
    const settings = toSettings(form)
    if (settings === null) return
    // 把输入换成要提交的形式（去掉了首尾空白等）：服务端存下的就是这一份，
    // 刷新回来时两者相同，草稿随之结束（见 lib/useDraft）。
    onChange(toForm(settings))
    onSave(settings)
  }

  return (
    // contents：表单只是把两张卡片归到同一次提交里，不参与布局，卡片仍然直接
    // 排在 Page 的纵向节奏里。noValidate 关掉浏览器自带的气泡提示，校验结果
    // 统一显示在字段下方。
    <form id={id} noValidate onSubmit={submit} className="contents">
      <Card>
        <CardHeader title="面板信息" />
        <CardSection className="flex flex-col gap-4">
          <Field label="面板地址" layout="row" hint="节点通过这个地址连接面板；它会写进节点安装命令和新建用户的登录信息。">
            <Input
              mono
              readOnly={busy}
              value={form.panel_url}
              onChange={v => set('panel_url', v)}
              placeholder="https://panel.example.com"
              inputMode="url"
              autoCapitalize="none"
              spellCheck={false}
            />
          </Field>
          <Field label="面板名称" layout="row" hint="显示在登录页、侧栏和浏览器标签页上。留空时显示 nft-forward。">
            <Input readOnly={busy} value={form.panel_name} onChange={v => set('panel_name', v)} placeholder="nft-forward" />
          </Field>
        </CardSection>
      </Card>

      <Card>
        <CardHeader title="转发" />
        <CardSection className="flex flex-col gap-4">
          <Field label="显示倍率" layout="row">
            <div className="flex min-h-control items-center">
              <Checkbox disabled={busy} checked={form.show_rate_to_user} onChange={v => set('show_rate_to_user', v)}>
                向普通用户显示节点与链路的计费倍率
              </Checkbox>
            </div>
          </Field>
          <Field
            label="TCP 连接池"
            layout="row"
            hint="userspace 模式下，每个转发端口预先与目标建立的 TCP 连接数。0 表示不预先建立，默认值是 4。"
            error={poolSizeError}
          >
            <Input
              type="number"
              min={0}
              max={POOL_SIZE_MAX}
              step={1}
              readOnly={busy}
              value={form.pool_size}
              onChange={v => set('pool_size', v)}
              className="w-28"
            />
          </Field>
        </CardSection>
      </Card>
    </form>
  )
}

export default function Settings() {
  const toast = useToast()
  const { setPanelName } = useSession()
  const formId = useId()
  const settings = useResource(() => api.get('/settings'), [])
  // 服务端的设置，读到之后才有。表单显示的是它，叠上用户还没保存的改动（草稿）；
  // 草稿与它内容相同即没有改动。
  const saved = settings.state.status === 'ready' ? toForm(settings.state.data) : null
  const draft = useDraft(saved, (edited, stored) => stored !== null && sameForm(edited, stored))

  const save = useAction(async (payload) => {
    await api.post('/settings', payload)
    toast('设置已保存')
    // 面板名称还显示在侧栏和标签页标题上，那两处读的是 SessionProvider 里的
    // 那一份。保存已经成功，新名称就是刚提交的值（toSettings 已经去掉首尾
    // 空白，与服务端存下的相同），直接交过去，不需要为此重新确认一次会话。
    setPanelName(payload.panel_name)
    await settings.reload()
  })

  return (
    <Page width="form">
      {/* 表单的初值只来自已加载的服务端设置：没加载出来就不渲染表单，也不
          渲染指向它的保存按钮 —— 否则保存出去的是与服务端无关的值，会覆盖
          真实设置。没有改动时按钮禁用：此时保存不会改变任何东西。 */}
      <PageHeader
        title="系统设置"
        actions={saved !== null && (
          <Button type="submit" form={formId} variant="primary" loading={save.pending} disabled={!draft.dirty}>保存设置</Button>
        )}
      />
      <ResourceView resource={settings}>
        {/* ResourceView 只在设置读到之后渲染这里，此时 saved 不是 null，
            draft.value 因此总是一份表单值。 */}
        {() => (
          <SettingsForm
            id={formId}
            form={draft.value}
            dirty={draft.dirty}
            busy={save.pending}
            onChange={draft.set}
            onSave={save.run}
          />
        )}
      </ResourceView>
      <ApiTokenSection />
    </Page>
  )
}
