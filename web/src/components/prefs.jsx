import { createContext, useCallback, useContext, useMemo, useState } from 'react'
import { getStoredTheme, resolvedDark, setStoredTheme } from '../lib/theme'

/* 显示偏好：主题、敏感信息脱敏、复制代理链接时用的格式。
   三者都只存在本浏览器（localStorage），与账号无关。Provider 挂在路由之上，
   外壳里的开关与页面里的读取方共用同一份状态。 */

const PrefsCtx = createContext(/** @type {any} */ (null))

export function PrefsProvider({ children }) {
  // null 表示跟随系统；用户点过一次开关后固定为 'light' / 'dark'。
  const [theme, setTheme] = useState(getStoredTheme)
  const [blurred, setBlurred] = useState(() => localStorage.getItem('nf-blur') === '1')
  const [copyFmt, setCopyFmtState] = useState(() => (localStorage.getItem('nf-copy-fmt') === 'yaml' ? 'yaml' : 'uri'))

  const isDark = resolvedDark(theme)
  const toggleTheme = useCallback(() => {
    const next = resolvedDark(getStoredTheme()) ? 'light' : 'dark'
    setStoredTheme(next)
    setTheme(next)
  }, [])
  const toggleBlur = useCallback(() => setBlurred(v => {
    localStorage.setItem('nf-blur', v ? '0' : '1')
    return !v
  }), [])
  const setCopyFmt = useCallback((fmt) => {
    localStorage.setItem('nf-copy-fmt', fmt)
    setCopyFmtState(fmt)
  }, [])

  const value = useMemo(
    () => ({ isDark, toggleTheme, blurred, toggleBlur, copyFmt, setCopyFmt }),
    [isDark, toggleTheme, blurred, toggleBlur, copyFmt, setCopyFmt],
  )
  return <PrefsCtx.Provider value={value}>{children}</PrefsCtx.Provider>
}

/** @returns {{ isDark: boolean, toggleTheme: () => void, blurred: boolean, toggleBlur: () => void, copyFmt: 'uri' | 'yaml', setCopyFmt: (fmt: 'uri' | 'yaml') => void }} */
export function usePrefs() {
  return useContext(PrefsCtx)
}

/** 是否模糊敏感信息（地址、URI、密钥）。 */
export function useBlur() {
  return usePrefs().blurred
}

/** 复制代理链接时用的格式：'uri' 或 'yaml'。 */
export function useCopyFmt() {
  return usePrefs().copyFmt
}
