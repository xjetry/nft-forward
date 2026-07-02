import { useState, useEffect, useMemo } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { Layout, useToast, useBlur, useUser, useCopyFmt } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, SensText, useConfirm, ExitKindBadge } from '../../components/ui'
import { copyToClipboard } from '../../lib/clipboard'
import { RuleFormModal, ruleToForm } from '../../components/RuleFormModal'
import { uriToClashYaml } from '../../lib/yaml-convert'
import { parseURIs, landingIndex, mergeLanding, loadLocalURIs, loadSubCache, fetchNodeRoles, loadLocalRoles, nodeHasRole, ROLE_LANDING } from '../../lib/landing'

export default function MyRuleDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [showEdit, setShowEdit] = useState(false)
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()
  const { user } = useUser()
  const { copyFmt } = useCopyFmt()

  const [nodeRoles, setNodeRoles] = useState({})
  useEffect(() => {
    fetchNodeRoles().then(sr => setNodeRoles({ ...sr, ...loadLocalRoles(user?.username) }))
  }, [user])
  const localNodes = useMemo(() => {
    const isLanding = n => nodeHasRole(nodeRoles, n, ROLE_LANDING)
    const manual = parseURIs(loadLocalURIs(user?.username)).filter(isLanding)
    const sub = loadSubCache(user?.username).filter(isLanding)
    return mergeLanding(manual, sub)
  }, [user, nodeRoles])

  const load = () => {
    setLoading(true)
    api.get(`/my/rules/${id}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="规则不存在" /></Layout>

  const { rule, nodes = [], node_by_id = {}, show_rate } = data
  const node = node_by_id[rule.node_id]

  const exitOf = (r) => (r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : '')

  const saveEdit = async (form) => {
    await api.put(`/my/rules/${rule.id}`, {
      node_id: Number(form.node_id), name: form.name, proto: form.proto,
      mode: form.mode || undefined,
      exit: form.exit, entry_port: form.entry_port ? Number(form.entry_port) : undefined,
      comment: form.comment || undefined,
    })
    toast('已保存并重下发'); setShowEdit(false); load()
  }

  const deleteRule = async () => {
    if (!(await confirm({ title: '删除规则', message: `确认删除规则「${rule.name}」？`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/my/rules/${rule.id}`); toast('已删除'); navigate('/my/rules') } catch (err) { toast(err.message, 'error') }
  }

  const landingNodes = mergeLanding(localNodes, [])

  return (
    <Layout>
      <div className="h-full flex flex-col">
      <div className="flex items-baseline gap-3.5 mb-[22px]">
        <Link to="/my/rules" className="text-blue-600 text-[13px] font-semibold hover:underline inline-flex items-center gap-1">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
          我的规则
        </Link>
        <h1 className="m-0 text-2xl font-bold text-ink">{rule.name}</h1>
      </div>

      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">入口</h3><span className="text-xs text-ink-mut">复制给客户端</span></div>
        <div className="p-5">
          {rule.entry ? (
            <div className="space-y-2">
              <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
                <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">ENTRY</span>
                {rule.entry_node_id && node_by_id[rule.entry_node_id] && <span className="bg-indigo-600/20 text-indigo-300 text-[11px] font-semibold px-2 py-0.5 rounded">{node_by_id[rule.entry_node_id].name}</span>}
                <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1"><SensText blurred={blurred}>{rule.entry}</SensText></span>
                <button onClick={() => copyToClipboard(rule.entry).then(() => toast('入口地址已复制')).catch(() => toast('复制失败', 'error'))}
                  className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
                  <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                  复制
                </button>
              </div>
              {rule.relay_uri && (
                <div className="flex items-center gap-2.5 bg-[#0e1117] rounded-lg px-4 py-3">
                  <span className="text-[11px] font-semibold uppercase tracking-wider text-gray-500">PROXY</span>
                  <span className="text-[#e8edf4] font-mono text-sm font-semibold flex-1 truncate"><SensText blurred={blurred}>{rule.relay_uri}</SensText></span>
                  <button onClick={() => {
                    const yaml = copyFmt === 'yaml' ? uriToClashYaml(rule.relay_uri) : null
                    copyToClipboard(yaml || rule.relay_uri).then(() => toast('代理 URI 已复制')).catch(() => toast('复制失败', 'error'))
                  }}
                    className="ml-auto bg-[#1c242f] border border-[#2a3340] text-[#aeb9c7] h-7 px-2.5 rounded text-xs flex items-center gap-1.5 hover:bg-[#26323f] hover:text-[#e8edf4]">
                    <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
                    复制
                  </button>
                </div>
              )}
            </div>
          ) : <span className="text-ink-mut text-sm">尚未分配入口</span>}
        </div>
      </div>

      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">规则信息</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[90px_1fr] gap-4 items-center text-sm">
            <span className="text-ink-soft font-semibold">名称</span>
            <span className="font-semibold">{rule.name}</span>
            <span className="text-ink-soft font-semibold">节点</span>
            <span className="font-mono">{node?.name || `#${rule.node_id}`}</span>
            <span className="text-ink-soft font-semibold">协议</span>
            <span><ProtoBadge proto={rule.proto} /></span>
            <span className="text-ink-soft font-semibold">出口</span>
            <span className="font-mono inline-flex items-center gap-2">
              <ExitKindBadge kind={rule.exit_kind} protocol={rule.landing_protocol} />
              {rule.exit_kind === 'landing' && rule.landing_name
                ? <span className="font-sans">{rule.landing_name}</span>
                : <SensText blurred={blurred}>{exitOf(rule) || '--'}</SensText>}
            </span>
            {show_rate && <>
              <span className="text-ink-soft font-semibold">倍率</span>
              <span><Badge color="blue">×{rule.rate_multiplier ?? 1}</Badge></span>
            </>}
            <span className="text-ink-soft font-semibold">流量</span>
            <span className="font-mono text-ink-mut">{fmtBytes(rule.total_bytes || 0)}</span>
            {rule.comment && <>
              <span className="text-ink-soft font-semibold">备注</span>
              <span className="text-ink-soft">{rule.comment}</span>
            </>}
          </div>
        </div>
      </div>

      <div className="flex items-center gap-3 flex-wrap">
        <button onClick={() => setShowEdit(true)} className="btn-primary text-xs">编辑规则</button>
        <button onClick={deleteRule} className="btn-secondary text-xs">删除规则</button>
      </div>
      </div>

      <RuleFormModal
        open={showEdit} onClose={() => setShowEdit(false)} title="编辑规则" submitLabel="保存并重下发"
        nodes={nodes} landingNodes={landingNodes} initial={showEdit ? ruleToForm(rule) : null}
        onSubmit={saveEdit} showRate={show_rate} />
    </Layout>
  )
}
