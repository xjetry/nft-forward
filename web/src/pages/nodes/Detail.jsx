import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes, fmtTime, nullStr } from '../../lib/fmt'
import { assertNever, combineResources } from '../../lib/resource'
import { upgradeVerdict, upgradeWorthShowing } from '../../lib/upgrade'
import { useDraft } from '../../lib/useDraft'
import { useResource } from '../../lib/useResource'
import { ModeTag, NodeStackTags, NodeStatus, NodeTypeIcon, NodeTypeTag, ProtoTag } from '../../components/net'
import { Page, PageHeader, TableBox } from '../../components/page'
import { PerNodeRolesForm } from '../../components/PerNodeRolesForm'
import { useBlur } from '../../components/prefs'
import { ResourceView } from '../../components/ResourceView'
import {
  Alert, Badge, Button, Card, CardHeader, CardSection, Checkbox, CodeBlock, DescItem, DescList, Empty, Field,
  IconButton, IconRefresh, IconTrash, IconX, Input, Segmented, Select, SensText, TextLink, useAction, useConfirm,
  useToast,
} from '../../components/ui'
import {
  GrantUserSelect, HopListEditor, MODE_OPTIONS, ROLE_ENTRY, ROLE_VIA, hopRowsError, hopRowsPayload, removeNode,
  useGrantableUsers,
} from './common'

/* ==========================================================================
   详情页
   ========================================================================== */

const BACK = { to: '/nodes', label: '节点' }

/* 离线节点的刷新间隔：安装命令跑完、Agent 一连上面板，页面就切到在线视图。 */
const OFFLINE_POLL_MS = 3000
const UPGRADE_POLL_MS = 2000
/* 等待升级结论的上限。服务端在推送被确认后的 90 秒内报 pending，之后改报
   stuck（internal/server/upgrade_status.go 的 upgradeGrace），所以轮询只要
   拿得到响应，结论由服务端给出；这个上限只在迟迟拿不到响应时才会先到。 */
const UPGRADE_WAIT_MS = 120000

/**
 * `ms` 为数字期间反复调用 `tick`：上一次落定之后才开始计下一次的间隔，所以
 * 响应比间隔慢时请求不会叠在一起。`ms` 为 null 时停止；卸载或 `ms` 变化时
 * 清掉定时器，已经在途的那一次落定后不再续下一次。
 *
 * 前提：`tick` 不抛错（资源的 reload 把失败记进资源状态，自身不抛）。
 */
function usePolling(tick, ms) {
  const latest = useRef(tick)
  latest.current = tick
  useEffect(() => {
    if (ms == null) return
    let stopped = false
    let timer
    const schedule = () => {
      timer = setTimeout(async () => {
        await latest.current()
        if (!stopped) schedule()
      }, ms)
    }
    schedule()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [ms])
}

export default function NodeDetail() {
  const { id } = useParams()
  const resource = useResource(() => api.get(`/nodes/${id}`), [id])
  return (
    <Page>
      {/* 还没有节点数据时（加载中、失败、不存在）也留一条回列表的路。 */}
      {resource.state.status !== 'ready' && <PageHeader title="节点详情" back={BACK} />}
      <ResourceView resource={resource} notFound="节点不存在">
        {data => <NodeView data={data} resource={resource} />}
      </ResourceView>
    </Page>
  )
}

function NodeView({ data, resource }) {
  const { state, reload } = resource
  const { node, latest_agent_version: latest } = data
  const navigate = useNavigate()
  const toast = useToast()
  const confirm = useConfirm()
  const composite = node.node_type === 'composite'
  // Agent 版本是否低于面板版本，由服务端判定（节点列表的“可升级”标记与批量
  // 升级选对象用的是同一个判定）。
  const outdated = data.agent_outdated === true
  // 面板地址没有配置时，服务端返回的 panel_url 是空串；安装命令改用管理员
  // 当前访问面板的地址，并在安装命令卡片里说明。
  const panelUrl = data.panel_url_configured ? data.panel_url : window.location.origin

  // 组合节点没有自己的 Agent，在线状态由服务端按子节点聚合，不存在“等它连上来”。
  const offline = !composite && node.online !== 1
  // 一次被节点确认的升级推送之后，等服务端给出结论的阶段：
  // null 不在等；'waiting' 在等；'expired' 等到上限仍没有结论。
  const [upgradeWatch, setUpgradeWatch] = useState(/** @type {null | 'waiting' | 'expired'} */ (null))
  usePolling(reload, upgradeWatch ? UPGRADE_POLL_MS : offline ? OFFLINE_POLL_MS : null)

  // 升级会让 Agent 重启、节点短暂离线再上线；那一次上线由升级结论的提示说明，
  // 不再另报“已上线”。
  const wasOffline = useRef(offline)
  useEffect(() => {
    if (wasOffline.current && !offline && !upgradeWatch) toast('节点已上线')
    wasOffline.current = offline
  }, [offline, upgradeWatch, toast])

  useEffect(() => {
    if (upgradeWatch !== 'waiting') return
    const timer = setTimeout(() => setUpgradeWatch(w => (w === 'waiting' ? 'expired' : w)), UPGRADE_WAIT_MS)
    return () => clearTimeout(timer)
  }, [upgradeWatch])

  // 屏幕上的 data 是否来自一次已经落定且成功的拉取。开始等待升级结论的同一刻
  // 发起了一次刷新（见 upgrade），此后凡是 settled 的 data 都是推送之后取回的；
  // 刷新在途或失败时 data 仍可能是推送之前的，不能拿来下结论。
  const settled = state.status === 'ready' && !state.refreshing && state.refreshError === null
  useEffect(() => {
    if (!upgradeWatch) return
    const verdict = settled ? upgradeVerdict(data.upgrade?.status) : 'running'
    if (verdict === 'running' && upgradeWatch === 'waiting') return
    // 结束等待与提示只发生在这里，每次等待因此恰好给出一个结论。
    setUpgradeWatch(null)
    switch (verdict) {
      case 'succeeded':
        toast(`升级完成，Agent 现在是 ${node.agent_version}`)
        break
      case 'failed':
        toast(`升级失败：${data.upgrade.error || '节点没有给出原因'}`, 'error')
        break
      case 'stuck':
        toast('节点已确认接收新版本，但上报的版本没有变化，可能是重启失败', 'error')
        break
      case 'running':
        toast('2 分钟内没有取得升级结果，请稍后刷新页面查看', 'error')
        break
      default:
        assertNever(verdict)
    }
  }, [upgradeWatch, settled, data, node.agent_version, toast])

  // 启用 / 禁用在服务端是翻转语义：等刷新回来再放开按钮，按钮上的文字与
  // 服务端状态一致之后才允许下一次点击。
  const toggle = useAction(async () => {
    await api.post(`/nodes/${node.id}/toggle`)
    toast(node.disabled ? '节点已启用' : '节点已禁用')
    await reload()
  })

  const resync = useAction(async () => {
    try {
      await api.post(`/nodes/${node.id}/resync`)
      toast('已重新同步')
    } finally {
      // 下发失败时服务端也会把错误记到节点上，所以成败都刷新。
      await reload()
    }
  })

  const upgrade = useAction(async () => {
    const agreed = await confirm({
      title: '升级节点',
      message: `把「${node.name}」的 Agent 升级到 ${latest}？面板会把新版本推送到节点，节点替换程序后重启 Agent。`,
      confirmText: '升级',
    })
    if (!agreed) return
    try {
      await api.post(`/nodes/${node.id}/upgrade`)
    } catch (err) {
      // 推送失败也会被服务端记成一次升级记录，刷新后“升级状态”一行才看得到。
      await reload()
      throw err
    }
    toast('升级已推送，正在等待节点重启')
    // 先发起刷新、再开始等待：等待期间的结论判定只认这次刷新及之后取回的数据。
    const refreshed = reload()
    setUpgradeWatch('waiting')
    await refreshed
  })

  const remove = useAction(async () => {
    if (!(await removeNode(node.id, confirm))) return
    toast('节点已删除')
    navigate('/nodes')
  })

  const ruleHops = data.rule_hops || []
  const referencedBy = data.referenced_by_composites || []

  return (
    <>
      <PageHeader
        title={node.name}
        back={BACK}
        meta={<>
          <NodeTypeTag type={node.node_type} />
          <NodeStackTags node={node} />
          <NodeStatus node={node} />
        </>}
        actions={<>
          <Button loading={toggle.pending} onClick={() => toggle.run()}>{node.disabled ? '启用节点' : '禁用节点'}</Button>
          {!composite && <Button icon={<IconRefresh />} loading={resync.pending} onClick={() => resync.run()}>重新同步</Button>}
          <Button danger icon={<IconTrash />} loading={remove.pending} onClick={() => remove.run()}>删除节点</Button>
          {outdated && (
            <Button variant="primary" loading={upgrade.pending || upgradeWatch !== null} onClick={() => upgrade.run()}>
              升级到 {latest}
            </Button>
          )}
        </>}
      />

      {composite ? (
        <>
          <Alert>组合节点是一条虚拟链路，流量依次经过下面的各跳；它自己没有 Agent、连接 IP 和同步状态。</Alert>
          <ConfigCard node={node} reload={reload} />
        </>
      ) : (
        <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
          <div className="flex min-w-0 flex-col gap-5">
            <InfoCard node={node} upgrade={data.upgrade} outdated={outdated} />
            {node.node_type !== 'self' && (
              <InstallCommandCard node={node} panelUrl={panelUrl} panelUrlConfigured={data.panel_url_configured} />
            )}
          </div>
          <ConfigCard node={node} reload={reload} />
        </div>
      )}

      <RolesCard node={node} reload={reload} />

      {composite && (
        <CompositeHopsCard nodeId={node.id} hops={data.node_hops || []} candidates={data.single_nodes || []} reload={reload} />
      )}

      {referencedBy.length > 0 && (
        <Card>
          <CardHeader title="所属组合" description={`被 ${referencedBy.length} 个组合节点用作成员`} />
          <CardSection className="flex flex-wrap gap-x-5 gap-y-2">
            {referencedBy.map(c => (
              <TextLink key={c.id} to={`/nodes/${c.id}`}>
                <NodeTypeIcon type={c.node_type} />
                {c.name}
              </TextLink>
            ))}
          </CardSection>
        </Card>
      )}

      <RuleHopsCard ruleHops={ruleHops} />
      <GrantsCard node={node} grantedUsers={data.granted_users || []} reload={reload} />
    </>
  )
}

/* ---------- 基本信息 ---------- */

function Unset({ children }) {
  return <span className="text-ink-mut">{children}</span>
}

function UpgradeStatus({ upgrade, node }) {
  switch (upgrade.status) {
    case 'ok':
      return <><Badge tone="ok">升级成功</Badge> <span className="text-ink-mut">{upgrade.version} · {fmtTime(upgrade.at)}</span></>
    case 'error':
      return <><Badge tone="danger">升级失败</Badge> <span className="text-ink-soft">{upgrade.error}</span></>
    case 'pending':
      return <><Badge tone="accent">升级中</Badge> <span className="text-ink-mut">已推送 {upgrade.version} · {fmtTime(upgrade.at)}</span></>
    case 'stuck':
      return (
        <>
          <Badge tone="warn">可能未生效</Badge>{' '}
          <span className="text-ink-soft">
            节点已确认接收 {upgrade.version}（{fmtTime(upgrade.at)}），但当前仍是 {node.agent_version || '未知版本'}，可能是重启失败
          </span>
        </>
      )
    default:
      return null
  }
}

function InfoCard({ node, upgrade, outdated }) {
  const blurred = useBlur()
  const error = nullStr(node.last_error)
  const showUpgrade = upgradeWorthShowing(upgrade?.status)
  return (
    <Card>
      <CardHeader title="基本信息" />
      <CardSection className="flex flex-col gap-4">
        <DescList>
          <DescItem label="连接 IP">
            {node.address ? <span className="font-mono"><SensText blurred={blurred}>{node.address}</SensText></span> : <Unset>未连接</Unset>}
          </DescItem>
          <DescItem label="中继地址">
            {node.relay_host
              ? <span className="font-mono"><SensText blurred={blurred}>{node.relay_host}</SensText></span>
              : <Unset>未设置（设置后才能进入链路）</Unset>}
          </DescItem>
          <DescItem label="IPv6 中继地址">
            {node.relay_host_v6 ? <span className="font-mono"><SensText blurred={blurred}>{node.relay_host_v6}</SensText></span> : <Unset>未设置</Unset>}
          </DescItem>
          <DescItem label="Token">
            <span className="break-all font-mono"><SensText blurred={blurred}>{node.secret}</SensText></span>
          </DescItem>
          <DescItem label="最近心跳">
            {fmtTime(node.last_seen ?? null)} <Unset>· {node.online === 1 ? 'Agent 已连接' : 'Agent 未连接'}</Unset>
          </DescItem>
          <DescItem label="最近同步">{fmtTime(node.last_apply_at?.Valid ? node.last_apply_at.Int64 : null)}</DescItem>
          <DescItem label="Agent 版本">
            {node.agent_version
              ? <><span className="font-mono">{node.agent_version}</span> {outdated ? <Badge tone="warn">非最新</Badge> : <Badge tone="ok">最新</Badge>}</>
              : <Unset>未知</Unset>}
          </DescItem>
          {showUpgrade && <DescItem label="升级状态"><UpgradeStatus upgrade={upgrade} node={node} /></DescItem>}
        </DescList>
        {error && <Alert tone="danger" className="break-all">{error}</Alert>}
        {!error && node.last_warning && <Alert tone="warn" className="break-all">{node.last_warning}</Alert>}
      </CardSection>
    </Card>
  )
}

/* ---------- 安装命令 ---------- */

/* 节点默认的端口范围；与它相同就不必写进安装命令。 */
const DEFAULT_PORT_RANGE = '10001-20000'
const INSTALL_SCRIPT = 'https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh'

function InstallCommandCard({ node, panelUrl, panelUrlConfigured }) {
  const blurred = useBlur()
  const [useGhProxy, setUseGhProxy] = useState(false)
  const [ghProxy, setGhProxy] = useState('https://gh-proxy.com/')
  const [relayHost, setRelayHost] = useState('')
  const [relayHostV6, setRelayHostV6] = useState('')

  // gh-proxy 是一个 URL 前缀：既加在 install.sh 的下载地址前，也通过
  // --gh-proxy 传给脚本，让脚本下载二进制时同样走这个镜像。
  const proxy = useGhProxy ? ghProxy.trim() : ''
  const proxyPrefix = proxy && !proxy.endsWith('/') ? `${proxy}/` : proxy
  const args = [
    `--panel-url ${panelUrl}`,
    `--token ${node.secret}`,
    node.port_range && node.port_range !== DEFAULT_PORT_RANGE && `--port-range ${node.port_range}`,
    relayHost.trim() && `--relay-host ${relayHost.trim()}`,
    relayHostV6.trim() && `--relay-host-v6 ${relayHostV6.trim()}`,
    proxyPrefix && `--gh-proxy ${proxyPrefix}`,
  ].filter(Boolean)
  const command = [`curl -fsSL ${proxyPrefix}${INSTALL_SCRIPT} | bash -s agent`, ...args].join(' \\\n  ')

  return (
    <Card>
      <CardHeader title="安装命令" description="在目标机器上以 root 执行（需要已安装 nftables）" />
      <CardSection className="flex flex-col gap-4">
        {!panelUrlConfigured && (
          <Alert tone="warn">
            还没有设置面板地址，命令里用的是你当前访问面板的地址 <span className="font-mono">{panelUrl}</span>。
            如果节点要通过别的地址连接面板，先到<TextLink to="/settings">系统设置</TextLink>填写面板地址，再复制命令。
          </Alert>
        )}
        <div className="flex flex-col gap-2">
          <Checkbox checked={useGhProxy} onChange={setUseGhProxy}>通过 gh-proxy 下载（目标机器访问 GitHub 受限时）</Checkbox>
          {useGhProxy && (
            <Input mono value={ghProxy} onChange={setGhProxy} placeholder="https://gh-proxy.com/" aria-label="gh-proxy 地址" className="sm:max-w-sm" />
          )}
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="声明中继地址" optional hint="双出口机器用。写进命令后，中继地址由节点的启动参数决定，面板里不能再修改。">
            <Input mono value={relayHost} onChange={setRelayHost} placeholder="IPv4 或域名" />
          </Field>
          <Field label="声明 IPv6 中继地址" optional>
            <Input mono value={relayHostV6} onChange={setRelayHostV6} placeholder="IPv6 地址" />
          </Field>
        </div>
        <CodeBlock text={command}><SensText blurred={blurred}>{command}</SensText></CodeBlock>
      </CardSection>
    </Card>
  )
}

/* ---------- 节点配置 ---------- */

const trimmed = (text) => text.trim()

/** 倍率输入整理成数字的规范写法（“1.50” → “1.5”），与服务端存下的值可比；不是数字时原样返回，交给校验报错。 */
function canonRate(text) {
  const t = text.trim()
  return t !== '' && Number.isFinite(Number(t)) ? String(Number(t)) : t
}

function rateError(value) {
  return value !== '' && Number.isFinite(Number(value)) && Number(value) >= 0 ? null : '倍率必须是大于等于 0 的数字'
}

/* 端口范围在提交前先校验格式，输错时错误直接标在输入框下面。留空交给服务端
   处理。 */
function portRangeError(value) {
  if (!value) return null
  for (const part of value.split(',')) {
    const p = part.trim()
    const m = p.match(/^(\d{1,5})(?:-(\d{1,5}))?$/)
    if (!m) return `「${p}」格式不对：应为端口或端口区间，例如 23333 或 10001-19999`
    const a = Number(m[1])
    const b = m[2] ? Number(m[2]) : a
    if (a < 1 || a > 65535 || b < 1 || b > 65535) return `「${p}」端口需在 1–65535 之间`
    if (a > b) return `「${p}」区间的起始端口不能大于结束端口`
  }
  return null
}

/**
 * 一个配置项：一个输入框加它自己的保存按钮。输入中的内容是草稿（见
 * useDraft）；校验与保存失败的原因显示在输入框下面。
 *
 * @param {{
 *   label: string,
 *   hint?: string,
 *   saved: string,
 *   onSave: (value: string) => Promise<unknown>,
 *   canon?: (text: string) => string,
 *   check?: (value: string) => string | null,
 *   locked?: boolean,
 *   inputClassName?: string,
 * } & Record<string, unknown>} props `canon` 把输入整理成服务端会存下的形式；
 * `locked` 表示这一项不由面板管理，只读。其余属性透传给 Input。
 */
function SettingForm({ label, hint, saved, onSave, canon = trimmed, check, locked = false, inputClassName = 'flex-1', ...input }) {
  const draft = useDraft(saved)
  const [error, setError] = useState(/** @type {string | null} */ (null))
  const save = useAction(async () => {
    const value = canon(draft.value)
    const problem = check ? check(value) : null
    setError(problem)
    if (problem) return
    // 草稿换成整理后的值：刷新回来的服务端值才会与它相等。
    draft.set(value)
    if (value === saved) return
    await onSave(value)
  }, { onError: err => setError(err instanceof Error ? err.message : '保存失败') })

  return (
    <Field label={label} hint={hint} error={error}>
      <form noValidate className="flex gap-2" onSubmit={e => { e.preventDefault(); void save.run() }}>
        <Input
          {...input}
          className={inputClassName}
          value={draft.value}
          disabled={locked}
          onChange={text => { draft.set(text); setError(null) }}
        />
        <Button type="submit" loading={save.pending} disabled={locked || !draft.dirty}>保存</Button>
      </form>
    </Field>
  )
}

const BILLING_OPTIONS = [{ value: 'both', label: '双向' }, { value: 'larger', label: '单向' }]
const DIRECT_EXIT_OPTIONS = [{ value: 'allow', label: '允许' }, { value: 'deny', label: '禁止' }]

function ConfigCard({ node, reload }) {
  const toast = useToast()
  const composite = node.node_type === 'composite'

  const post = async (path, body, done) => {
    await api.post(`/nodes/${node.id}/${path}`, body)
    toast(done)
    await reload()
  }
  const setBilling = useAction(async (unidirectional) => {
    await post('unidirectional', { unidirectional }, unidirectional ? '已改为单向计费' : '已改为双向计费')
  })
  const setDirectExit = useAction(async (noDirectExit) => {
    await post('no-direct-exit', { no_direct_exit: noDirectExit }, noDirectExit ? '已禁止直接转发' : '已允许直接转发')
  })

  return (
    <Card>
      <CardHeader title="节点配置" />
      <CardSection className="flex flex-col gap-5">
        <SettingForm
          label="节点名称"
          saved={node.name || ''}
          check={value => (value ? null : '名称不能为空')}
          onSave={name => post('rename', { name }, '名称已保存')}
        />
        {!composite && (
          <>
            <SettingForm
              label="中继地址"
              mono
              placeholder="数据面的公网 IP 或域名"
              saved={node.relay_host || ''}
              locked={!!node.relay_host_declared}
              hint={node.relay_host_declared
                ? '由节点的启动参数 --relay-host 声明，面板里不能修改；要变更请改启动参数后重启 Agent。'
                : '链路里的上一跳用这个地址连到本节点。'}
              onSave={relayHost => post('relay-host', { relay_host: relayHost }, '中继地址已保存')}
            />
            <SettingForm
              label="IPv6 中继地址"
              mono
              placeholder="数据面的公网 IPv6 地址"
              saved={node.relay_host_v6 || ''}
              locked={!!node.relay_host_v6_declared}
              hint={node.relay_host_v6_declared
                ? '由节点的启动参数 --relay-host-v6 声明，面板里不能修改；要变更请改启动参数后重启 Agent。'
                : '设置后本节点可以转发 IPv6 目标；留空表示不支持 IPv6。'}
              onSave={relayHostV6 => post('relay-host-v6', { relay_host_v6: relayHostV6 }, 'IPv6 中继地址已保存')}
            />
            <SettingForm
              label="端口范围"
              mono
              placeholder="例如 10001-19999,23333,40000-42000"
              saved={node.port_range || ''}
              hint="规则自动分配监听端口时从这个范围里选。"
              check={portRangeError}
              onSave={portRange => post('port-range', { port_range: portRange }, '端口范围已保存')}
            />
          </>
        )}
        <SettingForm
          label="倍率"
          mono
          type="number"
          min="0"
          step="0.1"
          inputClassName="w-28"
          saved={String(node.rate_multiplier ?? 1)}
          hint={composite ? '组合节点自身的计费倍率，作用于整条链路承担的流量。' : '影响本节点承担流量的计费。'}
          canon={canonRate}
          check={rateError}
          onSave={rate => post('rate-multiplier', { rate_multiplier: Number(rate) }, '倍率已保存')}
        />
        {!composite && (
          <Field label="计费方向" hint="双向按出站 + 入站计费，单向取出站与入站中较大的一方。">
            <div className="flex">
              <Segmented
                value={node.unidirectional ? 'larger' : 'both'}
                disabled={setBilling.pending}
                onChange={choice => { if ((choice === 'larger') !== !!node.unidirectional) void setBilling.run(choice === 'larger') }}
                options={BILLING_OPTIONS}
              />
            </div>
          </Field>
        )}
        {/* 计费方向与“直接转发”是物理节点的设置，组合节点上没有这两项：计费方向取
            上报流量的成员节点自己的设置，能否作链尾取最后一个成员节点的设置（服务端
            也拒绝在组合节点上写这两项）。 */}
        {!composite && (
          <Field label="直接转发" hint="禁止后本节点不能作为链路的最后一跳直连目标，规则必须在它后面再接线路；只影响之后新建或编辑的规则。">
            <div className="flex">
              <Segmented
                value={node.no_direct_exit ? 'deny' : 'allow'}
                disabled={setDirectExit.pending}
                onChange={choice => { if ((choice === 'deny') !== !!node.no_direct_exit) void setDirectExit.run(choice === 'deny') }}
                options={DIRECT_EXIT_OPTIONS}
              />
            </div>
          </Field>
        )}
      </CardSection>
    </Card>
  )
}

/* ---------- 节点用途与上下游绑定 ---------- */

/* 绑定边的两侧。上游边：本节点接在哪些节点之后（本节点是下游）；
   下游边：哪些节点接在本节点之后（本节点是上游）。两侧各有一个整表替换的接口。 */
const EDGE_SIDES = {
  upstream: { path: 'bindings', idKey: 'upstream_node_id' },
  downstream: { path: 'downstream-bindings', idKey: 'downstream_node_id' },
}

/* 绑定是一个集合：服务端按对端节点 id 排序返回，不保存顺序。所以比较两份
   绑定时不看顺序，只看“有哪些对端、各自什么模式”。 */
function sameEdgeMembers(a, b) {
  return !!a && !!b && a.length === b.length && a.every(x => b.some(y => y.id === x.id))
}
function sameEdges(a, b) {
  return !!a && !!b && a.length === b.length && a.every(x => b.some(y => y.id === x.id && y.mode === x.mode))
}

/**
 * 节点一侧的绑定：服务端保存的那份，加上本地还没保存的修改。
 *
 * 不变式：没有成功读到服务端保存的绑定之前，`rows` 为 null、`dirty` 为假，
 * 调用方既无从编辑也不会提交。这一侧的保存是整表替换，一次读取失败之后若
 * 仍允许提交，就会用一份空表盖掉已有的绑定。
 *
 * @param {number} nodeId
 * @param {'upstream' | 'downstream'} side
 */
function useEdgeSet(nodeId, side) {
  const { path, idKey } = EDGE_SIDES[side]
  const url = `/nodes/${nodeId}/${path}`
  const stored = useResource(
    async () => ((await api.get(url)).bindings || []).map(b => ({ id: b[idKey], mode: b.mode })),
    [url],
  )
  const saved = stored.state.status === 'ready' ? stored.state.data : null
  const draft = useDraft(saved, sameEdges)
  return {
    state: stored.state,
    reload: stored.reload,
    saved,
    rows: saved === null ? null : draft.value,
    dirty: saved !== null && draft.dirty,
    edit: draft.set,
    discard: draft.discard,
    save: async (rows) => {
      await api.post(url, { bindings: rows.map(r => ({ [idKey]: r.id, mode: r.mode })) })
      await stored.reload()
    },
  }
}

function EdgeEditor({ title, hint, placeholder, edge, roster, selfId, fit, unfitLabel, locked, onMode }) {
  const combined = combineResources(roster.state, edge.state)
  const retry = () => {
    if (roster.state.status === 'error') void roster.reload()
    if (edge.state.status === 'error') void edge.reload()
  }
  let body
  switch (combined.status) {
    case 'loading':
      body = <p className="text-small text-ink-mut">正在加载…</p>
      break
    case 'error':
      body = (
        <Alert tone="danger">
          绑定没有加载出来：{combined.message}。加载成功之前这一侧不会被保存，已有的绑定不受影响。{' '}
          <TextLink onClick={retry}>重试</TextLink>
        </Alert>
      )
      break
    case 'ready': {
      const [nodes] = combined.data
      const rows = edge.rows
      const byId = new Map(nodes.map(n => [n.id, n]))
      const toOption = n => ({ value: n.id, label: n.name, icon: <NodeTypeIcon type={n.node_type} /> })
      // 不符合条件的候选不隐藏，而是排到一条说明之后：用户看得到“节点在，
      // 只是还缺用途”，选中后由保存流程补齐。
      const group = (list) => {
        if (!fit) return list.map(toOption)
        const unfit = list.filter(n => !fit(n))
        const ok = list.filter(fit).map(toOption)
        return unfit.length ? [...ok, { heading: unfitLabel }, ...unfit.map(toOption)] : ok
      }
      const candidates = nodes.filter(n => n.id !== selfId)
      const pick = (ids) => edge.edit([
        ...rows.filter(r => ids.includes(r.id)),
        ...ids.filter(id => !rows.some(r => r.id === id)).map(id => ({ id, mode: 'userspace' })),
      ])
      body = (
        <>
          {combined.refreshError && (
            <Alert tone="warn">
              刷新失败，下面显示的可能不是最新数据：{combined.refreshError}{' '}
              <TextLink onClick={() => { void roster.reload(); void edge.reload() }}>重试</TextLink>
            </Alert>
          )}
          <Select
            multiple
            searchable
            tabs
            selectAll
            placeholder={placeholder}
            value={rows.map(r => r.id)}
            onChange={pick}
            groups={[
              { label: '单点', options: group(candidates.filter(n => n.node_type !== 'composite')) },
              { label: '组合', options: group(candidates.filter(n => n.node_type === 'composite')) },
            ]}
          />
          {rows.map(r => {
            const peer = byId.get(r.id)
            return (
              <div key={r.id} className="flex flex-wrap items-center gap-2 rounded-control bg-subtle px-3 py-2">
                <NodeTypeIcon type={peer?.node_type} />
                <span className="min-w-0 flex-1 truncate text-small">{peer ? peer.name : `节点 #${r.id}`}</span>
                <Segmented
                  size="sm"
                  label={`与 ${peer ? peer.name : `节点 #${r.id}`} 之间的转发模式`}
                  value={r.mode}
                  disabled={locked}
                  onChange={mode => { if (mode !== r.mode) onMode(r.id, mode) }}
                  options={MODE_OPTIONS}
                />
                <IconButton size="sm" danger label="移除这条绑定" disabled={locked} onClick={() => edge.edit(rows.filter(x => x.id !== r.id))}>
                  <IconX size={14} />
                </IconButton>
              </div>
            )
          })}
        </>
      )
      break
    }
    default:
      return assertNever(combined)
  }
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-baseline gap-2">
        <h3 className="text-body font-medium text-ink">{title}</h3>
        {edge.rows && <span className="text-caption text-ink-mut">{edge.rows.length} 个</span>}
      </div>
      <p className="text-caption text-ink-mut">{hint}</p>
      {body}
    </div>
  )
}

function RolesCard({ node, reload }) {
  const toast = useToast()
  const rolesDraft = useDraft(node.roles)
  const roles = rolesDraft.value
  const entryOn = (roles & ROLE_ENTRY) !== 0
  const viaOn = (roles & ROLE_VIA) !== 0

  // 候选节点与绑定行里的名称都来自这一份节点列表。
  const roster = useResource(async () => (await api.get('/nodes')).nodes || [], [])
  const downstream = useEdgeSet(node.id, 'downstream')
  const upstream = useEdgeSet(node.id, 'upstream')
  // 取消勾选的用途，其绑定编辑区不显示，里面的修改也不算待保存、不会被提交。
  const downDirty = entryOn && downstream.dirty
  const upDirty = viaOn && upstream.dirty
  const dirty = rolesDraft.dirty || downDirty || upDirty

  // “保存”与“改模式即保存”共用一个动作：两者都会整表提交绑定，不能同时在途。
  const commit = useAction(async (kind, edge, rows) => {
    if (kind === 'mode') {
      await edge.save(rows)
      toast('转发模式已保存')
      return
    }
    try {
      if (rolesDraft.dirty) await api.post(`/nodes/${node.id}/roles`, { roles })
      if (upDirty) await upstream.save(upstream.rows)
      if (downDirty) {
        // 下游必须有中转用途才能接在本节点之后；只有入口用途的先补上，
        // 否则这次绑定会被服务端拒绝。
        const all = roster.state.status === 'ready' ? roster.state.data : []
        for (const row of downstream.rows) {
          const peer = all.find(n => n.id === row.id)
          if (peer && (peer.roles & ROLE_VIA) === 0) await api.post(`/nodes/${peer.id}/roles`, { roles: peer.roles | ROLE_VIA })
        }
        await downstream.save(downstream.rows)
      }
      toast('节点用途已保存')
    } finally {
      // 上面几次提交不是一个事务，中途失败时已经落库的部分不会回滚。成败都
      // 刷新：已保存的部分与服务端对上后不再算待保存，没存上的仍然标着。
      await Promise.all([reload(), roster.reload()])
    }
  })

  const setRole = (bit, on) => rolesDraft.set(on ? roles | bit : roles & ~bit)
  const changeMode = (edge) => (id, mode) => {
    const next = edge.rows.map(r => (r.id === id ? { ...r, mode } : r))
    edge.edit(next)
    // 这一侧除了模式之外没有别的待保存修改（对端节点与已保存的一致、用途也
    // 没改）时，整表提交只会落下这一处模式变化，直接保存；否则留给“保存”
    // 连同其它修改一起提交。
    if (!rolesDraft.dirty && sameEdgeMembers(next, edge.saved)) void commit.run('mode', edge, next)
  }
  const discard = () => {
    rolesDraft.discard()
    downstream.discard()
    upstream.discard()
  }

  return (
    <Card>
      <CardHeader
        title="节点用途"
        actions={<>
          {dirty && <Button size="sm" variant="ghost" disabled={commit.pending} onClick={discard}>放弃修改</Button>}
          <Button size="sm" loading={commit.pendingKey === 'all'} disabled={!dirty || roles === 0 || commit.pending} onClick={() => commit.run('all')}>
            保存用途
          </Button>
        </>}
      />
      <CardSection className="flex flex-col gap-4">
        <p className="text-small text-ink-mut">
          入口：可以被规则选作入口，并在它后面绑定下游。中转：可以绑定到上游之后，供规则级联。至少保留一个。
          模式作用于两个节点衔接的那一段；对绑定的修改只影响之后新建的规则。
        </p>
        {roles === 0 && <Alert tone="danger">至少保留一个用途。</Alert>}
        <div className="grid gap-x-8 gap-y-6 md:grid-cols-2">
          <div className="flex flex-col gap-3">
            <Checkbox checked={entryOn} onChange={on => setRole(ROLE_ENTRY, on)}>入口</Checkbox>
            {entryOn && (
              <EdgeEditor
                title="已绑定的下游"
                placeholder="选择下游节点"
                hint="选中的下游可以在以本节点为上游的规则里级联接入。"
                edge={downstream}
                roster={roster}
                selfId={node.id}
                fit={n => (n.roles & ROLE_VIA) !== 0}
                unfitLabel="以下节点只有入口用途，选中后保存时会自动为它们加上中转用途"
                locked={commit.pending}
                onMode={changeMode(downstream)}
              />
            )}
          </div>
          <div className="flex flex-col gap-3">
            <Checkbox checked={viaOn} onChange={on => setRole(ROLE_VIA, on)}>中转</Checkbox>
            {viaOn && (
              <EdgeEditor
                title="已绑定的上游"
                placeholder="选择上游节点"
                hint="绑定后，选中这些上游（入口或中转）的规则可以级联接入本节点。"
                edge={upstream}
                roster={roster}
                selfId={node.id}
                locked={commit.pending}
                onMode={changeMode(upstream)}
              />
            )}
          </div>
        </div>
      </CardSection>
    </Card>
  )
}

/* ---------- 组合节点的跳序 ---------- */

function sameHops(a, b) {
  return a.length === b.length && a.every((x, i) => x.nodeId === b[i].nodeId && x.mode === b[i].mode)
}

function CompositeHopsCard({ nodeId, hops, candidates, reload }) {
  const toast = useToast()
  // 已保存的行用它在跳序里的位置作 key：同一份服务端数据每次渲染得到同样的
  // key，行不会因为轮询刷新而重建。
  const saved = hops.map(h => ({ key: `saved-${h.position}`, nodeId: h.hop_node_id, mode: h.mode }))
  const draft = useDraft(saved, sameHops)
  const rows = draft.value
  const [error, setError] = useState(/** @type {string | null} */ (null))

  const save = useAction(async () => {
    const problem = hopRowsError(rows)
    setError(problem)
    if (problem) return
    await api.post(`/nodes/${nodeId}/hops`, { hops: hopRowsPayload(rows) })
    toast('跳序已保存')
    await reload()
  })

  return (
    <Card>
      <CardHeader
        title="跳序"
        description={`${rows.length} 跳，从入口到出口；修改只影响之后新建的规则`}
        actions={<>
          {draft.dirty && <Button size="sm" variant="ghost" disabled={save.pending} onClick={() => { draft.discard(); setError(null) }}>放弃修改</Button>}
          <Button size="sm" loading={save.pending} disabled={!draft.dirty} onClick={() => save.run()}>保存跳序</Button>
        </>}
      />
      <CardSection className="flex flex-col gap-3">
        {error && <Alert tone="danger">{error}</Alert>}
        <HopListEditor rows={rows} candidates={candidates} memberLinks onChange={next => { draft.set(next); setError(null) }} />
      </CardSection>
    </Card>
  )
}

/* ---------- 经过该节点的规则 ---------- */

function RuleHopsCard({ ruleHops }) {
  const blurred = useBlur()
  return (
    <Card className="overflow-hidden">
      <CardHeader title="经过该节点的规则" description={ruleHops.length ? `${ruleHops.length} 条` : undefined} />
      {ruleHops.length === 0 ? (
        <Empty title="还没有规则经过这个节点" desc="规则选它作入口、中转或出口之后，会列在这里。">
          <Button to="/rules">去创建规则</Button>
        </Empty>
      ) : (
        <TableBox>
          <table className="tbl">
            <thead>
              <tr>
                <th>规则</th><th>用户</th><th>链路位置</th><th>协议</th><th>模式</th><th>监听端口</th><th>目标</th><th className="num">流量</th>
              </tr>
            </thead>
            <tbody>
              {ruleHops.map(rh => (
                <tr key={rh.id}>
                  <td>
                    {rh.rule_id
                      ? <TextLink to={`/rules/${rh.rule_id}`} tone="ink" className="font-medium">{rh.rule_name || `#${rh.rule_id}`}</TextLink>
                      : '--'}
                  </td>
                  <td>{rh.owner_id ? <TextLink to={`/users/${rh.owner_id}`}>{rh.owner_name}</TextLink> : <Unset>--</Unset>}</td>
                  <td>
                    {rh.total_hops > 1
                      ? <TextLink to={`/nodes/${rh.rule_node_id}`}>第 {rh.position + 1} / {rh.total_hops} 跳</TextLink>
                      : <Unset>单跳</Unset>}
                  </td>
                  <td><ProtoTag proto={rh.proto} /></td>
                  <td><ModeTag mode={rh.mode} /></td>
                  <td className="font-mono">{rh.listen_port}</td>
                  <td className="font-mono"><SensText blurred={blurred}>{rh.target_host ? `${rh.target_host}:${rh.target_port}` : '--'}</SensText></td>
                  <td className="num text-ink-soft">{fmtBytes(rh.total_bytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableBox>
      )}
    </Card>
  )
}

/* ---------- 已授权用户 ---------- */

const NO_GRANT_CHANGE = { add: [], remove: [] }

/**
 * 批量编辑“哪些用户可以使用这个节点”。保存时把增减一次提交给批量接口。
 *
 * 待保存的修改按“相对当前授权的增减”保存，而不是一份完整的勾选列表：
 * 授权随时可能在别处变化（下面表格里的“取消授权”、轮询刷新），增减叠加在
 * 最新的授权之上仍然是用户的本意；一份完整列表则会把别处刚取消的授权
 * 当成“新增”重新提交。已经成为事实的增减（要加的已被授权、要减的已不在）
 * 在渲染时剔除，所以保存成功、数据刷新回来之后待保存的修改自然清空。
 */
function GrantEditor({ nodeId, grantedUsers, reload }) {
  const toast = useToast()
  const confirm = useConfirm()
  const users = useGrantableUsers()
  const granted = grantedUsers.map(g => g.user_id)
  const [change, setChange] = useState(NO_GRANT_CHANGE)
  const add = change.add.filter(id => !granted.includes(id))
  const remove = change.remove.filter(id => granted.includes(id))
  if (add.length !== change.add.length || remove.length !== change.remove.length) setChange({ add, remove })
  const selected = [...granted.filter(id => !remove.includes(id)), ...add]
  const dirty = add.length > 0 || remove.length > 0

  const nameOf = new Map(grantedUsers.map(g => [g.user_id, g.username]))

  const save = useAction(async () => {
    if (remove.length) {
      const agreed = await confirm({
        title: '取消授权',
        message: `取消 ${remove.map(id => nameOf.get(id) ?? `#${id}`).join('、')} 对该节点的授权？他们经过该节点的规则会被一并删除，无法恢复。`,
        confirmText: '取消授权',
        cancelText: '保留授权',
        danger: true,
      })
      if (!agreed) return
    }
    try {
      const res = await api.post(`/nodes/${nodeId}/grants/batch`, { add_user_ids: add, remove_user_ids: remove })
      toast(`授权已更新：新增 ${res?.granted ?? add.length} 人，取消 ${res?.revoked ?? remove.length} 人`)
    } finally {
      // 批量接口逐个应用，中途失败时前面的已经生效。成败都刷新，剩下没生效的
      // 增减才会按实际授权重新算出来。
      await reload()
    }
  })

  return (
    <div className="flex max-w-xl flex-wrap items-start gap-2">
      <GrantUserSelect
        className="flex-1"
        users={users}
        placeholder="选择要授权的用户"
        known={grantedUsers.map(g => ({ id: g.user_id, username: g.username }))}
        value={selected}
        onChange={ids => setChange({ add: ids.filter(id => !granted.includes(id)), remove: granted.filter(id => !ids.includes(id)) })}
      />
      <Button loading={save.pending} disabled={!dirty} onClick={() => save.run()}>保存授权</Button>
    </div>
  )
}

function GrantsCard({ node, grantedUsers, reload }) {
  const toast = useToast()
  const confirm = useConfirm()
  const revoke = useAction(async (userId, username) => {
    const agreed = await confirm({
      title: '取消授权',
      message: `取消 ${username} 对该节点的授权？该用户经过该节点的规则会被一并删除，无法恢复。`,
      confirmText: '取消授权',
      cancelText: '保留授权',
      danger: true,
    })
    if (!agreed) return
    await api.del(`/users/${userId}/grants/${node.id}`)
    toast('已取消授权')
    await reload()
  })

  return (
    <Card className="overflow-hidden">
      <CardHeader title="已授权用户" description={grantedUsers.length ? `${grantedUsers.length} 人` : undefined} />
      <CardSection>
        <GrantEditor nodeId={node.id} grantedUsers={grantedUsers} reload={reload} />
      </CardSection>
      {grantedUsers.length === 0 ? (
        <div className="border-t border-line-soft">
          <Empty title="还没有用户被授权使用这个节点" desc="在上面选择用户并保存，他们就能用这个节点创建规则。" />
        </div>
      ) : (
        <TableBox className="border-t border-line-soft">
          <table className="tbl">
            <thead>
              <tr><th>用户</th><th>规则配额</th><th>用途</th><th>授权时间</th><th className="num">操作</th></tr>
            </thead>
            <tbody>
              {grantedUsers.map(g => (
                <tr key={g.user_id}>
                  <td><TextLink to={`/users/${g.user_id}`} tone="ink" className="font-medium">{g.username}</TextLink></td>
                  <td>{g.max_forwards}</td>
                  <td><PerNodeRolesForm userId={g.user_id} nodeId={node.id} roles={g.roles} onDone={reload} /></td>
                  <td className="text-ink-soft">{fmtTime(g.granted_at)}</td>
                  <td className="num">
                    <Button size="sm" variant="ghost" danger loading={revoke.pendingKey === g.user_id} onClick={() => revoke.run(g.user_id, g.username)}>
                      取消授权
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableBox>
      )}
    </Card>
  )
}
