import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtTime, fmtBytes, nullStr } from '../../lib/fmt'
import { Layout, useToast, useBlur } from '../../components/Layout'
import { Loading, Empty, Badge, ProtoBadge, ModeBadge, SensText, NodeTypeBadge, NodeTypeIcon, NodeStackBadge, useConfirm, Select } from '../../components/ui'
import { TableBox } from '../../components/page'
import { copyToClipboard } from '../../lib/clipboard'

const card = 'bg-surface border border-line rounded-[14px] shadow-[0_1px_2px_rgba(16,24,40,0.04)]'

function semverLT(a, b) {
  const pa = (a || '').replace(/^v/, '').split('.').map(Number)
  const pb = (b || '').replace(/^v/, '').split('.').map(Number)
  for (let i = 0; i < 3; i++) {
    if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) < (pb[i] || 0)
  }
  return false
}

export default function NodeDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [name, setName] = useState('')
  const [relayHost, setRelayHost] = useState('')
  const [relayHostV6, setRelayHostV6] = useState('')
  const [portRange, setPortRange] = useState('')
  const [rateMult, setRateMult] = useState('1')
  const [unidirectional, setUnidirectional] = useState(false)
  const [noDirectExit, setNoDirectExit] = useState(false)
  const [useGhProxy, setUseGhProxy] = useState(false)
  const [ghProxy, setGhProxy] = useState('https://gh-proxy.com/')
  const [cmdRelayHost, setCmdRelayHost] = useState('')
  const [cmdRelayHostV6, setCmdRelayHostV6] = useState('')
  const toast = useToast()
  const blurred = useBlur()
  const confirm = useConfirm()

  const applyData = (d) => {
    setData(d)
    setName(d.node?.name || '')
    setRelayHost(d.node?.relay_host || '')
    setRelayHostV6(d.node?.relay_host_v6 || '')
    setPortRange(d.node?.port_range || '')
    setRateMult(String(d.node?.rate_multiplier ?? 1))
    setUnidirectional(!!d.node?.unidirectional)
    setNoDirectExit(!!d.node?.no_direct_exit)
  }
  const load = () => {
    setLoading(true)
    api.get(`/nodes/${id}`).then(applyData).catch(console.error).finally(() => setLoading(false))
  }
  // Refresh without the full-page Loading swap: load() unmounts every card,
  // which would wipe in-progress child edits (e.g. binding rows) — the silent
  // path keeps them mounted and just refreshes the data underneath.
  const reloadSilent = () => api.get(`/nodes/${id}`).then(applyData).catch(console.error)
  useEffect(load, [id])

  // 离线的实体节点每 3 秒静默轮询：安装命令跑完、agent 一连上面板，页面即自动
  // 切到在线视图，无需手动刷新。上线后 offline 翻转，定时器随 effect 清理停止；
  // 轮询期间只更新展示数据不动表单，避免覆盖正在编辑的输入。
  const offline = !!data?.node && data.node.node_type !== 'composite' && data.node.online !== 1
  useEffect(() => {
    if (!offline) return
    const t = setInterval(() => {
      api.get(`/nodes/${id}`).then(d => {
        if (d?.node?.online === 1) { applyData(d); toast('节点已上线') }
        else setData(d)
      }).catch(() => { /* 网络抖动，等下一轮 */ })
    }, 3000)
    return () => clearInterval(t)
  }, [offline, id])

  if (loading) return <Layout><Loading /></Layout>
  if (!data) return <Layout><Empty title="节点不存在" /></Layout>

  const { node, panel_url, panel_url_configured, latest_agent_version } = data
  const ruleHops = data.rule_hops || []
  const nodeHops = data.node_hops || []
  const grantedUsers = data.granted_users || []
  const isComposite = node.node_type === 'composite'
  const agentOutdated = node.agent_version && semverLT(node.agent_version, latest_agent_version)
  const up = data.upgrade || { status: 'none' }
  const showUpgrade = up.status !== 'none'

  const saveName = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/rename`, { name }); toast('名称已保存'); load() } catch (err) { toast(err.message, 'error') }
  }
  const saveRelay = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/relay-host`, { relay_host: relayHost }); toast('中继地址已保存'); load() } catch (err) { toast(err.message, 'error') }
  }
  const saveRelayV6 = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/relay-host-v6`, { relay_host_v6: relayHostV6 }); toast('IPv6 中继地址已保存'); load() } catch (err) { toast(err.message, 'error') }
  }
  const savePortRange = async (e) => {
    e.preventDefault()
    try { await api.post(`/nodes/${id}/port-range`, { port_range: portRange }); toast('端口范围已保存'); load() } catch (err) { toast(err.message, 'error') }
  }
  const saveRateMult = async (e) => {
    e.preventDefault()
    const rm = parseFloat(rateMult)
    try { await api.post(`/nodes/${id}/rate-multiplier`, { rate_multiplier: rm >= 0 ? rm : 1 }); toast('倍率已保存'); load() } catch (err) { toast(err.message, 'error') }
  }
  const toggleBillingDir = async () => {
    const next = !unidirectional
    try { await api.post(`/nodes/${id}/unidirectional`, { unidirectional: next }); setUnidirectional(next); toast(next ? '已切换为单向计费' : '已切换为双向计费') } catch (err) { toast(err.message, 'error') }
  }
  const toggleNoDirectExit = async () => {
    const next = !noDirectExit
    try { await api.post(`/nodes/${id}/no-direct-exit`, { no_direct_exit: next }); setNoDirectExit(next); toast(next ? '已禁止直接转发' : '已允许直接转发') } catch (err) { toast(err.message, 'error') }
  }
  const resync = async () => {
    try { await api.post(`/nodes/${id}/resync`); toast('已发起同步') } catch (err) { toast(err.message, 'error') }
  }
  const upgrade = async () => {
    if (!(await confirm({ title: '升级节点', message: '推送 agent 二进制到此节点并重启？', confirmText: '升级' }))) return
    try {
      await api.post(`/nodes/${id}/upgrade`); toast('已发起升级')
      const poll = setInterval(async () => {
        try {
          const d = await api.get(`/nodes/${id}`)
          const st = d?.upgrade?.status
          if (st === 'ok' || st === 'error') {
            clearInterval(poll)
            toast(st === 'ok' ? '升级成功' : '升级失败: ' + (d.upgrade.error || ''), st === 'ok' ? undefined : 'error')
            load()
          }
        } catch { clearInterval(poll) }
      }, 2000)
      setTimeout(() => clearInterval(poll), 120000)
    } catch (err) { toast(err.message, 'error') }
  }
  const toggle = async () => {
    try { await api.post(`/nodes/${id}/toggle`); toast(node.disabled ? '已启用' : '已禁用'); load() } catch (err) { toast(err.message, 'error') }
  }

  const remove = async () => {
    if (!(await confirm({ title: '删除节点', message: `删除节点「${node.name}」？经过它的规则会被重新连接或清除，此操作不可撤销。`, confirmText: '删除', danger: true }))) return
    try { await api.del(`/nodes/${id}`); toast('节点已删除'); navigate('/nodes') } catch (err) { toast(err.message, 'error') }
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
  const relayHostPart = cmdRelayHost.trim() ? ` \\\n  --relay-host ${cmdRelayHost.trim()}` : ''
  const relayHostV6Part = cmdRelayHostV6.trim() ? ` \\\n  --relay-host-v6 ${cmdRelayHostV6.trim()}` : ''
  const installCmd = `curl -fsSL ${proxyPrefix}https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | bash -s agent \\\n  --panel-url ${panel_url} \\\n  --token ${node.secret}${portRangePart}${relayHostPart}${relayHostV6Part}${proxyPrefix ? ` \\\n  --gh-proxy ${proxyPrefix}` : ''}`

  return (
    <Layout>
      <div className="mx-auto max-w-[1160px] flex flex-col gap-[18px]">

        {/* back link */}
        <Link to="/nodes" className="inline-flex items-center gap-1.5 w-fit text-[13.5px] text-ink-mut hover:text-ink transition-colors">
          <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5M12 19l-7-7 7-7" /></svg>
          返回节点列表
        </Link>

        {/* ===== HEADER ===== */}
        <header className={`${card} px-[26px] py-[22px]`}>
          <div className="flex items-start gap-5">
            <div className="w-[52px] h-[52px] flex-none rounded-[14px] grid place-items-center text-blue-600 text-[22px] font-bold hidden md:grid"
              style={{ background: 'linear-gradient(135deg,#eef3ff,#dbe6ff)' }}>⬡</div>
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2.5 flex-wrap">
                <h1 className="m-0 text-[22px] font-bold tracking-[-0.01em]">{node.name}</h1>
                <NodeTypeBadge type={node.node_type} />
                <NodeStackBadge node={node} />
                {isComposite
                  ? (node.disabled && <Badge color="amber">已禁用</Badge>)
                  : <HeaderStatus node={node} />}
              </div>
              {isComposite ? (
                <div className="mt-2 text-[13px] text-ink-mut">虚拟链路节点，由下列各跳依次转发；自身无 Agent / IP / 同步状态</div>
              ) : (
                <div className="mt-2 flex items-center gap-[18px] flex-wrap text-[13px] text-ink-mut">
                  <span>连接 IP&nbsp;&nbsp;{node.address
                    ? <b className="text-ink-soft font-mono font-semibold"><SensText blurred={blurred}>{node.address}</SensText></b>
                    : <span className="text-ink-mut">未连接</span>}</span>
                  <span className="text-[#cfd6df] hidden sm:inline">|</span>
                  <span className="hidden sm:inline">Agent&nbsp;&nbsp;{node.agent_version
                    ? <><span className="font-mono text-ink-soft">{node.agent_version}</span>{agentOutdated && <span className="ml-1"><Badge color="amber">非最新</Badge></span>}</>
                    : <span className="text-ink-mut">未知</span>}</span>
                  <span className="text-[#cfd6df] hidden sm:inline">|</span>
                  <span className="hidden sm:inline">最近同步&nbsp;&nbsp;<b className="text-ink-soft font-semibold">{fmtTime(node.last_apply_at?.Valid ? node.last_apply_at.Int64 : null)}</b></span>
                </div>
              )}
            </div>
          </div>

          {/* header actions */}
          <div className="flex flex-wrap gap-2.5 mt-4 md:mt-3">
            {node.disabled ? (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-[#eafaf1] text-[#0a8a4f] border border-[#c6ecd6] hover:brightness-95 transition cursor-pointer">启用节点</button>
            ) : (
              <button onClick={toggle} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-[#b42318] border border-[#f1c7c2] hover:bg-[#fef3f2] transition-colors cursor-pointer">禁用节点</button>
            )}
            {!isComposite && (
              <button onClick={resync} className="inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-ink-soft border border-[#d7dce3] hover:bg-[#f7f9fc] transition-colors cursor-pointer">重新同步</button>
            )}
            <button onClick={remove} className="hidden md:inline-flex items-center px-3.5 py-[9px] rounded-[10px] text-[13px] font-semibold bg-surface text-[#b42318] border border-[#f1c7c2] hover:bg-[#fef3f2] transition-colors cursor-pointer">删除节点</button>
            {!isComposite && agentOutdated && (
              <button onClick={upgrade} title={`推送升级到 ${latest_agent_version}`} className="hidden md:inline-flex items-center gap-1.5 px-4 py-[9px] rounded-[10px] text-[13px] font-semibold bg-blue-600 text-white hover:bg-blue-700 border-0 cursor-pointer transition-colors max-w-[280px] truncate">⤴ 推送升级到 {latest_agent_version}</button>
            )}
          </div>
        </header>

        {/* ===== 组合节点：仅名称配置（desktop only） ===== */}
        {isComposite ? (
          <section className={`${card} px-[26px] py-[22px] hidden md:flex flex-col gap-[18px]`}>
            <h2 className="m-0 text-[15px] font-bold">节点配置</h2>
            <ConfigField label="节点名称">
              <form onSubmit={saveName} className="flex gap-2 max-w-md">
                <input className="input-field flex-1" value={name} onChange={e => setName(e.target.value)} required />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>
            <ConfigField label="倍率" hint="组合节点自身的计费倍率，作用于整条链路承担的流量">
              <form onSubmit={saveRateMult} className="flex gap-2 max-w-md">
                <input className="input-field font-mono" type="number" min="0" step="0.1" value={rateMult} onChange={e => setRateMult(e.target.value)} style={{ width: 100 }} />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>
            <ConfigField label="直接转发" hint="禁止后本节点不能作为链尾直连目标，规则必须在其后级联线路层；对之后新建/编辑的规则生效">
              <div className="flex items-center gap-2">
                <button onClick={toggleNoDirectExit} className={`inline-flex items-center gap-1.5 px-3.5 py-[7px] rounded-[8px] text-[13px] font-semibold border cursor-pointer transition-colors ${noDirectExit ? 'bg-amber-50 text-amber-700 border-amber-200 hover:bg-amber-100' : 'bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100'}`}>
                  {noDirectExit ? '禁止直接转发' : '允许直接转发'}
                </button>
                <span className="text-xs text-ink-mut">当前：{noDirectExit ? '禁止' : '允许'}</span>
              </div>
            </ConfigField>
          </section>
        ) : (
        /* ===== TWO COLUMN: 基本信息 + 安装命令 | 节点配置 ===== */
        <div className="grid grid-cols-1 lg:grid-cols-[1.55fr_1fr] gap-[18px] items-start">

          <div className="flex flex-col gap-[18px] min-w-0">

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
              <InfoRow label="IPv6 中继地址" mono>
                {node.relay_host_v6 ? <SensText blurred={blurred}>{node.relay_host_v6}</SensText> : <span className="text-ink-mut">未设置</span>}
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
            {nullStr(node.last_error) && (
              <div className="mt-2 text-[12.5px] text-red-700 dark:text-red-400 bg-red-500/[.08] border border-red-500/30 rounded-lg px-3 py-2 break-all">
                {nullStr(node.last_error)}
              </div>
            )}
            {node.last_warning && !nullStr(node.last_error) && (
              <div className="mt-2 text-[12.5px] text-[#b25000] bg-[#fef6ec] border border-[#f6d9ac] rounded-lg px-3 py-2 break-all">
                {node.last_warning}
              </div>
            )}
          </section>

          {/* 安装命令（实体节点专属）— desktop only */}
          {node.node_type !== 'self' && (
          <section className={`${card} px-[26px] py-[22px] hidden md:block`}>
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
            <div className="mb-3 flex items-center gap-2.5 flex-wrap text-[13px]">
              <span className="text-ink-soft">中继地址声明（双出口机器用，可空）</span>
              <input className="input-field font-mono" style={{ height: 30, maxWidth: 220 }} value={cmdRelayHost}
                onChange={e => setCmdRelayHost(e.target.value)} placeholder="IPv4 / 域名（可选）" />
              <input className="input-field font-mono" style={{ height: 30, maxWidth: 220 }} value={cmdRelayHostV6}
                onChange={e => setCmdRelayHostV6(e.target.value)} placeholder="IPv6（可选）" />
            </div>
            <div className="relative bg-[#1e1e2e] dark:bg-app border border-line rounded-[10px] px-5 py-[18px]">
              <button onClick={() => copyToClipboard(installCmd).then(() => toast('已复制')).catch(() => toast('复制失败', 'error'))}
                className="absolute top-3.5 right-3.5 text-[12.5px] font-semibold text-[#a0a4b0] bg-[#2a2a3c] border border-[#3a3a4c] px-3.5 py-[6px] rounded-[7px] cursor-pointer hover:bg-[#33334a] transition-colors">复制</button>
              <pre className="m-0 font-mono text-[13px] leading-[1.75] text-[#e8ecf3] whitespace-pre-wrap break-all">
                <SensText blurred={blurred}>{installCmd}</SensText>
              </pre>
            </div>
          </section>
          )}

          </div>

          {/* 节点配置 — desktop only */}
          <section className={`${card} px-6 py-[22px] flex-col gap-[18px] hidden md:flex`}>
            <h2 className="m-0 text-[15px] font-bold">节点配置</h2>

            <ConfigField label="节点名称">
              <form onSubmit={saveName} className="flex gap-2">
                <input className="input-field flex-1" value={name} onChange={e => setName(e.target.value)} required />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            <ConfigField
              label="中继地址（数据面）"
              hint={node.relay_host_declared ? '由 daemon 启动参数 --relay-host 管理，UI 不可修改；如需变更请更新节点配置后重启 daemon' : '中继链路用它作为上一跳打向本节点的目标地址'}
            >
              <form onSubmit={saveRelay} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHost} onChange={e => setRelayHost(e.target.value)} placeholder="数据面公网 IP 或域名" disabled={node.relay_host_declared} />
                <button type="submit" className="btn-primary flex-none px-5" disabled={node.relay_host_declared}>保存</button>
              </form>
            </ConfigField>

            <ConfigField
              label="IPv6 中继地址"
              hint={node.relay_host_v6_declared ? '由 daemon 启动参数 --relay-host-v6 管理，UI 不可修改；如需变更请更新节点配置后重启 daemon' : '设置后该节点可转发 IPv6 目标，留空表示不支持 IPv6'}
            >
              <form onSubmit={saveRelayV6} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHostV6} onChange={e => setRelayHostV6(e.target.value)} placeholder="数据面公网 IPv6 地址" disabled={node.relay_host_v6_declared} />
                <button type="submit" className="btn-primary flex-none px-5" disabled={node.relay_host_v6_declared}>保存</button>
              </form>
            </ConfigField>

            <ConfigField label="端口范围" hint="规则自动分配监听端口时从该范围中选取">
              <form onSubmit={savePortRange} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={portRange} onChange={e => setPortRange(e.target.value)} placeholder="例如 10001-19999,23333,40000-42000" />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            <ConfigField label="倍率" hint="影响该节点承担流量的计费">
              <form onSubmit={saveRateMult} className="flex gap-2">
                <input className="input-field font-mono" type="number" min="0" step="0.1" value={rateMult} onChange={e => setRateMult(e.target.value)} style={{ width: 100 }} />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            <ConfigField label="计费方向" hint="单向计费只计算出站流量，双向计费计算出站+入站">
              <div className="flex items-center gap-2">
                <button onClick={toggleBillingDir} className={`inline-flex items-center gap-1.5 px-3.5 py-[7px] rounded-[8px] text-[13px] font-semibold border cursor-pointer transition-colors ${unidirectional ? 'bg-amber-50 text-amber-700 border-amber-200 hover:bg-amber-100' : 'bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100'}`}>
                  {unidirectional ? '单向计费（仅出站）' : '双向计费（出站+入站）'}
                </button>
                <span className="text-xs text-ink-mut">当前：{unidirectional ? '单向' : '双向'}</span>
              </div>
            </ConfigField>

            <ConfigField label="直接转发" hint="禁止后本节点不能作为链尾直连目标，规则必须在其后级联线路层；对之后新建/编辑的规则生效">
              <div className="flex items-center gap-2">
                <button onClick={toggleNoDirectExit} className={`inline-flex items-center gap-1.5 px-3.5 py-[7px] rounded-[8px] text-[13px] font-semibold border cursor-pointer transition-colors ${noDirectExit ? 'bg-amber-50 text-amber-700 border-amber-200 hover:bg-amber-100' : 'bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100'}`}>
                  {noDirectExit ? '禁止直接转发' : '允许直接转发'}
                </button>
                <span className="text-xs text-ink-mut">当前：{noDirectExit ? '禁止' : '允许'}</span>
              </div>
            </ConfigField>
          </section>
        </div>
        )}

        {/* ===== 节点角色（含中间层的上游绑定） ===== */}
        <RolesCard node={node} onDone={reloadSilent} />

        {/* ===== 组合节点跳序 — desktop only ===== */}
        {isComposite && (
          <div className="hidden md:block">
            <CompositeHopsCard nodeId={id} hops={nodeHops} singleNodes={data.single_nodes || []} onDone={load} />
          </div>
        )}

        {/* ===== 经过该节点的规则 ===== */}
        <section className={`${card} px-[26px] pt-[22px] pb-2`}>
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h2 className="m-0 text-[15px] font-bold">经过该节点的规则</h2>
            <span className="text-[12.5px] text-ink-mut">{ruleHops.length} 条</span>
          </div>
          {ruleHops.length ? (
            <TableBox>
            <table className="tbl">
              <thead><tr><th>规则</th><th>用户</th><th>类型</th><th>协议</th><th>模式</th><th>监听端口</th><th>目标</th><th className="text-right">流量</th></tr></thead>
              <tbody>
                {ruleHops.map((rh, i) => (
                  <tr key={i}>
                    <td className="font-semibold">
                      {rh.rule_id ? <Link to={`/rules/${rh.rule_id}`} className="text-blue-600 hover:underline">{rh.rule_name || `#${rh.rule_id}`}</Link> : '--'}
                    </td>
                    <td className="text-sm">
                      {rh.owner_id ? <Link to={`/users/${rh.owner_id}`} className="text-blue-600 hover:underline">{rh.owner_name}</Link> : <span className="text-ink-mut">--</span>}
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
            </TableBox>
          ) : <div className="pb-4"><Empty title="该节点尚无规则经过"><Link to="/rules" className="text-blue-600 text-xs font-semibold">去添加</Link></Empty></div>}
        </section>

        {/* ===== 已授权用户 ===== */}
        <section className={`${card} px-[26px] pt-[22px] pb-2`}>
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h2 className="m-0 text-[15px] font-bold">已授权用户</h2>
            <span className="text-[12.5px] text-ink-mut">{grantedUsers.length} 人</span>
          </div>
          <GrantEditor nodeId={node.id} grantedUsers={grantedUsers} onChanged={load} />
          {grantedUsers.length ? (
            <TableBox>
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
                        try { await api.del(`/users/${g.user_id}/grants/${node.id}`); toast('已取消授权'); load() }
                        catch (e) { toast(e.message, 'error') }
                      }} className="text-red-500 text-xs font-semibold hover:underline cursor-pointer bg-transparent border-0 p-0">取消授权</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            </TableBox>
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
  } else if (node.last_warning) {
    text = '警告'; cls = 'text-[#b25000] bg-[#fef6ec] border-[#f6d9ac]'; dot = '#e0892f'
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

/* 多选下拉直接编辑该节点的授权集合：默认选中已授权用户，保存时按 diff
   新增走 grant（默认转发上限同用户页）、移除走 revoke。移除会让该用户在此
   节点的规则失去授权依据，所以先确认。 */
function GrantEditor({ nodeId, grantedUsers, onChanged }) {
  const [allUsers, setAllUsers] = useState(null)
  const [sel, setSel] = useState(grantedUsers.map(g => String(g.user_id)))
  const [saving, setSaving] = useState(false)
  const toast = useToast()
  const confirm = useConfirm()

  // 只在授权集合真实变化时重置选择，静默轮询刷新不打断编辑中的下拉。
  const grantedKey = grantedUsers.map(g => g.user_id).sort((a, b) => a - b).join(',')
  useEffect(() => { setSel(grantedUsers.map(g => String(g.user_id))) }, [grantedKey])
  useEffect(() => {
    api.get('/users').then(d => setAllUsers((d.users || []).filter(u => u.role === 'user'))).catch(() => setAllUsers([]))
  }, [])

  // 已授权但不在用户列表里的（角色变更等）也要可见，否则无法取消勾选。
  const byId = new Map((allUsers || []).map(u => [String(u.id), u.username]))
  for (const g of grantedUsers) if (!byId.has(String(g.user_id))) byId.set(String(g.user_id), g.username)
  const options = [...byId].map(([value, label]) => ({ value, label }))

  const grantedIds = new Set(grantedUsers.map(g => String(g.user_id)))
  const added = sel.filter(v => !grantedIds.has(v))
  const removed = [...grantedIds].filter(v => !sel.includes(v))
  const dirty = added.length > 0 || removed.length > 0

  const save = async () => {
    if (removed.length && !(await confirm({
      title: '取消授权',
      message: `将取消 ${removed.length} 名用户对该节点的授权，其在此节点的规则会失去授权依据。继续？`,
      confirmText: '继续', danger: true,
    }))) return
    setSaving(true)
    try {
      for (const uid of added) await api.post(`/users/${uid}/grants`, { node_id: Number(nodeId) })
      for (const uid of removed) await api.del(`/users/${uid}/grants/${nodeId}`)
      toast('授权已更新')
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false); onChanged() }
  }

  return (
    <div className="flex items-center gap-2 mb-3 max-w-xl">
      <Select multiple searchable className="flex-1" placeholder="选择要授权的用户…"
        value={sel} onChange={setSel} options={options} />
      <button onClick={save} disabled={saving || !dirty} className="btn-primary flex-none px-5">保存</button>
    </div>
  )
}

function CompositeHopsCard({ nodeId, hops: initHops, singleNodes, onDone }) {
  const [rows, setRows] = useState(initHops.map(h => ({
    node_id: h.hop_node_id, node_name: h.node_name || `#${h.hop_node_id}`,
    mode: h.mode,
  })))
  const [saving, setSaving] = useState(false)
  const [dragIdx, setDragIdx] = useState(null)
  const toast = useToast()

  const nodeById = Object.fromEntries(singleNodes.map(n => [n.id, n]))
  const dirty = rows.length !== initHops.length || rows.some((r, i) => {
    const h = initHops[i]
    return !h || r.node_id !== h.hop_node_id || r.mode !== h.mode
  })

  const setField = (i, k, v) => setRows(rs => rs.map((r, j) => j === i ? { ...r, [k]: v } : r))
  const addHop = () => setRows(rs => [...rs, { node_id: '', node_name: '', mode: 'userspace' }])
  const removeHop = (i) => setRows(rs => rs.filter((_, j) => j !== i))
  const onDrop = (toIdx) => {
    if (dragIdx === null || dragIdx === toIdx) { setDragIdx(null); return }
    setRows(rs => {
      const arr = [...rs]
      const [moved] = arr.splice(dragIdx, 1)
      arr.splice(toIdx, 0, moved)
      return arr
    })
    setDragIdx(null)
  }

  const save = async () => {
    if (rows.length < 2) { toast('至少需要 2 个子节点', 'error'); return }
    if (rows.some(r => !r.node_id)) { toast('请选择所有节点', 'error'); return }
    setSaving(true)
    try {
      await api.post(`/nodes/${nodeId}/hops`, {
        hops: rows.map(r => ({ node_id: Number(r.node_id), mode: r.mode }))
      })
      toast('已保存')
      onDone()
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false) }
  }

  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <div className="flex items-baseline gap-2.5 mb-1.5">
        <h2 className="m-0 text-[15px] font-bold">组合节点跳序</h2>
        <span className="text-[12.5px] text-ink-mut">{rows.length} 跳</span>
      </div>
      <p className="text-[12.5px] text-ink-mut mb-2.5">
        拖拽 ⠿ 调整顺序。模式作用于该跳到下一跳之间的段（内核态支持 TCP / UDP / TCP+UDP；用户态仅支持 TCP）；
        出口段（最后一跳 → 目标）的模式在规则上选择。修改对此后新建的规则生效。
      </p>
      <div className="space-y-2">
        {rows.map((r, i) => (
          <div key={i}
            draggable onDragStart={() => setDragIdx(i)} onDragOver={e => e.preventDefault()} onDrop={() => onDrop(i)}
            className={`flex items-center gap-2 bg-raised rounded-lg px-3 py-2 ${dragIdx === i ? 'opacity-50' : ''}`}>
            <span className="text-ink-mut cursor-move select-none" title="拖拽排序">⠿</span>
            <span className="text-xs text-ink-mut w-5 text-center font-mono">{i + 1}</span>
            <Select className="flex-1" placeholder="-- 选择节点 --" searchable value={r.node_id}
              onChange={v => {
                const nd = nodeById[v]
                setField(i, 'node_id', v)
                if (nd) setField(i, 'node_name', nd.name)
              }}
              options={singleNodes.filter(n => n.id === Number(r.node_id) || !rows.some((rr, j) => j !== i && Number(rr.node_id) === n.id)).map(n => ({ value: n.id, label: n.name }))} />
            {/* 尾行没有模式可配：出口段的模式归规则所有；这行存的 mode 只在
                之后被重排成中间跳时才重新生效 */}
            {i === rows.length - 1 ? (
              <span className="text-xs text-ink-mut text-center shrink-0 cursor-help" style={{ width: 120 }} title="出口段（最后一跳 → 目标）的转发模式在规则上选择">-</span>
            ) : (
              <Select value={r.mode} onChange={v => setField(i, 'mode', v)} style={{ width: 120 }}
                options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
            )}
            {rows.length > 2 && (
              <button type="button" onClick={() => removeHop(i)} className="btn-danger-sm text-xs px-1.5">×</button>
            )}
          </div>
        ))}
      </div>
      <div className="flex items-center gap-3 pt-3">
        <button type="button" onClick={addHop} className="btn-secondary text-xs">+ 添加一跳</button>
        <button onClick={save} disabled={saving || !dirty} className="btn-primary">保存</button>
      </div>
    </section>
  )
}

// A node's role bitmask controls where it can appear in a rule chain: entry
// (bit0) lets a rule start here, via (bit1) lets other nodes bind behind it as
// a middle layer. Both bits can be set at once; at least one must stay set or
// the node becomes unreachable from any rule.
//
// Bindings are edges of the middle-layer graph: this node (downstream) lists
// which nodes it may sit behind. Checking 中间层 expands the bindings editor
// inline so roles and bindings can be configured together with one save; the
// save posts roles first because the bindings endpoint rejects nodes whose
// stored roles lack the via bit. Unchecking only hides the editor — edited
// rows stay in memory and stored bindings stay in the DB — so re-checking
// restores what was there.
function RolesCard({ node, onDone }) {
  const [roles, setRoles] = useState(node.roles ?? 1)
  const [saving, setSaving] = useState(false)
  // rows === null means bindings not fetched yet; they load lazily on first
  // expansion so entry-only nodes never pay the extra requests. savedRows
  // mirrors the server state for dirty tracking.
  const [rows, setRows] = useState(null) // [{upstream_node_id, mode}]
  const [savedRows, setSavedRows] = useState(null)
  const [loadErr, setLoadErr] = useState(false)
  const [allNodes, setAllNodes] = useState([])
  // Edges where this node is the upstream: which nodes may cascade in behind
  // it. Read-only here — each edge is owned and edited by its downstream
  // node's detail page — so a load failure can safely degrade to "none".
  const [downstreams, setDownstreams] = useState([])
  const toast = useToast()
  // NodeDetail stays mounted when only the :id param changes (e.g. following a
  // composite link in the rules table), so mount-time seeding alone would keep
  // the previous node's checkboxes and save its bitmask onto the new node.
  useEffect(() => setRoles(node.roles ?? 1), [node.id, node.roles])
  useEffect(() => { setRows(null); setSavedRows(null); setLoadErr(false) }, [node.id])
  useEffect(() => {
    let stale = false
    setDownstreams([])
    api.get('/node-bindings')
      .then(d => { if (!stale) setDownstreams((d.bindings || []).filter(b => b.upstream_node_id === node.id)) })
      .catch(() => { /* 只读展示，失败按无下游处理 */ })
    return () => { stale = true }
  }, [node.id])

  const viaChecked = (roles & 2) !== 0
  // A failed load must not seed an empty editor: saving replaces the whole
  // binding set server-side, so treating an error as "0 条" would let one
  // transient blip wipe every stored binding on the next save. Keep rows null
  // (nothing to save) and offer a retry instead. The stale flag drops responses
  // from a superseded run — un/re-checking 中间层 quickly can leave two fetches
  // in flight, and the older one must not clobber rows the user already edited.
  useEffect(() => {
    if (!viaChecked || rows !== null || loadErr) return
    let stale = false
    api.get(`/nodes/${node.id}/bindings`)
      .then(d => {
        if (stale) return
        const rs = (d.bindings || []).map(b => ({ upstream_node_id: b.upstream_node_id, mode: b.mode }))
        setRows(rs); setSavedRows(rs)
      })
      .catch(() => { if (!stale) setLoadErr(true) })
    return () => { stale = true }
  }, [viaChecked, rows, node.id, loadErr])
  // The candidate list is the full node roster fetched here rather than reused
  // from the composite hop picker, since that picker is scoped to single nodes
  // only and would silently drop composite upstreams from the choices. The
  // read-only downstream list needs the same roster for names, so it shares
  // this fetch; entry-only nodes with no downstream still skip it entirely.
  const needNodeNames = viaChecked || downstreams.length > 0
  useEffect(() => {
    if (!needNodeNames) return
    let stale = false
    api.get('/nodes').then(d => { if (!stale) setAllNodes(d.nodes || []) }).catch(() => { if (!stale) setAllNodes([]) })
    return () => { stale = true }
  }, [needNodeNames, node.id])

  const toggle = (bit) => setRoles(r => r ^ bit)
  const candidates = allNodes.filter(n => n.id !== node.id)
  const nodeById = Object.fromEntries(allNodes.map(n => [n.id, n]))
  // The multi-select owns which upstreams are bound; each picked upstream then
  // gets its own mode row. Reconciling instead of rebuilding keeps the mode a
  // row already carries when the selection set changes around it.
  const pickUpstreams = (next) => setRows(rs => {
    const keep = rs.filter(r => next.includes(String(r.upstream_node_id)))
    const have = new Set(keep.map(r => String(r.upstream_node_id)))
    const added = next.filter(v => !have.has(v)).map(v => ({ upstream_node_id: Number(v), mode: 'userspace' }))
    return [...keep, ...added]
  })
  const setRowMode = (i, v) => setRows(rs => rs.map((r, j) => j === i ? { ...r, mode: v } : r))
  const removeRow = (i) => setRows(rs => rs.filter((_, j) => j !== i))
  const addAll = () => setRows(rs => candidates.map(n =>
    rs.find(r => Number(r.upstream_node_id) === n.id) || { upstream_node_id: n.id, mode: 'userspace' }))

  const rolesDirty = roles !== (node.roles ?? 1)
  // Hidden edits are not dirty: with 中间层 unchecked they wouldn't be saved,
  // so they shouldn't light up the save button either.
  const bindingsDirty = viaChecked && rows !== null && (
    rows.length !== savedRows.length ||
    rows.some((r, i) => {
      const s = savedRows[i]
      return !s || Number(r.upstream_node_id) !== Number(s.upstream_node_id) || r.mode !== s.mode
    })
  )
  const dirty = rolesDirty || bindingsDirty

  // onDone runs on failure too: the two POSTs are not a transaction, so roles
  // may land while bindings error out. Refetching keeps node.roles — the dirty
  // baseline — in sync with what was actually persisted; otherwise the save
  // button can go dead while UI and server disagree. onDone must be the silent
  // refresh: a full reload would remount this card and wipe the edited rows,
  // which are exactly what the user needs to keep for a retry.
  const save = async () => {
    if (!roles) { toast('至少保留一个角色', 'error'); return }
    setSaving(true)
    try {
      if (rolesDirty) await api.post(`/nodes/${node.id}/roles`, { roles })
      if (bindingsDirty) {
        const bs = rows.map(r => ({ upstream_node_id: Number(r.upstream_node_id), mode: r.mode }))
        await api.post(`/nodes/${node.id}/bindings`, { bindings: bs })
        setSavedRows(bs)
      }
      toast('已保存')
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false); onDone() }
  }

  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <h2 className="m-0 text-[15px] font-bold mb-1.5">节点角色</h2>
      <p className="text-[12.5px] text-ink-mut mb-2.5">
        入口：可被规则选为入口。中间层：可绑定到上游节点之后，供规则级联选用。可同时勾选。
      </p>
      <div className="flex items-center gap-1.5">
        {[[1, '入口', 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'],
          [2, '中间层', 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700']].map(([bit, label, cls]) => (
          <button key={bit} type="button" onClick={() => toggle(bit)}
            className={`px-3 py-1 text-[12.5px] font-semibold rounded-md border transition-colors ${
              (roles & bit) !== 0 ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
            }`}>{label}</button>
        ))}
        <button onClick={save} disabled={saving || !dirty} className="btn-primary ml-auto">保存</button>
      </div>
      {viaChecked && (
        <div className="mt-4 pt-3.5 border-t border-line-soft">
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h3 className="m-0 text-[13.5px] font-bold">上游绑定</h3>
            <span className="text-[12.5px] text-ink-mut">{rows ? `${rows.length} 条` : loadErr ? '' : '加载中…'}</span>
          </div>
          <p className="text-[12.5px] text-ink-mut mb-2.5">
            绑定后，选中这些上游（入口或中间层）的规则可以级联接入本节点。模式作用于衔接段
            （上游段尾跳 → 本层首跳）；修改对此后新建的规则生效。与角色一起点「保存」生效。
          </p>
          {loadErr && (
            <div className="text-[12.5px] text-red-600 flex items-center gap-2.5">
              已有绑定加载失败，为避免误覆盖，修好前不会保存绑定。
              <button type="button" onClick={() => setLoadErr(false)} className="btn-secondary text-xs">重试</button>
            </div>
          )}
          {rows && (
            <>
              <div className="flex items-center gap-2 max-w-xl mb-2">
                <Select multiple searchable className="flex-1" placeholder="选择上游节点…"
                  value={rows.map(r => String(r.upstream_node_id))} onChange={pickUpstreams}
                  options={candidates.map(n => ({ value: n.id, label: n.name, icon: <NodeTypeIcon type={n.node_type} /> }))} />
                <button type="button" onClick={addAll} className="btn-secondary flex-none text-xs">全选</button>
              </div>
              {rows.length > 0 && (
                <div className="space-y-2">
                  {rows.map((r, i) => {
                    const n = nodeById[Number(r.upstream_node_id)]
                    return (
                      <div key={r.upstream_node_id} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
                        <NodeTypeIcon type={n?.node_type} />
                        <span className="flex-1 min-w-0 truncate text-[13.5px]">{n?.name || `#${r.upstream_node_id}`}</span>
                        <Select value={r.mode} onChange={v => setRowMode(i, v)} style={{ width: 120 }}
                          options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
                        <button type="button" onClick={() => removeRow(i)} className="btn-danger-sm text-xs px-1.5">×</button>
                      </div>
                    )
                  })}
                </div>
              )}
            </>
          )}
        </div>
      )}
      {downstreams.length > 0 && (
        <div className="mt-4 pt-3.5 border-t border-line-soft">
          <div className="flex items-baseline gap-2.5 mb-1.5">
            <h3 className="m-0 text-[13.5px] font-bold">已绑定的下游</h3>
            <span className="text-[12.5px] text-ink-mut">{downstreams.length} 条</span>
          </div>
          <p className="text-[12.5px] text-ink-mut mb-2.5">
            这些节点把本节点设为上游，选中本节点的规则可以级联接入它们。绑定关系在对应下游节点的详情页编辑。
          </p>
          <div className="space-y-2 max-w-xl">
            {downstreams.map(b => {
              const n = nodeById[Number(b.downstream_node_id)]
              return (
                <div key={b.downstream_node_id} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
                  <NodeTypeIcon type={n?.node_type} />
                  <Link to={`/nodes/${b.downstream_node_id}`} className="flex-1 min-w-0 truncate text-[13.5px] text-blue-600 no-underline hover:underline">
                    {n?.name || `#${b.downstream_node_id}`}
                  </Link>
                  <ModeBadge mode={b.mode} />
                </div>
              )
            })}
          </div>
        </div>
      )}
    </section>
  )
}
