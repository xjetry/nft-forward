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

/** Extract Int64 from Go sql.NullInt64 JSON shape */
export function nullInt(v) {
  if (v == null) return null
  if (typeof v === 'object' && 'Valid' in v) return v.Valid ? v.Int64 : null
  return v
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
