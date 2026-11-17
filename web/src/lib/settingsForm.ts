/* 系统设置表单的取值、校验与比较。 */

/** 服务端接受的连接池大小上限（POST /settings 对 pool_size 的校验是 0–64）。 */
export const POOL_SIZE_MAX = 64

/** 服务端的设置（GET /settings 的答复，也是 POST /settings 的请求体）。 */
export interface Settings {
  readonly panel_url: string
  readonly panel_name: string
  readonly show_rate_to_user: boolean
  readonly pool_size: number
}

/**
 * 表单里的值。连接池大小是字符串：输入过程中它可以是空的或写到一半的，
 * 提交时再由 toSettings 判定。
 */
export interface SettingsForm {
  readonly panel_url: string
  readonly panel_name: string
  readonly show_rate_to_user: boolean
  readonly pool_size: string
}

export function toForm(settings: Settings): SettingsForm {
  return {
    panel_url: settings.panel_url,
    panel_name: settings.panel_name,
    show_rate_to_user: settings.show_rate_to_user,
    pool_size: String(settings.pool_size),
  }
}

/** 连接池大小的输入 → 0 到 POOL_SIZE_MAX 的整数；不是这样的整数时返回 null。 */
export function parsePoolSize(text: string): number | null {
  const digits = text.trim()
  if (!/^\d+$/.test(digits)) return null
  const n = Number(digits)
  return n <= POOL_SIZE_MAX ? n : null
}

/**
 * 表单值 → 要提交的设置；连接池大小不合法时返回 null。
 *
 * 地址与名称在这里去掉首尾空白：服务端存下的就是去掉之后的值
 * （apiSaveSettings 的 TrimSpace），提交的值与存下的值因此相同。
 */
export function toSettings(form: SettingsForm): Settings | null {
  const poolSize = parsePoolSize(form.pool_size)
  if (poolSize === null) return null
  return {
    panel_url: form.panel_url.trim(),
    panel_name: form.panel_name.trim(),
    show_rate_to_user: form.show_rate_to_user,
    pool_size: poolSize,
  }
}

/** 两份表单值的内容是否相同。字段都是原始值，逐个比较即可。 */
export function sameForm(a: SettingsForm, b: SettingsForm): boolean {
  return a.panel_url === b.panel_url
    && a.panel_name === b.panel_name
    && a.show_rate_to_user === b.show_rate_to_user
    && a.pool_size === b.pool_size
}
