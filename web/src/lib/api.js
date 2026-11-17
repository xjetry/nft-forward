// @ts-check
const BASE = '/api'

/**
 * 请求失败。`status` 是 HTTP 状态码；网络层失败（连不上、被中断）时为 0。
 * 调用方用它区分“不存在”（404）与其它失败，而不是去匹配文案。
 */
export class ApiError extends Error {
  /**
   * @param {string} message
   * @param {number} status
   */
  constructor(message, status) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/** @param {unknown} err */
export function isNotFound(err) {
  return err instanceof ApiError && err.status === 404
}

/**
 * 服务端没有返回 JSON {error} 时（反向代理的 502/503 HTML 页、内部错误），
 * 按状态码给出一条用户能看懂的中文提示。
 * @param {number} status
 */
function httpErrorMessage(status) {
  if (status === 502 || status === 503 || status === 504) return `服务暂时不可用（${status}），请稍后重试`
  if (status === 500) return '服务器内部错误（500）'
  if (status === 401) return '登录已失效，请重新登录'
  if (status === 403) return '没有权限执行该操作'
  if (status === 404) return '请求的资源不存在'
  if (status === 429) return '操作过于频繁，请稍后再试'
  return `请求失败（${status}）`
}

/** 永不落定的 Promise：会话已经结束，发起请求的页面即将被换成登录页。 */
const NEVER = new Promise(() => {})

/**
 * 某个请求被答复 401、会话因此结束时要做的事，由 SessionProvider 登记：它把
 * 会话状态改成未登录，路由守卫随即在页内转到登录页。请求层自己不做跳转 ——
 * 会话状态只有一份，由它的持有者来改。
 * @type {null | (() => void)}
 */
let onSessionEnded = null

/** @param {null | (() => void)} handler */
export function setSessionEndedHandler(handler) {
  onSessionEnded = handler
}

/**
 * @typedef {object} RequestOptions
 * @property {boolean} [own401] 这个请求的 401 本身就是调用方要的答复，照常
 *   抛给调用方。只有两处：确认会话的 /me（401 = 没有登录）与登录请求本身
 *   （401 = 用户名或密码错误）。
 */

/**
 * 不变式：request 要么返回成功的数据（204 为 null），要么抛出 ApiError。
 * 它不会用返回值表达失败 —— 调用方不需要、也不应该去判断“返回了 null
 * 是不是其实失败了”。
 *
 * 401 的两种情形：
 * - 会话结束：需要登录的接口只在请求没有带来可用的会话时回 401
 *   （requireAPIAuth：没有 cookie，或它对应的会话不存在、已过期）。通知会话的
 *   持有者，并返回一个永不落定的 Promise —— 发起请求的页面马上会被登录页
 *   换掉，让调用方停在原地（保持加载态），比让它先走进错误分支、闪一下错误
 *   提示再被换掉要干净。
 * - 调用方声明了 own401，或者还没有人登记会话结束的处理：按普通失败抛出。
 *
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 * @param {RequestOptions} [options]
 * @returns {Promise<any>}
 */
async function request(method, path, body, options) {
  /** @type {RequestInit} */
  const opts = { method, headers: {} }
  if (body !== undefined) {
    opts.headers = { 'Content-Type': 'application/json' }
    opts.body = JSON.stringify(body)
  }
  let res
  try {
    res = await fetch(BASE + path, opts)
  } catch {
    throw new ApiError('网络错误，请检查网络连接后重试', 0)
  }
  if (res.status === 401 && !options?.own401 && onSessionEnded) {
    onSessionEnded()
    return NEVER
  }
  if (res.status === 204) return null

  // 只有服务端确实返回 JSON 时才解析；网关错误页是 HTML。
  const ct = res.headers.get('content-type') || ''
  let data = null
  if (ct.includes('application/json')) {
    try { data = await res.json() } catch { data = null }
  }
  if (!res.ok) throw new ApiError((data && data.error) || httpErrorMessage(res.status), res.status)
  return data
}

export const api = {
  /** @param {string} path @param {RequestOptions} [options] */
  get: (path, options) => request('GET', path, undefined, options),
  /** @param {string} path @param {unknown} [body] @param {RequestOptions} [options] */
  post: (path, body, options) => request('POST', path, body, options),
  /** @param {string} path @param {unknown} [body] */
  put: (path, body) => request('PUT', path, body),
  /** @param {string} path @param {unknown} [body] */
  patch: (path, body) => request('PATCH', path, body),
  /** @param {string} path */
  del: (path) => request('DELETE', path),
}
