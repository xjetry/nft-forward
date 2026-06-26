import { useState, useRef, useEffect, useMemo } from 'react'
import { api } from '../lib/api'
import { useToast } from './Layout'
import { SensText } from './ui'
import {
  loadLocalURIs, saveLocalURIs, parseURIs,
  loadSubURLs, saveSubURLs, loadSubCache, saveSubCache,
  nodeRoleKey, fetchNodeRoles,
  loadLocalRoles, saveLocalRoles, applyNodeRole, applyNodeRoleBatch,
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
  const [showManualConfig, setShowManualConfig] = useState(() => parseURIs(loadLocalURIs(username)).length > 0)

  useEffect(() => { fetchNodeRoles().then(setServerRoles) }, [])
  const [showSub, setShowSub] = useState(() => loadSubURLs(username).trim() !== '' || loadSubCache(username).length > 0)
  const toast = useToast()
  const [subRef, subH] = usePersistedHeight(`nf-sub-textarea-h:${username}`)
  const [manualRef, manualH] = usePersistedHeight(`nf-manual-textarea-h:${username}`)

  const roles = useMemo(() => ({ ...serverRoles, ...localRoles }), [serverRoles, localRoles])
  const roleOf = (n) => { const k = nodeRoleKey(n); return k && roles[k] ? roles[k] : 'none' }
  const manualCount = parseURIs(text).length
  const mLanding = manualParsed.filter(n => roleOf(n) === 'landing').length
  const mDirect = manualParsed.filter(n => roleOf(n) === 'direct').length
  const mUnconfigured = manualParsed.length - mLanding - mDirect
  const landingCount = subNodes.filter(n => roleOf(n) === 'landing').length
  const directCount = subNodes.filter(n => roleOf(n) === 'direct').length
  const unconfiguredCount = subNodes.length - landingCount - directCount

  const saveManual = () => {
    saveLocalURIs(username, text)
    const lines = text.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    const parsed = parseURIs(text)
    setManualParsed(parsed)
    const failed = lines.length - parsed.length
    if (failed > 0) toast(`已保存 · ${failed} 行无法解析，已跳过`)
    else toast('已保存到本浏览器')
    if (parsed.length > 0) setShowManualConfig(true)
  }

  const refreshSubs = async () => {
    const urls = subURLs.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    if (!urls.length) { toast('请先填写订阅地址'); return }
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
      if (errors.length) toast(`${allNodes.length} 个节点，${errors.length} 条订阅失败`)
      else toast(`已更新，共 ${allNodes.length} 个节点`)
    } catch (err) { toast(err.message) } finally { setFetching(false) }
  }

  const handleSetRole = (n, role) => {
    const next = applyNodeRole(localRoles, n, role)
    setLocalRoles(next)
    saveLocalRoles(username, next)
  }

  const handleMarkAll = (role) => {
    const next = applyNodeRoleBatch(localRoles, subNodes, role)
    setLocalRoles(next)
    saveLocalRoles(username, next)
  }

  const handleMarkAllManual = (role) => {
    const next = applyNodeRoleBatch(localRoles, manualParsed, role)
    setLocalRoles(next)
    saveLocalRoles(username, next)
  }

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
              <span className="text-ink-soft font-semibold">
                {subNodes.length} 个节点
                <span className="font-normal ml-2">{landingCount} 落地 · {directCount} 直连 · {unconfiguredCount} 未配置</span>
              </span>
              <div className="flex gap-1.5">
                <button onClick={() => handleMarkAll('landing')} className="text-emerald-600 hover:underline">全部落地</button>
                <span className="text-ink-mut">|</span>
                <button onClick={() => handleMarkAll('direct')} className="text-blue-600 hover:underline">全部直连</button>
                <span className="text-ink-mut">|</span>
                <button onClick={() => handleMarkAll('none')} className="text-ink-mut hover:underline">全部未配置</button>
              </div>
            </div>
            <div className="overflow-y-auto" style={{ maxHeight: MAX_H }}>
              <table className="w-full text-[13px]">
                <tbody>
                  {subNodes.map((n, i) => (
                    <tr key={i} className="border-t border-line-soft">
                      <td className="px-3 py-1.5 truncate max-w-[200px]" title={n.name}>{n.name || '(未命名)'}</td>
                      <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.protocol}</td>
                      <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">
                        <SensText blurred={blurred}>{nodeRoleKey(n)}</SensText>
                      </td>
                      <td className="px-3 py-1.5 text-right">
                        <TriToggle state={roleOf(n)} onChange={(k) => handleSetRole(n, k)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}

        {/* Manual URIs */}
        <label className="text-[13px] font-semibold text-ink-soft mb-1.5">手动填写</label>
        <textarea ref={manualRef} className="input-field font-mono w-full min-h-[80px] resize-y !py-3 !px-3.5 text-[13px]"
          style={{ maxHeight: MAX_H, ...(manualH ? { height: manualH } : {}) }}
          value={text} onChange={e => setText(e.target.value)}
          placeholder={'vless://…\ntrojan://…\n🇭🇰 Name = snell, host, port, psk = xxx, version = 5'} />
        <button onClick={saveManual} className="btn-primary mt-3 self-start">保存</button>

        {manualParsed.length > 0 && (
          <>
            <button type="button" onClick={() => setShowManualConfig(v => !v)}
              className="inline-flex items-center gap-1.5 text-[13px] text-blue-600 hover:text-blue-500 mt-3 self-start transition-colors">
              <svg className={`w-3 h-3 transition-transform ${showManualConfig ? 'rotate-90' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="m9 18 6-6-6-6"/></svg>
              配置表<span className="text-ink-mut">（{manualParsed.length} 个节点）</span>
            </button>
            {showManualConfig && (
              <div className="mt-2 border border-line rounded-[10px] overflow-hidden">
                <div className="flex items-center justify-between px-3 py-2 bg-raised text-[12px]">
                  <span className="text-ink-soft font-semibold">
                    {manualParsed.length} 个节点
                    <span className="font-normal ml-2">{mLanding} 落地 · {mDirect} 直连 · {mUnconfigured} 未配置</span>
                  </span>
                  <div className="flex gap-1.5">
                    <button onClick={() => handleMarkAllManual('landing')} className="text-emerald-600 hover:underline">全部落地</button>
                    <span className="text-ink-mut">|</span>
                    <button onClick={() => handleMarkAllManual('direct')} className="text-blue-600 hover:underline">全部直连</button>
                    <span className="text-ink-mut">|</span>
                    <button onClick={() => handleMarkAllManual('none')} className="text-ink-mut hover:underline">全部未配置</button>
                  </div>
                </div>
                <div className="overflow-y-auto" style={{ maxHeight: MAX_H }}>
                  <table className="w-full text-[13px]">
                    <tbody>
                      {manualParsed.map((n, i) => (
                        <tr key={i} className="border-t border-line-soft">
                          <td className="px-3 py-1.5 truncate max-w-[200px]" title={n.name}>{n.name || '(未命名)'}</td>
                          <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.protocol}</td>
                          <td className="px-2 py-1.5 text-ink-mut font-mono text-[11px]">{n.host}:{n.port}</td>
                          <td className="px-3 py-1.5 text-right">
                            <TriToggle state={roleOf(n)} onChange={(k) => handleSetRole(n, k)} />
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function TriToggle({ state, onChange }) {
  const opts = [
    ['landing', '落地', 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'],
    ['direct', '直连', 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700'],
    ['none', '未配置', 'bg-gray-50 text-gray-500 border-gray-200 dark:bg-gray-800/40 dark:text-gray-400 dark:border-gray-600'],
  ]
  return (
    <div className="inline-flex gap-px rounded-md overflow-hidden border border-line">
      {opts.map(([key, label, cls]) => (
        <button key={key} onClick={() => onChange(key)}
          className={`px-2 py-0.5 text-[11px] font-semibold transition-colors ${
            state === key ? cls : 'bg-transparent text-ink-mut/40 hover:text-ink-mut'
          }`}>
          {label}
        </button>
      ))}
    </div>
  )
}
