import { useState, useEffect, Fragment } from 'react'
import { Modal, Select, ProbeButton, nodeStack } from './ui'
import { useToast } from './Layout'
import { tryParseURI } from '../lib/landing'

const EMPTY = { node_id: '', name: '', proto: 'tcp', exit: '', exit_kind: 'custom', entry_port: '', comment: '', mode: 'kernel', entry_family: 'v4', via_node_ids: [] }

/* Shared create/edit form for forwarding rules, used by both the admin
   (`/rules`) and user (`/my/rules`) pages so create, edit and copy share one
   layout. The parent owns the API call via onSubmit(form) -> Promise (it knows
   create vs edit and admin vs user endpoint); this component only manages form
   state, the single/composite node grouping and validation. `initial` seeds the
   fields for edit/copy prefills and is re-applied every time the modal opens.

   When `landingNodes` is provided (the user side passes the merged landing-node
   list — admin-assigned plus the user's own browser-local URIs, even when
   empty), the exit gains a custom/landing toggle: a landing exit picks a node's
   host:port, a custom exit takes a host:port. The user's proxy URIs never leave
   the browser, so the modal only deals in host:port here; the rules page
   resolves the relay URI client-side. Admin callers omit the prop and keep the
   plain host:port box. */
export function RuleFormModal({ open, onClose, title, submitLabel = '保存', nodes = [], landingNodes, bindings = [], initial, onSubmit, onAddProxyURI, showRate }) {
  const [form, setForm] = useState(EMPTY)
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const landingEnabled = Array.isArray(landingNodes)

  useEffect(() => {
    if (!open) return
    const seed = { ...EMPTY, ...(initial || {}) }
    // A landing exit whose node no longer resolves falls back to custom so its
    // host:port stays editable instead of showing an empty picker.
    if (seed.exit_kind === 'landing' && landingEnabled &&
        !landingNodes.some(n => `${n.host}:${n.port}` === seed.exit)) {
      seed.exit_kind = 'custom'
    }
    setForm(seed)
  }, [open])

  // A node switch that lands on a single-stack node can't keep a v6/both
  // entry type — force it back to v4 rather than submitting a family the
  // new node doesn't support.
  useEffect(() => {
    const n = nodes.find(x => String(x.id) === String(form.node_id))
    if (!n) return
    const { entryV4, entryV6 } = nodeStack(n)
    if (!(entryV4 && entryV6)) {
      setForm(f => f.entry_family === 'v4' ? f : { ...f, entry_family: 'v4' })
    }
  }, [form.node_id, nodes])

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  // Switching the entry invalidates any chosen middle-layer chain — the
  // binding graph downstream of the old entry has nothing to do with the
  // new one. This lives in the picker's own onChange rather than an effect
  // keyed on form.node_id: the seed effect above also assigns node_id (from
  // `initial`, together with its own via_node_ids) every time the modal
  // opens, and an effect can't tell that assignment apart from a real user
  // switch — it would wipe the edit prefill's chain right after seeding it.
  const pickEntry = (v) => setForm(f => ({ ...f, node_id: v, via_node_ids: [] }))

  const handleExitBlur = () => {
    if (!landingEnabled || form.exit_kind !== 'custom') return
    const val = form.exit.trim()
    if (!val.includes('://')) return
    const node = tryParseURI(val)
    if (!node) return
    const hp = node.host.includes(':') ? `[${node.host}]:${node.port}` : `${node.host}:${node.port}`
    if (onAddProxyURI) onAddProxyURI(val)
    setForm(f => {
      const next = { ...f, exit_kind: 'landing', exit: hp }
      if (!f.comment.trim() && node.name) next.comment = node.name
      return next
    })
    toast(`已识别 ${node.protocol} 代理并保存`)
  }

  const submit = async (e) => {
    e.preventDefault()
    if (!form.node_id) { toast('请选择节点', 'error'); return }
    if (landingEnabled && form.exit_kind === 'landing' && !form.exit) { toast('请选择出口节点', 'error'); return }
    setLoading(true)
    try {
      await onSubmit(form)
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }

  // Select 的 label 必须是纯字符串（既要参与搜索过滤的 .toLowerCase()，
  // 也不支持渲染 JSX），沿用 landingOptions 那种文本前缀写法标协议栈。
  const fmtStack = (n) => {
    const { entryV4, entryV6, exitV6 } = nodeStack(n)
    const parts = [entryV4 && 'v4', entryV6 && 'v6'].filter(Boolean)
    let tag = parts.join('+')
    if (exitV6 !== entryV6) {
      const note = exitV6 ? '出口支持v6' : '出口不支持v6'
      tag = tag ? `${tag} ${note}` : note
    }
    return tag ? `[${tag}] ` : ''
  }
  const fmtRate = (n) => {
    const stack = fmtStack(n)
    if (showRate === false) return `${stack}${n.name}`
    const r = n.rate_multiplier ?? 1
    return r !== 1 ? `${stack}${n.name} (×${r})` : `${stack}${n.name}`
  }
  // Only entry-role nodes can be the rule's entry — roles missing (nodes
  // reported by a server that predates the roles column) default to entry,
  // so old deployments keep working until an admin explicitly narrows roles.
  const entryNodes = nodes.filter(n => ((n.roles ?? 1) & 1) !== 0)
  const groups = [
    { label: '单点', options: entryNodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
    { label: '组合', options: entryNodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
  ]

  // Show protocol + node remark only — the real connection address is hidden
  // from the picker. The value stays host:port (the rule's exit target).
  const landingOptions = (landingNodes || []).map(n => ({
    value: `${n.host}:${n.port}`,
    label: `${n.protocol ? `[${n.protocol}] ` : ''}${n.name || '(未命名)'}`,
  }))

  // Cascaded middle-layer picker: chain[i]'s candidates are the binding
  // graph's downstreams of chain[i-1] (chain[-1] = the entry), narrowed to
  // nodes we actually have (the my-side list is already the granted
  // intersection) with the via role, excluding the entry itself and any
  // node already on the chain. Missing roles default to "not a via node"
  // (the opposite default from entry) — an unrolled node shouldn't silently
  // become choosable as a middle hop. Candidates empty and nothing chosen
  // at a level means stop rendering further levels — "有框就有得选".
  const nodeById = Object.fromEntries(nodes.map(n => [n.id, n]))
  const viaChain = (form.via_node_ids || []).map(Number).filter(id => nodeById[id])
  const viaCandidates = (upstreamId, chainSoFar) =>
    bindings
      .filter(b => b.upstream_node_id === upstreamId)
      .map(b => nodeById[b.downstream_node_id])
      .filter(n => n && ((n.roles ?? 0) & 2) !== 0)
      .filter(n => n.id !== Number(form.node_id) && !chainSoFar.includes(n.id))
  const pickVia = (level, v) => setForm(f => {
    const next = (f.via_node_ids || []).slice(0, level)
    if (v) next.push(Number(v))
    return { ...f, via_node_ids: next }
  })
  const viaLevels = []
  if (form.node_id) {
    let upstream = Number(form.node_id)
    const soFar = []
    for (let level = 0; ; level++) {
      const chosen = viaChain[level]
      const cands = viaCandidates(upstream, soFar)
      if (!cands.length && !chosen) break
      viaLevels.push({ level, cands, chosen })
      if (!chosen) break
      soFar.push(chosen)
      upstream = chosen
    }
  }
  // Exit-capability hints follow the chain tail (the last via, or the entry
  // itself when the chain is empty) — the tail is the node that actually
  // dials the target, so its stack is what the outbound leg depends on.
  const tailNode = viaChain.length ? nodeById[viaChain[viaChain.length - 1]] : nodeById[Number(form.node_id)]

  return (
    <Modal open={open} onClose={onClose} title={title}>
      <form onSubmit={submit} className="space-y-[22px]">
        <div className="grid grid-cols-[120px_1fr] gap-6 items-center">
          <label className="fl">入口节点</label>
          <Select value={form.node_id} onChange={pickEntry} placeholder="-- 选择节点 --" searchable tabs groups={groups} />
          {(() => {
            const selNode = nodes.find(n => String(n.id) === String(form.node_id))
            if (!selNode) return null
            const { entryV4, entryV6 } = nodeStack(selNode)
            if (!(entryV4 && entryV6)) return null
            const composite = selNode.node_type === 'composite'
            // IPv6 ingress rides the userspace relay (kernel DNAT can't
            // cross address families), which is TCP-only. Single-node rules
            // can be auto-switched here; a composite's entry segment comes
            // from the node config, so only the hint applies.
            const pickFamily = (k) => setForm(f => {
              const next = { ...f, entry_family: k }
              if (k !== 'v4' && !composite) next.mode = 'userspace'
              return next
            })
            return (
              <>
                <label className="fl">入口类型</label>
                <div className="flex items-center gap-2.5 flex-wrap">
                  <div className="inline-flex gap-1 p-1 rounded-[10px] border border-line bg-surface w-fit">
                    {[['v4', 'IPv4'], ['v6', 'IPv6'], ['both', 'IPv4+IPv6']].map(([k, lbl]) => (
                      <button key={k} type="button" onClick={() => pickFamily(k)}
                        className={`px-4 py-[9px] rounded-[7px] text-[14px] font-semibold transition-colors ${(form.entry_family || 'v4') === k ? 'bg-blue-600 text-white' : 'text-ink-soft hover:text-ink'}`}>
                        {lbl}
                      </button>
                    ))}
                  </div>
                  {(form.entry_family === 'v6' || form.entry_family === 'both') && (
                    <span className="text-xs text-ink-mut">
                      {composite ? 'v6 入口要求组合节点第一段为用户态转发，且协议仅 TCP' : 'v6 入口走用户态转发，协议仅 TCP'}
                    </span>
                  )}
                </div>
              </>
            )
          })()}
          {viaLevels.map(({ level, cands, chosen }) => (
            <Fragment key={level}>
              <label className="fl">{level === 0 ? '线路层' : `线路层 ${level + 1}`}</label>
              <Select value={chosen ?? ''} onChange={v => pickVia(level, v)} placeholder="直接转发"
                options={[{ value: '', label: '直接转发' },
                  ...cands.map(n => ({ value: n.id, label: fmtRate(n) }))]} />
            </Fragment>
          ))}
          {viaChain.length > 0 && (
            <>
              <label className="fl"></label>
              <div className="text-xs text-ink-mut">
                <span className="font-mono">
                  {[nodeById[Number(form.node_id)]?.name, ...viaChain.map(id => nodeById[id]?.name), '目标']
                    .filter(Boolean).join(' → ')}
                </span>
                <span className="ml-2">链路更长的规则占用更多全局转发名额</span>
              </div>
            </>
          )}
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
          {/* 模式作用于出口段（最后一跳 → 目标）：单点规则即唯一一跳；
              组合链路的节点间各跳模式由组合节点配置决定，这里只管出口段 */}
          {(() => {
            const selNode = nodes.find(n => String(n.id) === String(form.node_id))
            if (!selNode) return null
            // The mode field governs the outbound leg (tail → target): a
            // composite entry already has that split internally, and a via
            // chain extends it the same way, so either makes this the
            // "出口段" (vs. the whole rule) regardless of the entry's own type.
            const composite = tailNode?.node_type === 'composite' || viaChain.length > 0
            return (
              <>
                <label className="fl">{composite ? '出口段模式' : '转发模式'}</label>
                <div className="flex items-center gap-2.5 flex-wrap">
                  <Select value={form.mode || 'kernel'} onChange={v => set('mode', v)} style={{ width: 160 }}
                    options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
                  <span className="text-xs text-ink-mut">
                    {composite
                      ? '仅作用于最后一跳 → 目标；节点间各跳由组合节点配置决定。用户态仅 TCP，UDP 自动走内核态'
                      : '内核态支持 TCP/UDP；用户态仅 TCP，UDP 自动走内核态'}
                  </span>
                </div>
              </>
            )
          })()}
          <label className="fl">入口端口 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field font-mono" type="number" min="1" max="65535" value={form.entry_port} onChange={e => set('entry_port', e.target.value)}
            placeholder="留空自动分配" style={{ maxWidth: 200 }} />

          {landingEnabled ? (
            <>
              <label className="fl">出口类型</label>
              <div className="inline-flex gap-1 p-1 rounded-[10px] border border-line bg-surface w-fit">
                {[['custom', '自定义'], ['landing', '出口节点']].map(([k, lbl]) => (
                  <button key={k} type="button" onClick={() => set('exit_kind', k)}
                    className={`px-5 py-[9px] rounded-[7px] text-[14px] font-semibold transition-colors ${form.exit_kind === k ? 'bg-blue-600 text-white' : 'text-ink-soft hover:text-ink'}`}>
                    {lbl}
                  </button>
                ))}
              </div>

              {form.exit_kind === 'landing' ? (
                <>
                  <label className="fl">出口节点</label>
                  <div className="flex items-center gap-3">
                    {landingOptions.length ? (
                      <Select value={form.exit} onChange={v => set('exit', v)} placeholder="-- 选择出口节点 --" searchable options={landingOptions} className="flex-1" />
                    ) : (
                      <div className="text-xs text-ink-mut">尚无可用出口节点，请在概览页添加代理 URI 或联系管理员。</div>
                    )}
                    {form.node_id && form.exit && <ProbeButton target={form.exit} nodeId={form.node_id} />}
                  </div>
                </>
              ) : (
                <>
                  <label className="fl">出口地址</label>
                  <div className="flex items-center gap-3">
                    <input className="input-field font-mono flex-1" value={form.exit} onChange={e => set('exit', e.target.value)} onBlur={handleExitBlur} required placeholder="host:port 或代理 URI" />
                    {form.node_id && form.exit && <ProbeButton target={form.exit} nodeId={form.node_id} />}
                  </div>
                </>
              )}
            </>
          ) : (
            <>
              <label className="fl">出口</label>
              <div className="flex items-center gap-3">
                <input className="input-field font-mono flex-1" value={form.exit} onChange={e => set('exit', e.target.value)} required placeholder="host:port" />
                {form.node_id && form.exit && <ProbeButton target={form.exit} nodeId={form.node_id} />}
              </div>
            </>
          )}

          <label className="fl">备注 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field" value={form.comment} onChange={e => set('comment', e.target.value)} placeholder="备注" />
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-line-soft">
          <button type="submit" disabled={loading} className="btn-primary">{submitLabel}</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}

/* Map a rule into the form's editable fields. The rule-list row carries split
   exit_host/exit_port; the detail view a combined exit string — accept either.
   exit_kind comes from the (possibly client-enriched) rule so edit prefills the
   right exit mode. */
export function ruleToForm(rule) {
  const exit = rule.exit != null ? rule.exit
    : (rule.exit_host && rule.exit_port ? `${rule.exit_host}:${rule.exit_port}` : '')
  return {
    node_id: rule.node_id,
    name: rule.name,
    proto: rule.proto,
    exit,
    exit_kind: rule.exit_kind === 'landing' ? 'landing' : 'custom',
    entry_port: rule.entry_listen_port > 0 ? String(rule.entry_listen_port) : '',
    comment: rule.comment || '',
    // 模式字段的语义是出口段（尾跳）；entry_mode 兜底兼容旧列表载荷，
    // 单点规则两者本就相同。
    mode: rule.exit_mode || rule.entry_mode || 'kernel',
    entry_family: rule.entry_family || 'v4',
    via_node_ids: rule.via_node_ids || [],
  }
}

/* Prefill for "copy": same chain/target, name suffixed _Copy. entry_port stays
   blank — the source rule still holds its port, so the copy needs a fresh one. */
export function copyInitial(rule) {
  return { ...ruleToForm(rule), name: `${rule.name}_Copy`, entry_port: '' }
}

/* Map the form's fields to the create/edit request body — the single source
   of the payload shape for every rules page, so a new field can't silently
   go missing from one of the call sites. form.mode is the exit-segment mode;
   it is sent as both exit_mode (the real field) and mode (legacy alias for
   single-node rules, so older servers keep honoring it). via_node_ids is
   always sent (never omitted): the form owns the whole chain, so an empty
   array means "clear the chain", not "leave it untouched". */
export function ruleFormToPayload(form) {
  return {
    node_id: Number(form.node_id), name: form.name, proto: form.proto,
    mode: form.mode || undefined, exit_mode: form.mode || undefined,
    exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
    comment: form.comment || undefined,
    entry_family: form.entry_family || undefined,
    via_node_ids: (form.via_node_ids || []).map(Number),
  }
}
