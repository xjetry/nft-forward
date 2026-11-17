import { useState } from 'react'
import { api } from '../../lib/api'
import { assertNever } from '../../lib/resource'
import { useResource } from '../../lib/useResource'
import { NodeTypeIcon } from '../../components/net'
import {
  Alert, Button, IconArrowDown, IconArrowUp, IconButton, IconExternal, IconGrip, IconPlus, IconX, Segmented, Select, Tag,
  TextLink, cx,
} from '../../components/ui'

/* 节点列表页与详情页共用的部分：用途位、版本比较、可授权用户、删除节点的
   确认流程、组合节点的跳序编辑器。 */


/* 节点用途位，与服务端 nodes.roles 的位定义一致（db.NodeRoleEntry / NodeRoleVia）。 */
export const ROLE_ENTRY = 1
export const ROLE_VIA = 2

/** 可以被授权使用节点的用户：授权的对象只有普通用户（role 为 user）。 */
export function useGrantableUsers() {
  return useResource(async () => ((await api.get('/users')).users || []).filter(u => u.role === 'user'), [])
}

/**
 * 选择“哪些用户可以使用这个节点”的多选框，连同用户列表的三种加载状态：
 * 列表没读到时显示原因与重试，而不是一个看起来“没有用户可选”的空下拉。
 *
 * @param {{
 *   users: ReturnType<typeof useGrantableUsers>,
 *   value: number[],
 *   onChange: (ids: number[]) => void,
 *   placeholder?: string,
 *   known?: Array<{ id: number, username: string }>,
 *   className?: string,
 * }} props `known` 是不依赖用户列表也已知的用户（如已被授权的）：它们即使
 * 不在可授权用户列表里（后来被改成了管理员）也要出现在选项中，否则无从取消。
 */
export function GrantUserSelect({ users, value, onChange, placeholder = '选择用户', known = [], className }) {
  switch (users.state.status) {
    case 'loading':
      return <Select multiple disabled className={className} placeholder="正在加载用户…" value={[]} onChange={() => {}} options={[]} />
    case 'error':
      return (
        <Alert tone="danger" className={className}>
          用户列表没有加载出来：{users.state.message}{' '}
          <TextLink onClick={users.reload}>重试</TextLink>
        </Alert>
      )
    case 'ready': {
      const nameOf = new Map(known.map(u => [u.id, u.username]))
      for (const u of users.state.data) nameOf.set(u.id, u.username)
      return (
        <Select
          multiple
          searchable
          className={className}
          placeholder={placeholder}
          value={value}
          onChange={onChange}
          options={[...nameOf].map(([id, username]) => ({ value: id, label: username }))}
        />
      )
    }
    default:
      return assertNever(users.state)
  }
}

/** 删除确认框里的说明：被删的是谁、会连带改变什么。数字全部取自节点详情。 */
function removalImpact(detail) {
  const { node } = detail
  const ruleCount = new Set((detail.rule_hops || []).map(h => h.rule_id)).size
  const grantCount = (detail.granted_users || []).length
  const composites = detail.referenced_by_composites || []
  const effects = []
  if (ruleCount) {
    effects.push(node.node_type === 'composite'
      ? `经过它的 ${ruleCount} 条规则会被删除`
      : `经过它的 ${ruleCount} 条规则会被重新连接或清除`)
  }
  if (grantCount) effects.push(`${grantCount} 名用户对它的授权会被移除`)
  if (composites.length) {
    effects.push(`它会从组合${composites.map(c => `「${c.name}」`).join('')}中移除，这些组合的跳序随之改变`)
  }
  const head = `删除节点「${node.name}」后无法恢复。`
  if (!effects.length) return `${head}当前没有规则经过它，也没有用户被授权使用它。`
  return `${head}\n${effects.map(e => `· ${e}`).join('\n')}`
}

/**
 * 删除节点：一次确认说清全部连带影响，然后删除。
 *
 * 确认框里的影响取自点击当下重新读取的节点详情，而不是页面上可能已经过期
 * 的数据。
 *
 * 不变式：`confirm=1`（让服务端跳过“该节点被组合引用”的拦截）只在用户已经
 * 在确认框里看到这些组合时才发送。确认框打开期间节点才被加入某个组合时，
 * 这次删除不带 confirm=1，服务端会拦下并返回 needs_confirm，于是按新的
 * 引用关系重新确认 —— 用户不会在不知情的情况下改掉一个组合的跳序。
 *
 * @param {number | string} nodeId
 * @param {(opts: object) => Promise<boolean>} confirm useConfirm() 的返回值
 * @returns {Promise<boolean>} 已删除为 true，用户取消为 false。
 */
export async function removeNode(nodeId, confirm) {
  for (;;) {
    const detail = await api.get(`/nodes/${nodeId}`)
    const sawComposites = (detail.referenced_by_composites || []).length > 0
    const agreed = await confirm({
      title: '删除节点',
      message: removalImpact(detail),
      confirmText: '删除节点',
      danger: true,
    })
    if (!agreed) return false
    const res = await api.del(`/nodes/${nodeId}${sawComposites ? '?confirm=1' : ''}`)
    if (!res?.needs_confirm) return true
  }
}

/* ---------- 组合节点的跳序编辑 ---------- */

export const MODE_OPTIONS = [{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]

let blankHopSeq = 0

/**
 * 一行尚未选节点的新跳。
 * 每一跳带一个 key，只用来在增删、重排之后认出“还是那一行”（React key 与
 * 拖动定位）；它不随所选节点变化，也不提交给服务端。
 * @returns {{ key: string, nodeId: number | null, mode: string }}
 */
export function newHopRow() {
  blankHopSeq += 1
  return { key: `blank-${blankHopSeq}`, nodeId: null, mode: 'userspace' }
}

/** 跳序能否提交；不能时返回给用户看的原因。 */
export function hopRowsError(rows) {
  // 空行必须显式拦下而不是过滤掉：过滤会让“最后一跳”落到另一行上，
  // 用户为最后一跳选的模式就配给了别的节点。
  if (rows.some(r => r.nodeId == null)) return '请为每一跳选择节点，或移除空行'
  if (rows.length < 2) return '组合节点至少需要 2 跳'
  return null
}

/** 提交给服务端的跳序。 */
export function hopRowsPayload(rows) {
  return rows.map(r => ({ node_id: r.nodeId, mode: r.mode }))
}

/**
 * 组合节点的跳序编辑器，创建弹窗与详情页的跳序卡片共用。受控：行由调用方
 * 持有。
 *
 * 行内三个控件用同一档紧凑高度；窄屏下一行放不下时自动换行。
 *
 * @param {{
 *   rows: Array<{ key: string, nodeId: number | null, mode: string }>,
 *   onChange: (rows: Array<{ key: string, nodeId: number | null, mode: string }>) => void,
 *   candidates: Array<{ id: number, name: string, node_type?: string }>,
 *   memberLinks?: boolean,
 * }} props `candidates` 是可以作为成员的节点；已被别的行选走的节点不再出现
 * 在其它行的选项里。`memberLinks` 为真时每行带一个打开该成员详情的链接
 * （会离开当前页，所以弹窗里不开）。
 */
export function HopListEditor({ rows, onChange, candidates, memberLinks = false }) {
  const [dragKey, setDragKey] = useState(/** @type {string | null} */ (null))
  const byId = new Map(candidates.map(n => [n.id, n]))
  const taken = new Set(rows.map(r => r.nodeId))

  const update = (key, fields) => onChange(rows.map(r => (r.key === key ? { ...r, ...fields } : r)))
  const move = (from, to) => {
    if (from < 0 || to < 0 || to >= rows.length || from === to) return
    const next = [...rows]
    next.splice(to, 0, ...next.splice(from, 1))
    onChange(next)
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-2">
        {rows.map((row, i) => {
          const member = row.nodeId == null ? undefined : byId.get(row.nodeId)
          return (
            <div
              key={row.key}
              onDragOver={dragKey ? e => e.preventDefault() : undefined}
              onDrop={dragKey ? () => { move(rows.findIndex(r => r.key === dragKey), i); setDragKey(null) } : undefined}
              className={cx('flex flex-wrap items-center gap-2 rounded-control bg-subtle px-3 py-2', dragKey === row.key && 'opacity-50')}
            >
              <span
                draggable
                onDragStart={() => setDragKey(row.key)}
                onDragEnd={() => setDragKey(null)}
                className="flex cursor-grab items-center text-ink-mut"
                title="拖动调整顺序"
              >
                <IconGrip size={14} />
              </span>
              <span className="w-5 flex-none text-center font-mono text-caption text-ink-mut">{i + 1}</span>
              <Select
                size="sm"
                searchable
                className="min-w-40 flex-1"
                placeholder="选择节点"
                value={row.nodeId}
                onChange={nodeId => update(row.key, { nodeId })}
                options={candidates
                  .filter(n => n.id === row.nodeId || !taken.has(n.id))
                  .map(n => ({ value: n.id, label: n.name, icon: <NodeTypeIcon type={n.node_type} /> }))}
              />
              {memberLinks && row.nodeId != null && (
                <IconButton size="sm" label="打开这个节点的详情" to={`/nodes/${row.nodeId}`}><IconExternal size={14} /></IconButton>
              )}
              {member?.node_type === 'composite' ? (
                <span className="text-caption text-ink-mut">模式由该组合内部各跳决定</span>
              ) : (
                <Segmented
                  size="sm"
                  label={`第 ${i + 1} 跳的转发模式`}
                  value={row.mode}
                  onChange={mode => update(row.key, { mode })}
                  options={MODE_OPTIONS}
                />
              )}
              {i === rows.length - 1 && <Tag>末跳</Tag>}
              <span className="ml-auto flex flex-none items-center gap-1">
                <IconButton size="sm" label="上移" disabled={i === 0} onClick={() => move(i, i - 1)}><IconArrowUp size={14} /></IconButton>
                <IconButton size="sm" label="下移" disabled={i === rows.length - 1} onClick={() => move(i, i + 1)}><IconArrowDown size={14} /></IconButton>
                {/* 组合至少 2 跳：只剩 2 行时不提供移除，行数不会被减到无法提交。 */}
                {rows.length > 2 && (
                  <IconButton size="sm" danger label="移除这一跳" onClick={() => onChange(rows.filter(r => r.key !== row.key))}><IconX size={14} /></IconButton>
                )}
              </span>
            </div>
          )
        })}
      </div>
      <div className="flex">
        <Button size="sm" icon={<IconPlus size={14} />} onClick={() => onChange([...rows, newHopRow()])}>添加一跳</Button>
      </div>
      <p className="text-caption text-ink-mut">
        模式作用于这一跳到下一跳之间的那一段：线路稳定、丢包低用 kernel（性能更好，支持 TCP / UDP / TCP+UDP）；
        跨境或丢包高用 userspace（更抗抖动，仅 TCP）。末跳的模式只在组合被用作中转时生效；组合作为规则出口时，由规则的出口模式决定。
      </p>
    </div>
  )
}
