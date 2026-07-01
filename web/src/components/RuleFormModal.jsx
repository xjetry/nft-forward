import { useState, useEffect } from 'react'
import { Modal, Select, ProbeButton, nodeStack } from './ui'
import { useToast } from './Layout'
import { tryParseURI } from '../lib/landing'

const EMPTY = { node_id: '', name: '', proto: 'tcp', exit: '', exit_kind: 'custom', entry_port: '', comment: '' }

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
export function RuleFormModal({ open, onClose, title, submitLabel = '保存', nodes = [], landingNodes, initial, onSubmit, onAddProxyURI, showRate }) {
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

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

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
  const groups = [
    { label: '单点', options: nodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
    { label: '组合', options: nodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: fmtRate(n) })) },
  ]

  // Show protocol + node remark only — the real connection address is hidden
  // from the picker. The value stays host:port (the rule's exit target).
  const landingOptions = (landingNodes || []).map(n => ({
    value: `${n.host}:${n.port}`,
    label: `${n.protocol ? `[${n.protocol}] ` : ''}${n.name || '(未命名)'}`,
  }))

  return (
    <Modal open={open} onClose={onClose} title={title}>
      <form onSubmit={submit} className="space-y-[22px]">
        <div className="grid grid-cols-[120px_1fr] gap-6 items-center">
          <label className="fl">入口节点</label>
          <Select value={form.node_id} onChange={v => set('node_id', v)} placeholder="-- 选择节点 --" searchable tabs groups={groups} />
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
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
    entry_port: '',
    comment: rule.comment || '',
  }
}

/* Prefill for "copy": same chain/target, name suffixed _Copy. */
export function copyInitial(rule) {
  return { ...ruleToForm(rule), name: `${rule.name}_Copy` }
}
