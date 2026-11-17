import { useState } from 'react'
import { useLocation, useNavigate, useParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { enrichRule, hostPortKey } from '../../lib/landing'
import { ModeTag, NodeTypeIcon, ProtoTag, SuppressedBadge } from '../../components/net'
import { Page, PageHeader, TableBox } from '../../components/page'
import { useBlur } from '../../components/prefs'
import { ResourceView } from '../../components/ResourceView'
import { RULE_SCOPES, RuleDialog, isPort, useDeleteRule } from '../../components/RuleFormModal'
import { RuleEntryCard, RuleExit, useRulesWithCatalog } from '../../components/RulesTable'
import { useUser } from '../../components/session'
import {
  Button, Card, CardHeader, CardSection, DescItem, DescList, Empty, Input, SensText, TextLink, useAction, useToast,
} from '../../components/ui'

const SCOPE = RULE_SCOPES.admin

export default function RulesDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const location = useLocation()
  const { user } = useUser()
  // 列表页进入详情时带来它当时的搜索与筛选；返回与删除后都回到那个列表。
  const backTo = `/rules${location.state?.rulesQuery || ''}`

  // 绑定关系是全局的一张图，编辑表单的线路层级联要用它，随规则一起取。
  const { resource, reload } = useRulesWithCatalog(user, async () => {
    const [detail, graph] = await Promise.all([api.get(`/rules/${id}`), api.get('/node-bindings')])
    return { ...detail, bindings: graph.bindings || [] }
  }, [id])
  const [editing, setEditing] = useState(false)
  const deletion = useDeleteRule(SCOPE, () => navigate(backTo))

  const loaded = resource.state.status === 'ready' ? resource.state.data : null
  const rule = loaded ? enrichRule(loaded[0].rule, loaded[1].landingByKey) : null

  return (
    <Page>
      <PageHeader
        title={rule ? rule.name : '规则'}
        meta={rule ? <><span className="font-mono">#{rule.id}</span><SuppressedBadge reason={rule.suppressed} detailed /></> : null}
        back={{ to: backTo, label: '规则' }}
        actions={rule && (
          <>
            <Button danger loading={deletion.pending} onClick={() => deletion.request(rule)}>删除规则</Button>
            <Button variant="primary" onClick={() => setEditing(true)}>编辑规则</Button>
          </>
        )}
      />
      <ResourceView resource={resource} notFound="规则不存在">
        {([data, catalog]) => {
          const nodeById = data.node_by_id || {}
          const node = nodeById[rule.node_id]
          const hops = data.hops || []
          return (
            <>
              <RuleEntryCard rule={rule} entryNodeName={nodeById[rule.entry_node_id]?.name} />

              <Card>
                <CardHeader title="规则信息" />
                <CardSection>
                  <DescList>
                    {rule.owner_id?.Valid && (
                      <DescItem label="所有者">
                        <TextLink to={`/users/${rule.owner_id.Int64}`}>{rule.owner_name}</TextLink>
                      </DescItem>
                    )}
                    <DescItem label="入口节点">
                      {node ? (
                        <span className="inline-flex items-center gap-1.5">
                          <NodeTypeIcon type={node.node_type} />
                          <TextLink to={`/nodes/${node.id}`}>{node.name}</TextLink>
                        </span>
                      ) : `#${rule.node_id}`}
                    </DescItem>
                    <DescItem label="协议"><ProtoTag proto={rule.proto} /></DescItem>
                    <DescItem label="出口"><RuleExit rule={rule} scope={SCOPE} /></DescItem>
                    <DescItem label="流量">{fmtBytes(rule.total_bytes || 0)}</DescItem>
                    {rule.comment && <DescItem label="备注">{rule.comment}</DescItem>}
                  </DescList>
                </CardSection>
              </Card>

              <Card>
                <CardHeader title="各跳状态" description={hops.length ? `共 ${hops.length} 跳` : undefined} />
                {hops.length === 0 ? (
                  <Empty title="还没有跳数据" desc="规则下发到节点之后，这里会列出链路上每一跳的监听端口、转发目标与流量。" />
                ) : (
                  <HopsTable ruleId={rule.id} hops={hops} nodeById={nodeById} onChanged={reload} />
                )}
              </Card>

              {editing && (
                <RuleDialog
                  dialog={{ kind: 'edit', rule }}
                  scope={SCOPE}
                  nodes={data.nodes || []}
                  bindings={data.bindings}
                  catalog={catalog}
                  userId={user.id}
                  onSaved={reload}
                  onClose={() => setEditing(false)}
                />
              )}
            </>
          )
        }}
      </ResourceView>
    </Page>
  )
}

function HopsTable({ ruleId, hops, nodeById, onChanged }) {
  const blurred = useBlur()
  return (
    <TableBox>
      <table className="tbl">
        <thead>
          <tr>
            <th>#</th>
            <th>节点</th>
            <th>所属逻辑节点</th>
            <th>监听端口</th>
            <th>转发目标</th>
            <th>模式</th>
            <th className="num">流量</th>
            <th className="num">更换端口</th>
          </tr>
        </thead>
        <tbody>
          {hops.map(h => (
            <tr key={h.position}>
              <td className="text-ink-mut">{h.position + 1}</td>
              <td>
                <TextLink to={`/nodes/${h.node_id}`} tone="ink" className="font-medium">
                  {nodeById[h.node_id]?.name || `#${h.node_id}`}
                </TextLink>
              </td>
              <td className="text-ink-soft">{nodeById[h.via_node_id]?.name || `#${h.via_node_id}`}</td>
              <td className="font-mono">:{h.listen_port}</td>
              <td className="font-mono">
                <SensText blurred={blurred}>{h.target_host ? hostPortKey(h.target_host, h.target_port) : '--'}</SensText>
              </td>
              <td><ModeTag mode={h.mode} /></td>
              <td className="num text-ink-soft">{fmtBytes(h.total_bytes)}</td>
              <td className="num">
                {/* 以当前端口为 key：刷新后端口变了的那一跳，输入框从新端口重新
                    开始；端口没变的行保持原样，包括输入到一半的内容。 */}
                <ReallocateForm key={h.listen_port} ruleId={ruleId} position={h.position} current={h.listen_port} onDone={onChanged} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </TableBox>
  )
}

/* 给某一跳换监听端口。输入框预填当前端口，方便在原值上微调；清空提交时
   请求里不带端口，由服务端另行分配。 */
function ReallocateForm({ ruleId, position, current, onDone }) {
  const toast = useToast()
  const [port, setPort] = useState(current ? String(current) : '')
  const valid = port === '' || isPort(port)
  const reallocate = useAction(async () => {
    await api.post(`/rules/${ruleId}/hops/${position}/reallocate`, { port: port ? Number(port) : undefined })
    toast('端口已更换')
    await onDone()
  })

  return (
    <form className="inline-flex items-center gap-1.5" onSubmit={e => { e.preventDefault(); if (valid) reallocate.run() }}>
      <Input
        size="sm"
        mono
        type="number"
        min={1}
        max={65535}
        value={port}
        onChange={setPort}
        invalid={!valid}
        placeholder="随机"
        aria-label={`第 ${position + 1} 跳的监听端口，1–65535，留空随机分配`}
        className="w-24"
      />
      <Button size="sm" type="submit" loading={reallocate.pending} disabled={!valid}>换端口</Button>
    </form>
  )
}
