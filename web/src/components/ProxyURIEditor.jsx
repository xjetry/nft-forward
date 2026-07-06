import { useState, useRef, useEffect, useMemo } from 'react'
import { api } from '../lib/api'
import { useToast } from './Layout'
import { SensText } from './ui'
import {
  loadLocalURIs, saveLocalURIs, parseURIs,
  loadSubURLs, saveSubURLs, loadSubCache, saveSubCache,
  nodeRoleKey, fetchNodeRoles,
  loadLocalRoles, saveLocalRoles, applyNodeRole, applyNodeRoleBatch,
  ROLE_LANDING, ROLE_DIRECT, rolesFirstOrder,
} from '../lib/landing'

const MAX_H = 420

function usePersistedHeight(key) {
  const ref = useRef(null)
  const saved = useRef(null)
  try { saved.current = localStorage.getItem(key) } catch {}
  const initial = saved.current ? Number(saved.current) : undefined

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => {
      const h = el.offsetHeight
      if (h > 0) try { localStorage.setItem(key, String(h)) } catch {}
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [key])

  return [ref, initial]
}

export function ProxyURIEditor({ username, blurred }) {
  const [text, setText] = useState(() => loadLocalURIs(username))
  const [subURLs, setSubURLs] = useState(() => loadSubURLs(username))
  const [subNodes, setSubNodes] = useState(() => loadSubCache(username))
  const [serverRoles, setServerRoles] = useState({})
  const [localRoles, setLocalRoles] = useState(() => loadLocalRoles(username))
  const [fetching, setFetching] = useState(false)
  const [manualParsed, setManualParsed] = useState(() => parseURIs(loadLocalURIs(username)))
  const [showManual, setShowManual] = useState(() => loadLocalURIs(username).trim() !== '')
  const [selSub, setSelSub] = useState(new Set())
  const [selManual, setSelManual] = useState(new Set())

  useEffect(() => { fetchNodeRoles().then(setServerRoles) }, [])
  useEffect(() => { setSelSub(new Set()) }, [subNodes])
  useEffect(() => { setSelManual(new Set()) }, [manualParsed])
  const [showSub, setShowSub] = useState(() => loadSubURLs(username).trim() !== '' || loadSubCache(username).length > 0)
  const toast = useToast()
  const [subRef, subH] = usePersistedHeight(`nf-sub-textarea-h:${username}`)
  const [manualRef, manualH] = usePersistedHeight(`nf-manual-textarea-h:${username}`)

  const roles = useMemo(() => ({ ...serverRoles, ...localRoles }), [serverRoles, localRoles])
  const roleOf = (n) => { const k = nodeRoleKey(n); return (k && roles[k]) || 0 }
  const manualCount = parseURIs(text).length
  const mLanding = manualParsed.filter(n => roleOf(n) & ROLE_LANDING).length
  const mDirect = manualParsed.filter(n => roleOf(n) & ROLE_DIRECT).length
  const mUnconfigured = manualParsed.filter(n => !roleOf(n)).length
  const landingCount = subNodes.filter(n => roleOf(n) & ROLE_LANDING).length
  const directCount = subNodes.filter(n => roleOf(n) & ROLE_DIRECT).length
  const unconfiguredCount = subNodes.filter(n => !roleOf(n)).length
  // Unconfigured nodes sink below configured ones; selection stays keyed to
  // the original index so re-sorting never re-targets a checked row.
  const subOrder = useMemo(() => rolesFirstOrder(subNodes, roleOf), [subNodes, roles])
  const manualOrder = useMemo(() => rolesFirstOrder(manualParsed, roleOf), [manualParsed, roles])

  const saveManual = () => {
    saveLocalURIs(username, text)
    const lines = text.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    const parsed = parseURIs(text)
    setManualParsed(parsed)
    const failed = lines.length - parsed.length
    if (failed > 0) toast(`已保存 · ${failed} 行无法解析，已跳过`)
    else toast('已保存到本浏览器')
  }

  const refreshSubs = async () => {
    const urls = subURLs.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    if (!urls.length) { toast('请先填写订阅地址', 'error'); return }
    saveSubURLs(username, subURLs)
    setFetching(true)
    try {
      const allNodes = []
      const errors = []
      for (const url of urls) {
        try {
          const resp = await api.post('/sub-fetch', { url })
          if (resp?.nodes) allNodes.push(...resp.nodes)
        } catch (err) { errors.push(err.message) }
      }
      saveSubCache(username, allNodes)
      setSubNodes(allNodes)
      if (errors.length) toast(`${allNodes.length} 个节点，${errors.length} 条订阅失败`, 'error')
      else toast(`已更新，共 ${allNodes.length} 个节点`)
    } catch (err) { toast(err.message, 'error') } finally { setFetching(false) }
  }

  const handleSetRole = (n, bit) => {
    const next = applyNodeRole(localRoles, n, bit)
    setLocalRoles(next)
    saveLocalRoles(username, next)
  }

  const handleBulkRole = (nodesList, bit, on) => {
    const next = applyNodeRoleBatch(localRoles, nodesList, bit, on)
    setLocalRoles(next)
    saveLocalRoles(username, next)
  }

  const toggleSel = (setSel) => (i) => setSel(s => {
    const next = new Set(s)
    if (next.has(i)) next.delete(i); else next.add(i)
    return next
  })
  const toggleSelAll = (setSel, count) => () => setSel(s =>
    s.size === count ? new Set() : new Set(Array.from({ length: count }, (_, i) => i)))

  const hasNodes = showSub && subNodes.length > 0

  return (
    <div className="card flex flex-col">
      <div className="px-6 py-[22px] flex-1 flex flex-col">
        <div className="flex items-baseline gap-2.5 mb-3.5">
          <h3 className="text-[16px] font-bold">我的代理 URI</h3>
          <span className="text-[13px] text-ink-mut">
            {(manualParsed.length + subNodes.length > 0) && `${mLanding + landingCount} 落地 · ${mDirect + directCount} 直连 · ${mUnconfigured + unconfiguredCount} 未配置`}
          </span>
        </div>
        <p className="text-[13px] leading-[1.7] text-ink-soft mb-3.5">
          手动填写的 URI 保存在本浏览器，本地与服务器相同地址的节点以本地为准。
          节点用途可在下方配置，覆盖管理员默认值，仅在本浏览器生效：
          <span className="font-semibold text-emerald-600">落地</span>可作为规则出口；
          <span className="font-semibold text-blue-600">直连</span>出现在「我的代理」；
          <span className="font-semibold text-ink-mut">未配置</span>不参与任何功能。
        </p>

        {/* Subscription URL input */}
        <button type="button" onClick={() => setShowSub(v => !v)}
          className="inline-flex items-center gap-1.5 text-[13px] text-blue-600 hover:text-blue-500 mb-2 self-start transition-colors">
          <svg className={`w-3 h-3 transition-transform ${showSub ? 'rotate-90' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="m9 18 6-6-6-6"/></svg>
          订阅地址{subNodes.length > 0 && <span className="text-ink-mut">（{subNodes.length} 个节点）</span>}
        </button>
        {showSub && (
          <div className="mb-3 pl-0.5">
            <textarea ref={subRef} className="input-field font-mono w-full min-h-[60px] resize-y !py-2.5 !px-3 text-[13px]"
              style={subH ? { height: subH } : undefined}
              value={subURLs} onChange={e => setSubURLs(e.target.value)}
              placeholder="https://example.com/subscribe?token=..." />
            <div className="flex items-center gap-2 mt-2">
              <button onClick={refreshSubs} disabled={fetching} className="btn-primary text-xs">
                {fetching ? '获取中…' : '更新订阅'}
              </button>
              <span className="text-[12px] text-ink-mut">通过服务器代理获取。</span>
            </div>
          </div>
        )}

        {/* Node list */}
        {hasNodes && (
          <div className="mb-4 border border-line rounded-[10px] overflow-hidden">
            <div className="flex items-center justify-between px-3 py-2 bg-raised text-[12px]">
              <span className="text-ink-soft font-semibold flex items-center gap-2">
                <input type="checkbox" className="accent-blue-600"
                  checked={subNodes.length > 0 && selSub.size === subNodes.length}
                  onChange={toggleSelAll(setSelSub, subNodes.length)} />
                {subNodes.length} 个节点
                <span className="font-normal">{landingCount} 落地 · {directCount} 直连 · {unconfiguredCount} 未配置</span>
              </span>
              <RoleBulkToggle nodes={subNodes.filter((_, i) => selSub.has(i))} roleOf={roleOf}
                onToggle={(bit, on) => handleBulkRole(subNodes.filter((_, i) => selSub.has(i)), bit, on)} />
            </div>
            <div className="overflow-y-auto" style={{ maxHeight: MAX_H }}>
              <table className="w-full text-[13px]">
                <tbody>
                  {subOrder.map((i) => { const n = subNodes[i]; return (
                    <tr key={i} className="border-t border-line-soft">
                      <td className="pl-3 py-1.5 w-6"><input type="checkbox" className="accent-blue-600"
                        checked={selSub.has(i)} onChange={() => toggleSel(setSelSub)(i)} /></td>
                      <td className="px-2 py-1.5 truncate max-w-[200px]" title={n.name}>{n.name || '(未命名)'}</td>
                      <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.protocol}</td>
                      <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">
                        <SensText blurred={blurred}>{nodeRoleKey(n)}</SensText>
                      </td>
                      <td className="px-3 py-1.5 text-right">
                        <RoleToggle state={roleOf(n)} onChange={(bit) => handleSetRole(n, bit)} />
                      </td>
                    </tr>
                  )})}
                </tbody>
              </table>
            </div>
          </div>
        )}

        {/* Manual URIs — same collapsible pattern as the subscription block,
            with the role-config table folded inside. */}
        <button type="button" onClick={() => setShowManual(v => !v)}
          className="inline-flex items-center gap-1.5 text-[13px] text-blue-600 hover:text-blue-500 mb-2 self-start transition-colors">
          <svg className={`w-3 h-3 transition-transform ${showManual ? 'rotate-90' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="m9 18 6-6-6-6"/></svg>
          手动填写{manualParsed.length > 0 && <span className="text-ink-mut">（{manualParsed.length} 个节点）</span>}
        </button>
        {showManual && (
          <div className="pl-0.5">
            <textarea ref={manualRef} className="input-field font-mono w-full min-h-[80px] resize-y !py-3 !px-3.5 text-[13px]"
              style={{ maxHeight: MAX_H, ...(manualH ? { height: manualH } : {}) }}
              value={text} onChange={e => setText(e.target.value)}
              placeholder={'vless://…\ntrojan://…\n🇭🇰 Name = snell, host, port, psk = xxx, version = 5'} />
            <div className="mt-2"><button onClick={saveManual} className="btn-primary">保存</button></div>
            {manualParsed.length > 0 && (
              <div className="mt-3 border border-line rounded-[10px] overflow-hidden">
                <div className="flex items-center justify-between px-3 py-2 bg-raised text-[12px]">
                  <span className="text-ink-soft font-semibold flex items-center gap-2">
                    <input type="checkbox" className="accent-blue-600"
                      checked={manualParsed.length > 0 && selManual.size === manualParsed.length}
                      onChange={toggleSelAll(setSelManual, manualParsed.length)} />
                    {manualParsed.length} 个节点
                    <span className="font-normal">{mLanding} 落地 · {mDirect} 直连 · {mUnconfigured} 未配置</span>
                  </span>
                  <RoleBulkToggle nodes={manualParsed.filter((_, i) => selManual.has(i))} roleOf={roleOf}
                    onToggle={(bit, on) => handleBulkRole(manualParsed.filter((_, i) => selManual.has(i)), bit, on)} />
                </div>
                <div className="overflow-y-auto" style={{ maxHeight: MAX_H }}>
                  <table className="w-full text-[13px]">
                    <tbody>
                      {manualOrder.map((i) => { const n = manualParsed[i]; return (
                        <tr key={i} className="border-t border-line-soft">
                          <td className="pl-3 py-1.5 w-6"><input type="checkbox" className="accent-blue-600"
                            checked={selManual.has(i)} onChange={() => toggleSel(setSelManual)(i)} /></td>
                          <td className="px-2 py-1.5 truncate max-w-[200px]" title={n.name}>{n.name || '(未命名)'}</td>
                          <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.protocol}</td>
                          <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.host}:{n.port}</td>
                          <td className="px-3 py-1.5 text-right">
                            <RoleToggle state={roleOf(n)} onChange={(bit) => handleSetRole(n, bit)} />
                          </td>
                        </tr>
                      )})}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

const ROLE_OPTS = [
  [ROLE_LANDING, '落地', 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'],
  [ROLE_DIRECT, '直连', 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700'],
]

// Two independent per-node switches — landing and direct can both be on at
// once (a node can be a rule exit and appear in "我的代理" simultaneously).
function RoleToggle({ state, onChange }) {
  return (
    <div className="inline-flex gap-1.5">
      {ROLE_OPTS.map(([bit, label, cls]) => (
        <button key={bit} onClick={() => onChange(bit)}
          className={`px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors ${
            state & bit ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
          }`}>
          {label}
        </button>
      ))}
    </div>
  )
}

// Same switches, but scoped to a multi-selected set of nodes: highlighted
// when every selected node already has the bit, click flips it for all of them.
function RoleBulkToggle({ nodes, roleOf, onToggle }) {
  if (!nodes.length) return null
  return (
    <div className="flex gap-1.5">
      {ROLE_OPTS.map(([bit, label, cls]) => {
        const allOn = nodes.every(n => roleOf(n) & bit)
        return (
          <button key={bit} onClick={() => onToggle(bit, !allOn)}
            className={`px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors ${
              allOn ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
            }`}>
            {label}
          </button>
        )
      })}
    </div>
  )
}
