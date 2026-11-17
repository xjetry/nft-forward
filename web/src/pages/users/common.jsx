import { fmtDate, GB } from '../../lib/fmt'
import { Badge, Button, CodeBlock, Field, Input, Modal, ModalActions, TextLink } from '../../components/ui'

/* 用户列表页、用户详情页与粘贴授权弹窗共用的部分：数值输入的解析、到期日的
   显示与输入、账号级操作的确认文案、一次性凭据弹窗。 */


/** 字节数 → GB 输入框里的文字：至多两位小数，不带多余的 0。 */
export function bytesToGBText(bytes) {
  return String(Number(((bytes || 0) / GB).toFixed(2)))
}

/**
 * 解析数值输入框里的文字。空、不是数字、小于 min、或要求整数却带小数时返回
 * null —— 调用方据此在字段上报错，而不是把非法输入换成某个默认值提交出去。
 * @param {string} text
 * @param {{ min?: number, integer?: boolean }} [rule]
 * @returns {number | null}
 */
export function parseNumber(text, { min = 0, integer = false } = {}) {
  const s = String(text).trim()
  if (s === '') return null
  const n = Number(s)
  if (!Number.isFinite(n) || n < min) return null
  if (integer && !Number.isInteger(n)) return null
  return n
}

/**
 * 到期日所处的状态：已到期、7 天内到期，或都不是（null）。
 * @param {number | null} unix 到期时刻（秒）；没有到期日时为空
 */
export function expiryAlert(unix) {
  if (!unix) return null
  const days = Math.ceil((unix - Date.now() / 1000) / 86400)
  if (days <= 0) return { tone: 'danger', label: '已到期' }
  if (days <= 7) return { tone: 'warn', label: `剩 ${days} 天` }
  return null
}

/** 到期日及其状态，用在表格单元格与列表项里。 */
export function Expiry({ unix }) {
  if (!unix) return <span className="text-ink-mut">永不到期</span>
  const flag = expiryAlert(unix)
  return (
    <span className="inline-flex items-center gap-1.5">
      {fmtDate(unix)}
      {flag && <Badge tone={flag.tone}>{flag.label}</Badge>}
    </span>
  )
}

const EXPIRY_STEPS = [
  ['+1 天', d => d.setDate(d.getDate() + 1)],
  ['+7 天', d => d.setDate(d.getDate() + 7)],
  ['+1 月', d => d.setMonth(d.getMonth() + 1)],
  ['+3 月', d => d.setMonth(d.getMonth() + 3)],
  ['+1 年', d => d.setFullYear(d.getFullYear() + 1)],
]

/**
 * 到期日输入：日期框 + 快捷顺延。值是 'YYYY-MM-DD'，空串表示永不到期。
 * 顺延从已填的日期算起；还没填时从今天算起。
 *
 * @param {{
 *   value: string,
 *   onChange: (value: string) => void,
 *   saved?: string,
 *   layout?: 'stack' | 'row',
 * }} props `saved` 是服务端当前的到期日；与 value 不同时给出“还原”。
 */
export function ExpiryField({ value, onChange, saved = '', layout }) {
  const shift = (apply) => {
    const d = value ? new Date(`${value}T00:00:00`) : new Date()
    apply(d)
    onChange(fmtDate(d.getTime() / 1000))
  }
  // 服务端把这个日期按 UTC 0 点换算成到期时刻，时刻一过账号即为已到期。
  const past = value !== '' && Date.parse(`${value}T00:00:00Z`) <= Date.now()
  return (
    <Field
      label="到期日"
      layout={layout}
      hint={past
        ? <span className="text-warn-ink">所选日期的 UTC 0 点已经过去，保存后该用户即为已到期。</span>
        : '留空 = 永不到期。账号在所选日期的 UTC 0 点到期。'}
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <Input type="date" value={value} onChange={onChange} className="w-44" />
        <span className="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-small">
          {EXPIRY_STEPS.map(([label, apply]) => <TextLink key={label} onClick={() => shift(apply)}>{label}</TextLink>)}
          {value !== '' && <TextLink tone="ink" onClick={() => onChange('')}>永不到期</TextLink>}
          {saved !== '' && value !== saved && <TextLink tone="ink" onClick={() => onChange(saved)}>还原</TextLink>}
        </span>
      </div>
    </Field>
  )
}

/* 账号级破坏性操作的确认文案。列表页与详情页都能发起这两个操作，文案只在
   这里写一份，同一个动作在两处的措辞与强度因此一致。 */
export const userConfirms = {
  resetPassword: (username) => ({
    title: '重置密码',
    message: `为 ${username} 生成一个新密码？旧密码立即失效，该用户已登录的会话会被登出。新密码只显示一次。`,
    confirmText: '重置密码',
  }),
  remove: (username, ruleCount) => ({
    title: '删除用户',
    message: `删除用户 ${username}？${ruleCount > 0 ? `该用户的 ${ruleCount} 条规则会一并删除并停止转发。` : ''}删除后无法恢复。`,
    confirmText: '删除用户',
    danger: true,
  }),
}

/**
 * 只显示一次的凭据（新密码、登录信息）。弹窗是 persistent 的：Esc 与点遮罩
 * 不会关掉它，因为关掉之后这段内容再也拿不回来。
 * @param {{ title: string, intro: string, text: string, onClose: () => void }} props
 */
export function OneTimeSecretModal({ title, intro, text, onClose }) {
  return (
    <Modal
      title={title}
      size="sm"
      persistent
      onClose={onClose}
      footer={<ModalActions><Button variant="primary" onClick={onClose}>我已保存</Button></ModalActions>}
    >
      <p className="mb-3 text-small text-ink-soft">{intro}</p>
      <CodeBlock text={text} />
    </Modal>
  )
}

/**
 * 重置密码后显示新密码。列表页与详情页都能重置密码，结果用同一个弹窗展示。
 * @param {{ username: string, password: string, onClose: () => void }} props
 */
export function NewPasswordModal({ username, password, onClose }) {
  return (
    <OneTimeSecretModal
      title="新密码"
      intro={`这是 ${username} 的新密码，只显示这一次。复制并交给用户后再关闭。`}
      text={password}
      onClose={onClose}
    />
  )
}
