import { useState, useEffect } from 'react'

// useIsMobile tracks Tailwind's `md` breakpoint (768px) so list pages can render
// only the layout they need — the desktop table OR the mobile cards — instead of
// rendering both and hiding one with CSS, which doubles the DOM nodes on large
// lists.
export function useIsMobile(breakpoint = 768) {
  const query = `(max-width: ${breakpoint - 1}px)`
  const [isMobile, setIsMobile] = useState(
    () => typeof window !== 'undefined' && window.matchMedia(query).matches
  )
  useEffect(() => {
    const mql = window.matchMedia(query)
    const onChange = () => setIsMobile(mql.matches)
    mql.addEventListener('change', onChange)
    onChange()
    return () => mql.removeEventListener('change', onChange)
  }, [query])
  return isMobile
}
