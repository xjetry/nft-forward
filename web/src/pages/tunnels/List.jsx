import { useState, useEffect } from 'react'
import { api } from '../../lib/api'
import { Layout, useToast } from '../../components/Layout'
import { Loading, Empty, ProtoBadge } from '../../components/ui'

export default function TunnelList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const toast = useToast()

  const load = () => {
    setLoading(true)
    api.get('/tunnels').then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [])

  const deleteTunnel = async (t) => {
    if (!confirm('删除该通道？会同时删除其下所有转发。')) return
    try { await api.del(`/tunnels/${t.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { tunnels = [], nodes = [], node_by_id = {} } = data || {}

  return (
    <Layout>
      {/* Tunnel table */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">所有通道</h3>
          <span className="text-xs text-gray-400">{tunnels.length} 条</span>
        </div>
        {tunnels.length ? (
          <table className="tbl">
            <thead><tr><th className="w-12">ID</th><th>名称</th><th>节点</th><th>协议</th><th>端口段</th><th>允许目标 CIDR</th><th>带宽</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {tunnels.map(t => {
                const node = node_by_id?.[t.node_id]
                return (
                  <tr key={t.id}>
                    <td className="font-mono text-xs text-gray-400">{t.id}</td>
                    <td className="font-semibold">{t.name}</td>
                    <td className="font-semibold">{node ? node.name : `#${t.node_id}`}</td>
                    <td><ProtoBadge proto={t.proto_mask} /></td>
                    <td className="font-mono">{t.port_start}-{t.port_end}</td>
                    <td className="font-mono text-gray-500">{t.target_cidr_allow}</td>
                    <td className="font-mono text-gray-400">{t.bandwidth_mbps === 0 ? '不限速' : `${t.bandwidth_mbps} Mbps`}</td>
                    <td className="text-right">
                      <button onClick={() => deleteTunnel(t)} className="btn-danger-sm text-xs">删除</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        ) : <Empty title="尚无通道" />}
      </div>

      {/* Create tunnel form */}
      <CreateTunnelCard nodes={nodes} onDone={load} />
    </Layout>
  )
}

function CreateTunnelCard({ nodes, onDone }) {
  const [form, setForm] = useState({ name: '', node_id: '', proto_mask: 'tcp+udp', port_start: '', port_end: '', target_cidr_allow: '0.0.0.0/0', bandwidth_mbps: '0' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/tunnels', {
        name: form.name,
        node_id: Number(form.node_id),
        proto_mask: form.proto_mask,
        port_start: Number(form.port_start),
        port_end: Number(form.port_end),
        target_cidr_allow: form.target_cidr_allow,
        bandwidth_mbps: Number(form.bandwidth_mbps),
      })
      toast('通道已创建')
      setForm({ name: '', node_id: '', proto_mask: 'tcp+udp', port_start: '', port_end: '', target_cidr_allow: '0.0.0.0/0', bandwidth_mbps: '0' })
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  if (!nodes.length) return (
    <div className="card">
      <div className="card-header"><h3 className="text-sm font-bold">新建通道</h3></div>
      <Empty title="请先添加节点" />
    </div>
  )

  return (
    <div className="card">
      <div className="card-header"><h3 className="text-sm font-bold">新建通道</h3></div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-4 max-w-2xl">
          <div className="grid grid-cols-[150px_1fr] gap-4 items-center">
            <label className="fl">名称</label>
            <input className="input-field" value={form.name} onChange={e => set('name', e.target.value)} required placeholder="例如 hk-default" />

            <label className="fl">节点</label>
            <select className="input-field" value={form.node_id} onChange={e => set('node_id', e.target.value)} required>
              <option value="">-- 选择节点 --</option>
              {nodes.map(n => <option key={n.id} value={n.id}>{n.name}</option>)}
            </select>

            <label className="fl">协议</label>
            <div className="flex gap-2">
              {['tcp+udp', 'tcp', 'udp'].map(v => (
                <label key={v} className="seg-label">
                  <input type="radio" name="proto_mask" value={v} checked={form.proto_mask === v} onChange={() => set('proto_mask', v)} className="sr-only peer" />
                  <span className="seg-span">{v.toUpperCase()}</span>
                </label>
              ))}
            </div>

            <label className="fl">端口段</label>
            <div className="flex items-center gap-2" style={{ maxWidth: 340 }}>
              <input className="input-field font-mono flex-1" type="number" min="1" max="65535" value={form.port_start} onChange={e => set('port_start', e.target.value)} required placeholder="起始端口" />
              <span className="text-gray-300">--</span>
              <input className="input-field font-mono flex-1" type="number" min="1" max="65535" value={form.port_end} onChange={e => set('port_end', e.target.value)} required placeholder="结束端口" />
            </div>

            <label className="fl">允许目标 CIDR</label>
            <input className="input-field font-mono" value={form.target_cidr_allow} onChange={e => set('target_cidr_allow', e.target.value)} style={{ maxWidth: 340 }} />

            <label className="fl">带宽限速 <span className="text-gray-400 font-normal text-xs">(Mbps)</span></label>
            <input className="input-field font-mono" type="number" min="0" value={form.bandwidth_mbps} onChange={e => set('bandwidth_mbps', e.target.value)} style={{ maxWidth: 160 }} />
          </div>

          <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
            <button type="submit" disabled={loading} className="btn-primary">创建通道</button>
            <span className="text-xs text-gray-400">CIDR 限制用户可指向的目标地址；带宽 0 = 不限速。</span>
          </div>
        </form>
      </div>
    </div>
  )
}
