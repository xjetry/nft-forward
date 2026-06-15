import { useState, useEffect } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty } from '../../components/ui'

export default function ForwardEdit() {
  const { id } = useParams()
  const navigate = useNavigate()
  const toast = useToast()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [form, setForm] = useState({})
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    api.get(`/forwards/${id}/edit`).then(d => {
      setData(d)
      const f = d.forward
      setForm({
        node_id: String(f.node_id),
        proto: f.proto,
        mode: f.mode,
        listen_port: String(f.listen_port),
        target_ip: f.target_ip,
        target_port: String(f.target_port),
        comment: f.comment || '',
      })
    }).catch(console.error).finally(() => setLoading(false))
  }, [id])

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.put(`/forwards/${id}`, {
        node_id: Number(form.node_id),
        proto: form.proto,
        mode: form.mode,
        listen_port: Number(form.listen_port),
        target_ip: form.target_ip,
        target_port: Number(form.target_port),
        comment: form.comment,
      })
      toast('已保存')
      navigate('/forwards')
    } catch (err) { toast(err.message) } finally { setSaving(false) }
  }

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="转发不存在" /></Layout>

  const { nodes = [] } = data

  return (
    <Layout>
      <div className="card">
        <div className="card-header"><h3 className="text-sm font-bold">编辑转发 #{id}</h3></div>
        <div className="p-5">
          <form onSubmit={submit} className="space-y-4 max-w-2xl">
            <div className="grid grid-cols-[150px_1fr] gap-4 items-center">
              <label className="fl">节点</label>
              <select className="input-field" value={form.node_id} onChange={e => set('node_id', e.target.value)} required>
                {nodes.map(n => <option key={n.id} value={n.id}>{n.name}</option>)}
              </select>

              <label className="fl">协议</label>
              <div className="flex gap-2">
                {['tcp', 'udp', 'tcp+udp'].map(v => (
                  <label key={v} className="seg-label">
                    <input type="radio" name="edit-proto" value={v} checked={form.proto === v} onChange={() => set('proto', v)} className="sr-only peer" />
                    <span className="seg-span">{v.toUpperCase()}</span>
                  </label>
                ))}
              </div>

              <label className="fl">模式</label>
              <div className="flex gap-2">
                {[['kernel', '内核态 (零拷贝)'], ['userspace', '用户态 (split-TCP)']].map(([v, l]) => (
                  <label key={v} className="seg-label">
                    <input type="radio" name="edit-mode" value={v} checked={form.mode === v} onChange={() => set('mode', v)} className="sr-only peer" />
                    <span className="seg-span">{l}</span>
                  </label>
                ))}
              </div>

              <label className="fl">监听端口</label>
              <input className="input-field font-mono" type="number" min="1" max="65535" required value={form.listen_port} onChange={e => set('listen_port', e.target.value)} style={{ maxWidth: 200 }} />

              <label className="fl">目标</label>
              <div className="flex items-center gap-2" style={{ maxWidth: 480 }}>
                <input className="input-field font-mono" required value={form.target_ip} onChange={e => set('target_ip', e.target.value)} style={{ flex: 2 }} />
                <span className="text-gray-300">:</span>
                <input className="input-field font-mono" type="number" min="1" max="65535" required value={form.target_port} onChange={e => set('target_port', e.target.value)} style={{ flex: 1 }} />
              </div>

              <label className="fl">备注 <span className="text-gray-400 font-normal text-xs">(可选)</span></label>
              <input className="input-field" value={form.comment} onChange={e => set('comment', e.target.value)} />
            </div>

            <div className="flex gap-3 pt-4 border-t border-gray-100">
              <button type="submit" disabled={saving} className="btn-primary">保存</button>
              <Link to="/forwards" className="btn-secondary">取消</Link>
            </div>
          </form>
        </div>
      </div>

      <Link to="/forwards" className="inline-flex items-center gap-1 text-blue-600 text-[13px] font-semibold hover:underline mt-5">
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回转发列表
      </Link>
    </Layout>
  )
}
