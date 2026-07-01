import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, ProtoBadge, ModeBadge, SensText, useConfirm, ExitKindBadge } from '../../components/ui'
import { RuleFormModal, ruleToForm } from '../../components/RuleFormModal'

export default function RulesDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showEdit, setShowEdit] = useState(false)
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()

  const load = () => {
    setLoading(true)
    api.get(`/rules/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="规则不存在" /></Layout>

  const { rule, hops = [], nodes = [], node_by_id = {} } = data
  const node = node_by_id[rule.node_id]

  const saveEdit = async (form) => {
    await api.put(`/rules/${rule.id}`, {
      node_id: Number(form.node_id), name: form.name, proto: form.proto,
      exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
      comment: form.comment || undefined,
    })
    toast('已保存并重下发'); setShowEdit(false); load()
  }

  const deleteRule = async () => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/rules/${rule.id}`); toast('已删除'); navigate('/rules') } catch (err) { toast(err.message, 'error') }
  }

  return (
    <Layout>
      {/* Entry info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">入口</h3><span className="text-xs text-ink-mut">复制给客户端</span></div>
        <div className="p-5">
          {rule.entry ? (
            <div className="space-y-2">
              <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
                <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">ENTRY</span>
                {rule.entry_node_id && node_by_id[rule.entry_node_id] && <span className="bg-indigo-600/20 text-indigo-300 text-[11px] font-semibold px-2 py-0.5 rounded">{node_by_id[rule.entry_node_id].name}</span>}
                <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1"><SensText blurred={blurred}>{rule.entry}</SensText></span>
                <button onClick={() => { navigator.clipboard.writeText(rule.entry); toast('入口地址已复制') }}
                  className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
                  <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                  复制
                </button>
              </div>
              {/* Relay proxy URI: the landing/custom proxy with its host:port
                  rewritten to the entry endpoint, so a client dials the relay. */}
              {rule.relay_uri && (
                <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
                  <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">PROXY</span>
                  <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1 truncate"><SensText blurred={blurred}>{rule.relay_uri}</SensText></span>
                  <button onClick={() => { navigator.clipboard.writeText(rule.relay_uri); toast('代理 URI 已复制') }}
                    className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                    复制
                  </button>
                </div>
              )}
            </div>
          ) : <span className="text-ink-mut text-sm">尚未分配入口</span>}
          <div className="grid grid-cols-[90px_1fr] gap-4 items-center mt-5 text-sm">
            <span className="text-ink-soft font-semibold">名称</span>
            <span className="font-semibold">{rule.name}</span>
            {rule.owner_id?.Valid && <><span className="text-ink-soft font-semibold">用户</span>
            <span className="font-semibold">
              <Link to={`/users/${rule.owner_id.Int64}`} className="text-blue-600 hover:underline">{rule.owner_name}</Link>
            </span></>}
            <span className="text-ink-soft font-semibold">节点</span>
            <span className="inline-flex items-center gap-1.5 font-mono">
              {node ? <Link to={`/nodes/${node.id}`} className="text-blue-600 hover:underline">{node.name}</Link> : `#${rule.node_id}`}
            </span>
            <span className="text-ink-soft font-semibold">协议</span>
            <span><ProtoBadge proto={rule.proto} /></span>
            <span className="text-ink-soft font-semibold">出口</span>
            <span className="font-mono inline-flex items-center gap-2">
              <ExitKindBadge kind={rule.exit_kind} />
              <SensText blurred={blurred}>{rule.exit || '--'}</SensText>
              {rule.exit_kind === 'landing' && rule.landing_name && <span className="text-ink-mut text-xs font-sans">{rule.landing_name}</span>}
            </span>
            <span className="text-ink-soft font-semibold">流量</span>
            <span className="font-mono text-ink-mut">{fmtBytes(rule.total_bytes || 0)}</span>
            {rule.comment && <>
              <span className="text-ink-soft font-semibold">备注</span>
              <span className="text-ink-soft">{rule.comment}</span>
            </>}
          </div>
        </div>
      </div>

      {/* Hop table */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">各跳状态</h3>
          <span className="text-xs text-ink-mut">{hops.length} 跳</span>
        </div>
        {hops.length ? (
          <div className="tbl-scroll">
          <table className="tbl">
            <thead><tr><th className="w-10">#</th><th>节点</th><th>监听端口</th><th>目标</th><th>模式</th><th>流量</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {hops.map(h => {
                const hopNode = node_by_id?.[h.node_id]
                return (
                  <tr key={h.position}>
                    <td className="font-mono text-xs text-ink-mut">{h.position + 1}</td>
                    <td className="font-semibold"><Link to={`/nodes/${h.node_id}`} className="text-blue-600 hover:underline">{hopNode?.name || `#${h.node_id}`}</Link></td>
                    <td className="font-mono">:{h.listen_port}</td>
                    <td className="font-mono"><SensText blurred={blurred}>{h.target_host ? `${h.target_host}:${h.target_port}` : '--'}</SensText></td>
                    <td><ModeBadge mode={h.mode} /></td>
                    <td className="font-mono text-xs text-ink-mut">{fmtBytes(h.total_bytes)}</td>
                    <td className="text-right">
                      <ReallocateForm ruleId={rule.id} position={h.position} onDone={load} />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          </div>
        ) : <Empty title="暂无跳数据" />}
      </div>

      {/* Actions */}
      <div className="flex items-center gap-3 flex-wrap mt-5">
        <button onClick={() => setShowEdit(true)} className="btn-primary text-xs">编辑规则</button>
        <button onClick={deleteRule} className="btn-secondary text-xs">删除规则</button>
        <Link to="/rules" className="text-blue-600 text-[13px] font-semibold hover:underline inline-flex items-center gap-1">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
          返回规则列表
        </Link>
      </div>

      <RuleFormModal
        open={showEdit} onClose={() => setShowEdit(false)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} initial={ruleToForm(rule)} onSubmit={saveEdit} />
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
    } catch (err) { toast(err.message, 'error') } finally { setLoading(false) }
  }

  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="1" max="65535" value={port} onChange={e => setPort(e.target.value)} placeholder="随机" style={{ width: 90, height: 28, fontSize: 12, padding: '0 6px' }} />
      <button type="submit" disabled={loading} className="btn-secondary text-xs">换端口</button>
    </form>
  )
}

