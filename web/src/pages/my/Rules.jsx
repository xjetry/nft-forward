import { useState } from 'react'
import { api } from '../../lib/api'
import { RULE_SCOPES } from '../../components/RuleFormModal'
import { RulesListPage } from '../../components/RulesTable'

/* 用户侧的列表接口一并带回授权范围内的节点、它们之间的绑定关系，以及是否
   向用户显示倍率。 */
async function loadRules() {
  const d = await api.get('/my/rules')
  return { rules: d.rules || [], nodes: d.nodes || [], bindings: d.bindings || [], show_rate: !!d.show_rate }
}

export default function MyRules() {
  const [search, setSearch] = useState('')
  return (
    <RulesListPage
      scope={RULE_SCOPES.my}
      title="我的规则"
      load={loadRules}
      search={search}
      onSearch={setSearch}
      onReset={() => setSearch('')}
    />
  )
}
