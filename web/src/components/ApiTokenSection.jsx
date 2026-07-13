import { useState, useEffect } from 'react'
import { api } from '../lib/api'
import { useToast } from './Layout'
import { Loading, Badge, useConfirm } from './ui'
import { fmtDate } from '../lib/fmt'
import { copyToClipboard } from '../lib/clipboard'

// /my/token is session-authenticated, not role-gated — any signed-in account
// (regular user or admin) manages its own token here. Admins are routed away
// from /my by UserRoute, so this section is shared between the user dashboard
// and the admin settings page instead of living only under /my.
export function ApiTokenSection() {
  const toast = useToast()
  const confirm = useConfirm()

  const [token, setToken] = useState(null)
  const [tokenLoading, setTokenLoading] = useState(true)
  const [showTokenModal, setShowTokenModal] = useState(false)
  const [newToken, setNewToken] = useState('')

  useEffect(() => {
    api.get('/my/token').then(setToken).catch(console.error).finally(() => setTokenLoading(false))
  }, [])

  const createToken = async (scope) => {
    try {
      const res = await api.post('/my/token', { scope })
      setNewToken(res.token)
      setShowTokenModal(true)
      const t = await api.get('/my/token')
      setToken(t)
    } catch (err) { toast(err.message, 'error') }
  }

  const deleteToken = async () => {
    if (!(await confirm({ title: '删除 Token', message: '删除后使用此 Token 的所有外部服务将失效。', confirmText: '删除', danger: true }))) return
    try {
      await api.del('/my/token')
      setToken({ has_token: false })
    } catch (err) { toast(err.message, 'error') }
  }

  const refreshToken = async () => {
    if (!(await confirm({ title: '刷新 Token', message: '旧 Token 将立即失效，使用它的外部服务需要更新。', confirmText: '刷新', danger: true }))) return
    try {
      const res = await api.post('/my/token/refresh')
      setNewToken(res.token)
      setShowTokenModal(true)
      const t = await api.get('/my/token')
      setToken(t)
    } catch (err) { toast(err.message, 'error') }
  }

  const toggleToken = async () => {
    try {
      const res = await api.post('/my/token/toggle')
      setToken(prev => ({ ...prev, disabled: res.disabled }))
    } catch (err) { toast(err.message, 'error') }
  }

  return (
    <>
      <TokenCard token={token} tokenLoading={tokenLoading}
        createToken={createToken} deleteToken={deleteToken}
        refreshToken={refreshToken} toggleToken={toggleToken} />

      {showTokenModal && <TokenModal token={newToken} onClose={() => setShowTokenModal(false)} />}
    </>
  )
}

function TokenCard({ token, tokenLoading, createToken, deleteToken, refreshToken, toggleToken }) {
  const [showHelp, setShowHelp] = useState(false)
  // Default to the least-privileged read scope; a user opts into write power
  // (creating/editing their own forward rules over the API) explicitly.
  const [scope, setScope] = useState('read')

  if (tokenLoading) return (
    <section className="user-section">
      <div className="user-section-head">
        <h3 className="user-section-title">API Token</h3>
      </div>
      <Loading />
    </section>
  )

  return (
    <section className="user-section">
      <div className="user-section-head">
        <div>
          <h3 className="user-section-title">API Token</h3>
          <div className="user-section-sub">程序化 / AI agent 调用 API 的访问凭证</div>
        </div>
        <div className="relative">
          <button onClick={() => setShowHelp(!showHelp)} className="btn-secondary text-xs h-8 px-3">使用说明</button>
          {showHelp && (
            <>
              <div className="fixed inset-0 z-40" onClick={() => setShowHelp(false)} />
              <div className="absolute right-0 top-full mt-2 z-50 bg-surface border border-line rounded-xl shadow-xl p-4 w-[min(380px,calc(100vw-32px))] text-sm">
                <p className="font-semibold mb-2">程序化 / AI agent 调用 API</p>
                <p className="text-ink-soft mb-2">鉴权（二选一）：</p>
                <code className="block bg-raised rounded-lg p-2 text-xs font-mono mb-1.5 break-all">GET /api/v1/info?token=YOUR_TOKEN</code>
                <code className="block bg-raised rounded-lg p-2 text-xs font-mono mb-3 break-all">curl -H "Authorization: Bearer YOUR_TOKEN" /api/v1/info</code>
                <p className="text-ink-soft text-xs mb-1"><b>只读</b>：<code className="font-mono">GET /api/v1/info</code>、<code className="font-mono">/my/rules</code>、<code className="font-mono">/my/nodes</code>、<code className="font-mono">/probe</code>、<code className="font-mono">/probe-chain</code>。</p>
                <p className="text-ink-soft text-xs"><b>读写</b>（需读写 Token）：<code className="font-mono">POST/PUT/DELETE /my/rules</code> 自助建改删转发规则；建规支持 <code className="font-mono">node_name</code> 按名寻址、<code className="font-mono">?dry_run=1</code> 预览端口、<code className="font-mono">Idempotency-Key</code> 幂等重试。</p>
              </div>
            </>
          )}
        </div>
      </div>
      <div className="px-5 sm:px-6 py-5">
        {!token?.has_token ? (
          <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
            <div>
              <div className="font-semibold text-ink">尚未创建 Token</div>
              <div className="text-sm text-ink-soft mt-1">创建后只会完整显示一次。</div>
              <div className="flex items-center gap-2 mt-3 flex-wrap">
                <span className="text-sm text-ink-soft">权限</span>
                <select value={scope} onChange={e => setScope(e.target.value)} className="input-field text-sm !py-1 !w-auto">
                  <option value="read">只读（查询）</option>
                  <option value="readwrite">读写（自助管理转发规则）</option>
                </select>
              </div>
            </div>
            <button onClick={() => createToken(scope)} className="btn-primary text-sm">创建 API Token</button>
          </div>
        ) : (
          <div className="space-y-4">
            <div className="user-token-grid">
              <div className="user-token-item">
                <div className="user-token-label">Token</div>
                <div className="user-token-value font-mono flex items-center gap-2 flex-wrap">
                  {token.token_prefix}...
                  <Badge color={token.disabled ? 'gray' : 'green'}>{token.disabled ? '已停用' : '启用中'}</Badge>
                  <Badge color={token.scope === 'readwrite' ? 'blue' : 'gray'}>{token.scope === 'readwrite' ? '读写' : '只读'}</Badge>
                </div>
              </div>
              <div className="user-token-item">
                <div className="user-token-label">创建时间</div>
                <div className="user-token-value">{fmtDate(token.created_at)}</div>
              </div>
              <div className="user-token-item">
                <div className="user-token-label">最近使用</div>
                <div className="user-token-value">{token.last_used_at ? fmtDate(token.last_used_at) : '从未使用'}</div>
              </div>
            </div>
            <div className="flex items-center gap-2 flex-wrap">
              <button onClick={toggleToken} className="btn-secondary text-xs">{token.disabled ? '启用' : '停用'}</button>
              <button onClick={refreshToken} className="btn-secondary text-xs">刷新</button>
              <button onClick={deleteToken} className="btn-danger text-xs">删除</button>
            </div>
          </div>
        )}
      </div>
    </section>
  )
}

function TokenModal({ token, onClose }) {
  const [copied, setCopied] = useState(false)
  const copy = () => {
    copyToClipboard(token).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    }).catch(() => { /* token stays visible for manual copy; no toast here */ })
  }
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-[2px] px-4" onClick={onClose}>
      <div className="bg-surface border border-line rounded-2xl shadow-[0_24px_70px_-20px_rgba(0,0,0,0.7)] max-w-lg w-full overflow-hidden" onClick={e => e.stopPropagation()}>
        <div className="px-6 py-5 border-b border-line-soft">
          <h3 className="text-lg font-bold">API Token 已生成</h3>
          <p className="text-sm text-ink-soft mt-1">请立即复制保存，关闭后无法再次查看。</p>
        </div>
        <div className="p-6">
          <div className="copy-panel">
            <span className="copy-panel-label">TOKEN</span>
            <code className="copy-panel-value select-all">{token}</code>
            <button onClick={copy} className="copy-panel-button">{copied ? '已复制' : '复制'}</button>
          </div>
          <div className="mt-5 flex justify-end">
            <button onClick={onClose} className="btn-secondary text-sm">关闭</button>
          </div>
        </div>
      </div>
    </div>
  )
}
