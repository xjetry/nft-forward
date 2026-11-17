import { useId, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { ROLE_LANDING, addLocalURI, hostPortKey, saveLocalRoles, setLocalRoleBatch, tryParseURI } from '../lib/landing'
import { nodeStack } from '../lib/nodeHealth'
import { NodeTypeIcon } from './net'
import { ProbeButton } from './probe'
import { Alert, Button, Field, Input, Modal, ModalActions, Segmented, Select, useAction, useConfirm, useToast } from './ui'

/* 规则的写入侧：创建 / 复制 / 编辑用的表单弹窗，以及删除动作。
   管理员页面（/rules）与用户页面（/my/rules）共用这里的全部内容；两侧的
   差别只有 RULE_SCOPES 里列出的那几项。 */

/* 规则接口的两个作用域。请求体与规则字段两侧相同，不同的只有路径，以及
   创建成功的响应里新规则 id 所在的字段。页面把作用域交给 RuleDialog 与
   useDeleteRule，发出写请求的代码因此只有本文件一处。 */
export const RULE_SCOPES = {
  admin: { admin: true, api: '/rules', path: '/rules', createdId: (res) => res.rule.id },
  my: { admin: false, api: '/my/rules', path: '/my/rules', createdId: (res) => res.rule_id },
}

/* ---------- 表单模型 ---------- */

/* 表单状态的形状（ruleToForm 产出，ruleFormToPayload 消费）：
     name / proto / mode / comment   字符串
     node_id        入口节点 id（数字），未选时为 null
     via_node_ids   线路层节点 id（数字）的有序数组
     entry_port     输入框里的文本，'' 表示自动分配
     exit           可辨识联合：
                      { kind: 'custom', address }   手填的 host:port
                      { kind: 'landing', key }      落地节点的规范键（hostPortKey）
   出口的两种取值各有自己的字段，exitTarget 只读当前 kind 的那一个，界面也只
   渲染当前 kind 的控件，所以提交出去的出口一定是屏幕上看得见的那个值。 */
const BLANK = {
  name: '',
  node_id: null,
  via_node_ids: [],
  proto: 'tcp',
  mode: 'kernel',
  entry_port: '',
  exit: { kind: 'custom', address: '' },
  comment: '',
}

/** 出口的 host:port；尚未填写 / 选择时为 ''。 */
function exitTarget(exit) {
  return exit.kind === 'landing' ? exit.key : exit.address.trim()
}

/** 文本是否是合法端口号（1–65535 的整数）。 */
export function isPort(text) {
  if (!/^\d+$/.test(text)) return false
  const n = Number(text)
  return n >= 1 && n <= 65535
}

/**
 * 把一条规则映射成表单状态（编辑时的预填）。
 *
 * 出口是“落地节点”还是“自定义地址”只看它是否在当前用户的落地节点目录里
 * （landingByKey，见 lib/landing.js 的 buildCatalog）：表单的落地节点下拉框
 * 列的就是这张目录，出口不在其中时只能以地址的形式编辑。
 *
 * @param {object} rule 规则（列表行或详情，两者都带 exit_host / exit_port）
 * @param {Map<string, object>} landingByKey
 */
export function ruleToForm(rule, landingByKey) {
  const key = hostPortKey(rule.exit_host, rule.exit_port)
  return {
    name: rule.name,
    node_id: rule.node_id,
    via_node_ids: rule.via_node_ids || [],
    proto: rule.proto,
    // 表单里的模式指出口段（链路最后一跳）。规则还没有任何一跳时两个字段
    // 都为空，取默认的 kernel。
    mode: rule.exit_mode || rule.entry_mode || 'kernel',
    entry_port: rule.entry_listen_port > 0 ? String(rule.entry_listen_port) : '',
    exit: landingByKey.has(key) ? { kind: 'landing', key } : { kind: 'custom', address: key },
    comment: rule.comment || '',
  }
}

/* “复制为新规则”的预填：链路与出口照搬，名称加后缀。入口端口留空 —— 原规则
   还占着它的端口，同一个入口节点上不能再分配一次。 */
export function copyInitial(rule, landingByKey) {
  return { ...ruleToForm(rule, landingByKey), name: `${rule.name}_Copy`, entry_port: '' }
}

/* 表单状态 → 创建 / 编辑接口的请求体。所有规则页面的请求体都由这里产生，
   新增字段不会在某一个调用点漏掉。
   - 表单里的 mode 是出口段的模式，以 exit_mode 发出。请求体没有顶层的 mode
     字段，带了它服务端回 400。
   - via_node_ids 总是发出：表单持有整条链路，空数组表示“清空线路层”，而
     不是“保持不变”。
   - 不发 entry_family：服务端按入口节点的中转地址推导。 */
export function ruleFormToPayload(form) {
  return {
    node_id: form.node_id,
    name: form.name,
    proto: form.proto,
    exit_mode: form.mode || undefined,
    exit: exitTarget(form.exit),
    entry_port: form.entry_port ? Number(form.entry_port) : undefined,
    comment: form.comment || undefined,
    via_node_ids: form.via_node_ids,
  }
}

/* ---------- 链路推导 ---------- */

/* 节点的角色位（服务端 nodes.roles / 授权覆盖后的生效值）。 */
const NODE_ROLE_ENTRY = 1
const NODE_ROLE_VIA = 2

/* Select 的 label 必须是纯字符串（搜索按它过滤），所以 IP 栈与倍率写成文字
   前后缀；单点 / 组合 / 自身的区分放在选项的 icon 里。 */
function nodeLabel(n, showRate) {
  const { entryV4, entryV6, exitV6 } = nodeStack(n)
  const parts = [[entryV4 && 'v4', entryV6 && 'v6'].filter(Boolean).join('+')]
  if (exitV6 !== entryV6) parts.push(exitV6 ? '出口支持v6' : '出口不支持v6')
  const stack = parts.filter(Boolean).join(' ')
  const rate = n.rate_multiplier ?? 1
  return `${stack ? `[${stack}] ` : ''}${n.name}${showRate !== false && rate !== 1 ? ` (×${rate})` : ''}`
}

/**
 * 由表单里的入口与线路层选择，推导出级联选择器要画的每一级。
 *
 * 第 i 级线路层的候选 = 绑定图里上一级（第 0 级的上一级是入口）的下游，再
 * 限定为：在 nodes 里（用户侧的 nodes 已经是授权范围内的节点）、带线路层
 * 角色、不是入口本身、也不在已选的链上。没有 roles 字段的节点按“不是线路层”
 * 处理 —— 与入口的缺省相反：一个没有声明角色的节点不应当悄悄变成可选的
 * 中间跳。
 *
 * 某一级没有候选也没有已选值时停止：有下拉框就一定有得选。
 *
 * 返回：
 *   entry    入口节点（未选或不在 nodes 里时为 undefined）
 *   ids      界面上画出来的线路层 id —— via_node_ids 里仍在 nodes 中的那些。
 *            选择与提交都以它为准，画出来的链与提交的链因此是同一条
 *   levels   [{ level, candidates, chosen, mustVia }]；mustVia 表示上一级不
 *            允许直接转发到出口，这一级没有“直接转发”选项
 *   tail     链路最后一跳：真正去连接出口的节点。“不能直接转发到出口”
 *            （no_direct_exit）是物理节点的设置；组合节点的这个字段由服务端按
 *            它最后一个物理成员给出，所以这里对单点与组合读同一个字段，不必
 *            （用户侧也拿不到）成员节点。服务端建规则时按同一个物理跳校验
 *   issue    链路不能以 tail 结束时的说明：
 *            { at: 'entry' | 级号, message, pending }。pending 为 true 表示
 *            at 那一级还等着用户选择；为 false 表示已无可选，当前选择本身
 *            就走不通
 */
function deriveChain(form, nodes, bindings) {
  const byId = new Map(nodes.map(n => [n.id, n]))
  const entryId = form.node_id
  const entry = entryId === null ? undefined : byId.get(entryId)
  const ids = form.via_node_ids.filter(id => byId.has(id))

  const levels = []
  if (entryId !== null) {
    let upstream = entryId
    const used = []
    for (let level = 0; ; level++) {
      const chosen = ids[level]
      const candidates = bindings
        .filter(b => b.upstream_node_id === upstream)
        .map(b => byId.get(b.downstream_node_id))
        .filter(n => n && ((n.roles ?? 0) & NODE_ROLE_VIA) !== 0 && n.id !== entryId && !used.includes(n.id))
      if (candidates.length === 0 && chosen === undefined) break
      levels.push({ level, candidates, chosen, mustVia: !!byId.get(upstream)?.no_direct_exit })
      if (chosen === undefined) break
      used.push(chosen)
      upstream = chosen
    }
  }

  const tail = ids.length ? byId.get(ids[ids.length - 1]) : entry
  let issue = null
  if (tail?.no_direct_exit) {
    const open = levels.length && levels[levels.length - 1].chosen === undefined ? levels[levels.length - 1] : null
    issue = open
      ? { at: open.level, pending: true, message: `「${tail.name}」不能直接转发到出口，请选择线路层。` }
      : {
        at: ids.length ? ids.length - 1 : 'entry',
        pending: false,
        message: `「${tail.name}」不能直接转发到出口，它后面也没有可选的线路层。请改选其它节点。`,
      }
  }
  return { byId, entry, ids, levels, tail, issue }
}

/* 链路预览里把组合节点展开成它的成员：规则里存的是组合节点的引用，实际
   部署的是它的成员。成员来自节点数据里已解析好的 hops。 */
function memberNames(byId, id) {
  const n = byId.get(id)
  if (!n) return []
  if (n.node_type === 'composite' && n.hops?.length) {
    return n.hops.flatMap(h => (h.node_type === 'composite' ? memberNames(byId, h.node_id) : [h.name || `#${h.node_id}`]))
  }
  return [n.name]
}

/** 校验表单，返回 { 字段: 错误文字 }；没有错误时是空对象。 */
function validate(form, chain) {
  const errors = {}
  if (!form.name.trim()) errors.name = '请填写规则名称'
  if (form.node_id == null) errors.node = '请选择入口节点'
  if (chain.issue) errors.chain = chain.issue.message
  if (form.entry_port !== '' && !isPort(form.entry_port)) errors.entry_port = '端口须为 1–65535 的整数'
  if (!exitTarget(form.exit)) errors.exit = form.exit.kind === 'landing' ? '请选择落地节点' : '请填写出口地址'
  return errors
}

const PROTO_OPTIONS = [{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]
const MODE_OPTIONS = [{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]
/* 线路层下拉里“这一级不接线路层、直接转发到出口”那个选项的值。 */
const DIRECT = 'direct'
const EXIT_KIND_OPTIONS = [{ value: 'custom', label: '自定义地址' }, { value: 'landing', label: '落地节点' }]

/**
 * 规则表单弹窗。只管表单状态、链路级联与校验；发请求由调用方的
 * onSubmit(payload) 负责（通常经 RuleDialog）。
 *
 * 前提：挂载时 nodes / bindings / catalog 已经就绪 —— 调用方只在页面资源
 * ready 之后渲染它。表单初值因此在挂载时一次算好，不存在“数据到了再重置
 * 表单”的步骤。
 *
 * @param {{
 *   title: string,
 *   submitLabel: string,
 *   initial?: ReturnType<typeof ruleToForm> | null,
 *   nodes: object[],
 *   bindings: Array<{ upstream_node_id: number, downstream_node_id: number }>,
 *   catalog: object,
 *   userId: number,
 *   showRate?: boolean,
 *   onSubmit: (payload: ReturnType<typeof ruleFormToPayload>) => Promise<unknown>,
 *   onClose: () => void,
 * }} props `catalog` 是 useLandingCatalog 就绪后的 data；`showRate === false`
 * 时节点选项里不显示倍率（用户侧由面板设置决定）。
 */
export function RuleFormModal({ title, submitLabel, initial, nodes, bindings, catalog, userId, showRate, onSubmit, onClose }) {
  const toast = useToast()
  const formId = useId()
  const [form, setForm] = useState(() => initial ?? BLANK)
  // 校验错误在第一次尝试提交之后才显示，之后随输入实时更新。
  const [attempted, setAttempted] = useState(false)
  const set = (patch) => setForm(f => ({ ...f, ...patch }))

  const chain = deriveChain(form, nodes, bindings)
  const errors = validate(form, chain)
  const shown = attempted ? errors : {}

  // 换入口会让已选的线路层失效：它们是旧入口在绑定图上的下游。Select 在点击
  // 已选中的选项时也会触发 onChange，所以先比较 —— 重选同一个入口不清空线路层。
  const pickEntry = (id) => setForm(f => (id === f.node_id ? f : { ...f, node_id: id, via_node_ids: [] }))
  // 从 chain.ids（画出来的链）截断再追加，而不是从 via_node_ids 原值：两者在
  // 有节点已不在 nodes 里时下标对不上。
  const pickVia = (level, id) => set({ via_node_ids: [...chain.ids.slice(0, level), ...(id === DIRECT ? [] : [id])] })

  // 切换出口类型是一次换算，不留下另一类的值：落地 → 自定义时把该节点的
  // 地址交给用户继续改；自定义 → 落地时，只有手填的地址恰好是某个落地节点
  // 才选中它。
  const switchExitKind = (kind) => setForm(f => {
    if (f.exit.kind === kind) return f
    if (kind === 'custom') return { ...f, exit: { kind, address: f.exit.key } }
    const key = f.exit.address.trim()
    return { ...f, exit: { kind, key: catalog.landingByKey.has(key) ? key : '' } }
  })

  /* 出口地址框里填的若是代理链接而不是 host:port，就把它收为本浏览器的落地
     节点，并让出口指向它。收为落地节点要做两件事：把链接存进手动 URI 列表、
     在本地角色里给它标上“落地”。目录的口径是“节点来源里有它，且生效角色含
     落地”（lib/landing.js 的 buildCatalog），一个此前不在任何来源里的节点，
     两步缺一步都进不了目录。
     返回采用后的表单；不是代理链接时原样返回。 */
  const adoptPastedProxy = (current) => {
    if (current.exit.kind !== 'custom') return current
    const uri = current.exit.address.trim()
    const node = tryParseURI(uri)
    if (!node) return current
    try {
      addLocalURI(userId, uri)
      saveLocalRoles(userId, setLocalRoleBatch(catalog.serverRoles, catalog.localRoles, [node], ROLE_LANDING, true))
    } catch (err) {
      // 存不进本浏览器就没有这个落地节点可指：出口保持原样，由校验去提示。
      toast(err instanceof Error ? err.message : '没能保存到本浏览器', 'error')
      return current
    }
    const next = {
      ...current,
      exit: { kind: 'landing', key: hostPortKey(node.host, node.port) },
      comment: current.comment.trim() || !node.name ? current.comment : node.name,
    }
    setForm(next)
    toast(`已识别 ${node.protocol} 代理，并存为本浏览器的落地节点`)
    return next
  }

  const submit = useAction(async () => {
    // 在输入框里按回车提交时不会先触发失焦，所以提交路径上也要识别一次。
    const current = adoptPastedProxy(form)
    setAttempted(true)
    // 采用代理链接只改出口与备注，链路推导不受影响，chain 可以直接沿用。
    if (Object.keys(validate(current, chain)).length > 0) return
    await onSubmit(ruleFormToPayload({ ...current, via_node_ids: chain.ids }))
  })

  const option = (n) => ({ value: n.id, label: nodeLabel(n, showRate), icon: <NodeTypeIcon type={n.node_type} /> })
  // 只有带入口角色的节点能作入口。没有 roles 字段的节点按“可作入口”处理。
  const entryNodes = nodes.filter(n => ((n.roles ?? NODE_ROLE_ENTRY) & NODE_ROLE_ENTRY) !== 0)
  const entryGroups = [
    { label: '单点', options: entryNodes.filter(n => n.node_type !== 'composite').map(option) },
    { label: '组合', options: entryNodes.filter(n => n.node_type === 'composite').map(option) },
  ].filter(g => g.options.length > 0)
  // 选项只显示协议与节点名，不显示连接地址；值是规范键，也就是规则的出口。
  const landingOptions = catalog.landing.map(n => ({
    value: hostPortKey(n.host, n.port),
    label: `${n.protocol ? `[${n.protocol}] ` : ''}${n.name || '（未命名）'}`,
  }))

  const issueAt = (at) => (chain.issue?.at === at ? chain.issue : null)
  const chainError = (at) => {
    const issue = issueAt(at)
    return issue && (attempted || !issue.pending) ? issue.message : undefined
  }
  const chainPreview = chain.ids.length > 0
    ? [...memberNames(chain.byId, chain.entry?.id), ...chain.ids.flatMap(id => memberNames(chain.byId, id)), '出口'].join(' → ')
    : null
  // 模式作用于出口段（最后一跳 → 出口）。链路只有一个单点节点时它就是整条
  // 规则的模式；有组合节点或线路层时，节点之间各跳的模式由节点配置决定。
  const multiHop = chain.tail?.node_type === 'composite' || chain.ids.length > 0

  const target = exitTarget(form.exit)
  // 连通性从链路最后一跳测：去连接出口的是它，从入口直连出口测的是一条流量
  // 不会走的路径。最后一跳是组合节点时，服务端用它的最后一个成员去连。
  const probeFrom = chain.tail?.id ?? form.node_id
  const probe = form.node_id != null && target ? (
    <div className="flex items-center gap-2 text-caption text-ink-mut">
      {/* 探测结果属于“这个节点、这个协议、这个出口”；任一项变了就换一个
          按钮实例，上一个出口的结果不会留在新出口旁边。 */}
      <ProbeButton key={`${probeFrom}|${form.proto}|${target}`} request={{ target, nodeId: probeFrom, proto: form.proto }} />
      <span>从{chain.tail ? `「${chain.tail.name}」` : '入口节点'}测试到出口的连通性</span>
    </div>
  ) : null

  return (
    <Modal
      title={title}
      onClose={onClose}
      busy={submit.pending}
      footer={
        <ModalActions>
          <Button onClick={onClose} disabled={submit.pending}>取消</Button>
          <Button variant="primary" type="submit" form={formId} loading={submit.pending}>{submitLabel}</Button>
        </ModalActions>
      }
    >
      <form id={formId} noValidate className="flex flex-col gap-4" onSubmit={e => { e.preventDefault(); submit.run() }}>
        <Field label="名称" error={shown.name}>
          <Input value={form.name} onChange={v => set({ name: v })} placeholder="例如 web-hk" data-autofocus />
        </Field>

        <Field label="入口节点" error={shown.node ?? chainError('entry')}>
          <Select value={form.node_id} onChange={pickEntry} placeholder="选择入口节点" searchable tabs groups={entryGroups} />
        </Field>
        {chain.levels.map(({ level, candidates, chosen, mustVia }) => (
          <Field
            key={level}
            label={level === 0 ? '线路层' : `线路层 ${level + 1}`}
            hint={issueAt(level)?.message}
            error={chainError(level)}
          >
            <Select
              value={chosen ?? (mustVia ? null : DIRECT)}
              onChange={id => pickVia(level, id)}
              placeholder="选择线路层"
              options={[...(mustVia ? [] : [{ value: DIRECT, label: '直接转发' }]), ...candidates.map(option)]}
            />
          </Field>
        ))}
        {chainPreview && (
          <p className="text-caption text-ink-mut">
            <span className="font-mono text-ink-soft">{chainPreview}</span>
          </p>
        )}

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="协议">
            <Segmented value={form.proto} onChange={v => set({ proto: v })} options={PROTO_OPTIONS} className="self-start" />
          </Field>
          <Field label="入口端口" optional error={shown.entry_port}>
            <Input
              mono
              type="number"
              min={1}
              max={65535}
              value={form.entry_port}
              onChange={v => set({ entry_port: v })}
              placeholder="自动分配"
            />
          </Field>
        </div>

        {chain.entry && (
          <Field
            label={multiHop ? '出口段模式' : '转发模式'}
            hint={`${multiHop ? '只作用于最后一跳到出口这一段。' : ''}线路稳定、丢包低时用 kernel（性能更好）；跨境或丢包高时用 userspace（更抗抖动）。userspace 只转发 TCP，UDP 仍走 kernel。`}
          >
            <Segmented value={form.mode} onChange={v => set({ mode: v })} options={MODE_OPTIONS} className="self-start" />
          </Field>
        )}

        <Field label="出口类型">
          <Segmented value={form.exit.kind} onChange={switchExitKind} options={EXIT_KIND_OPTIONS} className="self-start" />
        </Field>
        {form.exit.kind === 'landing' ? (
          <Field label="落地节点" error={shown.exit}>
            {landingOptions.length > 0 || form.exit.key ? (
              <Select
                value={form.exit.key}
                onChange={key => set({ exit: { kind: 'landing', key } })}
                placeholder="选择落地节点"
                searchable
                options={landingOptions}
              />
            ) : (
              <Alert>还没有可选的落地节点。切到“自定义地址”粘贴一条代理链接就能添加；管理员分配的落地节点也会出现在这里。</Alert>
            )}
            {probe}
          </Field>
        ) : (
          <Field label="出口地址" hint="host:port。也可以粘贴代理链接，它会被存为落地节点。" error={shown.exit}>
            <Input
              mono
              value={form.exit.address}
              onChange={v => set({ exit: { kind: 'custom', address: v } })}
              onBlur={() => adoptPastedProxy(form)}
              placeholder="example.com:443"
            />
            {probe}
          </Field>
        )}

        <Field label="备注" optional>
          <Input value={form.comment} onChange={v => set({ comment: v })} />
        </Field>
      </form>
    </Modal>
  )
}

/* ---------- 页面入口：弹窗与删除 ---------- */

/**
 * 页面上“创建 / 复制 / 编辑规则”的弹窗。dialog 是页面持有的可辨识联合：
 *   { kind: 'create' } | { kind: 'copy', rule } | { kind: 'edit', rule }
 * 页面在它非空时渲染本组件（挂载即打开）。
 *
 * 创建与复制成功后进入新规则的详情页；编辑成功后先等 onSaved（页面的刷新）
 * 落定再关闭，关闭时屏幕上已经是保存后的数据。
 *
 * @param {{
 *   dialog: { kind: 'create' } | { kind: 'copy', rule: object } | { kind: 'edit', rule: object },
 *   scope: typeof RULE_SCOPES.admin,
 *   nodes: object[],
 *   bindings: object[],
 *   catalog: object,
 *   userId: number,
 *   showRate?: boolean,
 *   onSaved?: () => Promise<unknown>,
 *   onClose: () => void,
 * }} props
 */
export function RuleDialog({ dialog, scope, nodes, bindings, catalog, userId, showRate, onSaved, onClose }) {
  const toast = useToast()
  const navigate = useNavigate()
  const editing = dialog.kind === 'edit'

  const submit = async (payload) => {
    if (editing) {
      await api.put(`${scope.api}/${dialog.rule.id}`, payload)
      toast('规则已保存')
      await onSaved?.()
      onClose()
      return
    }
    const res = await api.post(scope.api, payload)
    toast('规则已创建')
    navigate(`${scope.path}/${scope.createdId(res)}`)
  }

  return (
    <RuleFormModal
      title={editing ? '编辑规则' : dialog.kind === 'copy' ? '复制规则' : '创建规则'}
      submitLabel={editing ? '保存修改' : '创建规则'}
      initial={
        editing ? ruleToForm(dialog.rule, catalog.landingByKey)
          : dialog.kind === 'copy' ? copyInitial(dialog.rule, catalog.landingByKey)
            : null
      }
      nodes={nodes}
      bindings={bindings}
      catalog={catalog}
      userId={userId}
      showRate={showRate}
      onSubmit={submit}
      onClose={onClose}
    />
  )
}

/**
 * 删除规则：确认 → 请求 → 提示 → onDeleted（列表页刷新，详情页返回列表）。
 *
 * 确认在动作之外：pending / pendingId 只覆盖请求在途的那一段，确认框开着时
 * 行与按钮不显示成“进行中”。
 *
 * @param {typeof RULE_SCOPES.admin} scope
 * @param {() => unknown} onDeleted
 * @returns {{ request: (rule: { id: number, name: string }) => Promise<void>, pending: boolean, pendingId: unknown }}
 */
export function useDeleteRule(scope, onDeleted) {
  const confirm = useConfirm()
  const toast = useToast()
  const action = useAction(async (id) => {
    await api.del(`${scope.api}/${id}`)
    toast('规则已删除')
    await onDeleted()
  })
  const request = async (rule) => {
    if (!(await confirm({
      title: '删除规则',
      message: `确认删除规则「${rule.name}」？\n删除后它的入口不再转发流量，且无法恢复。`,
      confirmText: '删除规则',
      danger: true,
    }))) return
    await action.run(rule.id)
  }
  return { request, pending: action.pending, pendingId: action.pendingKey }
}
