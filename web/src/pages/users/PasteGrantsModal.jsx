import { useState } from 'react'
import { api } from '../../lib/api'
import { GB } from '../../lib/fmt'
import { useResource } from '../../lib/useResource'
import { TableBox } from '../../components/page'
import { ResourceView } from '../../components/ResourceView'
import { Button, Checkbox, Field, Modal, ModalActions, Select, StatusDot, Textarea, useAction, useToast } from '../../components/ui'

/* 一条授权在文本里没写某一项时取的值；新建一条授权而不套用文本设置时也用它。
   规则配额 10 与服务端的缺省一致（batchApplyGrants 把 ≤ 0 的值记为 10）。 */
const GRANT_DEFAULTS = { maxForwards: 10, quotaGB: 0, rateMBytes: 0 }

/**
 * 解析授权文本。每行一个节点：`节点名 | max=10 | quota=5GB | rate=10`，
 * 空行与 # 开头的行跳过，没写的项取 GRANT_DEFAULTS。
 * @param {string} text
 * @param {Array<{ id: number, name: string }>} allNodes 用来把节点名对应到节点
 */
export function parseGrantText(text, allNodes) {
  const nodeByName = new Map(allNodes.map(n => [n.name, n]))
  return text.split('\n')
    .map(line => line.trim())
    .filter(line => line && !line.startsWith('#'))
    .map(line => {
      const parts = line.split('|').map(s => s.trim())
      const name = parts[0]
      let { maxForwards, quotaGB, rateMBytes } = GRANT_DEFAULTS
      for (let i = 1; i < parts.length; i++) {
        const p = parts[i]
        const mMatch = p.match(/^max=(\d+)$/i)
        if (mMatch) { maxForwards = Number(mMatch[1]); continue }
        const qMatch = p.match(/^quota=([\d.]+)\s*GB$/i)
        if (qMatch) { quotaGB = Number(qMatch[1]); continue }
        const rMatch = p.match(/^rate=(\d+)$/i)
        if (rMatch) { rateMBytes = Number(rMatch[1]); continue }
      }
      const node = nodeByName.get(name)
      return { name, maxForwards, quotaGB, rateMBytes, nodeId: node?.id || null, found: !!node }
    })
}

function grantPayload(name, { maxForwards, quotaGB, rateMBytes }) {
  return {
    node_name: name,
    max_forwards: maxForwards,
    traffic_quota_bytes: Math.round(quotaGB * GB),
    rate_limit_mbytes: rateMBytes,
  }
}

/* 只给还没有授权的 (用户, 节点) 新建授权，已有的授权一项都不动。

   batch-apply 对请求里的每个 (用户, 节点) 都是“写成给定的值”：授权已存在时
   规则配额、流量配额、限速三项都会被请求里的值覆盖，请求里省略的字段按 0
   写入（规则配额 ≤ 0 记为 10）。它没有“这一项保持原值”的表达方式，所以
   不改已有授权的唯一办法是不把它们放进请求 —— 先读出每个节点已授权给谁，
   再只提交缺的那些。

   各用户缺的节点可能不同，而一次请求是“用户 × 节点”的全组合，所以按
   “缺的节点集合”把用户分组，每组一次请求。

   读与写之间别处新建的授权仍会被这里的默认值覆盖；这个窗口只有服务端
   提供“仅新增”的语义才能消除。 */
async function grantMissingOnly(userIds, rows) {
  const holders = await Promise.all(rows.map(async (row) => {
    const d = await api.get(`/nodes/${row.nodeId}`)
    return new Set((d?.granted_users || []).map(g => g.user_id))
  }))
  const groups = new Map()
  let kept = 0
  for (const userId of userIds) {
    const lacking = rows.filter((_, i) => !holders[i].has(userId))
    kept += rows.length - lacking.length
    if (lacking.length === 0) continue
    const key = lacking.map(row => row.nodeId).join(',')
    const group = groups.get(key) || { userIds: [], rows: lacking }
    group.userIds.push(userId)
    groups.set(key, group)
  }
  let granted = 0
  const skipped = new Set()
  for (const group of groups.values()) {
    const d = await api.post('/grants/batch-apply', {
      user_ids: group.userIds,
      grants: group.rows.map(row => grantPayload(row.name, GRANT_DEFAULTS)),
    })
    granted += d?.granted || 0
    for (const name of d?.skipped_nodes || []) skipped.add(name)
  }
  return { granted, kept, skipped: [...skipped] }
}

/**
 * 把一段授权文本批量授权给若干用户。
 *
 * 节点与用户的候选项在弹窗打开时自己加载：两个调用页各自只持有其中一半，
 * 而且解析“节点名 → 节点”需要的是打开这一刻的节点列表。
 *
 * @param {{
 *   initialUserIds?: number[],
 *   onClose: () => void,
 *   onDone: () => unknown,
 * }} props `onDone` 在授权写入后调用（通常是调用页的 reload），弹窗等它完成再关闭。
 */
export default function PasteGrantsModal({ initialUserIds = [], onClose, onDone }) {
  const toast = useToast()
  const options = useResource(async () => {
    const [nodes, users] = await Promise.all([api.get('/nodes'), api.get('/users')])
    return { nodes: nodes?.nodes || [], users: (users?.users || []).filter(u => u.role === 'user') }
  }, [])
  const [text, setText] = useState('')
  const [userIds, setUserIds] = useState(() => initialUserIds.map(String))
  const [applySettings, setApplySettings] = useState(true)

  const nodes = options.state.status === 'ready' ? options.state.data.nodes : []
  // 同一个节点写了多行时，服务端按行依次写入，最后一行生效；预览与提交都只留它。
  const rows = [...new Map(parseGrantText(text, nodes).map(row => [row.name, row])).values()]
  const valid = rows.filter(row => row.found)

  const submit = useAction(async () => {
    const targets = userIds.map(Number)
    let skipped
    if (applySettings) {
      const d = await api.post('/grants/batch-apply', {
        user_ids: targets,
        grants: valid.map(row => grantPayload(row.name, row)),
      })
      skipped = d?.skipped_nodes || []
      toast(`已把 ${valid.length - skipped.length} 个节点的授权写给 ${targets.length} 个用户`)
    } else {
      const result = await grantMissingOnly(targets, valid)
      skipped = result.skipped
      toast(result.granted > 0
        ? `已新增 ${result.granted} 项授权${result.kept > 0 ? `，另有 ${result.kept} 项已有授权保持不变` : ''}`
        : '所选用户已经拥有这些节点的授权，没有改动')
    }
    if (skipped.length) toast(`${skipped.length} 个节点已不存在，没有授权：${skipped.join('、')}`, 'error')
    await onDone()
    onClose()
  })

  const canSubmit = valid.length > 0 && userIds.length > 0
  // 提交按钮不可用时说明还差什么。
  const missing = options.state.status !== 'ready' || canSubmit ? null
    : rows.length === 0 ? '先粘贴授权文本。'
      : valid.length === 0 ? '文本里的节点都不存在，检查节点名是否与节点列表一致。'
        : '还没有选择目标用户。'

  return (
    <Modal
      title="粘贴授权"
      onClose={onClose}
      busy={submit.pending}
      footer={
        <ModalActions note={missing}>
          <Button onClick={onClose} disabled={submit.pending}>取消</Button>
          <Button variant="primary" loading={submit.pending} disabled={!canSubmit} onClick={submit.run}>
            {canSubmit ? `授权 ${valid.length} 个节点给 ${userIds.length} 个用户` : '授权'}
          </Button>
        </ModalActions>
      }
    >
      <ResourceView resource={options}>
        {({ users }) => (
          <div className="flex flex-col gap-4">
            <Field
              label="授权文本"
              hint="每行一个节点：节点名 | max=规则配额 | quota=流量配额GB | rate=限速MB/s。可以直接粘贴用户详情页“复制授权”得到的文本。"
            >
              <Textarea
                mono
                rows={8}
                value={text}
                onChange={setText}
                placeholder={'gateway-hk | max=10 | quota=5GB | rate=10\nrelay-jp | max=20'}
                autoFocus
              />
            </Field>
            <Field label="目标用户" hint={users.length === 0 ? '还没有普通用户，先新建用户。' : undefined}>
              <Select multiple searchable selectAll value={userIds} onChange={setUserIds} options={users.map(u => ({ value: u.id, label: u.username }))} placeholder="选择用户" />
            </Field>
            <div className="flex flex-col gap-1.5">
              <Checkbox checked={applySettings} onChange={setApplySettings}>套用文本里的规则配额、流量配额与限速</Checkbox>
              <p className="text-caption text-ink-mut">
                {applySettings
                  ? '已有授权会被改成文本里的值；文本里没写的项按默认值写入（规则配额 10、流量配额不限、不限速）。'
                  : '只为还没有授权的节点新建授权（规则配额 10、流量配额不限、不限速）；已有授权保持不变。'}
              </p>
            </div>
            {rows.length > 0 && (
              <TableBox className="rounded-control border border-line">
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>节点</th>
                      {applySettings && <><th className="num">规则配额</th><th className="num">流量配额</th><th className="num">限速</th></>}
                      <th>状态</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map(row => (
                      <tr key={row.name}>
                        <td className="font-mono">{row.name}</td>
                        {applySettings && (
                          <>
                            <td className="num">{row.maxForwards}</td>
                            <td className="num">{row.quotaGB ? `${row.quotaGB} GB` : '不限'}</td>
                            <td className="num">{row.rateMBytes ? `${row.rateMBytes} MB/s` : '不限'}</td>
                          </>
                        )}
                        <td>
                          {row.found
                            ? <StatusDot tone="ok">可授权</StatusDot>
                            : <StatusDot tone="danger">没有这个节点</StatusDot>}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableBox>
            )}
          </div>
        )}
      </ResourceView>
    </Modal>
  )
}
