import { useState } from 'react'
import { api } from '../lib/api'
import { fmtDate } from '../lib/fmt'
import { useResource } from '../lib/useResource'
import { ResourceView } from './ResourceView'
import { useUser } from './session'
import {
  Alert, Badge, Button, Card, CardHeader, CardSection, CodeBlock, Collapsible, DescItem, DescList, Empty, Modal,
  ModalActions, Segmented, Tag, TextLink, useAction, useConfirm, useToast,
} from './ui'

const SCOPE_OPTIONS = [
  { value: 'read', label: '只读' },
  { value: 'readwrite', label: '读写' },
]

/* 一个 Token 能调用哪些接口取决于账号的角色：/api/v1 的路由按角色分组
   （服务端 registerV1Routes），管理员的 Token 调不了 /my/*，普通用户的调不了
   /nodes、/users 等管理接口。说明文字因此按角色给出。

   read 列的是只读 Token 常用的查询接口，write 概括读写 Token 额外能做的事，
   writeDetail 是对它的补充说明。两者都是摘要，完整的接口表以服务端提供的
   OpenAPI 文档为准（说明末尾有链接）。 */
const API_ACCESS = {
  user: {
    read: ['/info', '/my/nodes', '/my/rules', '/probe', '/probe-chain'],
    write: '创建、修改、删除你自己的转发规则',
    writeDetail: (
      <>
        对应 <code>POST / PUT / DELETE /my/rules</code>；创建规则时可以用 <code>node_name</code> 按名称指定节点，
        加 <code>?dry_run=1</code> 预览端口，带 <code>Idempotency-Key</code> 请求头做幂等重试。
      </>
    ),
  },
  admin: {
    read: ['/info', '/nodes', '/users', '/dashboard', '/usage', '/landing-usage', '/probe', '/probe-chain'],
    write: '创建用户，调整用户的配额、到期、启用状态与节点授权，管理用户的 Token，重新同步节点等',
    writeDetail: null,
  },
}

/**
 * 调用方式说明。示例里的地址取当前页面的 origin，所以示例是一条指向本面板的
 * 完整命令，把 YOUR_TOKEN 换成真实的 Token 即可。
 * @param {{ access: typeof API_ACCESS.user | typeof API_ACCESS.admin }} props
 */
function ApiUsage({ access }) {
  const base = `${window.location.origin}/api/v1`
  return (
    <div className="flex flex-col gap-3 text-small text-ink-soft">
      <p>每个请求都要带上 Token：放在 Authorization 请求头里，或者作为 token 参数。</p>
      <CodeBlock text={`curl -H "Authorization: Bearer YOUR_TOKEN" ${base}/info`} />
      <CodeBlock text={`curl "${base}/info?token=YOUR_TOKEN"`} />
      <DescList>
        <DescItem label="只读 Token">
          可以查询，例如：
          {access.read.map((path, i) => (
            <span key={path}>{i > 0 && '、'}<code>GET {path}</code></span>
          ))}
        </DescItem>
        <DescItem label="读写 Token">
          除了查询，还可以{access.write}。{access.writeDetail}
        </DescItem>
      </DescList>
      <p>
        完整的接口定义见{' '}
        <TextLink href="/api/v1/openapi.json" target="_blank" rel="noreferrer">OpenAPI 文档</TextLink>。
      </p>
    </div>
  )
}

/** @param {{ token: { token_prefix: string, scope: string, disabled: boolean, created_at: number, last_used_at: number | null } }} props */
function TokenFacts({ token }) {
  return (
    <DescList>
      <DescItem label="Token"><span className="font-mono">{token.token_prefix}…</span></DescItem>
      <DescItem label="状态">
        {token.disabled ? <Badge tone="warn" dot>已停用</Badge> : <Badge tone="ok" dot>启用中</Badge>}
      </DescItem>
      <DescItem label="权限"><Tag>{token.scope === 'readwrite' ? '读写' : '只读'}</Tag></DescItem>
      <DescItem label="创建时间">{fmtDate(token.created_at)}</DescItem>
      <DescItem label="最近使用">{token.last_used_at ? fmtDate(token.last_used_at) : '从未使用'}</DescItem>
    </DescList>
  )
}

/**
 * 刚签发的 Token 明文。服务端只在签发的那次响应里给出明文，之后只存哈希，
 * 所以这个弹窗关掉就再也拿不回来：
 * - persistent：Esc 与点遮罩不关闭，只有“我已保存”和右上角的关闭按钮能关。
 * - 初始焦点放在正文容器上而不是某个按钮上。弹窗默认聚焦第一个可聚焦元素，
 *   也就是右上角的关闭按钮；那样的话弹窗一出现，一次回车或空格就把它关掉了。
 *
 * @param {{ secret: string, regenerated: boolean, onClose: () => void }} props
 */
function IssuedTokenModal({ secret, regenerated, onClose }) {
  return (
    <Modal
      persistent
      title={regenerated ? 'Token 已重新生成' : 'Token 已创建'}
      onClose={onClose}
      footer={<ModalActions><Button variant="primary" onClick={onClose}>我已保存</Button></ModalActions>}
    >
      <div data-autofocus tabIndex={-1} className="flex flex-col gap-3 outline-none">
        <Alert tone="warn">完整的 Token 只显示这一次，关闭后无法再次查看。请现在复制并保存到安全的地方。</Alert>
        <CodeBlock text={secret} />
      </div>
    </Modal>
  )
}

/**
 * 当前账号的 API Token：查看、创建、停用 / 启用、重新生成、删除。
 *
 * /my/token 按会话鉴权、不分角色，每个账号管理自己的那一个 Token，所以这张
 * 卡片同时用在普通用户的概览页和管理员的系统设置页，不带任何 props。
 */
export function ApiTokenSection() {
  const toast = useToast()
  const confirm = useConfirm()
  const { user } = useUser()
  const token = useResource(() => api.get('/my/token'), [])
  // 新 Token 默认只读（权限最小的一档）；需要写权限时由用户显式选择。
  const [scope, setScope] = useState('read')
  const [issued, setIssued] = useState(/** @type {null | { secret: string, regenerated: boolean }} */ (null))
  const [usageOpen, setUsageOpen] = useState(false)

  // 角色只按“是否管理员”二分，与路由守卫 RequireAuth 的判定一致。
  const access = user.role === 'admin' ? API_ACCESS.admin : API_ACCESS.user
  const current = token.state.status === 'ready' && token.state.data.has_token ? token.state.data : null

  const operations = {
    create: async () => {
      const res = await api.post('/my/token', { scope })
      setIssued({ secret: res.token, regenerated: false })
      await token.reload()
    },
    toggle: async () => {
      const res = await api.post('/my/token/toggle')
      token.patch(t => ({ ...t, disabled: res.disabled }))
      toast(res.disabled ? 'Token 已停用' : 'Token 已启用')
    },
    regenerate: async () => {
      const res = await api.post('/my/token/refresh')
      setIssued({ secret: res.token, regenerated: true })
      await token.reload()
    },
    remove: async () => {
      await api.del('/my/token')
      token.patch(() => ({ has_token: false }))
      toast('Token 已删除')
    },
  }
  /* 四种操作作用在同一个 Token 上，共用一个 action：useAction 的重入守卫因此
     覆盖任意两种操作之间（停用的请求在途时点删除会被忽略），而不只是同一种
     操作的连点。停用 / 启用在服务端是翻转语义（每收到一次请求就取反一次），
     连点两次等于没改，靠的也是这个守卫。pendingKey 是在途操作的名字，按钮
     据此只让被点的那一个显示进行中。 */
  const operation = useAction(/** @param {keyof typeof operations} name */ name => operations[name]())

  /* 每种操作在这里声明执行前要不要确认：不可恢复的两种（重新生成、删除）
     确认，创建与可撤销的停用 / 启用不确认。 */
  const confirmations = {
    create: null,
    toggle: null,
    regenerate: {
      title: '重新生成 Token',
      message: '当前的 Token 会立即失效，正在使用它的程序要换成新的 Token 才能继续调用。权限保持不变。'
        + (current?.disabled ? '\n这个 Token 目前已停用，重新生成后会恢复启用。' : ''),
      confirmText: '重新生成',
    },
    remove: {
      title: '删除 Token',
      message: '正在使用这个 Token 的程序会立即失效。删除后无法恢复，需要时只能重新创建一个新的 Token。',
      confirmText: '删除',
    },
  }
  /* 确认框在 action 之外：等待用户回答期间 action 不在途，触发它的按钮没有被
     禁用，确认框关闭时 Modal 才能把焦点还给这个按钮（禁用的按钮不能获得焦点）。
     这段时间不会有第二个操作插进来 —— 确认框是模态的，盖住并困住了整个页面。 */
  const start = async (/** @type {keyof typeof operations} */ name) => {
    const question = confirmations[name]
    if (question && !(await confirm({ ...question, danger: true }))) return
    await operation.run(name)
  }
  const button = (/** @type {keyof typeof operations} */ name) => ({
    loading: operation.pendingKey === name,
    disabled: operation.pending,
    onClick: () => start(name),
  })

  return (
    <Card>
      <CardHeader
        title="API Token"
        description="脚本或 AI agent 调用面板 API 时使用的访问凭证"
        actions={current && (
          <>
            <Button size="sm" {...button('toggle')}>{current.disabled ? '启用' : '停用'}</Button>
            <Button size="sm" {...button('regenerate')}>重新生成</Button>
            <Button size="sm" danger {...button('remove')}>删除</Button>
          </>
        )}
      />
      <CardSection className="flex flex-col gap-4">
        <ResourceView resource={token}>
          {data => (data.has_token ? <TokenFacts token={data} /> : (
            <Empty
              title="还没有 Token"
              desc={`只读 Token 只能查询；读写 Token 还可以${access.write}。完整的 Token 只在创建时显示一次。`}
            >
              <Segmented label="权限" value={scope} onChange={setScope} options={SCOPE_OPTIONS} />
              <Button {...button('create')}>创建 Token</Button>
            </Empty>
          ))}
        </ResourceView>
      </CardSection>
      <CardSection>
        <Collapsible title="调用方式" open={usageOpen} onToggle={() => setUsageOpen(open => !open)}>
          <ApiUsage access={access} />
        </Collapsible>
      </CardSection>
      {issued && <IssuedTokenModal secret={issued.secret} regenerated={issued.regenerated} onClose={() => setIssued(null)} />}
    </Card>
  )
}
