/* 代理 URI 的解析、端点改写，以及“代理节点目录”的推导 —— 服务端
   internal/landing 包在浏览器里的对应物。

   隐私边界：用户手动填写的代理 URI 只存在 localStorage，不会发给服务端。
   订阅地址不在这条边界内：抓取订阅要经过服务端的 /sub-fetch 代理，订阅
   URL 会到达服务端（抓回来的节点缓存仍只存在浏览器）。

   服务端负责解析管理员分配的落地节点；这里负责用户自己的节点，并把两者
   合并成一份目录，host:port 冲突时用户自己的优先。 */

import { api } from './api.js'
import { adoptUserData, browserStore, readSlot, writeSlot } from './userStore.ts'

/* 本地数据（URI、订阅、订阅缓存、本地标记）任何一项变化都发同一个事件。
   同一个标签页里的其它组件收不到原生的 storage 事件，靠它得知变化；
   useLandingCatalog 同时监听两者，所以所有读取方只有这一种同步方式。 */
function notifyChanged() {
  try { window.dispatchEvent(new Event('nf-landing-changed')) } catch { /* 非浏览器环境 */ }
}

/* 本模块对用户本地数据的读写都经过这两个函数，键与失败语义由 lib/userStore
   统一决定：读不到按没有处理，写不进去会抛出 StoreWriteError，由发起保存的
   界面告诉用户。写入成功后发变更事件。 */
const read = (userId, slot) => (userId == null ? null : readSlot(browserStore(), userId, slot))
function write(userId, slot, value) {
  writeSlot(browserStore(), userId, slot, value)
  notifyChanged()
}

/** 读出 JSON 形式的一项；没有或解析不了时返回 null。 */
function readJSON(userId, slot) {
  const raw = read(userId, slot)
  if (raw === null) return null
  try { return JSON.parse(raw) } catch { return null }
}

export function loadLocalURIs(userId) {
  return read(userId, 'landing-uris') || ''
}

export function saveLocalURIs(userId, text) {
  write(userId, 'landing-uris', text.trim() ? text : null)
}

export function hasLocalURIs(userId) {
  return loadLocalURIs(userId).trim() !== ''
}

export function hasLocalProxies(userId) {
  return hasLocalURIs(userId) || loadSubURLs(userId).trim() !== ''
}

export function loadSubURLs(userId) {
  return read(userId, 'sub-urls') || ''
}

export function saveSubURLs(userId, text) {
  write(userId, 'sub-urls', text.trim() ? text : null)
}

/** 订阅文本框的内容 → 订阅地址名单：每行一个，去掉空行、# 注释行与重复项。 */
export function subscriptionURLs(text) {
  const lines = text.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
  return [...new Set(lines)]
}

/* 订阅缓存：每条订阅地址上一次成功抓到的节点，按地址分开存放
   （{ [订阅地址]: 节点[] }）。

   不变式：一条订阅的节点跟着这条订阅走 —— 地址在名单里，节点就在（抓取成功
   换成新的，失败留着上一次的）；地址不在名单里，它的节点也不在。唯一的写入
   口径是 nextSubCache，所以不存在“地址已经换了、节点还是旧地址的”这种
   状态。 */

/** @returns {Record<string, any[]>} */
export function loadSubCache(userId) {
  const value = readJSON(userId, 'sub-cache')
  return value && typeof value === 'object' && !Array.isArray(value) ? value : {}
}

/** @param {number} userId @param {Record<string, any[]>} cache */
export function saveSubCache(userId, cache) {
  write(userId, 'sub-cache', Object.keys(cache).length ? JSON.stringify(cache) : null)
}

/**
 * 订阅名单与一次抓取的结果 → 新的缓存。纯函数。
 *
 * 名单里的每个地址：本次抓到了就用新节点（抓到零个也算抓到），没抓到就沿用
 * 它在 previous 里的节点；名单之外的地址不进入结果。传空的 fetched 就是
 * “只按名单裁剪”。
 *
 * @param {Record<string, any[]>} previous
 * @param {string[]} urls 订阅地址名单
 * @param {Map<string, any[]>} fetched 本次抓取成功的地址 → 节点
 * @returns {Record<string, any[]>}
 */
export function nextSubCache(previous, urls, fetched) {
  const entries = []
  for (const url of urls) {
    // 地址是用户输入的任意字符串，只认 previous 自己的属性，不沿原型链找。
    const nodes = fetched.get(url) ?? (Object.hasOwn(previous, url) ? previous[url] : undefined)
    if (nodes) entries.push([url, nodes])
  }
  return Object.fromEntries(entries)
}

/**
 * 把一个用户的数组形式的订阅缓存改写成按地址分开的形式。
 *
 * 浏览器里可能还存着数组形式的缓存：一张不记录来源的节点表。它的节点全部
 * 记到名单里第一条订阅名下 —— 只有一条订阅时这就是事实；有多条时，其余
 * 订阅在下一次更新成功后各自拿到自己的节点，地址相同的由 buildCatalog 去重。
 * 名单为空时这些节点没有所属的订阅，按上面的不变式丢掉。
 *
 * @param {import('./userStore.ts').KeyValueStore} store
 * @param {number} userId
 */
export function migrateSubCache(store, userId) {
  let value
  // 解析不了的内容不动它：loadSubCache 会把它读成空缓存。
  try { value = JSON.parse(readSlot(store, userId, 'sub-cache') || 'null') } catch { return }
  if (!Array.isArray(value)) return
  const [first] = subscriptionURLs(readSlot(store, userId, 'sub-urls') || '')
  const next = first === undefined || value.length === 0 ? null : JSON.stringify(Object.fromEntries([[first, value]]))
  writeSlot(store, userId, 'sub-cache', next)
}

/**
 * 登录确认之后、任何读取之前调用一次：把这个用户按别的布局存放的本地数据
 * 整理成本模块读写的唯一形式 —— 按用户 id 分键（lib/userStore 的
 * adoptUserData），订阅缓存按地址分开（migrateSubCache）。两步都可以重复
 * 执行。
 *
 * 整理失败（存储不可用或已满）时数据留在原处，下次登录再试；这里没有需要
 * 用户处理的事，所以不向上报告。
 *
 * @param {{ id: number, username: string }} user
 */
export function adoptLocalData(user) {
  const store = browserStore()
  if (!store) return
  try {
    adoptUserData(store, user)
    migrateSubCache(store, user.id)
  } catch { /* 见上：留在原处，下次再试 */ }
}

export function proxyKey(n) {
  return n.protocol && n.host && n.port ? `${n.protocol}:${n.host}:${n.port}` : null
}

/* 一个代理节点上可以打的两种标记，各占一位，可以同时打：
   “落地”——可以选作规则的出口；“直连”——出现在用户自己的代理列表里。
   标记按代理的地址键（proxyKey）记，管理员打的存在服务端的一行设置里，用户自己
   打的存在本浏览器里（见下面的本地覆盖）。它与转发节点的用途（入口/中转，
   lib/seats.js）是两回事：那是节点行上的设置，这是代理条目上的标记。 */
export const MARK_LANDING = 1
export const MARK_DIRECT = 2
const MARK_MASK = MARK_LANDING | MARK_DIRECT

/* 界面上列出两种标记时用的次序与文字。 */
export const PROXY_MARKS = Object.freeze([
  { bit: MARK_LANDING, label: '落地' },
  { bit: MARK_DIRECT, label: '直连' },
])

// 标记改成按位存之前，服务端的设置行与浏览器的 localStorage 里存的是字符串
// 'landing' / 'direct'。读的时候两种都认，已有的标记不因存法变了而丢。
function markBits(v) {
  if (typeof v === 'number') return v & MARK_MASK
  if (v === 'landing') return MARK_LANDING
  if (v === 'direct') return MARK_DIRECT
  return 0
}

export function proxyHasMark(marks, n, bit) {
  const key = proxyKey(n)
  return !!(key && (markBits(marks[key]) & bit))
}

function cleanMarks(marks) {
  const out = {}
  for (const [k, v] of Object.entries(marks || {})) {
    const bits = markBits(v)
    if (bits) out[k] = bits
  }
  return out
}

/* 读取服务端的标记表。失败时抛出，而不是返回空表：空表与“确实没有任何
   标记”无法区分，调用方会据此把所有节点当成未配置，写入方更会用这张
   空表覆盖掉服务端的整张表。 */
export async function fetchProxyMarks() {
  const d = await api.get('/node-roles')
  return cleanMarks(d?.roles)
}

/* 服务端标记表的写入口。

   这张表是全局的一行设置，所有管理员共用。写入表达的是一次改动（这些
   节点、这一位、开或关），由服务端在锁内读表、应用、写回（internal/server
   的 apiSetProxyMarks），所以两个人同时改不同的节点、或同一个节点的两个
   标记，都会生效，谁也不会用自己页面上的旧表盖掉别人的改动。

   同一个页面的写入排成一条串行队列：对同一个节点的同一位先开后关，两次
   请求必须按点击的顺序到达服务端，而并发的请求之间顺序没有保证。

   返回写入后的整表。 */
let markWriteQueue = Promise.resolve()
export function setProxyMarkBit(nodes, bit, on) {
  const keys = nodes.map(proxyKey).filter(Boolean)
  const task = markWriteQueue.then(async () => {
    // 没有可写的节点时不发请求（服务端会拒绝空的改动），结果就是当前的表。
    if (keys.length === 0) return fetchProxyMarks()
    const d = await api.post('/node-roles', { keys, role: bit, on })
    return cleanMarks(d?.roles)
  })
  // 队列本身不能因某次失败而中断；失败由 task 的调用方处理。
  markWriteQueue = task.catch(() => {})
  return task
}

/* 本地标记是对服务端标记表的“逐节点覆盖”：某个 key 在本地有值，就以本地
   为准，否则沿用服务端的值。本地值可以是 0 —— 它表示“我明确不要这个节点
   的任何标记”，用来盖掉服务端给它打的标记，所以读写都必须保留 0，不能把
   它当成“没有这一项”清理掉。 */
function normalizeOverrides(marks) {
  const out = {}
  for (const [k, v] of Object.entries(marks || {})) out[k] = markBits(v)
  return out
}

export function loadLocalMarks(userId) {
  return normalizeOverrides(readJSON(userId, 'local-roles') || {})
}

export function saveLocalMarks(userId, marks) {
  const clean = normalizeOverrides(marks)
  write(userId, 'local-roles', Object.keys(clean).length ? JSON.stringify(clean) : null)
}

/* 生效标记 = 本地覆盖优先，否则服务端值。返回的表里只有非 0 的项。 */
export function effectiveMarks(serverMarks, localMarks) {
  const out = { ...(serverMarks || {}) }
  for (const [k, v] of Object.entries(localMarks || {})) {
    if (v) out[k] = v
    else delete out[k]
  }
  return out
}

/* 把某个节点的生效标记设为 value，返回新的本地覆盖表。覆盖值与服务端值
   相同时删掉这一项：它不再表达任何差异，留着只会在服务端值以后变化时
   继续把它盖住。 */
function withOverride(serverMarks, localMarks, key, value) {
  const next = { ...localMarks }
  if (value === markBits(serverMarks?.[key])) delete next[key]
  else next[key] = value
  return next
}

function effectiveBits(serverMarks, localMarks, key) {
  return key in (localMarks || {}) ? markBits(localMarks[key]) : markBits(serverMarks?.[key])
}

/* 在“生效标记”上翻转一个标记位，结果记为本地覆盖。必须从生效值出发计算：
   只在本地表上翻转的话，服务端已经标了的位永远关不掉（本地表里没有它，
   翻转等于把它再打开一次）。 */
export function toggleLocalMark(serverMarks, localMarks, n, bit) {
  const key = proxyKey(n)
  if (!key) return localMarks
  return withOverride(serverMarks, localMarks, key, effectiveBits(serverMarks, localMarks, key) ^ bit)
}

/* 对一批节点统一打开（on=true）或关闭某个标记位，语义同 toggleLocalMark。 */
export function setLocalMarkBatch(serverMarks, localMarks, nodes, bit, on) {
  let next = localMarks
  for (const n of nodes) {
    const key = proxyKey(n)
    if (!key) continue
    const cur = effectiveBits(serverMarks, next, key)
    next = withOverride(serverMarks, next, key, on ? (cur | bit) : (cur & ~bit))
  }
  return next
}

/* 下面两个函数在一张标记表上算出改动后的样子，不写任何地方。管理员侧用它
   在请求落定前把改动先显示出来，真正的写入走 setProxyMarkBit，由服务端把
   同样的改动应用到它自己的表上。用户侧编辑的是本地覆盖，用上面的
   toggleLocalMark / setLocalMarkBatch。 */

// 翻转一个节点的某一位标记，另一位不动。
export function applyProxyMark(marks, n, bit) {
  const next = { ...marks }
  const key = proxyKey(n)
  if (!key) return next
  const nv = markBits(next[key]) ^ bit
  if (nv) next[key] = nv; else delete next[key]
  return next
}

// 对一批节点统一打上（on=true）或去掉（on=false）某一位标记。
export function applyProxyMarkBatch(marks, nodes, bit, on) {
  const next = { ...marks }
  for (const n of nodes) {
    const key = proxyKey(n)
    if (!key) continue
    const cur = markBits(next[key])
    const nv = on ? (cur | bit) : (cur & ~bit)
    if (nv) next[key] = nv; else delete next[key]
  }
  return next
}

/* 节点表的显示次序：有标记（落地/直连任一）的排在前面、彼此保持原来的先后，
   没有标记的沉底。返回的是原数组的下标而不是重排后的数组，调用方按下标记的状态
   （勾选的行、React 的 key）在重新排序后仍然对得上。 */
export function markedFirstOrder(nodes, markOf) {
  const configured = []
  const unconfigured = []
  nodes.forEach((n, i) => (markOf(n) ? configured : unconfigured).push(i))
  return configured.concat(unconfigured)
}

/* 节点 / 出口地址的规范键："host:port"，IPv6 的 host 带方括号。
   landingIndex、mergeLanding 以及规则出口与落地节点的匹配都用这一个函数
   产生键 —— 任何地方手写 `${host}:${port}` 都会在 IPv6 下与这里对不上。 */
export function hostPortKey(host, port) {
  return joinHostPort(String(host), port)
}

export function nodeKey(n) {
  return n.host && n.port ? hostPortKey(n.host, n.port) : null
}

/* Parse a multiline blob of proxy URIs into landing nodes, skipping blank
   lines, comments (#...) and anything that doesn't resolve to host:port. */
export function parseURIs(text) {
  const out = []
  for (let line of (text || '').split('\n')) {
    line = line.trim()
    if (!line || line.startsWith('#')) continue
    const n = parseOne(line)
    if (n) out.push(n)
  }
  return out
}

/* Map "host:port" -> node; first wins, so callers should put higher-priority
   nodes (the user's own) first. */
export function landingIndex(nodes) {
  const m = new Map()
  for (const n of nodes) {
    const key = joinHostPort(n.host, n.port)
    if (!m.has(key)) m.set(key, n)
  }
  return m
}

/* Parse a "host:port" string (e.g. a rule's entry endpoint) into {host, port},
   or null if malformed. Handles bracketed IPv6. */
export function splitEndpoint(s) {
  return splitHostPort(s)
}

/* Merge landing-node lists, de-duplicated by host:port with earlier lists
   winning — pass the user's own nodes first so they override admin ones. */
export function mergeLanding(...lists) {
  const seen = new Set()
  const out = []
  for (const list of lists) {
    for (const n of list || []) {
      const key = joinHostPort(n.host, n.port)
      if (seen.has(key)) continue
      seen.add(key)
      out.push(n)
    }
  }
  return out
}

/* Replace a proxy URI's connection host:port, preserving everything else. */
export function rewriteEndpoint(uri, host, port) {
  const i = uri.indexOf('://')
  if (i <= 0) return rewriteSnell(uri, host, port)
  const scheme = uri.slice(0, i).toLowerCase()
  if (scheme === 'vmess') return rewriteVMess(uri, host, port)
  if (scheme === 'ss') return rewriteSS(uri, host, port)
  return rewriteAuthority(uri, host, port)
}

export function tryParseURI(uri) {
  return parseOne((uri || '').trim())
}

/* ---------- 代理节点目录 ----------

   “有哪些落地节点 / 直连节点”由这一处推导，所有页面（规则表单的出口
   选择、规则列表的中继 URI、落地节点页、我的代理页）消费同一份结果。
   口径：
     全部节点 = 手动 URI ∪ 订阅缓存 ∪ 服务端分配；按 hostPortKey 去重，
               先出现的保留，所以用户自己的节点优先于分配的。
     落地节点 = 全部节点里生效标记含 MARK_LANDING 的（可作规则出口）。
     直连节点 = 全部节点里生效标记含 MARK_DIRECT 的（直接出现在代理列表）。
   每个节点带 source：'local'（来自本浏览器）或 'admin'（服务端分配）。 */

/* 读出本浏览器里的两类节点。订阅节点按名单顺序把各条订阅的节点接在一起。 */
export function loadLocalNodes(userId) {
  return { manual: parseURIs(loadLocalURIs(userId)), sub: Object.values(loadSubCache(userId)).flat() }
}

/**
 * 纯函数：由三路来源与生效标记表推导目录。
 * @param {{ manual?: any[], sub?: any[], server?: any[], marks?: Record<string, number> }} sources
 */
export function buildCatalog({ manual = [], sub = [], server = [], marks = {} }) {
  const tag = (list, source) => (list || []).map(n => ({ ...n, source }))
  const all = mergeLanding(tag(manual, 'local'), tag(sub, 'local'), tag(server, 'admin'))
  const landing = all.filter(n => proxyHasMark(marks, n, MARK_LANDING))
  const direct = all.filter(n => proxyHasMark(marks, n, MARK_DIRECT))
  return { all, landing, direct, landingByKey: landingIndex(landing) }
}

/* 规则的出口若是目录里的落地节点，返回该节点以及“把节点 URI 的端点换成
   规则入口”后的中继 URI（双栈规则另有一条 v6 的）；否则返回 null。 */
export function relayFor(rule, landingByKey) {
  if (!rule?.exit_host || !rule?.exit_port) return null
  const node = landingByKey.get(hostPortKey(rule.exit_host, rule.exit_port))
  if (!node) return null
  const rewrite = (entry) => {
    const ep = entry ? splitEndpoint(entry) : null
    return ep ? rewriteEndpoint(node.uri, ep.host, ep.port) : null
  }
  return { node, relay_uri: rewrite(rule.entry), relay_uri_v6: rewrite(rule.entry_v6) }
}

/* 导出成 Clash 配置时，名称就是代理的标识：mihomo 1.19.33 遇到同名代理会
   拒绝整份配置（proxy … is the duplicate name）。而中继条目的链接是由落地
   节点的链接改写端点得到的，链接里带的仍是落地节点的名称 —— 同一个节点
   既是直连节点、又被规则选作出口时，两条就同名。

   所以中继条目导出时不用链接里的名称，用 relayExportName：它只由落地节点、
   规则、是哪个入口决定，规则页与“我的代理”页复制同一条中继条目拿到的名称相同，
   也不会与直连节点自己的名称相同。 */

/**
 * 中继条目导出用的名称。
 * @param {string} nodeName 落地节点的名称
 * @param {string} ruleName 经过的规则
 * @param {boolean} [v6] 是不是规则的 v6 入口那一条
 */
export function relayExportName(nodeName, ruleName, v6 = false) {
  return `${nodeName}（经 ${ruleName}${v6 ? ' v6' : ''}）`
}

/**
 * 让一组一起导出的名称互不相同：重复出现的名称从第二个起加序号。纯函数，
 * 结果只取决于名称与顺序，所以同一份列表每次导出的名称相同。
 *
 * relayExportName 已经把直连与中继、不同规则分开了，这里兜的是剩下的情形：
 * 两个节点本来就同名，或两条同名规则指向同一个落地节点。
 *
 * @param {string[]} names
 * @returns {string[]} 与 names 一一对应
 */
export function exportNames(names) {
  const taken = new Set()
  return names.map((base) => {
    let name = base
    for (let n = 2; taken.has(name); n++) name = `${base} ${n}`
    taken.add(name)
    return name
  })
}

/* 给规则补上落地出口的展示信息与中继 URI。出口不是落地节点、或规则还没有
   入口可用于改写时，原样返回。 */
export function enrichRule(rule, landingByKey) {
  const hit = relayFor(rule, landingByKey)
  if (!hit || !hit.relay_uri) return rule
  const out = { ...rule, exit_kind: 'landing', landing_name: hit.node.name, landing_protocol: hit.node.protocol, relay_uri: hit.relay_uri }
  if (hit.relay_uri_v6) out.relay_uri_v6 = hit.relay_uri_v6
  return out
}

/* 往手动 URI 列表末尾追加一条（已存在则不动）。返回是否真的追加了。 */
export function addLocalURI(userId, uri) {
  const line = (uri || '').trim()
  if (!line) return false
  const lines = loadLocalURIs(userId).split('\n').map(l => l.trim()).filter(Boolean)
  if (lines.includes(line)) return false
  lines.push(line)
  saveLocalURIs(userId, lines.join('\n'))
  return true
}

/* ---------- internals ---------- */

/* 节点名取 URI 里写的那个，不替某一种面板的命名习惯做还原；服务端的解析器
   （internal/landing）也是这个口径，两边由同一份样例约束。 */
function parseOne(uri) {
  const i = uri.indexOf('://')
  if (i <= 0) return parseSnell(uri)
  const scheme = uri.slice(0, i).toLowerCase()
  if (scheme === 'vmess') return parseVMess(uri)
  if (scheme === 'ss') return parseSS(uri)
  if (scheme === 'http' || scheme === 'https') return null
  return parseAuthority(uri, scheme === 'hy2' ? 'hysteria2' : scheme)
}

function parseAuthority(uri, proto) {
  const i = uri.indexOf('://')
  let rest = uri.slice(i + 3)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = safeDecode(rest.slice(h + 1)); rest = rest.slice(0, h) }
  let end = rest.length
  for (let j = 0; j < rest.length; j++) {
    const c = rest[j]
    if (c === '/' || c === '?') { end = j; break }
  }
  let authority = rest.slice(0, end)
  const at = authority.lastIndexOf('@')
  if (at >= 0) authority = authority.slice(at + 1)
  const hp = splitHostPort(authority)
  if (!hp) return null
  return { name, protocol: proto, host: hp.host, port: hp.port, uri }
}

function parseVMess(uri) {
  const dec = b64decode(uri.slice('vmess://'.length))
  if (!dec) return null
  let m
  try { m = JSON.parse(dec) } catch { return null }
  const host = m.add
  const port = Number(m.port)
  if (!host || !(port >= 1 && port <= 65535)) return null
  return { name: m.ps || '', protocol: 'vmess', host, port, uri }
}

function parseSS(uri) {
  let rest = uri.slice('ss://'.length)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = safeDecode(rest.slice(h + 1)); rest = rest.slice(0, h) }
  const q = rest.indexOf('?')
  if (q >= 0) rest = rest.slice(0, q)
  let hostport
  const at = rest.lastIndexOf('@')
  if (at >= 0) {
    hostport = rest.slice(at + 1)
  } else {
    const dec = b64decode(rest)
    if (!dec) return null
    const at2 = dec.lastIndexOf('@')
    if (at2 < 0) return null
    hostport = dec.slice(at2 + 1)
  }
  const hp = splitHostPort(hostport)
  if (!hp) return null
  return { name, protocol: 'ss', host: hp.host, port: hp.port, uri }
}

function rewriteAuthority(uri, newHost, newPort) {
  const i = uri.indexOf('://')
  const prefix = uri.slice(0, i + 3)
  const rest = uri.slice(i + 3)
  let end = rest.length
  for (let j = 0; j < rest.length; j++) {
    const c = rest[j]
    if (c === '/' || c === '?' || c === '#') { end = j; break }
  }
  const authority = rest.slice(0, end)
  const tail = rest.slice(end)
  let userinfo = ''
  const at = authority.lastIndexOf('@')
  if (at >= 0) userinfo = authority.slice(0, at + 1)
  return prefix + userinfo + joinHostPort(newHost, newPort) + tail
}

function rewriteVMess(uri, newHost, newPort) {
  const dec = b64decode(uri.slice('vmess://'.length))
  if (!dec) return null
  let m
  try { m = JSON.parse(dec) } catch { return null }
  m.add = newHost
  m.port = String(newPort)
  return 'vmess://' + b64encode(JSON.stringify(m))
}

function rewriteSS(uri, newHost, newPort) {
  let rest = uri.slice('ss://'.length)
  let frag = ''
  const h = rest.indexOf('#')
  if (h >= 0) { frag = rest.slice(h); rest = rest.slice(0, h) }
  if (rest.lastIndexOf('@') >= 0) return rewriteAuthority(uri, newHost, newPort)
  let query = ''
  const q = rest.indexOf('?')
  if (q >= 0) { query = rest.slice(q); rest = rest.slice(0, q) }
  const dec = b64decode(rest)
  if (!dec) return null
  const at = dec.lastIndexOf('@')
  if (at < 0) return null
  const payload = dec.slice(0, at + 1) + joinHostPort(newHost, newPort)
  return 'ss://' + b64encode(payload) + query + frag
}

function parseSnell(line) {
  const eq = line.indexOf('=')
  if (eq < 0) return null
  const name = line.slice(0, eq).trim()
  const rest = line.slice(eq + 1).trim()
  const parts = rest.split(',').map(s => s.trim())
  if (parts.length < 3 || parts[0].toLowerCase() !== 'snell') return null
  const host = parts[1]
  const port = Number(parts[2])
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) return null
  return { name, protocol: 'snell', host, port, uri: line }
}

function rewriteSnell(line, newHost, newPort) {
  const eq = line.indexOf('=')
  if (eq < 0) return null
  const rest = line.slice(eq + 1).trim()
  const parts = rest.split(',')
  if (parts.length < 3 || parts[0].trim().toLowerCase() !== 'snell') return null
  parts[1] = ' ' + newHost
  parts[2] = ' ' + String(newPort)
  return line.slice(0, eq + 1) + ' ' + parts.join(',')
}

function splitHostPort(authority) {
  if (!authority) return null
  let host, portStr
  if (authority.startsWith('[')) {
    const close = authority.indexOf(']')
    if (close < 0) return null
    host = authority.slice(1, close)
    const rem = authority.slice(close + 1)
    if (!rem.startsWith(':')) return null
    portStr = rem.slice(1)
  } else {
    const c = authority.lastIndexOf(':')
    if (c < 0) return null
    host = authority.slice(0, c)
    portStr = authority.slice(c + 1)
  }
  const port = Number(portStr)
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) return null
  return { host, port }
}

function joinHostPort(host, port) {
  return host.includes(':') ? `[${host}]:${port}` : `${host}:${port}`
}

function b64decode(s) {
  const candidates = [s, s.replace(/-/g, '+').replace(/_/g, '/')]
  for (const v of candidates) {
    const pad = v.length % 4 ? '='.repeat(4 - (v.length % 4)) : ''
    try {
      const bin = atob(v + pad)
      try { return decodeURIComponent(escape(bin)) } catch { return bin }
    } catch { /* try next */ }
  }
  return null
}

function b64encode(s) {
  try { return btoa(unescape(encodeURIComponent(s))) } catch { return btoa(s) }
}

function safeDecode(s) {
  try { return decodeURIComponent(s) } catch { return s }
}
