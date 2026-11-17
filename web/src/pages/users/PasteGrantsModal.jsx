import { useState } from 'react'
import { api } from '../../lib/api'
import { GB } from '../../lib/fmt'
import { GRANT_DEFAULTS, parseGrantText } from '../../lib/grantText'
import { useResource } from '../../lib/useResource'
import { TableBox } from '../../components/page'
import { ResourceView } from '../../components/ResourceView'
import { Button, Checkbox, Field, Modal, ModalActions, Select, StatusDot, Textarea, useAction, useToast } from '../../components/ui'

function grantPayload(name, { maxRules, quotaGB, rateMBytes }) {
  return {
    node_name: name,
    max_forwards: maxRules,
    traffic_quota_bytes: Math.round(quotaGB * GB),
    rate_limit_mbytes: rateMBytes,
  }
}

/**
 * 把一段授权文本批量授权给若干用户。两种写法，都是一次 batch-apply 请求、
 * 在服务端的一个事务里写完：
 *
 * - 套用文本里的设置：每个 (用户, 节点) 都写成文本里的值，授权已存在时规则
 *   配额、流量配额、限速三项被覆盖。文本里有读不懂的设置时不能提交 —— 那
 *   一项写进去的就不是用户写的值了。
 * - 不套用：只给还没有这个节点的用户补上授权（按默认值），已有的授权不动
 *   （请求带 only_missing，由服务端在写入时判定有没有，不靠这里先读后写）。
 *
 * 预览表显示的是将要写入的值。
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
  const [userIds, setUserIds] = useState(initialUserIds)
  const [applySettings, setApplySettings] = useState(true)

  const nodes = options.state.status === 'ready' ? options.state.data.nodes : []
  // 同一个节点写了多行时，服务端按行依次写入，最后一行生效；预览与提交都只留它。
  const rows = [...new Map(parseGrantText(text, nodes).map(row => [row.name, row])).values()]
  const valid = rows.filter(row => row.found)
  // 读不懂的设置只在套用设置时有影响；不套用时文本里的设置整个被忽略。
  const unreadable = applySettings ? valid.filter(row => row.invalid.length > 0) : []

  // 一行将要写入的值：套用设置时是文本里解析出的，否则是默认值。
  const written = row => (applySettings ? row : GRANT_DEFAULTS)

  const submit = useAction(async () => {
    const d = await api.post('/grants/batch-apply', {
      user_ids: userIds,
      only_missing: !applySettings,
      grants: valid.map(row => grantPayload(row.name, written(row))),
    })
    const granted = d?.granted || 0
    const kept = d?.kept || 0
    const skipped = d?.skipped_nodes || []
    toast(kept > 0 ? `已写入 ${granted} 条授权，${kept} 条已有的授权保持不变` : `已写入 ${granted} 条授权`)
    if (skipped.length) toast(`${skipped.length} 个节点已不存在，没有授权：${skipped.join('、')}`, 'error')
    await onDone()
    onClose()
  })

  const canSubmit = valid.length > 0 && userIds.length > 0 && unreadable.length === 0
  // 提交按钮不可用时说明还差什么。
  const missing = options.state.status !== 'ready' || canSubmit ? null
    : rows.length === 0 ? '先粘贴授权文本。'
      : valid.length === 0 ? '文本里的节点都不存在，检查节点名是否与节点列表一致。'
        : unreadable.length > 0 ? `${unreadable.length} 个节点的设置读不懂，改对后才能套用。`
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
              hint="每行一个节点：节点名 | max=规则数上限（至少 1）| quota=流量配额GB | rate=限速MB/s。可以直接粘贴用户详情页“复制授权”得到的文本。"
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
              <Checkbox checked={applySettings} onChange={setApplySettings}>套用文本里的规则数上限、流量配额与限速</Checkbox>
              <p className="text-caption text-ink-mut">
                {applySettings
                  ? '已有授权会被改成文本里的值；文本里没写的项按默认值写入（规则数上限 10、流量配额不限、不限速）。'
                  : '只给还没有这些节点的用户补上授权，按默认值写入（规则数上限 10、流量配额不限、不限速）；已有的授权保持不变。'}
              </p>
            </div>
            {rows.length > 0 && (
              <TableBox className="rounded-control border border-line">
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>节点</th>
                      <th className="num">规则数上限</th>
                      <th className="num">流量配额</th>
                      <th className="num">限速</th>
                      <th>状态</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map(row => (
                      <tr key={row.name}>
                        <td className="font-mono">{row.name}</td>
                        <td className="num">{written(row).maxRules}</td>
                        <td className="num">{written(row).quotaGB ? `${written(row).quotaGB} GB` : '不限'}</td>
                        <td className="num">{written(row).rateMBytes ? `${written(row).rateMBytes} MB/s` : '不限'}</td>
                        <td>
                          {!row.found
                            ? <StatusDot tone="danger">没有这个节点</StatusDot>
                            : applySettings && row.invalid.length > 0
                              ? <StatusDot tone="danger">读不懂：<span className="font-mono">{row.invalid.join('、')}</span></StatusDot>
                              : <StatusDot tone="ok">可授权</StatusDot>}
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
