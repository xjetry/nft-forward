/* Convert a proxy URI to Clash YAML proxy config. Returns null for
   unsupported/non-URI formats (snell config lines, etc.) so callers
   can fall back to copying the raw URI. */

export function uriToClashYaml(uri) {
  const i = uri.indexOf('://')
  if (i <= 0) return null
  const scheme = uri.slice(0, i).toLowerCase()
  switch (scheme) {
    case 'ss': return ssToYaml(uri)
    case 'vmess': return vmessToYaml(uri)
    case 'vless': return vlessToYaml(uri)
    case 'trojan': return trojanToYaml(uri)
    case 'hy2':
    case 'hysteria2': return hy2ToYaml(uri)
    default: return null
  }
}

function ssToYaml(uri) {
  let rest = uri.slice('ss://'.length)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = dec(rest.slice(h + 1)); rest = rest.slice(0, h) }
  const q = rest.indexOf('?')
  let plugin = '', pluginOpts = ''
  if (q >= 0) {
    const params = parseQS(rest.slice(q + 1))
    plugin = params.plugin || ''
    pluginOpts = params['plugin-opts'] || ''
    rest = rest.slice(0, q)
  }
  let method, password, host, port
  const at = rest.lastIndexOf('@')
  if (at >= 0) {
    let userinfo = rest.slice(0, at)
    const decoded = b64(userinfo)
    if (decoded) userinfo = decoded
    const colon = userinfo.indexOf(':')
    if (colon < 0) return null
    method = userinfo.slice(0, colon)
    password = userinfo.slice(colon + 1)
    const hp = hostport(rest.slice(at + 1))
    if (!hp) return null
    host = hp[0]; port = hp[1]
  } else {
    const decoded = b64(rest)
    if (!decoded) return null
    const at2 = decoded.lastIndexOf('@')
    if (at2 < 0) return null
    const colon = decoded.indexOf(':')
    method = decoded.slice(0, colon)
    password = decoded.slice(colon + 1, at2)
    const hp = hostport(decoded.slice(at2 + 1))
    if (!hp) return null
    host = hp[0]; port = hp[1]
  }
  const L = []
  L.push(`- name: "${esc(name)}"`)
  L.push(`  type: ss`)
  L.push(`  server: ${host}`)
  L.push(`  port: ${port}`)
  L.push(`  cipher: ${method}`)
  L.push(`  password: "${esc(password)}"`)
  L.push(`  udp: true`)
  if (plugin) { L.push(`  plugin: ${plugin}`); if (pluginOpts) L.push(`  plugin-opts: ${pluginOpts}`) }
  return L.join('\n')
}

function vmessToYaml(uri) {
  const decoded = b64(uri.slice('vmess://'.length))
  if (!decoded) return null
  let m
  try { m = JSON.parse(decoded) } catch { return null }
  const L = []
  L.push(`- name: "${esc(m.ps || '')}"`)
  L.push(`  type: vmess`)
  L.push(`  server: ${m.add}`)
  L.push(`  port: ${m.port}`)
  L.push(`  uuid: ${m.id}`)
  L.push(`  alterId: ${m.aid || 0}`)
  L.push(`  cipher: ${m.scy || 'auto'}`)
  const net = m.net || 'tcp'
  if (net !== 'tcp') L.push(`  network: ${net}`)
  if (m.tls === 'tls') L.push(`  tls: true`)
  if (m.sni) L.push(`  servername: ${m.sni}`)
  if (net === 'ws') {
    L.push(`  ws-opts:`)
    if (m.path) L.push(`    path: "${esc(m.path)}"`)
    if (m.host) { L.push(`    headers:`); L.push(`      Host: ${m.host}`) }
  } else if (net === 'grpc' && m.path) {
    L.push(`  grpc-opts:`)
    L.push(`    grpc-service-name: "${esc(m.path)}"`)
  }
  L.push(`  udp: true`)
  return L.join('\n')
}

function vlessToYaml(uri) {
  const i = uri.indexOf('://')
  let rest = uri.slice(i + 3)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = dec(rest.slice(h + 1)); rest = rest.slice(0, h) }
  let params = {}
  const q = rest.indexOf('?')
  if (q >= 0) { params = parseQS(rest.slice(q + 1)); rest = rest.slice(0, q) }
  const at = rest.indexOf('@')
  if (at < 0) return null
  const uuid = rest.slice(0, at)
  const hp = hostport(rest.slice(at + 1))
  if (!hp) return null
  const L = []
  L.push(`- name: "${esc(name)}"`)
  L.push(`  type: vless`)
  L.push(`  server: ${hp[0]}`)
  L.push(`  port: ${hp[1]}`)
  L.push(`  uuid: ${uuid}`)
  if (params.flow) L.push(`  flow: ${params.flow}`)
  if (params.security === 'tls' || params.security === 'reality') L.push(`  tls: true`)
  if (params.sni) L.push(`  servername: ${params.sni}`)
  if (params.fp) L.push(`  client-fingerprint: ${params.fp}`)
  if (params.security === 'reality') {
    L.push(`  reality-opts:`)
    if (params.pbk) L.push(`    public-key: ${params.pbk}`)
    if (params.sid) L.push(`    short-id: ${params.sid}`)
  }
  const net = params.type || 'tcp'
  if (net !== 'tcp') L.push(`  network: ${net}`)
  appendTransport(L, net, params)
  L.push(`  udp: true`)
  return L.join('\n')
}

function trojanToYaml(uri) {
  const i = uri.indexOf('://')
  let rest = uri.slice(i + 3)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = dec(rest.slice(h + 1)); rest = rest.slice(0, h) }
  let params = {}
  const q = rest.indexOf('?')
  if (q >= 0) { params = parseQS(rest.slice(q + 1)); rest = rest.slice(0, q) }
  const at = rest.indexOf('@')
  if (at < 0) return null
  const password = rest.slice(0, at)
  const hp = hostport(rest.slice(at + 1))
  if (!hp) return null
  const L = []
  L.push(`- name: "${esc(name)}"`)
  L.push(`  type: trojan`)
  L.push(`  server: ${hp[0]}`)
  L.push(`  port: ${hp[1]}`)
  L.push(`  password: "${esc(password)}"`)
  if (params.sni) L.push(`  sni: ${params.sni}`)
  if (params.fp) L.push(`  client-fingerprint: ${params.fp}`)
  const net = params.type || 'tcp'
  if (net !== 'tcp') L.push(`  network: ${net}`)
  appendTransport(L, net, params)
  L.push(`  udp: true`)
  return L.join('\n')
}

function hy2ToYaml(uri) {
  const i = uri.indexOf('://')
  let rest = uri.slice(i + 3)
  let name = ''
  const h = rest.indexOf('#')
  if (h >= 0) { name = dec(rest.slice(h + 1)); rest = rest.slice(0, h) }
  let params = {}
  const q = rest.indexOf('?')
  if (q >= 0) { params = parseQS(rest.slice(q + 1)); rest = rest.slice(0, q) }
  let auth = ''
  const at = rest.indexOf('@')
  if (at >= 0) { auth = rest.slice(0, at); rest = rest.slice(at + 1) }
  const hp = hostport(rest)
  if (!hp) return null
  const L = []
  L.push(`- name: "${esc(name)}"`)
  L.push(`  type: hysteria2`)
  L.push(`  server: ${hp[0]}`)
  L.push(`  port: ${hp[1]}`)
  if (auth) L.push(`  password: "${esc(auth)}"`)
  if (params.sni) L.push(`  sni: ${params.sni}`)
  if (params.insecure === '1') L.push(`  skip-cert-verify: true`)
  if (params.obfs === 'salamander' && params['obfs-password']) {
    L.push(`  obfs: salamander`)
    L.push(`  obfs-password: "${esc(params['obfs-password'])}"`)
  }
  return L.join('\n')
}

function appendTransport(L, net, params) {
  if (net === 'ws') {
    L.push(`  ws-opts:`)
    if (params.path) L.push(`    path: "${esc(params.path)}"`)
    if (params.host) { L.push(`    headers:`); L.push(`      Host: ${params.host}`) }
  } else if (net === 'grpc' && params.serviceName) {
    L.push(`  grpc-opts:`)
    L.push(`    grpc-service-name: "${esc(params.serviceName)}"`)
  }
}

function parseQS(qs) {
  const m = {}
  for (const p of qs.split('&')) {
    const eq = p.indexOf('=')
    if (eq > 0) m[p.slice(0, eq)] = dec(p.slice(eq + 1))
  }
  return m
}

function hostport(s) {
  if (!s) return null
  let host, portStr
  if (s.startsWith('[')) {
    const close = s.indexOf(']')
    if (close < 0) return null
    host = s.slice(1, close)
    const rem = s.slice(close + 1)
    if (!rem.startsWith(':')) return null
    portStr = rem.slice(1)
  } else {
    const c = s.lastIndexOf(':')
    if (c < 0) return null
    host = s.slice(0, c)
    portStr = s.slice(c + 1)
  }
  const port = Number(portStr)
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) return null
  return [host, port]
}

function esc(s) { return (s || '').replace(/\\/g, '\\\\').replace(/"/g, '\\"') }
function dec(s) { try { return decodeURIComponent(s) } catch { return s } }
function b64(s) {
  const candidates = [s, s.replace(/-/g, '+').replace(/_/g, '/')]
  for (const v of candidates) {
    const pad = v.length % 4 ? '='.repeat(4 - (v.length % 4)) : ''
    try {
      const bin = atob(v + pad)
      try { return decodeURIComponent(escape(bin)) } catch { return bin }
    } catch { /* next */ }
  }
  return null
}
