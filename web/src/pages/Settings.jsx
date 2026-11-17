import { useId, useState } from 'react'
import { api } from '../lib/api'
import { useResource } from '../lib/useResource'
import { ApiTokenSection } from '../components/ApiTokenSection'
import { Page, PageHeader } from '../components/page'
import { ResourceView } from '../components/ResourceView'
import { useSession } from '../components/session'
import { Button, Card, CardHeader, CardSection, Checkbox, Field, Input, useAction, useToast } from '../components/ui'

/* 服务端接受的连接池大小上限（POST /settings 对 pool_size 的校验是 0–64）。 */
const POOL_SIZE_MAX = 64

/**
 * 服务端的设置 → 表单值。连接池大小以字符串进表单：输入过程中它可以是空的
 * 或写到一半的，提交时再由 parsePoolSize 判定。
 */
function toForm(settings) {
  return {
    panel_url: settings.panel_url,
    panel_name: settings.panel_name,
    show_rate_to_user: settings.show_rate_to_user,
    pool_size: String(settings.pool_size),
  }
}

/** 连接池大小的输入 → 0 到 POOL_SIZE_MAX 的整数；不是这样的整数时返回 null。 */
function parsePoolSize(text) {
  const digits = text.trim()
  if (!/^\d+$/.test(digits)) return null
  const n = Number(digits)
  return n <= POOL_SIZE_MAX ? n : null
}

/** 两份表单值的内容是否相同。字段都是原始值，逐个比较即可。 */
function sameForm(a, b) {
  return Object.keys(a).every(key => a[key] === b[key])
}

/**
 * 设置表单。
 *
 * 不变式：表单值是服务端设置的一份本地副本。它基于某一份服务端快照
 * （basis）；只有调用方交来的 initial 在内容上与 basis 不同时 —— 保存改变了
 * 服务端的值，表单才对齐到新的快照，显示服务端实际存下的值。内容没变时
 * （刷新失败、data 仍是上一份，或者这次保存没有改变服务端的值）表单值只
 * 随用户输入变化。对齐发生在渲染期、不重新挂载，输入框的焦点因此保留。
 *
 * 保存在途时（busy）控件不接受修改：保存落定后表单可能对齐到新的快照，这
 * 段时间里敲进去的内容会随之丢掉。输入框用只读而不是禁用 —— 禁用会让
 * 正在输入的那个框失去焦点。
 *
 * 提交按钮在页头（PageHeader 的 actions），通过 form 属性指向这里的 <form>，
 * 所以点按钮与在输入框里按回车走的是同一个 onSubmit。
 *
 * @param {{
 *   id: string,
 *   initial: ReturnType<typeof toForm>,
 *   busy: boolean,
 *   onSave: (payload: { panel_url: string, panel_name: string, show_rate_to_user: boolean, pool_size: number }) => void,
 * }} props
 */
function SettingsForm({ id, initial, busy, onSave }) {
  const [form, setForm] = useState(initial)
  const [basis, setBasis] = useState(initial)
  // 校验提示在第一次提交之后才显示，之后随输入实时重算，不会留下过期的红字。
  const [attempted, setAttempted] = useState(false)
  if (!sameForm(basis, initial)) {
    setBasis(initial)
    setForm(initial)
    setAttempted(false)
  }
  const set = (key, value) => setForm(f => ({ ...f, [key]: value }))

  const poolSize = parsePoolSize(form.pool_size)
  const poolSizeError = attempted && poolSize === null ? `请输入 0 到 ${POOL_SIZE_MAX} 之间的整数` : undefined

  const submit = (e) => {
    e.preventDefault()
    setAttempted(true)
    if (poolSize === null) return
    onSave({
      panel_url: form.panel_url,
      panel_name: form.panel_name,
      show_rate_to_user: form.show_rate_to_user,
      pool_size: poolSize,
    })
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

  const save = useAction(async (payload) => {
    await api.post('/settings', payload)
    toast('设置已保存')
    // 面板名称还显示在侧栏和标签页标题上，那两处读的是 SessionProvider 里的
    // 那一份。保存已经成功，新名称就是刚提交的值（服务端只做首尾去空白，
    // 见 apiSaveSettings），直接交过去，不需要为此重新确认一次会话。
    setPanelName(payload.panel_name.trim())
    await settings.reload()
  })

  return (
    <Page width="form">
      {/* 表单的初值只来自已加载的服务端设置：没加载出来就不渲染表单，也不
          渲染指向它的保存按钮 —— 否则保存出去的是与服务端无关的值，会覆盖
          真实设置。 */}
      <PageHeader
        title="系统设置"
        actions={settings.state.status === 'ready' && (
          <Button type="submit" form={formId} variant="primary" loading={save.pending}>保存设置</Button>
        )}
      />
      <ResourceView resource={settings}>
        {data => <SettingsForm id={formId} initial={toForm(data)} busy={save.pending} onSave={save.run} />}
      </ResourceView>
      <ApiTokenSection />
    </Page>
  )
}
