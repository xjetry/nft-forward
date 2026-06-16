// Color theme resolution. The default follows the OS; an explicit user choice
// persisted in localStorage overrides it. A null stored value means "follow
// system", in which case we also react to live OS changes.

const KEY = 'nf-theme'
const mq = () => window.matchMedia('(prefers-color-scheme: dark)')

export function resolvedDark(stored) {
  return stored === 'dark' || (stored == null && mq().matches)
}

export function getStoredTheme() {
  return localStorage.getItem(KEY) // 'dark' | 'light' | null(follow system)
}

export function applyTheme(stored) {
  document.documentElement.classList.toggle('dark', resolvedDark(stored))
}

export function setStoredTheme(theme) {
  if (theme == null) localStorage.removeItem(KEY)
  else localStorage.setItem(KEY, theme)
  applyTheme(theme)
}

// Keep following the OS while the user hasn't pinned an explicit choice.
export function initThemeWatcher() {
  mq().addEventListener('change', () => {
    if (getStoredTheme() == null) applyTheme(null)
  })
}
