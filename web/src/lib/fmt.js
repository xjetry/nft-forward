/* 字节单位。配额在界面上按 GB 填写、按字节存储；换算一律用这里的常量。 */
export const MB = 1048576
export const GB = 1073741824

/** Format a byte count with an adaptive unit (B/KB/MB/GB/TB), one decimal. */
export function fmtBytes(n) {
  if (n == null || n === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i]
}

function fmtAdaptive(bytes) {
  if (bytes < GB) return (bytes / MB).toFixed(1) + ' MB'
  return (bytes / GB).toFixed(1) + ' GB'
}

/* GB 文本被读回成字节数时有两种算法：前端 Math.round(n * GB)（授权与落地
   出口的配额），服务端更新用户时 int64(n * GB)（截断）。一段文本算“读得回
   来”，指两种算法都落在原值 ±1 字节以内，并且不会把非 0 读成 0 —— 配额 0
   的含义是不限。 */
function readsBackAs(n, bytes) {
  return [Math.round(n * GB), Math.floor(n * GB)]
    .every(read => Math.abs(read - bytes) <= 1 && (read === 0) === (bytes === 0))
}

/**
 * 字节数 → GB 数的文本，用在输入框与授权文本里：读得回来（见 readsBackAs）
 * 的写法中小数位最少的那个，从两位试起。
 *
 * 不能固定保留两位：不足 0.005 GB 的配额会被写成 "0"，这段文本再被存回去，
 * 一个很小的配额就变成了没有配额。
 */
export function bytesToGBText(bytes) {
  const value = bytes || 0
  // GB = 2^30，所以这个除法在浮点数里是精确的，商至多有 30 位二进制小数，
  // 写成 30 位十进制小数时没有舍入 —— 循环最迟在 30 位时结束。
  const exact = value / GB
  let text = ''
  for (let digits = 2; digits <= 30; digits++) {
    text = exact.toFixed(digits).replace(/\.?0+$/, '')
    if (readsBackAs(Number(text), value)) break
  }
  return text
}

/** “已用 / 配额”，不足 1 GB 的值用 MB。配额为 0 表示不限，显示为 ∞。 */
export function fmtTrafficGB(used, quota) {
  const u = fmtAdaptive(used || 0)
  if (!quota || quota === 0) return `${u} / ∞`
  return `${u} / ${fmtAdaptive(quota)}`
}

/**
 * 单个节点授权的“已用 / 配额”。
 *
 * 授权自己的配额为 0 只表示这个节点没有单独的上限（服务端 NodesExceedingQuota
 * 不把它算作超额），流量仍计入账户的总量；账户有配额时，用到上限这个账户
 * 名下的规则全部暂停转发（服务端的压制原因 user_quota；账号本身不停用）。
 * 所以这时写“账户配额”，只有账户也不限时才是 ∞。
 *
 * 三个参数都可能缺省（节点还没有对应的授权记录时），缺省按 0 处理。
 *
 * @param {number | undefined} used
 * @param {number | undefined} grantQuota 授权自己的配额，0 = 没有单独的上限
 * @param {number | undefined} accountQuota 账户的总配额，0 = 不限
 */
export function fmtGrantTraffic(used, grantQuota, accountQuota) {
  if (!grantQuota && (accountQuota || 0) > 0) return `${fmtAdaptive(used || 0)} / 账户配额`
  return fmtTrafficGB(used, grantQuota)
}

export function fmtTime(unix) {
  if (!unix) return '--'
  const diff = Math.floor(Date.now() / 1000) - unix
  if (diff < 0) return '刚刚'
  if (diff < 60) return `${diff} 秒前`
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  return `${Math.floor(diff / 86400)} 天前`
}

export function fmtDate(unix) {
  if (!unix) return '--'
  const d = new Date(unix * 1000)
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

export function fmtDateInput(unix) {
  if (!unix) return ''
  return fmtDate(unix)
}

export function pct(used, total) {
  if (!total || total === 0) return '0'
  return ((used / total) * 100).toFixed(1)
}

/** Extract String from Go sql.NullString JSON shape */
export function nullStr(v) {
  if (v == null) return null
  if (typeof v === 'object' && 'Valid' in v) return v.Valid ? v.String : null
  return v
}

export function isExpired(unix) {
  if (!unix) return false
  return unix < Math.floor(Date.now() / 1000)
}
