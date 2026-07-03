import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { useToast } from '../../components/Layout'
import { Badge, Modal, Select } from '../../components/ui'

export function parseGrantText(text, allNodes) {
  const nodeMap = Object.fromEntries(allNodes.map(n => [n.name, n]))
  return text.split('\n')
    .map(line => line.trim())
    .filter(line => line && !line.startsWith('#'))
    .map(line => {
      const parts = line.split('|').map(s => s.trim())
      const name = parts[0]
      let maxForwards = 10
      let quotaGB = 0
      let rateMBytes = 0
      for (let i = 1; i < parts.length; i++) {
        const p = parts[i]
        const mMatch = p.match(/^max=(\d+)$/i)
        if (mMatch) { maxForwards = Number(mMatch[1]); continue }
        const qMatch = p.match(/^quota=([\d.]+)\s*GB$/i)
        if (qMatch) { quotaGB = Number(qMatch[1]); continue }
        const rMatch = p.match(/^rate=(\d+)$/i)
        if (rMatch) { rateMBytes = Number(rMatch[1]); continue }
      }
      const node = nodeMap[name]
      return { name, maxForwards, quotaGB, rateMBytes, nodeId: node?.id || null, found: !!node }
    })
}

export default function PasteGrantsModal({ open, onClose, onDone, allNodes, allUsers, preSelectedUserIds }) {
  const [text, setText] = useState('')
  const [userIds, setUserIds] = useState(preSelectedUserIds || [])
  const [applySettings, setApplySettings] = useState(true)
  const [submitting, setSubmitting] = useState(false)
  const toast = useToast()

  useEffect(() => { if (open) { setText(''); setUserIds(preSelectedUserIds || []) } }, [open, preSelectedUserIds])

  const parsed = text.trim() ? parseGrantText(text, allNodes) : []
  const valid = parsed.filter(p => p.found)
  const canSubmit = valid.length > 0 && userIds.length > 0 && !submitting

  const submit = async () => {
    setSubmitting(true)
    try {
      const grants = valid.map(p => ({
        node_name: p.name,
        max_forwards: applySettings ? p.maxForwards : 10,
        traffic_quota_bytes: applySettings ? Math.round(p.quotaGB * 1073741824) : 0,
        rate_limit_mbytes: applySettings ? p.rateMBytes : 0,
      }))
      await api.post('/grants/batch-apply', { user_ids: userIds.map(Number), grants })
      toast(`已授权 ${valid.length} 个节点给 ${userIds.length} 个用户`)
      onClose()
      onDone()
    } catch (err) { toast(err.message, 'error') } finally { setSubmitting(false) }
  }

  const userOptions = (allUsers || []).filter(u => u.role === 'user').map(u => ({ value: u.id, label: u.username }))

  return (
    <Modal open={open} onClose={onClose} title="粘贴授权">
      <div className="space-y-4">
        <div>
          <label className="fl block mb-1.5">授权文本</label>
          <textarea className="input-field font-mono w-full" rows={8} value={text} onChange={e => setText(e.target.value)}
            placeholder={'gateway-hk | max=10 | quota=5GB | rate=10\nrelay-jp | max=20'} />
        </div>
        <div>
          <label className="fl block mb-1.5">目标用户 <span className="text-ink-mut font-normal text-xs">(可多选)</span></label>
          <Select value={userIds} onChange={setUserIds} placeholder="-- 选择用户 --" searchable multiple
            options={userOptions} />
        </div>
        <label className="flex items-center gap-2 text-sm cursor-pointer">
          <input type="checkbox" className="accent-blue-600" checked={applySettings} onChange={e => setApplySettings(e.target.checked)} />
          应用文本中的 per-node 设置
        </label>
        {parsed.length > 0 && (
          <div className="table-scroll border border-line rounded-lg overflow-auto max-h-[40vh]">
            <table className="tbl">
              <thead><tr><th>节点</th><th>规则上限</th><th>流量配额</th><th>限速</th><th>状态</th></tr></thead>
              <tbody>
                {parsed.map((p, i) => (
                  <tr key={i} className={p.found ? '' : 'bg-red-50 dark:bg-red-900/10'}>
                    <td className={`font-mono text-sm ${p.found ? '' : 'text-red-500'}`}>{p.name}</td>
                    <td className="font-mono text-sm">{applySettings ? p.maxForwards : 10}</td>
                    <td className="font-mono text-sm">{applySettings ? `${p.quotaGB}GB` : '不限'}</td>
                    <td className="font-mono text-sm">{applySettings && p.rateMBytes ? `${p.rateMBytes}MB/s` : '不限'}</td>
                    <td>{p.found ? <Badge color="blue">就绪</Badge> : <Badge color="red">未找到</Badge>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="flex items-center gap-3 pt-4 border-t border-line-soft">
          <button onClick={submit} disabled={!canSubmit} className="btn-primary">
            {submitting ? '授权中…' : `授权 ${valid.length} 个节点给 ${userIds.length} 个用户`}
          </button>
          <button onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </div>
    </Modal>
  )
}
