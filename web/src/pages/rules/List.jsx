import { useSearchParams } from 'react-router-dom'
import { api } from '../../lib/api'
import { RULE_SCOPES } from '../../components/RuleFormModal'
import { RulesListPage } from '../../components/RulesTable'
import { Button, IconX, Select } from '../../components/ui'

/* 绑定关系是全局的一张图（不按规则的所有者划分），随规则列表一起取：创建 /
   编辑表单的线路层级联要用它。两个请求任一失败，整页按加载失败处理。 */
async function loadRules() {
  const [list, graph] = await Promise.all([api.get('/rules'), api.get('/node-bindings')])
  return {
    rules: list.rules || [],
    nodes: list.nodes || [],
    users: list.users || [],
    bindings: graph.bindings || [],
  }
}

/** 读取 URL 里逗号分隔的 id 列表参数。 */
function idList(params, name) {
  return (params.get(name) || '').split(',').filter(Boolean).map(Number).filter(Number.isInteger)
}

export default function RulesList() {
  // 搜索与筛选放在 URL 里而不是组件状态里：进入某条规则的详情页再返回时，
  // 列表页会重新挂载，组件状态留不住。
  const [searchParams, setSearchParams] = useSearchParams()
  const search = searchParams.get('q') || ''
  const owners = idList(searchParams, 'owners')
  const nodeIds = idList(searchParams, 'nodes')
  // 一次改多个参数必须合成一次调用：同一轮渲染里先后两次 setSearchParams，
  // 各自都从这一轮的参数出发，后一次会把前一次的修改盖掉。
  const updateParams = (patch) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      for (const [k, v] of Object.entries(patch)) {
        if (v) next.set(k, v)
        else next.delete(k)
      }
      return next
    }, { replace: true })
  }
  const query = searchParams.toString()

  return (
    <RulesListPage
      scope={RULE_SCOPES.admin}
      title="规则"
      load={loadRules}
      search={search}
      onSearch={q => updateParams({ q })}
      onReset={() => updateParams({ q: '', owners: '', nodes: '' })}
      // 所有者与入口节点的筛选在本地做：列表已经取回了全部规则。
      narrow={rules => rules.filter(rule =>
        (owners.length === 0 || (rule.owner_id?.Valid && owners.includes(rule.owner_id.Int64)))
        && (nodeIds.length === 0 || nodeIds.includes(rule.node_id)))}
      filters={data => (
        <>
          {data.users.length > 0 && (
            <Select
              multiple
              searchable
              className="w-36"
              title="按所有者筛选"
              placeholder="全部用户"
              value={owners}
              onChange={ids => updateParams({ owners: ids.join(',') })}
              options={data.users.map(u => ({ value: u.id, label: u.username }))}
            />
          )}
          {data.nodes.length > 0 && (
            <Select
              multiple
              searchable
              className="w-36"
              title="按入口节点筛选"
              placeholder="全部节点"
              value={nodeIds}
              onChange={ids => updateParams({ nodes: ids.join(',') })}
              options={data.nodes.map(n => ({ value: n.id, label: n.name }))}
            />
          )}
          {(owners.length > 0 || nodeIds.length > 0) && (
            <Button variant="ghost" icon={<IconX />} onClick={() => updateParams({ owners: '', nodes: '' })}>清除筛选</Button>
          )}
        </>
      )}
      // 随链接带给详情页，它的“返回”据此回到带着同样搜索与筛选的列表。
      linkState={query ? { rulesQuery: `?${query}` } : undefined}
    />
  )
}
