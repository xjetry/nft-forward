import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, CopyText } from '../../components/ui'

export default function RulesDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get(`/rules/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="规则不存在" /></Layout>

  const { rule, hops = [], nodes = [], node_by_id = {} } = data
  const node = node_by_id[rule.node_id]

  const deleteRule = async () => {
    if (!confirm(`确认删除规则「${rule.name}」？`)) return
    try { await api.del(`/rules/${rule.id}`); toast('已删除'); navigate('/rules') } catch (err) { toast(err.message) }
  }

  return (
    <Layout>
      {/* Entry info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">入口</h3><span className="text-xs text-gray-400">复制给客户端</span></div>
        <div className="p-5">
          {rule.entry ? (
            <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
              <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">ENTRY</span>
              <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1"><SensText blurred={blurred}>{rule.entry}</SensText></span>
              <button onClick={() => { navigator.clipboard.writeText(rule.entry); toast('入口地址已复制') }}
                className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
                <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                复制
              </button>
            </div>
          ) : <span className="text-gray-400 text-sm">尚未分配入口</span>}
          <div className="grid grid-cols-[90px_1fr] gap-4 items-center mt-5 text-sm">
            <span className="text-gray-500 font-semibold">名称</span>
            <span className="font-semibold">{rule.name}</span>
            <span className="text-gray-500 font-semibold">节点</span>
            <span className="inline-flex items-center gap-1.5 font-mono">
              {node ? <Link to={`/nodes/${node.id}`} className="text-blue-600 hover:underline">{node.name}</Link> : `#${rule.node_id}`}
            </span>
            <span className="text-gray-500 font-semibold">协议</span>
            <span><ProtoBadge proto={rule.proto} /></span>
            <span className="text-gray-500 font-semibold">路径</span>
            <span className="font-mono text-gray-500"><SensText blurred={blurred}>{rule.path || '--'}</SensText></span>
            <span className="text-gray-500 font-semibold">出口</span>
            <span className="font-mono"><SensText blurred={blurred}>{rule.exit || '--'}</SensText></span>
            {rule.comment && <>
              <span className="text-gray-500 font-semibold">备注</span>
              <span className="text-gray-500">{rule.comment}</span>
            </>}
          </div>
        </div>
      </div>

      {/* Hop table */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">各跳状态</h3>
          <span className="text-xs text-gray-400">{hops.length} 跳</span>
        </div>
        {hops.length ? (
          <table className="tbl">
            <thead><tr><th className="w-10">#</th><th>节点</th><th>监听端口</th><th>目标</th><th>模式</th><th>流量</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {hops.map(h => {
                const hopNode = node_by_id?.[h.node_id]
                return (
                  <tr key={h.position}>
                    <td className="font-mono text-xs text-gray-400">{h.position + 1}</td>
                    <td className="font-semibold">{hopNode?.name || `#${h.node_id}`}</td>
                    <td className="font-mono">:{h.listen_port}</td>
                    <td className="font-mono"><SensText blurred={blurred}>{h.target || '--'}</SensText></td>
                    <td><ModeBadge mode={h.mode} /></td>
                    <td className="font-mono text-xs text-gray-400">{fmtBytes(h.total_bytes)}</td>
                    <td className="text-right">
                      <ReallocateForm ruleId={rule.id} position={h.position} onDone={load} />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        ) : <Empty title="暂无跳数据" />}
      </div>

      {/* Edit rule */}
      <EditRuleCard rule={rule} onDone={load} />

      {/* Actions */}
      <div className="flex items-center gap-3 flex-wrap mt-5">
        <button onClick={deleteRule} className="btn-danger-sm text-xs">删除规则</button>
        <Link to="/rules" className="text-blue-600 text-[13px] font-semibold hover:underline inline-flex items-center gap-1">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
          返回规则列表
        </Link>
      </div>
    </Layout>
  )
}

function ReallocateForm({ ruleId, position, onDone }) {
  const [port, setPort] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post(`/rules/${ruleId}/hops/${position}/reallocate`, { port: port ? Number(port) : undefined })
      toast('端口已重分配')
      setPort('')
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="1" max="65535" value={port} onChange={e => setPort(e.target.value)} placeholder="随机" style={{ width: 90, height: 28, fontSize: 12, padding: '0 6px' }} />
      <button type="submit" disabled={loading} className="btn-secondary text-xs">换端口</button>
    </form>
  )
}

function EditRuleCard({ rule, onDone }) {
  const [name, setName] = useState(rule.name)
  const [proto, setProto] = useState(rule.proto)
  const [exit, setExit] = useState(rule.exit || '')
  const [saving, setSaving] = useState(false)
  const toast = useToast()

  const submit = async (e) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.put(`/rules/${rule.id}`, { name, proto, exit })
      toast('已保存并重下发')
      onDone()
    } catch (err) { toast(err.message) } finally { setSaving(false) }
  }

  return (
    <div className="card mb-5">
      <div className="card-header"><h3 className="text-sm font-bold">编辑规则</h3></div>
      <div className="p-5">
        <form onSubmit={submit} className="space-y-4 max-w-2xl">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
            <label className="fl">名称</label>
            <input className="input-field" value={name} onChange={e => setName(e.target.value)} required />
            <label className="fl">协议</label>
            <select className="input-field" value={proto} onChange={e => setProto(e.target.value)} style={{ maxWidth: 200 }}>
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
              <option value="tcp+udp">TCP+UDP</option>
            </select>
            <label className="fl">出口</label>
            <input className="input-field font-mono" value={exit} onChange={e => setExit(e.target.value)} required placeholder="host:port" />
          </div>
          <div className="flex items-center gap-3 pt-4 border-t border-gray-100">
            <button type="submit" disabled={saving} className="btn-primary">保存并重下发</button>
          </div>
        </form>
      </div>
    </div>
  )
}
