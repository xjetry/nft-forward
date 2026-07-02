/* Client-side proxy-URI parsing and endpoint rewriting — the JS counterpart of
   the server's internal/landing package. It lives in the browser because the
   user's own proxy URIs are kept in localStorage and must never reach the
   server (privacy). The server still resolves admin-assigned landing nodes;
   this handles the user's own ones and merges them in, with the user's URIs
   winning on a host:port collision. */

const LS_PREFIX = 'nf-landing-uris:'
const LS_SUB_URL_PREFIX = 'nf-sub-urls:'
const LS_SUB_CACHE_PREFIX = 'nf-sub-cache:'

export function loadLocalURIs(username) {
  if (!username) return ''
  try { return localStorage.getItem(LS_PREFIX + username) || '' } catch { return '' }
}

export function saveLocalURIs(username, text) {
  if (!username) return
  try {
    if (text.trim()) localStorage.setItem(LS_PREFIX + username, text)
    else localStorage.removeItem(LS_PREFIX + username)
  } catch { /* storage unavailable — non-fatal */ }
  // Same-tab listeners (the nav) don't get the native 'storage' event, so emit
  // our own so the landing-nodes entry can appear/disappear immediately.
  try { window.dispatchEvent(new Event('nf-landing-changed')) } catch { /* SSR/no window */ }
}

export function hasLocalURIs(username) {
  return loadLocalURIs(username).trim() !== ''
}

export function hasLocalProxies(username) {
  return hasLocalURIs(username) || loadSubURLs(username).trim() !== ''
}

export function loadSubURLs(username) {
  if (!username) return ''
  try { return localStorage.getItem(LS_SUB_URL_PREFIX + username) || '' } catch { return '' }
}

export function saveSubURLs(username, text) {
  if (!username) return
  try {
    if (text.trim()) localStorage.setItem(LS_SUB_URL_PREFIX + username, text)
    else localStorage.removeItem(LS_SUB_URL_PREFIX + username)
  } catch {}
  try { window.dispatchEvent(new Event('nf-landing-changed')) } catch {}
}

export function loadSubCache(username) {
  if (!username) return []
  try {
    const raw = localStorage.getItem(LS_SUB_CACHE_PREFIX + username)
    return raw ? JSON.parse(raw) : []
  } catch { return [] }
}

export function saveSubCache(username, nodes) {
  if (!username) return
  try {
    if (nodes.length) localStorage.setItem(LS_SUB_CACHE_PREFIX + username, JSON.stringify(nodes))
    else localStorage.removeItem(LS_SUB_CACHE_PREFIX + username)
  } catch {}
  try { window.dispatchEvent(new Event('nf-landing-changed')) } catch {}
}

export function nodeRoleKey(n) {
  return n.protocol && n.host && n.port ? `${n.protocol}:${n.host}:${n.port}` : null
}

/* Node role is a bitmask so a node can be both a rule exit ("落地") and
   appear in the user's own proxy list ("直连") at the same time. */
export const ROLE_LANDING = 1
export const ROLE_DIRECT = 2
const ROLE_MASK = ROLE_LANDING | ROLE_DIRECT

// Pre-bitmask data (server settings row, browser localStorage) stored the
// role as the string 'landing'/'direct' — accept both on read so existing
// assignments survive the format change.
function roleBits(v) {
  if (typeof v === 'number') return v & ROLE_MASK
  if (v === 'landing') return ROLE_LANDING
  if (v === 'direct') return ROLE_DIRECT
  return 0
}

export function nodeHasRole(roles, n, bit) {
  const key = nodeRoleKey(n)
  return !!(key && (roleBits(roles[key]) & bit))
}

export async function fetchNodeRoles() {
  try {
    const res = await fetch('/api/node-roles')
    if (!res.ok) return {}
    const d = await res.json()
    const out = {}
    for (const [k, v] of Object.entries(d?.roles || {})) {
      const bits = roleBits(v)
      if (bits) out[k] = bits
    }
    return out
  } catch { return {} }
}

export async function saveNodeRoles(roles) {
  const clean = {}
  for (const [k, v] of Object.entries(roles)) {
    const bits = roleBits(v)
    if (bits) clean[k] = bits
  }
  await fetch('/api/node-roles', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ roles: clean }),
  })
  return clean
}

const LS_LOCAL_ROLES_PREFIX = 'nf-local-roles:'

export function loadLocalRoles(username) {
  if (!username) return {}
  try {
    const raw = localStorage.getItem(LS_LOCAL_ROLES_PREFIX + username)
    const parsed = raw ? JSON.parse(raw) : {}
    const out = {}
    for (const [k, v] of Object.entries(parsed)) {
      const bits = roleBits(v)
      if (bits) out[k] = bits
    }
    return out
  } catch { return {} }
}

export function saveLocalRoles(username, roles) {
  if (!username) return
  const clean = {}
  for (const [k, v] of Object.entries(roles)) {
    const bits = roleBits(v)
    if (bits) clean[k] = bits
  }
  try {
    if (Object.keys(clean).length) localStorage.setItem(LS_LOCAL_ROLES_PREFIX + username, JSON.stringify(clean))
    else localStorage.removeItem(LS_LOCAL_ROLES_PREFIX + username)
  } catch {}
}

// Toggle a single role bit for one node, leaving its other bit untouched.
export function applyNodeRole(roles, n, bit) {
  const next = { ...roles }
  const key = nodeRoleKey(n)
  if (!key) return next
  const nv = roleBits(next[key]) ^ bit
  if (nv) next[key] = nv; else delete next[key]
  return next
}

// Set (on=true) or clear (on=false) a role bit across many nodes at once.
export function applyNodeRoleBatch(roles, nodes, bit, on) {
  const next = { ...roles }
  for (const n of nodes) {
    const key = nodeRoleKey(n)
    if (!key) continue
    const cur = roleBits(next[key])
    const nv = on ? (cur | bit) : (cur & ~bit)
    if (nv) next[key] = nv; else delete next[key]
  }
  return next
}

export function nodeKey(n) {
  return n.host && n.port ? joinHostPort(n.host, n.port) : null
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

/* ---------- internals ---------- */

function parseOne(uri) {
  const n = parseRaw(uri)
  if (n) n.name = stripDedupSuffix(n.name)
  return n
}

function parseRaw(uri) {
  const i = uri.indexOf('://')
  if (i <= 0) return parseSnell(uri)
  const scheme = uri.slice(0, i).toLowerCase()
  if (scheme === 'vmess') return parseVMess(uri)
  if (scheme === 'ss') return parseSS(uri)
  if (scheme === 'http' || scheme === 'https') return null
  return parseAuthority(uri, scheme === 'hy2' ? 'hysteria2' : scheme)
}

/* Some panels (e.g. Remnawave) append "^~2~^"-style counters to same-named
   nodes in a subscription — typically the same node exported once per
   protocol. Nodes are identified by host:port here, so the counter is display
   noise; keep the name as-is when the counter is all there is. */
function stripDedupSuffix(name) {
  const out = (name || '').replace(/(\s*\^~\d+~\^)+$/, '').trim()
  return out || name
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
