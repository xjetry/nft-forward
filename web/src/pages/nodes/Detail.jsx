import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, fmtBytes, nullStr } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, NodeTypeBadge, useConfirm, Select } from '../../components/ui'

const card = 'bg-surface border border-line rounded-[14px] shadow-[0_1px_2px_rgba(16,24,40,0.04)]'

export default function NodeDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [name, setName] = useState('')
  const [relayHost, setRelayHost] = useState('')
  const [portRange, setPortRange] = useState('')
  const [useGhProxy, setUseGhProxy] = useState(false)
  const [ghProxy, setGhProxy] = useState('https://gh-proxy.com/')
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()

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

  const { node, panel_url, panel_url_configured, latest_agent_version } = data
  const ruleHops = data.rule_hops || []
  const nodeHops = data.node_hops || []
  const grantedUsers = data.granted_users || []
  const isComposite = node.node_type === 'composite'
  const agentOutdated = node.agent_version && node.agent_version !== latest_agent_version
  const up = data.upgrade || { status: 'none' }
  const showUpgrade = up.status !== 'none'

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
    if (!(await confirm({ title: '升级节点', message: '推送 agent 二进制到此节点并重启？', confirmText: '升级' }))) return
    try { await api.post(`/nodes/${id}/upgrade`); toast('已发起升级') } catch (err) { toast(err.message) }
  }
  const toggle = async () => {
    try { await api.post(`/nodes/${id}/toggle`); toast(node.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message) }
  }

  const toggleHidden = async () => {
    try { await api.post(`/nodes/${id}/hidden`); toast(node.hidden ? '已显示节点' : '已隐藏节点'); load() } catch (err) { toast(err.message) }
  }
  const remove = async () => {
    if (!(await confirm({ title: '删除节点', message: `删除节点「${node.name}」？经过它的规则会被重新连接或清除，此操作不可撤销。`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/nodes/${id}`); toast('节点已删除'); navigate('/nodes') } catch (err) { toast(err.message) }
  }

  // gh-proxy is a URL prefix: enabling it routes both the install.sh fetch and
  // the binary downloads (via --gh-proxy passed to the script) through the mirror
  // so nodes behind a GitHub-blocked network can still install.
  const proxyPrefix = useGhProxy && ghProxy.trim()
    ? (ghProxy.trim().endsWith('/') ? ghProxy.trim() : ghProxy.trim() + '/')
    : ''
  const portRangePart = node.port_range && node.port_range !== '10001-20000'
    ? ` \\\n  --port-range ${node.port_range}`
    : ''
  const installCmd = `curl -fsSL ${proxyPrefix}https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | bash -s agent \\\n  --panel-url ${panel_url} \\\n  --token ${node.secret}${portRangePart}${proxyPrefix ? ` \\\n  --gh-proxy ${proxyPrefix}` : ''}`

  return (
    <Layout>
      <div className="mx-auto max-w-[1160px] flex flex-col gap-[18px]">

        {/* back link */}
        <Link to="/nodes" className="inline-flex items-center gap-1.5 w-fit text-[13.5px] text-ink-mut hover:text-ink transition-colors">
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
              {isComposite
                ? (node.disabled && <Badge color="amber">已禁用</Badge>)
                : <HeaderStatus node={node} />}
              {node.hidden && <Badge color="gray">已隐藏</Badge>}
            </div>
            {isComposite ? (
              <div className="mt-2 text-[13px] text-ink-mut">虚拟链路节点，由下列各跳依次转发；自身无 Agent / IP / 同步状态</div>
            ) : (
              <div className="mt-2 flex items-center gap-[18px] flex-wrap text-[13px] text-ink-mut">
                <span>连接 IP&nbsp;&nbsp;{node.address
                  ? <b className="text-ink-soft font-mono font-semibold"><SensText blurred={blurred}>{node.address}</SensText></b>
                  : <span className="text-ink-mut">未连接</span>}</span>
                <span className="text-[#cfd6df]">|</span>
                <span>Agent&nbsp;&nbsp;{node.agent_version
                  ? <><span className="font-mono text-ink-soft">{node.agent_version}</span>{agentOutdated && <span className="ml-1"><Badge color="amber">非最新</Badge></span>}</>
                  : <span className="text-ink-mut">未知</span>}</span>
                <span className="text-[#cfd6df]">|</span>
                <span>最近同步&nbsp;&nbsp;<b className="text-ink-soft font-semibold">{fmtTime(node.last_apply_at?.Valid ? node.last_apply_at.Int64 : null)}</b></span>
              </div>
            )}
          </div>

          {/* header actions */}
          <div className="flex-none flex flex-wrap justify-end gap-2.5 max-w-[520px]">
            {node.disabled ? (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-[#eafaf1] text-[#0a8a4f] border border-[#c6ecd6] hover:brightness-95 transition cursor-pointer">启用节点</button>
            ) : (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-[#b42318] border border-[#f1c7c2] hover:bg-[#fef3f2] transition-colors cursor-pointer">禁用节点</button>
            )}
            <button onClick={toggleHidden} title="隐藏后，该节点默认不在节点列表显示，其规则也不在规则列表显示（不影响转发）" className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-ink-soft border border-[#d7dce3] hover:bg-[#f7f9fc] transition-colors cursor-pointer">{node.hidden ? '显示节点' : '隐藏节点'}</button>
            {!isComposite && (
              <button onClick={resync} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-ink-soft border border-[#d7dce3] hover:bg-[#f7f9fc] transition-colors cursor-pointer">重新同步</button>
            )}
            <button onClick={remove} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-[#b42318] border border-[#f1c7c2] hover:bg-[#fef3f2] transition-colors cursor-pointer">删除节点</button>
            {!isComposite && agentOutdated && (
              <button onClick={upgrade} title={`推送升级到 ${latest_agent_version}`} className="inline-flex items-center gap-1.5 px-4 py-[9px] rounded-[10px] text-[13px] font-semibold bg-blue-600 text-white hover:bg-blue-700 border-0 cursor-pointer transition-colors max-w-[280px] truncate">⤴ 推送升级到 {latest_agent_version}</button>
            )}
          </div>
        </header>

        {/* ===== 组合节点：仅名称配置 ===== */}
        {isComposite ? (
          <section className={`${card} px-[26px] py-[22px]`}>
            <h2 className="m-0 mb-[18px] text-[15px] font-bold">节点配置</h2>
            <ConfigField label="节点名称">
              <form onSubmit={saveName} className="flex gap-2 max-w-md">
                <input className="input-field flex-1" value={name} onChange={e => setName(e.target.value)} required />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>
          </section>
        ) : (
        /* ===== TWO COLUMN: 基本信息 + 节点配置 ===== */
        <div className="grid grid-cols-1 lg:grid-cols-[1.55fr_1fr] gap-[18px] items-start">

          {/* 基本信息 */}
          <section className={`${card} px-[26px] py-[22px]`}>
            <h2 className="m-0 mb-[18px] text-[15px] font-bold">基本信息</h2>
            <div className="grid grid-cols-[auto_1fr]">
              <InfoRow label="连接 IP" mono>
                {node.address ? <SensText blurred={blurred}>{node.address}</SensText> : <span className="text-ink-mut">未连接</span>}
              </InfoRow>
              <InfoRow label="中继地址（数据面）" mono>
                {node.relay_host ? <SensText blurred={blurred}>{node.relay_host}</SensText> : <span className="text-ink-mut">未设置（设置后才能进链路）</span>}
              </InfoRow>
              <InfoRow label="Token" mono valueClass="text-[12.5px] break-all leading-relaxed">
                <SensText blurred={blurred}>{node.secret}</SensText>
              </InfoRow>
              <InfoRow label="最近心跳">
                {fmtTime(node.last_seen ?? null)}
                <span className={`ml-1.5 font-semibold ${node.online === 1 ? 'text-[#0a8a4f]' : 'text-[#c2520a]'}`}>{node.online === 1 ? '在线' : '离线'}</span>
              </InfoRow>
              <InfoRow label="Agent 版本" mono valueClass="text-[12.5px]" last={!showUpgrade}>
                {node.agent_version
                  ? <>{node.agent_version} {agentOutdated ? <Badge color="amber">非最新</Badge> : <Badge color="green">最新</Badge>}</>
                  : <span className="text-ink-mut">未知</span>}
              </InfoRow>
              {showUpgrade && (
                <InfoRow label="升级状态" valueClass="text-[12.5px]" last>
                  {up.status === 'ok' && <><Badge color="green">升级成功</Badge> <span className="ml-1 text-ink-mut">{up.version} · {fmtTime(up.at)}</span></>}
                  {up.status === 'error' && <><Badge color="red">升级失败</Badge> <span className="ml-1 text-ink-mut break-all">{up.error}</span></>}
                  {up.status === 'pending' && <><Badge color="blue">升级中</Badge> <span className="ml-1 text-ink-mut">已推送 {up.version} · {fmtTime(up.at)}</span></>}
                  {up.status === 'stuck' && <><Badge color="amber">可能未生效</Badge> <span className="ml-1 text-ink-mut">已确认接收 {up.version}（{fmtTime(up.at)}），当前仍为 {node.agent_version || '未知'}，可能重启失败</span></>}
                </InfoRow>
              )}
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

            <ConfigField label="端口范围" hint="规则自动分配监听端口时从该范围中选取">
              <form onSubmit={savePortRange} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={portRange} onChange={e => setPortRange(e.target.value)} placeholder="例如 10001-19999,23333,40000-42000" />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>
          </section>
        </div>
        )}

        {/* ===== 组合节点跳序 ===== */}
        {isComposite && nodeHops.length > 0 && (
          <CompositeHopsCard nodeId={id} hops={nodeHops} onDone={load} />
        )}

        {/* ===== 安装命令（实体节点专属） ===== */}
        {!isComposite && (
        <section className={`${card} px-[26px] py-[22px]`}>
          <div className="flex items-baseline gap-3 mb-3.5 flex-wrap">
            <h2 className="m-0 text-[15px] font-bold">节点安装命令</h2>
            <span className="text-[12.5px] text-ink-mut">在目标节点（已安装 nftables，root 用户）执行下列命令即可</span>
          </div>
          {!panel_url_configured && (
            <div className="mb-3 px-3 py-2 bg-amber-50 border border-amber-200 rounded-lg text-amber-700 text-[13px]">
              尚未设置面板地址，下面用你当前访问的域名 <code className="bg-amber-100 px-1 rounded">{panel_url}</code> 推断。如 agent 走不同地址，请到<Link to="/nodes" className="text-blue-600 font-semibold">节点页</Link>设置后再复制。
            </div>
          )}
          <div className="mb-3 flex items-center gap-2.5 flex-wrap text-[13px]">
            <label className="inline-flex items-center gap-1.5 cursor-pointer select-none">
              <input type="checkbox" checked={useGhProxy} onChange={e => setUseGhProxy(e.target.checked)} className="accent-blue-600" />
              <span className="text-ink-soft">使用 gh-proxy（GitHub 受限网络）</span>
            </label>
            {useGhProxy && (
              <input className="input-field font-mono" style={{ height: 30, maxWidth: 280 }} value={ghProxy}
                onChange={e => setGhProxy(e.target.value)} placeholder="https://gh-proxy.com/" />
            )}
          </div>
          <div className="relative bg-[#1e1e2e] dark:bg-app border border-line rounded-[10px] px-5 py-[18px]">
            <button onClick={() => { navigator.clipboard.writeText(installCmd); toast('已复制') }}
              className="absolute top-3.5 right-3.5 text-[12.5px] font-semibold text-[#a0a4b0] bg-[#2a2a3c] border border-[#3a3a4c] px-3.5 py-[6px] rounded-[7px] cursor-pointer hover:bg-[#33334a] transition-colors">复制</button>
            <pre className="m-0 font-mono text-[13px] leading-[1.75] text-[#e8ecf3] whitespace-pre-wrap break-all">
              <SensText blurred={blurred}>{installCmd}</SensText>
            </pre>
          </div>
        </section>
        )}

        {/* ===== 经过该节点的规则 ===== */}
        <section className={`${card} px-[26px] pt-[22px] pb-2`}>
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h2 className="m-0 text-[15px] font-bold">经过该节点的规则</h2>
            <span className="text-[12.5px] text-ink-mut">{ruleHops.length} 条</span>
          </div>
          {ruleHops.length ? (
            <table className="tbl">
              <thead><tr><th>规则</th><th>类型</th><th>协议</th><th>模式</th><th>监听端口</th><th>目标</th><th className="text-right">流量</th></tr></thead>
              <tbody>
                {ruleHops.map((rh, i) => (
                  <tr key={i}>
                    <td className="font-semibold">
                      {rh.rule_id ? <Link to={`/rules/${rh.rule_id}`} className="text-blue-600 hover:underline">{rh.rule_name || `#${rh.rule_id}`}</Link> : '--'}
                    </td>
                    <td className="text-xs whitespace-nowrap">
                      {rh.total_hops > 1
                        ? <Link to={`/nodes/${rh.rule_node_id}`} className="text-purple-600 font-semibold hover:underline">组合 {rh.position + 1}/{rh.total_hops}</Link>
                        : <span className="text-ink-mut">单点</span>}
                    </td>
                    <td><ProtoBadge proto={rh.proto} /></td>
                    <td><ModeBadge mode={rh.mode} /></td>
                    <td className="font-mono">{rh.listen_port}</td>
                    <td className="font-mono"><SensText blurred={blurred}>{rh.target_host ? `${rh.target_host}:${rh.target_port}` : '--'}</SensText></td>
                    <td className="text-right font-mono text-xs text-ink-mut">{fmtBytes(rh.total_bytes)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <div className="pb-4"><Empty title="该节点尚无规则经过"><Link to="/rules" className="text-blue-600 text-xs font-semibold">去添加</Link></Empty></div>}
        </section>

        {/* ===== 已授权用户 ===== */}
        <section className={`${card} px-[26px] pt-[22px] pb-2`}>
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h2 className="m-0 text-[15px] font-bold">已授权用户</h2>
            <span className="text-[12.5px] text-ink-mut">{grantedUsers.length} 人</span>
          </div>
          {grantedUsers.length ? (
            <table className="tbl">
              <thead><tr><th>用户</th><th>单节点配额</th><th>授权时间</th><th className="text-right">操作</th></tr></thead>
              <tbody>
                {grantedUsers.map(g => (
                  <tr key={g.user_id}>
                    <td className="font-semibold">
                      <Link to={`/users/${g.user_id}`} className="text-blue-600 hover:underline">{g.username}</Link>
                    </td>
                    <td className="font-mono">{g.max_forwards}</td>
                    <td className="text-xs text-ink-mut">{fmtTime(g.granted_at)}</td>
                    <td className="text-right">
                      <button onClick={async () => {
                        if (!await confirm({ title: '取消授权', message: `确定取消 ${g.username} 对该节点的授权？`, confirmText: '取消授权', danger: true })) return
                        try { await api.delete(`/users/${g.user_id}/grants/${node.id}`); toast('已取消授权'); load() }
                        catch (e) { toast(e.message) }
                      }} className="text-red-500 text-xs font-semibold hover:underline cursor-pointer bg-transparent border-0 p-0">取消授权</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <div className="pb-4"><Empty title="尚无用户被授权使用此节点" /></div>}
        </section>

      </div>
    </Layout>
  )
}

function InfoRow({ label, mono, valueClass = '', last, children }) {
  const bb = last ? '' : 'border-b border-line-soft'
  return (
    <>
      <div className={`text-[13px] text-ink-mut pr-5 py-[11px] ${bb}`}>{label}</div>
      <div className={`text-[13.5px] text-ink py-[11px] min-w-0 ${mono ? 'font-mono' : ''} ${valueClass} ${bb}`}>{children}</div>
    </>
  )
}

function ConfigField({ label, hint, children }) {
  return (
    <div>
      <label className="block text-[12.5px] text-ink-soft mb-[7px] font-medium">{label}</label>
      {hint && <p className="mt-[-2px] mb-[7px] text-[11.5px] text-ink-mut leading-snug">{hint}</p>}
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

function CompositeHopsCard({ nodeId, hops, onDone }) {
  const [modes, setModes] = useState(hops.map(h => h.mode))
  const [mults, setMults] = useState(hops.map(h => String(h.traffic_multiplier ?? 1)))
  const [saving, setSaving] = useState(false)
  const toast = useToast()

  const dirty = hops.some((h, i) => h.mode !== modes[i] || String(h.traffic_multiplier ?? 1) !== mults[i])
  const setMode = (i, v) => setModes(m => m.map((x, j) => (j === i ? v : x)))
  const setMult = (i, v) => setMults(prev => prev.map((m, j) => j === i ? v : m))

  const save = async () => {
    setSaving(true)
    try {
      await api.post(`/nodes/${nodeId}/hops`, {
        hops: modes.map((m, i) => ({ mode: m, traffic_multiplier: parseFloat(mults[i]) >= 0 ? parseFloat(mults[i]) : 1 }))
      })
      toast('已保存')
      onDone()
    } catch (err) { toast(err.message) } finally { setSaving(false) }
  }

  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <div className="flex items-baseline gap-2.5 mb-1.5">
        <h2 className="m-0 text-[15px] font-bold">组合节点跳序</h2>
        <span className="text-[12.5px] text-ink-mut">{hops.length} 跳</span>
      </div>
      <p className="text-[12.5px] text-ink-mut mb-2.5">
        内核态支持 TCP / UDP / TCP+UDP；用户态仅支持 TCP（TCP+UDP 中的 UDP 自动走内核态）。修改对此后新建的规则生效。
      </p>
      <table className="tbl">
        <thead><tr><th className="w-10">#</th><th>节点</th><th>模式</th><th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft w-24">倍率</th></tr></thead>
        <tbody>
          {hops.map((h, i) => (
            <tr key={i}>
              <td className="font-mono text-xs text-ink-mut">{i + 1}</td>
              <td className="font-semibold"><Link to={`/nodes/${h.hop_node_id}`} className="text-blue-600 hover:underline">{h.node_name || `#${h.hop_node_id}`}</Link></td>
              <td>
                <Select value={modes[i]} onChange={v => setMode(i, v)} style={{ width: 130 }}
                  options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
              </td>
              <td className="px-3 py-2">
                <input className="input-field font-mono" type="number" min="0" step="0.1"
                  value={mults[i]} onChange={e => setMult(i, e.target.value)}
                  style={{ width: 80 }} title="全局流量计费倍率" />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="pt-3">
        <button onClick={save} disabled={saving || !dirty} className="btn-primary">保存</button>
      </div>
    </section>
  )
}
