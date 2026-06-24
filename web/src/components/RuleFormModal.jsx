import { useState, useEffect } from 'react'
import { Modal, Select } from './ui'
import { useToast } from './Layout'

const EMPTY = { node_id: '', name: '', proto: 'tcp', exit: '', comment: '' }

/* Shared create/edit form for forwarding rules, used by both the admin
   (`/rules`) and user (`/my/rules`) pages so create, edit and copy share one
   layout. The parent owns the API call via onSubmit(form) -> Promise (it knows
   create vs edit and admin vs user endpoint); this component only manages form
   state, the single/composite node grouping and validation. `initial` seeds the
   fields for edit/copy prefills and is re-applied every time the modal opens. */
export function RuleFormModal({ open, onClose, title, submitLabel = '保存', nodes = [], initial, onSubmit }) {
  const [form, setForm] = useState(EMPTY)
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  useEffect(() => {
    if (open) setForm({ ...EMPTY, ...(initial || {}) })
  }, [open])

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    if (!form.node_id) { toast('请选择节点'); return }
    setLoading(true)
    try {
      await onSubmit(form)
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  const groups = [
    { label: '单点', options: nodes.filter(n => n.node_type !== 'composite').map(n => ({ value: n.id, label: n.name })) },
    { label: '组合', options: nodes.filter(n => n.node_type === 'composite').map(n => ({ value: n.id, label: n.name })) },
  ]

  return (
    <Modal open={open} onClose={onClose} title={title}>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">入口节点</label>
          <Select value={form.node_id} onChange={v => set('node_id', v)} placeholder="-- 选择节点 --" searchable groups={groups} />
          <label className="fl">名称</label>
          <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="规则名称" />
          <label className="fl">协议</label>
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
          <label className="fl">出口</label>
          <input className="input-field font-mono" value={form.exit} onChange={e => set('exit', e.target.value)} required placeholder="host:port" />
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

/* Map a rule into the form's five editable fields. The rule-list row carries
   split exit_host/exit_port; the detail view a combined exit string — accept
   either. Used to seed the edit modal. */
export function ruleToForm(rule) {
  const exit = rule.exit != null ? rule.exit
    : (rule.exit_host && rule.exit_port ? `${rule.exit_host}:${rule.exit_port}` : '')
  return {
    node_id: rule.node_id,
    name: rule.name,
    proto: rule.proto,
    exit,
    comment: rule.comment || '',
  }
}

/* Prefill for "copy": same chain/target, name suffixed _Copy. */
export function copyInitial(rule) {
  return { ...ruleToForm(rule), name: `${rule.name}_Copy` }
}
