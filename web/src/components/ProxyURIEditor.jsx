import { useCallback, useMemo, useState } from 'react'
import { api } from '../lib/api'
import {
  ROLE_DIRECT, ROLE_LANDING, hostPortKey, loadLocalNodes, loadLocalURIs, loadSubURLs, nodeHasRole, rolesFirstOrder,
  saveLocalRoles, saveLocalURIs, saveSubCache, saveSubURLs, setLocalRoleBatch, toggleLocalRole, tryParseURI,
} from '../lib/landing'
import { useLandingCatalog, useLocalLandingVersion } from '../lib/useLandingCatalog'
import { useBlur } from './prefs'
import { ResourceView } from './ResourceView'
import { useUser } from './session'
import {
  Alert, Button, Card, CardHeader, CardSection, Checkbox, Collapsible, Field, SensText, Tag, Textarea, ToggleChip,
  useAction, useToast,
} from './ui'

/* 用户自己的代理节点的编辑器：两类来源（订阅地址、手动填写的链接），以及
   每个节点的用途（落地 / 直连）。

   这些数据都在本浏览器里（lib/landing.js 的 load* / save*），组件不保留它们
   的副本：
   - 已保存的内容每次从存储读出，并随 useLocalLandingVersion 的变化重读；
   - 文本框只有在用户动过之后才有自己的草稿，没动过时直接显示已保存的内容：
     另一个标签页保存的修改会立刻显示在这里，而不是留着一份旧副本、等下次
     保存时把它盖回去；
   - 节点与角色取自 useLandingCatalog，与落地节点页、我的代理页是同一份目录。 */

const ROLES = [
  { bit: ROLE_LANDING, label: '落地' },
  { bit: ROLE_DIRECT, label: '直连' },
]

/* 目录按地址（hostPortKey）去重，所以这个键在目录里唯一，用它标识一行。 */
const rowKey = n => hostPortKey(n.host, n.port)

/* 目录把本浏览器的两类节点合并去重后只标 source: 'local'，不区分手动与订阅。
   合并顺序是 手动 → 订阅 → 管理员分配（buildCatalog），同地址保留先出现的，
   所以地址出现在手动列表里的本地节点一定来自手动列表，其余的来自订阅。 */
function splitLocalNodes(all, manualNodes) {
  const manualKeys = new Set(manualNodes.map(rowKey))
  const manual = []
  const sub = []
  for (const n of all) {
    if (n.source !== 'local') continue
    ;(manualKeys.has(rowKey(n)) ? manual : sub).push(n)
  }
  return { manual, sub }
}

/* 订阅地址：每行一个，忽略空行与 # 开头的行；同一个地址只抓一次。 */
function subscriptionURLs(text) {
  const lines = text.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
  return [...new Set(lines)]
}

/* 手动填写的内容里无法识别成节点的行号（从 1 起）。判定与 parseURIs 用的是
   同一个解析函数，所以“被标出的行”恰好就是“没有计入节点的行”。 */
function unparsedLines(text) {
  const out = []
  text.split('\n').forEach((raw, i) => {
    const line = raw.trim()
    if (line && !line.startsWith('#') && !tryParseURI(line)) out.push(i + 1)
  })
  return out
}

function describeLines(lines) {
  const SHOWN = 8
  if (lines.length <= SHOWN) return `第 ${lines.join('、')} 行`
  return `第 ${lines.slice(0, SHOWN).join('、')} 等 ${lines.length} 行`
}

/**
 * 记住文本框被拖出来的高度（存在本浏览器的 `key` 下），下次出现时恢复。
 *
 * 返回一个 ref 回调而不是 ref 对象：文本框在折叠区里，展开才挂载、收起就
 * 卸载。观察器的生命周期必须跟着元素走 —— 元素挂上时建立、卸下或 key 变化
 * 时由回调返回的清理函数断开。
 *
 * 高度写在元素的 style.height 上：浏览器的拖拽手柄改的就是这个属性，恢复时
 * 写同一个属性，两者不会各管一个高度。
 */
function usePersistedHeight(key) {
  return useCallback((el) => {
    if (!el) return undefined
    let saved = 0
    try { saved = Number(localStorage.getItem(key)) } catch { /* 读不到就用默认高度 */ }
    if (saved > 0) el.style.height = `${saved}px`
    const observer = new ResizeObserver(() => {
      const height = el.offsetHeight
      if (height <= 0) return
      try { localStorage.setItem(key, String(height)) } catch { /* 存不下不影响编辑 */ }
    })
    observer.observe(el)
    return () => observer.disconnect()
  }, [key])
}

/**
 * “我添加的节点”卡片。自己取当前用户与脱敏偏好。
 * @param {{ fill?: boolean }} props `fill` 让卡片占满父容器的剩余高度，内容在卡片里滚动。
 */
export function ProxyURIEditor({ fill = false }) {
  const { user } = useUser()
  // 本地数据按用户名分开存放。草稿、勾选、展开状态都属于某一个用户名下的
  // 数据，用户名变了就整体重建，上一个用户名下没保存的草稿不会被存到新的名下。
  return <Editor key={user.username} user={user} fill={fill} />
}

function Editor({ user, fill }) {
  const username = user.username
  const blurred = useBlur()
  const toast = useToast()
  const catalog = useLandingCatalog(user)
  const version = useLocalLandingVersion()
  const saved = useMemo(
    () => ({ uris: loadLocalURIs(username), subURLs: loadSubURLs(username), ...loadLocalNodes(username) }),
    // version 不参与计算：它变化表示存储里的内容变了，需要重读。
    [username, version],
  )

  // 草稿为 null 表示用户没有动过文本框，显示的就是已保存的内容。
  const [uriDraft, setUriDraft] = useState(/** @type {string | null} */ (null))
  const [subDraft, setSubDraft] = useState(/** @type {string | null} */ (null))
  const uriText = uriDraft ?? saved.uris
  const subText = subDraft ?? saved.subURLs
  const uriDirty = uriText !== saved.uris
  const subDirty = subText !== saved.subURLs

  // 两个折叠区互斥展开，卡片里同一时刻至多一张长列表。初始展开有内容的
  // 那个，订阅优先。
  const [expanded, setExpanded] = useState(() => {
    if (saved.subURLs.trim() !== '' || saved.sub.length > 0) return 'sub'
    if (saved.uris.trim() !== '') return 'manual'
    return null
  })
  const toggle = section => setExpanded(current => (current === section ? null : section))

  // 最近一次订阅更新里失败的订阅。kept 说明节点列表现在是什么：
  // 'fetched' 其余订阅本次抓到的节点；'previous' 上一次的结果。
  const [subReport, setSubReport] = useState(/** @type {{ kept: 'fetched' | 'previous', failed: Array<{ url: string, message: string }> } | null} */ (null))

  const subHeightRef = usePersistedHeight(`nf-sub-textarea-h:${username}`)
  const manualHeightRef = usePersistedHeight(`nf-manual-textarea-h:${username}`)

  const catalogData = catalog.state.status === 'ready' ? catalog.state.data : null
  const rows = useMemo(
    () => (catalogData ? splitLocalNodes(catalogData.all, saved.manual) : null),
    [catalogData, saved.manual],
  )

  const urls = subscriptionURLs(subText)
  // 文本框里没有订阅地址时，按钮做的事取决于有没有东西可清：已保存的地址，
  // 或上次抓到的节点。
  const canClearSub = subscriptionURLs(saved.subURLs).length > 0 || saved.sub.length > 0
  const skippedLines = unparsedLines(uriText)

  const saveManual = useAction(async () => {
    saveLocalURIs(username, uriText)
    setUriDraft(null)
    toast(skippedLines.length ? `已保存，其中 ${skippedLines.length} 行无法识别，没有计入节点` : '已保存到本浏览器')
  })

  /* 订阅缓存是一张不分来源的节点表，saveSubCache 整表覆盖。一次更新因此只有
     三种结局：
     - 没有订阅地址：清空缓存。否则删掉地址之后，上次抓到的节点会一直留着。
     - 至少一条抓取成功：缓存换成本次抓到的节点。缓存里不记录节点来自哪条
       订阅，失败的那几条订阅的旧节点没法单独留下，所以把失败的订阅逐条列出。
     - 全部失败：不写缓存，列表保持上一次的结果。 */
  const refreshSubs = useAction(async () => {
    saveSubURLs(username, subText)
    setSubDraft(null)
    if (urls.length === 0) {
      saveSubCache(username, [])
      setSubReport(null)
      toast(canClearSub ? '订阅已清空，订阅来的节点已移除' : '已保存到本浏览器')
      return
    }
    const fetched = []
    const failed = []
    for (const url of urls) {
      try {
        const res = await api.post('/sub-fetch', { url })
        fetched.push(...(res?.nodes || []))
      } catch (err) {
        failed.push({ url, message: err instanceof Error ? err.message : '请求失败' })
      }
    }
    if (failed.length === urls.length) {
      setSubReport({ kept: 'previous', failed })
      toast('订阅没有更新成功，节点列表保持上次的结果', 'error')
      return
    }
    saveSubCache(username, fetched)
    setSubReport(failed.length ? { kept: 'fetched', failed } : null)
    if (failed.length) toast(`${failed.length} 条订阅没有更新成功，其余共 ${fetched.length} 个节点`, 'error')
    else toast(`订阅已更新，共 ${fetched.length} 个节点`)
  })

  /* 一张来源表的节点与角色。角色依赖服务端的角色表（目录的一部分）：它没有
     加载出来时不显示节点、也就无从编辑角色，上面的文本框与保存不受影响。

     角色写进本地覆盖表。新表由目录给出的“服务端表 + 当前覆盖表”算出
     （toggleLocalRole / setLocalRoleBatch 从生效值出发），saveLocalRoles 写入
     后发出变更事件，目录随之重新推导，下一次点击拿到的就是新的覆盖表。 */
  const nodeList = (section, foldedNote) => {
    if (saved[section].length === 0) return null
    return (
      <div className="flex flex-col gap-2">
        <h3 className="text-small font-medium text-ink-soft">节点与用途</h3>
        <ResourceView resource={catalog}>
          {data => (
            <NodeRoleList
              nodes={rows ? rows[section] : []}
              total={saved[section].length}
              foldedNote={foldedNote}
              roles={data.roles}
              blurred={blurred}
              onToggle={(node, bit) => saveLocalRoles(username, toggleLocalRole(data.serverRoles, data.localRoles, node, bit))}
              onBatch={(nodes, bit, on) => saveLocalRoles(username, setLocalRoleBatch(data.serverRoles, data.localRoles, nodes, bit, on))}
            />
          )}
        </ResourceView>
      </div>
    )
  }

  const summaryOf = (section, hasText) => {
    if (!rows) return null
    const count = rows[section].length
    return count > 0 || hasText ? `${count} 个节点` : '未添加'
  }

  return (
    <Card fill={fill}>
      <CardHeader title="我添加的节点" description={rows ? `共 ${rows.manual.length + rows.sub.length} 个` : undefined} />
      <div className={fill ? 'min-h-0 flex-1 overflow-y-auto' : undefined}>
        <CardSection>
          <p className="text-small text-ink-soft">
            在这里添加你自己的代理节点，并标记每个节点的用途：“落地”可以选作规则的出口，“直连”会出现在“我的代理”里，两者可以同时标记。与管理员分配的节点地址相同时，以你添加的为准。
          </p>
        </CardSection>

        <CardSection>
          <Collapsible title="订阅" summary={summaryOf('sub', saved.subURLs.trim() !== '')} open={expanded === 'sub'} onToggle={() => toggle('sub')}>
            <div className="flex flex-col gap-3">
              <Field
                label="订阅地址"
                hint="每行一个。地址保存在本浏览器；更新时会发送给面板，由面板下载订阅内容。下载到的节点只保存在本浏览器。"
              >
                <Textarea
                  ref={subHeightRef}
                  mono
                  rows={2}
                  value={subText}
                  onChange={setSubDraft}
                  placeholder="https://example.com/subscribe?token=…"
                  spellCheck={false}
                  className="min-h-16 max-h-105"
                />
              </Field>
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  loading={refreshSubs.pending}
                  disabled={urls.length === 0 && !canClearSub && !subDirty}
                  onClick={() => refreshSubs.run()}
                >
                  {urls.length > 0 ? '保存并更新订阅' : canClearSub ? '清空订阅' : '保存'}
                </Button>
                {subDirty && <span className="text-caption text-ink-mut">有未保存的修改</span>}
              </div>
              {subReport && (
                <Alert tone={subReport.kept === 'previous' ? 'danger' : 'warn'}>
                  <p>
                    {subReport.kept === 'previous'
                      ? '订阅没有更新成功，节点列表保持上次的结果：'
                      : '下面的订阅没有更新成功，节点列表里现在只有其余订阅的节点：'}
                  </p>
                  <ul className="mt-1 flex flex-col gap-1">
                    {subReport.failed.map(f => (
                      <li key={f.url} className="break-all">
                        <span className="font-mono"><SensText blurred={blurred}>{f.url}</SensText></span>
                        ：{f.message}
                      </li>
                    ))}
                  </ul>
                </Alert>
              )}
              {nodeList('sub', '个节点的地址与手动填写的节点、或排在前面的订阅节点相同，只保留先出现的那个（手动填写的优先）。')}
            </div>
          </Collapsible>
        </CardSection>

        <CardSection>
          <Collapsible title="手动填写" summary={summaryOf('manual', saved.uris.trim() !== '')} open={expanded === 'manual'} onToggle={() => toggle('manual')}>
            <div className="flex flex-col gap-3">
              <Field
                label="节点链接"
                hint="每行一条：vless://、trojan://、ss://、vmess://、hysteria2:// 等链接，或 snell 配置行；# 开头的行会被忽略。只保存在本浏览器。"
                error={skippedLines.length ? `${describeLines(skippedLines)}无法识别，不会计入节点。` : undefined}
              >
                <Textarea
                  ref={manualHeightRef}
                  mono
                  rows={3}
                  value={uriText}
                  onChange={setUriDraft}
                  placeholder={'vless://…\ntrojan://…\n名称 = snell, 地址, 端口, psk = …, version = 5'}
                  spellCheck={false}
                  className="min-h-20 max-h-105"
                />
              </Field>
              <div className="flex flex-wrap items-center gap-3">
                <Button loading={saveManual.pending} disabled={!uriDirty} onClick={() => saveManual.run()}>保存链接</Button>
                {uriDirty && <span className="text-caption text-ink-mut">有未保存的修改</span>}
              </div>
              {nodeList('manual', '条链接的地址与排在前面的链接相同，只保留先出现的那条。')}
            </div>
          </Collapsible>
        </CardSection>
      </div>
    </Card>
  )
}

/* 一组用途开关。落地与直连互相独立：一个节点可以既作规则出口，又出现在
   “我的代理”里。 */
function RoleChips({ has, onChange }) {
  return (
    <span className="flex flex-none items-center gap-1.5">
      {ROLES.map(({ bit, label }) => (
        <ToggleChip key={bit} pressed={has(bit)} onChange={on => onChange(bit, on)}>{label}</ToggleChip>
      ))}
    </span>
  )
}

/**
 * 一张来源表里的节点，每行可以标记用途；勾选多行后可以一起标记。
 *
 * 勾选记的是行的地址键而不是行的位置：标记用途会让行重新排序（有用途的
 * 排在前面），按键记录时重排不会改变被勾选的是哪些节点。
 *
 * @param {{
 *   nodes: any[],
 *   total: number,
 *   foldedNote: string,
 *   roles: Record<string, number>,
 *   blurred: boolean,
 *   onToggle: (node: any, bit: number) => void,
 *   onBatch: (nodes: any[], bit: number, on: boolean) => void,
 * }} props `total` 是这个来源里解析出的节点数；比 `nodes` 多出来的是因为地址
 * 重复而没有进入目录的，`foldedNote` 说明它们去了哪里。
 */
function NodeRoleList({ nodes, total, foldedNote, roles, blurred, onToggle, onBatch }) {
  const [selected, setSelected] = useState(() => new Set())
  const ordered = useMemo(
    () => rolesFirstOrder(nodes, n => nodeHasRole(roles, n, ROLE_LANDING | ROLE_DIRECT)).map(i => nodes[i]),
    [nodes, roles],
  )
  // 列表内容变化后，已经不在列表里的键不算数。
  const chosen = nodes.filter(n => selected.has(rowKey(n)))
  const allChosen = nodes.length > 0 && chosen.length === nodes.length
  const folded = total - nodes.length

  const toggleOne = key => setSelected(prev => {
    const next = new Set(prev)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    return next
  })
  const countWith = bit => nodes.filter(n => nodeHasRole(roles, n, bit)).length
  const unmarked = nodes.filter(n => !nodeHasRole(roles, n, ROLE_LANDING | ROLE_DIRECT)).length

  return (
    <div className="flex flex-col gap-2">
      {nodes.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <Checkbox checked={allChosen} onChange={on => setSelected(on ? new Set(nodes.map(rowKey)) : new Set())}>全选</Checkbox>
          <span className="text-caption text-ink-mut">
            {nodes.length} 个节点 · {countWith(ROLE_LANDING)} 落地 · {countWith(ROLE_DIRECT)} 直连 · {unmarked} 未标记
          </span>
          {chosen.length > 0 && (
            <span className="ml-auto flex items-center gap-2">
              <span className="text-caption text-ink-mut">已选 {chosen.length} 个</span>
              <RoleChips
                has={bit => chosen.every(n => nodeHasRole(roles, n, bit))}
                onChange={(bit, on) => onBatch(chosen, bit, on)}
              />
            </span>
          )}
        </div>
      )}
      {folded > 0 && <p className="text-caption text-ink-mut">另有 {folded} {foldedNote}</p>}
      {nodes.length > 0 && (
        <ul className="max-h-105 overflow-y-auto border-t border-line-soft">
          {ordered.map((n) => {
            const key = rowKey(n)
            return (
              <li key={key} className="flex items-center gap-3 border-b border-line-soft py-2 last:border-b-0">
                <Checkbox checked={selected.has(key)} onChange={() => toggleOne(key)} aria-label={`选择 ${n.name || key}`} />
                <div className="min-w-0 flex-1">
                  <div className="break-words text-small font-medium text-ink">{n.name || '未命名'}</div>
                  <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-caption text-ink-mut">
                    <Tag mono>{n.protocol}</Tag>
                    <span className="break-all font-mono"><SensText blurred={blurred}>{key}</SensText></span>
                  </div>
                </div>
                <RoleChips has={bit => nodeHasRole(roles, n, bit)} onChange={bit => onToggle(n, bit)} />
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
