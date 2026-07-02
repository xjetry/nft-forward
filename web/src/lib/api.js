const BASE = '/api'

// httpErrorMessage maps a status to a user-facing Chinese message for the case
// where the server didn't return a JSON {error} body — e.g. a reverse-proxy
// 502/503 HTML page or an internal error. Without this the caller would surface
// a cryptic "Unexpected token <" JSON parse error.
function httpErrorMessage(status) {
  if (status === 502 || status === 503 || status === 504) return `服务暂时不可用（${status}），请稍后重试`
  if (status === 500) return '服务器内部错误（500）'
  if (status === 403) return '没有权限执行该操作'
  if (status === 404) return '请求的资源不存在'
  if (status === 429) return '操作过于频繁，请稍后再试'
  return `请求失败（${status}）`
}

async function request(method, path, body) {
  const opts = { method, headers: {} }
  if (body) {
    opts.headers['Content-Type'] = 'application/json'
    opts.body = JSON.stringify(body)
  }
  let res
  try {
    res = await fetch(BASE + path, opts)
  } catch {
    throw new Error('网络错误，请检查网络连接后重试')
  }
  if (res.status === 401) {
    if (window.location.pathname !== '/login') {
      window.location.href = '/login'
    }
    return null
  }
  if (res.status === 204) return null

  // Only parse JSON when the server actually sent JSON; gateway errors are HTML.
  const ct = res.headers.get('content-type') || ''
  let data = null
  if (ct.includes('application/json')) {
    try { data = await res.json() } catch { data = null }
  }
  if (!res.ok) throw new Error((data && data.error) || httpErrorMessage(res.status))
  return data
}

export const api = {
  get: (path) => request('GET', path),
  post: (path, body) => request('POST', path, body),
  put: (path, body) => request('PUT', path, body),
  patch: (path, body) => request('PATCH', path, body),
  del: (path) => request('DELETE', path),
}
