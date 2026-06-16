import { useState, useEffect } from 'react'
import { useParams, Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, fmtGB, nullStr } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, NodeTypeBadge } from '../../components/ui'

const card = 'bg-white border border-[#e6e9ee] rounded-2xl shadow-[0_1px_2px_rgba(16,24,40,0.04)]'

export default function NodeDetail() {
  const { id } = useParams()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [name, setName] = useState('')
  const [relayHost, setRelayHost] = useState('')
  const [portRange, setPortRange] = useState('')
  const toast = useToast()
  const blurred = useBlur()

  const load = () => {
    setLoading(true)
    api.get(`/nodes/${id}`).then(d => {
      setData(d)
      setName(d.node?.name || '')
      setRelayHost(d.node?.relay_host || '')
      setPortRange(d.node?.port_range || '')
    }).catch(console.error).finally(() => setLoading(false))
  }
  useEffect(load, [id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="节点不存在" /></Layout>

  const { node, panel_url, panel_url_configured, server_version } = data
  const ruleHops = data.rule_hops || []
  const nodeHops = data.node_hops || []
  const isComposite = node.node_type === 'composite'
  const agentOutdated = node.agent_version && node.agent_version !== server_version

  const saveName = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/rename`, { name }); toast('名称已保存'); load() } catch (err) { toast(err.message) }
  }
  const saveRelay = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/relay-host`, { relay_host: relayHost }); toast('中继地址已保存'); load() } catch (err) { toast(err.message) }
  }
  const savePortRange = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/port-range`, { port_range: portRange }); toast('端口范围已保存'); load() } catch (err) { toast(err.message) }
  }
  const resync = async () => {
    try { await api.post(`/nodes/${id}/resync`); toast('已发起同步') } catch (err) { toast(err.message) }
  }
  const upgrade = async () => {
    if (!confirm('推送 server 二进制到此节点并重启？')) return
    try { await api.post(`/nodes/${id}/upgrade`); toast('已发起升级') } catch (err) { toast(err.message) }
  }
  const toggle = async () => {
    try { await api.post(`/nodes/${id}/toggle`); toast(node.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }

  const installCmd = `curl -fsSL https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh -o install.sh\nsudo bash install.sh agent \\\n  --panel-url ${panel_url} \\\n  --token ${node.secret}`

  return (
    <Layout>
      <div className="mx-auto max-w-[1160px] flex flex-col gap-[18px]">

        {/* back link */}
        <Link to="/nodes" className="inline-flex items-center gap-1.5 w-fit text-[13.5px] text-[#6b7685] hover:text-[#1f2733] transition-colors">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7" /></svg>
          返回节点列表
        </Link>

        {/* ===== HEADER ===== */}
        <header className={`${card} flex items-start gap-5 px-[26px] py-[22px]`}>
          <div className="w-[52px] h-[52px] flex-none rounded-[14px] grid place-items-center text-blue-600 text-[22px] font-bold"
            style={{ background: 'linear-gradient(135deg,#eef3ff,#dbe6ff)' }}>⬡</div>
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2.5 flex-wrap">
              <h1 className="m-0 text-[22px] font-bold tracking-[-0.01em]">{node.name}</h1>
              <NodeTypeBadge type={node.node_type} />
              <HeaderStatus node={node} />
            </div>
            <div className="mt-2 flex items-center gap-[18px] flex-wrap text-[13px] text-[#6b7685]">
              <span>连接 IP&nbsp;&nbsp;{node.address
                ? <b className="text-[#39424f] font-mono font-semibold"><SensText blurred={blurred}>{node.address}</SensText></b>
                : <span className="text-gray-300">未连接</span>}</span>
              <span className="text-[#cfd6df]">|</span>
              <span>Agent&nbsp;&nbsp;{node.agent_version
                ? <><span className="font-mono text-[#39424f]">{node.agent_version}</span>{agentOutdated && <span className="ml-1"><Badge color="amber">非最新</Badge></span>}</>
                : <span className="text-gray-300">未知</span>}</span>
              <span className="text-[#cfd6df]">|</span>
              <span>最近同步&nbsp;&nbsp;<b className="text-[#39424f] font-semibold">{fmtTime(node.last_apply_at?.Valid ? node.last_apply_at.Int64 : null)}</b></span>
            </div>
          </div>

          {/* header actions */}
          <div className="flex-none flex flex-wrap justify-end gap-2.5 max-w-[520px]">
            {node.disabled ? (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-[#eafaf1] text-[#0a8a4f] border border-[#c6ecd6] hover:brightness-95 transition cursor-pointer">启用节点</button>
            ) : (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-white text-[#b42318] border border-[#f1c7c2] hover:bg-[#fef3f2] transition-colors cursor-pointer">禁用节点</button>
            )}
            <button onClick={resync} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-white text-[#39424f] border border-[#d7dce3] hover:bg-[#f7f9fc] transition-colors cursor-pointer">重新同步</button>
            {agentOutdated && (
              <button onClick={upgrade} title={`推送升级到 ${server_version}`} className="inline-flex items-center gap-1.5 px-4 py-[9px] rounded-[10px] text-[13px] font-semibold bg-blue-600 text-white hover:bg-blue-700 border-0 cursor-pointer transition-colors max-w-[280px] truncate">⤴ 推送升级到 {server_version}</button>
            )}
          </div>
        </header>

        {/* ===== TWO COLUMN: 基本信息 + 节点配置 ===== */}
        <div className="grid grid-cols-1 lg:grid-cols-[1.55fr_1fr] gap-[18px] items-start">

          {/* 基本信息 */}
          <section className={`${card} px-[26px] py-[22px]`}>
            <h2 className="m-0 mb-[18px] text-[15px] font-bold">基本信息</h2>
            <div className="grid grid-cols-[auto_1fr]">
              <InfoRow label="连接 IP" mono>
                {node.address ? <SensText blurred={blurred}>{node.address}</SensText> : <span className="text-gray-300">未连接</span>}
              </InfoRow>
              <InfoRow label="中继地址（数据面）" mono>
                {node.relay_host ? <SensText blurred={blurred}>{node.relay_host}</SensText> : <span className="text-gray-300">未设置（设置后才能进链路）</span>}
              </InfoRow>
              <InfoRow label="Token" mono valueClass="text-[12.5px] break-all leading-relaxed">
                <SensText blurred={blurred}>{node.secret}</SensText>
              </InfoRow>
              <InfoRow label="最近心跳">
                {fmtTime(node.last_seen ?? null)}
                <span className={`ml-1.5 font-semibold ${node.online === 1 ? 'text-[#0a8a4f]' : 'text-[#c2520a]'}`}>{node.online === 1 ? '在线' : '离线'}</span>
              </InfoRow>
              <InfoRow label="Agent 版本" mono valueClass="text-[12.5px]" last>
                {node.agent_version
                  ? <>{node.agent_version} {agentOutdated ? <Badge color="amber">非最新</Badge> : <Badge color="green">最新</Badge>}</>
                  : <span className="text-gray-300">未知</span>}
              </InfoRow>
            </div>
          </section>

          {/* 节点配置 */}
          <section className={`${card} px-6 py-[22px] flex flex-col gap-[18px]`}>
            <h2 className="m-0 text-[15px] font-bold">节点配置</h2>

            <ConfigField label="节点名称">
              <form onSubmit={saveName} className="flex gap-2">
                <input className="input-field flex-1" value={name} onChange={e => setName(e.target.value)} required />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            <ConfigField label="中继地址（数据面）" hint="中继链路用它作为上一跳打向本节点的目标地址">
              <form onSubmit={saveRelay} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHost} onChange={e => setRelayHost(e.target.value)} placeholder="数据面公网 IPv4 或域名" />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            {!isComposite && (
              <ConfigField label="端口范围" hint="规则自动分配监听端口时从该范围中选取">
                <form onSubmit={savePortRange} className="flex gap-2">
                  <input className="input-field font-mono flex-1" value={portRange} onChange={e => setPortRange(e.target.value)} placeholder="例如 10001-19999,23333,40000-42000" />
                  <button type="submit" className="btn-primary flex-none px-5">保存</button>
                </form>
              </ConfigField>
            )}
          </section>
        </div>

        {/* ===== 组合节点跳序 ===== */}
        {isComposite && nodeHops.length > 0 && (
          <section className={`${card} px-[26px] pt-[22px] pb-2`}>
            <div className="flex items-baseline gap-2.5 mb-1.5">
              <h2 className="m-0 text-[15px] font-bold">组合节点跳序</h2>
              <span className="text-[12.5px] text-[#9aa4b2]">{nodeHops.length} 跳</span>
            </div>
            <table className="tbl">
              <thead><tr><th className="w-10">#</th><th>节点</th><th>模式</th></tr></thead>
              <tbody>
                {nodeHops.map((h, i) => (
                  <tr key={i}>
                    <td className="font-mono text-xs text-gray-400">{i + 1}</td>
                    <td className="font-semibold">{h.node_name || `#${h.hop_node_id}`}</td>
                    <td><ModeBadge mode={h.mode} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        )}

        {/* ===== 安装命令 ===== */}
        <section className={`${card} px-[26px] py-[22px]`}>
          <div className="flex items-baseline gap-3 mb-3.5 flex-wrap">
            <h2 className="m-0 text-[15px] font-bold">节点安装命令</h2>
            <span className="text-[12.5px] text-[#9aa4b2]">在目标节点（已安装 nftables，root 用户）执行下列命令即可</span>
          </div>
          {!panel_url_configured && (
            <div className="mb-3 px-3 py-2 bg-amber-50 border border-amber-200 rounded-lg text-amber-700 text-[13px]">
              尚未设置面板地址，下面用你当前访问的域名 <code className="bg-amber-100 px-1 rounded">{panel_url}</code> 推断。如 agent 走不同地址，请到<Link to="/nodes" className="text-blue-600 font-semibold">节点页</Link>设置后再复制。
            </div>
          )}
          <div className="relative bg-[#0e1320] rounded-xl px-5 py-[18px]">
            <button onClick={() => { navigator.clipboard.writeText(installCmd); toast('已复制') }}
              className="absolute top-3.5 right-3.5 text-[12px] font-semibold text-[#cdd5e0] bg-white/10 border border-white/15 px-3 py-[5px] rounded-lg cursor-pointer hover:bg-white/20 transition-colors">复制</button>
            <pre className="m-0 font-mono text-[13px] leading-[1.75] text-[#e8ecf3] whitespace-pre-wrap break-all">
              <SensText blurred={blurred}>{installCmd}</SensText>
            </pre>
          </div>
        </section>

        {/* ===== 经过该节点的规则 ===== */}
        <section className={`${card} px-[26px] pt-[22px] pb-2`}>
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h2 className="m-0 text-[15px] font-bold">经过该节点的规则</h2>
            <span className="text-[12.5px] text-[#9aa4b2]">{ruleHops.length} 条</span>
          </div>
          {ruleHops.length ? (
            <table className="tbl">
              <thead><tr><th>规则</th><th>协议</th><th>模式</th><th>监听端口</th><th>目标</th><th className="text-right">流量</th></tr></thead>
              <tbody>
                {ruleHops.map((rh, i) => (
                  <tr key={i}>
                    <td className="font-semibold">
                      {rh.rule_id ? <Link to={`/rules/${rh.rule_id}`} className="text-blue-600 hover:underline">{rh.rule_name || `#${rh.rule_id}`}</Link> : '--'}
                    </td>
                    <td><ProtoBadge proto={rh.proto} /></td>
                    <td><ModeBadge mode={rh.mode} /></td>
                    <td className="font-mono">{rh.listen_port}</td>
                    <td className="font-mono"><SensText blurred={blurred}>{rh.target || '--'}</SensText></td>
                    <td className="text-right font-mono text-xs text-gray-400">{fmtGB(rh.total_bytes)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <div className="pb-4"><Empty title="该节点尚无规则经过"><Link to="/rules" className="text-blue-600 text-xs font-semibold">去添加</Link></Empty></div>}
        </section>

      </div>
    </Layout>
  )
}

function InfoRow({ label, mono, valueClass = '', last, children }) {
  const bb = last ? '' : 'border-b border-[#f0f2f5]'
  return (
    <>
      <div className={`text-[13px] text-[#6b7685] pr-5 py-[11px] ${bb}`}>{label}</div>
      <div className={`text-[13.5px] text-[#1f2733] py-[11px] min-w-0 ${mono ? 'font-mono' : ''} ${valueClass} ${bb}`}>{children}</div>
    </>
  )
}

function ConfigField({ label, hint, children }) {
  return (
    <div>
      <label className="block text-[12.5px] text-[#6b7685] mb-[7px] font-medium">{label}</label>
      {hint && <p className="mt-[-2px] mb-[7px] text-[11.5px] text-[#9aa4b2] leading-snug">{hint}</p>}
      {children}
    </div>
  )
}

// Header status pill combining online + sync state, with disabled/error overriding.
function HeaderStatus({ node }) {
  let text, cls, dot
  if (node.disabled) {
    text = '已禁用'; cls = 'text-[#c2520a] bg-[#fdeede] border-[#f6d4ac]'; dot = '#e0892f'
  } else if (nullStr(node.last_error)) {
    text = '错误'; cls = 'text-[#b42318] bg-[#fef3f2] border-[#f1c7c2]'; dot = '#e5484d'
  } else if (node.online === 1) {
    text = node.last_apply_at?.Valid ? '在线 · 已同步' : '在线 · 待同步'
    cls = 'text-[#0a8a4f] bg-[#e7f7ee] border-[#c6ecd6]'; dot = '#22b46e'
  } else {
    text = '离线'; cls = 'text-[#6b7685] bg-[#f1f3f6] border-[#e1e5ea]'; dot = '#9aa4b2'
  }
  return (
    <span className={`inline-flex items-center gap-1.5 text-[12.5px] font-semibold px-2.5 py-[3px] rounded-full border ${cls}`}>
      <span className="w-1.5 h-1.5 rounded-full" style={{ background: dot }} />
      {text}
    </span>
  )
}
