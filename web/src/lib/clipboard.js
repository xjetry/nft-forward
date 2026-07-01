/* navigator.clipboard only exists in a secure context (HTTPS or localhost).
   Panels reached over plain HTTP on a non-localhost host get `undefined`
   there, so every caller needs the execCommand fallback below instead of
   calling navigator.clipboard.writeText directly. */

export async function copyToClipboard(text) {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }

  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0'
  document.body.appendChild(ta)
  ta.focus()
  ta.select()
  try {
    if (!document.execCommand('copy')) throw new Error('execCommand copy failed')
  } finally {
    document.body.removeChild(ta)
  }
}
