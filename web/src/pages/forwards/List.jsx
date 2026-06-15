import { useState, useEffect } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, nullInt } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Modal, ProtoBadge, ModeBadge, SensText, ProbeButton } from '../../components/ui'

export default function ForwardList() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = searchParams.get('tab') || 'normal'
  const [showAdd, setShowAdd] = useState(false)
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get(`/forwards?tab=${tab}`).then(setData).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [tab])

  const deleteForward = async (f) => {
    if (!confirm('确认删除该转发？')) return
    try { await api.del(`/forwards/${f.id}`); toast('已删除'); load() } catch (err) { toast(err.message) }
  }

  if (loading) return <Layout><Loading /></Layout>

  const { forwards = [], nodes = [], combos = [], node_by_id = {}, tenant_by_id = {}, hop_info = {} } = data || {}

  return (
    <Layout>
      <div className="card">
        <div className="card-header">
          <h3 className="text-sm font-bold">当前规则</h3>
          <span className="text-xs text-gray-400">{forwards.length} 条转发规则</span>
          {nodes.length > 0 && (
            <button onClick={() => setShowAdd(true)} className="btn-primary text-xs ml-auto">+ 添加转发</button>
          )}
        </div>

        {/* Tabs */}
        <div className="flex border-b border-gray-100 px-5">
          <button onClick={() => setSearchParams({ tab: 'normal' })}
            className={`px-4 py-2.5 text-[13px] font-semibold border-b-2 transition-colors ${tab === 'normal' ? 'text-blue-600 border-blue-600' : 'text-gray-400 border-transparent hover:text-gray-600'}`}>
            普通规则
          </button>
          <button onClick={() => setSearchParams({ tab: 'chain' })}
            className={`px-4 py-2.5 text-[13px] font-semibold border-b-2 transition-colors ${tab === 'chain' ? 'text-blue-600 border-blue-600' : 'text-gray-400 border-transparent hover:text-gray-600'}`}>
            链路规则
          </button>
        </div>

        {forwards.length ? (
          tab === 'normal' ? (
            <table className="tbl">
              <thead><tr><th>节点</th><th>用户</th><th>协议</th><th>模式</th><th>监听</th><th>目标</th><th className="text-right">累计流量</th><th>备注</th><th>测试</th><th className="text-right">操作</th></tr></thead>
              <tbody>
                {forwards.map(f => {
                  const node = node_by_id?.[f.node_id]
                  const tenantId = nullInt(f.tenant_id)
                  const tenant = tenantId ? tenant_by_id?.[tenantId] : null
                  return (
                    <tr key={f.id}>
                      <td className="font-semibold">{node?.name || `#${f.node_id}`}</td>
                      <td className="text-gray-500">{tenant ? tenant.name : 'admin'}</td>
                      <td><ProtoBadge proto={f.proto} /></td>
                      <td><ModeBadge mode={f.mode} /></td>
                      <td className="font-mono">:{f.listen_port}</td>
                      <td className="font-mono"><SensText blurred={blurred}>{f.target_ip}:{f.target_port}</SensText></td>
                      <td className="text-right font-mono">{fmtBytes(f.total_bytes)}</td>
                      <td className="text-gray-500">{f.comment || <span className="text-gray-300">--</span>}</td>
                      <td>{node?.relay_host ? <ProbeButton target={`${node.relay_host}:${f.listen_port}`} /> : <span className="text-gray-300">--</span>}</td>
                      <td className="text-right whitespace-nowrap">
                        <Link to={`/forwards/${f.id}/edit`} className="btn-secondary text-xs mr-1.5">编辑</Link>
                        <button onClick={() => deleteForward(f)} className="btn-danger-sm text-xs">删除</button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          ) : (
            <table className="tbl">
              <thead><tr><th>节点</th><th>用户</th><th>协议</th><th>模式</th><th>监听</th><th>目标</th><th className="text-right">累计流量</th><th>链路</th><th>测试</th><th className="text-right">操作</th></tr></thead>
              <tbody>
                {forwards.map(f => {
                  const node = node_by_id?.[f.node_id]
                  const tenantId = nullInt(f.tenant_id)
                  const tenant = tenantId ? tenant_by_id?.[tenantId] : null
                  const hi = hop_info?.[f.id]
                  const chainId = nullInt(f.chain_id)
                  return (
                    <tr key={f.id} className="bg-blue-50/30">
                      <td className="font-semibold">{node?.name || `#${f.node_id}`}</td>
                      <td className="text-gray-500">{tenant ? tenant.name : 'admin'}</td>
                      <td><ProtoBadge proto={f.proto} /></td>
                      <td><ModeBadge mode={f.mode} /></td>
                      <td className="font-mono">:{f.listen_port}</td>
                      <td className="font-mono"><SensText blurred={blurred}>{f.target_ip}:{f.target_port}</SensText></td>
                      <td className="text-right font-mono">{fmtBytes(f.total_bytes)}</td>
                      <td>
                        {chainId ? (
                          <Link to={`/chains/${chainId}`} className="inline-flex items-center gap-1 text-xs font-semibold text-blue-700 bg-blue-50 border border-blue-200 px-2 py-0.5 rounded">
                            <svg className="w-3 h-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9.5 14.5l5-5"/><path d="M8 9l-2.5 2.5a3.5 3.5 0 0 0 5 5L13 14"/><path d="M16 15l2.5-2.5a3.5 3.5 0 0 0-5-5L11 10"/></svg>
                            {hi ? `${hi.chain_name} · 第${hi.position + 1}/${hi.total_hops}跳` : `链路 #${chainId}`}
                          </Link>
                        ) : '--'}
                      </td>
                      <td>{node?.relay_host ? <ProbeButton target={`${node.relay_host}:${f.listen_port}`} /> : <span className="text-gray-300">--</span>}</td>
                      <td className="text-right">
                        <button onClick={() => deleteForward(f)} className="btn-danger-sm text-xs">删除</button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )
        ) : <Empty title="暂无转发规则" />}
      </div>

      {nodes.length > 0 && (
        <AddForwardModal open={showAdd} onClose={() => setShowAdd(false)} nodes={nodes} combos={combos} onDone={() => { setShowAdd(false); load() }} />
      )}
    </Layout>
  )
}

function AddForwardModal({ open, onClose, nodes, combos, onDone }) {
  const [nodeId, setNodeId] = useState('')
  const [proto, setProto] = useState('tcp')
  const [mode, setMode] = useState('kernel')
  const [listenPort, setListenPort] = useState('')
  const [exit, setExit] = useState('')
  const [comment, setComment] = useState('')
  const [chainName, setChainName] = useState('')
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const isCombo = nodeId.startsWith('combo:')

  useEffect(() => {
    if (combos?.length) setNodeId(`combo:${combos[0].id}`)
    else if (nodes?.length) setNodeId(String(nodes[0].id))
  }, [combos, nodes])

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/forwards', {
        node_id: isCombo ? nodeId : Number(nodeId),
        proto,
        mode: isCombo ? undefined : mode,
        listen_port: isCombo ? undefined : (listenPort ? Number(listenPort) : undefined),
        exit,
        comment: isCombo ? undefined : comment,
        chain_name: isCombo ? chainName : undefined,
      })
      toast('转发已添加')
      setListenPort(''); setExit(''); setComment(''); setChainName('')
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }

  return (
    <Modal open={open} onClose={onClose} title="添加转发" wide>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">节点 / 组合</label>
          <select className="input-field" value={nodeId} onChange={e => setNodeId(e.target.value)} required>
            {combos?.length > 0 && (
              <optgroup label="组合通道">
                {combos.map(c => <option key={`combo:${c.id}`} value={`combo:${c.id}`}>{c.name}</option>)}
              </optgroup>
            )}
            <optgroup label="单节点">
              {nodes.map(n => <option key={n.id} value={n.id}>{n.name}</option>)}
            </optgroup>
          </select>
        </div>

        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">协议</label>
          <div className="flex gap-2">
            {['tcp', 'udp', ...(isCombo ? [] : ['tcp+udp'])].map(v => (
              <label key={v} className="seg-label">
                <input type="radio" name="fwd-proto" value={v} checked={proto === v} onChange={() => setProto(v)} className="sr-only peer" />
                <span className="seg-span">{v.toUpperCase()}</span>
              </label>
            ))}
          </div>
        </div>

        {!isCombo && (
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
            <label className="fl">模式</label>
            <div className="flex gap-2">
              {[['kernel', '内核态 (零拷贝)'], ['userspace', '用户态 (split-TCP)']].map(([v, l]) => (
                <label key={v} className="seg-label">
                  <input type="radio" name="fwd-mode" value={v} checked={mode === v} onChange={() => setMode(v)} className="sr-only peer" />
                  <span className="seg-span">{l}</span>
                </label>
              ))}
            </div>
          </div>
        )}

        {!isCombo && (
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
            <label className="fl">监听端口</label>
            <input className="input-field font-mono" type="number" min="1" max="65535" value={listenPort} onChange={e => setListenPort(e.target.value)} placeholder="留空自动分配" style={{ maxWidth: 200 }} />
          </div>
        )}

        <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
          <label className="fl">目标</label>
          <input className="input-field font-mono" value={exit} onChange={e => setExit(e.target.value)} required placeholder="host:port（如 1.2.3.4:8443）" />
        </div>

        {!isCombo && (
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
            <label className="fl">备注 <span className="text-gray-400 font-normal text-xs">(可选)</span></label>
            <input className="input-field" value={comment} onChange={e => setComment(e.target.value)} placeholder="可选" />
          </div>
        )}

        {isCombo && (
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center">
            <label className="fl">链路名称</label>
            <input className="input-field" value={chainName} onChange={e => setChainName(e.target.value)} placeholder="自动命名或手填" />
          </div>
        )}

        <div className="flex gap-3 pt-4 border-t border-gray-100">
          <button type="submit" disabled={loading} className="btn-primary">添加转发</button>
          <button type="button" onClick={onClose} className="btn-secondary">取消</button>
        </div>
      </form>
    </Modal>
  )
}
