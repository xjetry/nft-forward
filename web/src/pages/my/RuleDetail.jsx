import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { fmtBytes } from '../../lib/fmt'
import { enrichRule } from '../../lib/landing'
import { ProtoTag } from '../../components/net'
import { Page, PageHeader } from '../../components/page'
import { ResourceView } from '../../components/ResourceView'
import { RULE_SCOPES, RuleDialog, useDeleteRule } from '../../components/RuleFormModal'
import { RuleEntryCard, RuleExit, useRulesWithCatalog } from '../../components/RulesTable'
import { useUser } from '../../components/session'
import { Button, Card, CardHeader, CardSection, DescItem, DescList } from '../../components/ui'

const SCOPE = RULE_SCOPES.my

/* 规则实际经过的节点，用 “a → b → c” 表示。
   优先用服务端给的 chain：它是已部署的物理路径（组合节点已展开成成员），
   与实际运行的一致。规则还没有下发、chain 为空时，退回到“入口 + 线路层”的
   逻辑路径。名称只从 nodeById（授权范围内的节点）解析；线路层里解析不出
   名称的节点不显示，不把用户认不出的 id 露出来。 */
function chainLabel(rule, nodeById) {
  const deployed = (rule.chain || []).map(c => c.name).filter(Boolean)
  if (deployed.length) return deployed.join(' → ')
  const entry = nodeById[rule.node_id]?.name || `#${rule.node_id}`
  const layers = (rule.via_node_ids || []).map(id => nodeById[id]?.name).filter(Boolean)
  return [entry, ...layers].join(' → ')
}

export default function MyRuleDetail() {
  const { id } = useParams()
  const navigate = useNavigate()
  const { user } = useUser()

  // 单条规则的接口不带绑定关系（授权节点之间的边只有列表接口会给），而编辑
  // 表单的线路层级联要用它，所以连同列表接口一起取。
  const { resource, reload } = useRulesWithCatalog(user, async () => {
    const [detail, list] = await Promise.all([api.get(`/my/rules/${id}`), api.get('/my/rules')])
    return { ...detail, bindings: list.bindings || [] }
  }, [id])
  const [editing, setEditing] = useState(false)
  const deletion = useDeleteRule(SCOPE, () => navigate('/my/rules'))

  const loaded = resource.state.status === 'ready' ? resource.state.data : null
  const rule = loaded ? enrichRule(loaded[0].rule, loaded[1].landingByKey) : null

  return (
    <Page>
      <PageHeader
        title={rule ? rule.name : '规则'}
        back={{ to: '/my/rules', label: '我的规则' }}
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
          return (
            <>
              <RuleEntryCard rule={rule} entryNodeName={nodeById[rule.entry_node_id]?.name} />

              <Card>
                <CardHeader title="规则信息" />
                <CardSection>
                  <DescList>
                    <DescItem label="线路">{chainLabel(rule, nodeById)}</DescItem>
                    <DescItem label="协议"><ProtoTag proto={rule.proto} /></DescItem>
                    <DescItem label="出口"><RuleExit rule={rule} scope={SCOPE} /></DescItem>
                    {data.show_rate && <DescItem label="倍率"><span className="font-mono">×{rule.rate_multiplier ?? 1}</span></DescItem>}
                    <DescItem label="流量">{fmtBytes(rule.total_bytes || 0)}</DescItem>
                    {rule.comment && <DescItem label="备注">{rule.comment}</DescItem>}
                  </DescList>
                </CardSection>
              </Card>

              {editing && (
                <RuleDialog
                  dialog={{ kind: 'edit', rule }}
                  scope={SCOPE}
                  nodes={data.nodes || []}
                  bindings={data.bindings}
                  catalog={catalog}
                  username={user.username}
                  showRate={!!data.show_rate}
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
