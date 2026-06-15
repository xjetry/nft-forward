import { useState, useEffect } from 'react'
import { useParams, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, nullStr } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, CopyText } from '../../components/ui'

export default function NodeDetail() {
  const { id } = useParams()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [name, setName] = useState('')
  const [relayHost, setRelayHost] = useState('')
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get(`/nodes/${id}`).then(d => {
      setData(d)
      setName(d.node?.name || '')
      setRelayHost(d.node?.relay_host || '')
    }).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="节点不存在" /></Layout>

  const { node, forwards = [], panel_url, panel_url_configured, server_version } = data

  const saveName = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/rename`, { name }); toast('名称已保存'); load() } catch (err) { toast(err.message) }
  }

  const saveRelay = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/relay-host`, { relay_host: relayHost }); toast('中继地址已保存'); load() } catch (err) { toast(err.message) }
  }

  const resync = async () => {
    try { await api.post(`/nodes/${id}/resync`); toast('已发起同步') } catch (err) { toast(err.message) }
  }

  const upgrade = async () => {
    if (!confirm('推送 server 二进制到此节点并重启？')) return
    try { await api.post(`/nodes/${id}/upgrade`); toast('已发起升级') } catch (err) { toast(err.message) }
  }

  const installCmd = `curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh -o install.sh\nsudo bash install.sh agent \\\n  --panel-url ${panel_url} \\\n  --token ${node.secret}`

  return (
    <Layout>
      {/* Basic info */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">基本信息</h3></div>
        <div className="p-5">
          <div className="grid grid-cols-[140px_1fr] gap-4 items-center text-sm">
            <span className="text-gray-500 font-semibold">连接 IP</span>
            <span className="font-mono">{node.address ? <SensText blurred={blurred}>{node.address}</SensText> : <span className="text-gray-300">未连接</span>}</span>
            <span className="text-gray-500 font-semibold">中继地址（数据面）</span>
            <span className="font-mono">{node.relay_host ? <SensText blurred={blurred}>{node.relay_host}</SensText> : <span className="text-gray-300">未设置（设置后才能进链路）</span>}</span>
            <span className="text-gray-500 font-semibold">Token</span>
            <span className="font-mono"><SensText blurred={blurred}>{node.secret}</SensText></span>
            <span className="text-gray-500 font-semibold">最近同步</span>
            <span className="font-mono text-gray-500">{fmtTime(node.last_apply_at?.Valid ? node.last_apply_at.Int64 : null)}</span>
            <span className="text-gray-500 font-semibold">最近心跳</span>
            <span className="font-mono">
              {fmtTime(node.last_seen?.Valid ? node.last_seen.Int64 : null)}
              {' '}
              {node.online === 1 ? <Badge color="green">在线</Badge> : <Badge color="amber">离线</Badge>}
            </span>
            <span className="text-gray-500 font-semibold">Agent 版本</span>
            <span className="font-mono">
              {node.agent_version ? (
                <>{node.agent_version} {node.agent_version !== server_version ? <Badge color="amber">非最新</Badge> : <Badge color="green">最新</Badge>}</>
              ) : <span className="text-gray-300">未知</span>}
            </span>
            <span className="text-gray-500 font-semibold">状态</span>
            <span><NodeStatusDetail node={node} /></span>
          </div>
        </div>
      </div>

      {/* Rename */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">节点名称</h3></div>
        <div className="p-5">
          <form onSubmit={saveName} className="flex items-center gap-3 max-w-xl">
            <label className="text-[13px] font-semibold text-gray-500">名称</label>
            <input className="input-field flex-1" value={name} onChange={e => setName(e.target.value)} required style={{ maxWidth: 320 }} />
            <button type="submit" className="btn-primary">保存名称</button>
          </form>
        </div>
      </div>

      {/* Relay host */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">中继地址</h3>
          <span className="text-xs text-gray-400">中继链路用它作为上一跳打向本节点的目标地址</span>
        </div>
        <div className="p-5">
          <form onSubmit={saveRelay} className="flex items-center gap-3 max-w-xl">
            <label className="text-[13px] font-semibold text-gray-500">数据面地址</label>
            <input className="input-field font-mono flex-1" value={relayHost} onChange={e => setRelayHost(e.target.value)} placeholder="数据面公网 IPv4 或域名" />
            <button type="submit" className="btn-primary">保存中继地址</button>
          </form>
        </div>
      </div>

      {/* Install command */}
      <div className="card mb-5">
        <div className="card-header"><h3 className="text-sm font-bold">节点安装命令</h3></div>
        <div className="p-5">
          <p className="text-[13px] text-gray-500 mb-2">在目标节点（已安装 nftables，root 用户）执行下列命令即可：</p>
          {!panel_url_configured && (
            <div className="mb-3 px-3 py-2 bg-amber-50 border border-amber-200 rounded text-amber-700 text-[13px]">
              尚未设置面板地址，下面用你当前访问的域名 <code className="bg-amber-100 px-1 rounded">{panel_url}</code> 推断。如 agent 走不同地址，请到<Link to="/nodes" className="text-blue-600 font-semibold">节点页</Link>设置后再复制。
            </div>
          )}
          <div className="relative">
            <pre className="bg-[#0e1117] text-[#e8edf4] px-4 py-3.5 rounded-lg font-mono text-[13px] leading-relaxed overflow-x-auto">
              <SensText blurred={blurred}>{installCmd}</SensText>
            </pre>
            <button onClick={() => { navigator.clipboard.writeText(installCmd); toast('已复制') }}
              className="absolute top-2 right-2 btn-secondary text-xs bg-[#1c242f] border-[#2a3340] text-[#aeb9c7] hover:bg-[#26323f]">复制</button>
          </div>
        </div>
      </div>

      {/* Forwards on this node */}
      <div className="card mb-5">
        <div className="card-header">
          <h3 className="text-sm font-bold">该节点上的转发</h3>
          <span className="text-xs text-gray-400">{forwards.length} 条</span>
        </div>
        {forwards.length ? (
          <table className="tbl">
            <thead><tr><th>ID</th><th>协议</th><th>模式</th><th>监听端口</th><th>目标</th><th>备注</th></tr></thead>
            <tbody>
              {forwards.map(f => (
                <tr key={f.id}>
                  <td className="font-mono text-xs text-gray-400">{f.id}</td>
                  <td><ProtoBadge proto={f.proto} /></td>
                  <td><ModeBadge mode={f.mode} /></td>
                  <td className="font-mono">{f.listen_port}</td>
                  <td className="font-mono"><SensText blurred={blurred}>{f.target_ip}:{f.target_port}</SensText></td>
                  <td className="text-gray-500">{f.comment || '--'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <Empty title="该节点尚无转发规则"><Link to="/forwards" className="text-blue-600 text-xs font-semibold">去添加</Link></Empty>}
      </div>

      {/* Actions */}
      <div className="flex items-center gap-3 flex-wrap">
        <button onClick={resync} className="btn-secondary">重新同步</button>
        {node.agent_version && node.agent_version !== server_version && (
          <button onClick={upgrade} className="btn-primary">推送升级到 {server_version}</button>
        )}
        <Link to="/nodes" className="text-blue-600 text-[13px] font-semibold hover:underline inline-flex items-center gap-1">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
          返回节点列表
        </Link>
      </div>
    </Layout>
  )
}

function NodeStatusDetail({ node }) {
  if (node.disabled) return <Badge color="amber">禁用</Badge>
  const lastErr = nullStr(node.last_error)
  if (lastErr) return <Badge color="red">错误：{lastErr}</Badge>
  if (node.last_apply_at?.Valid) return <Badge color="green">已同步</Badge>
  return <Badge color="amber">待同步（agent 尚未连上）</Badge>
}
