/* Shared list-page shell: rounded panel, page header, toolbar with search.
   One look across admin and user pages so the two never drift apart again.
   Colors use semantic tokens (surface/ink/line) so dark mode comes for free. */

/* Page title + item count, sits above the panel. */
export function PageHeader({ title, count, unit = '条' }) {
  return (
    <div className="flex items-baseline gap-2.5 mb-4">
      <h1 className="m-0 text-lg font-bold tracking-[-0.01em] text-ink">{title}</h1>
      {count != null && <span className="text-[13px] text-ink-mut">共 {count} {unit}</span>}
    </div>
  )
}

/* Rounded card that wraps a list's toolbar and table. With `fill`, it grows to
   fill a flex-column page and lays its children out as a column so a
   TableScroll child can scroll while the toolbar stays put. */
export function Panel({ children, className = '', fill = false }) {
  return (
    <section className={`bg-surface border border-line rounded-2xl shadow-[0_1px_2px_rgba(16,24,40,0.04)] overflow-hidden ${fill ? 'flex-1 min-h-0 flex flex-col' : ''} ${className}`}>
      {children}
    </section>
  )
}

/* Scroll container for a list table inside a `fill` Panel: only the rows
   scroll, while the sticky table header (and the Panel toolbar above) stay
   fixed. */
export function TableScroll({ children }) {
  return <div className="flex-1 min-h-0 overflow-auto">{children}</div>
}

/* Toolbar row inside a Panel — typically a SearchInput plus a primary action. */
export function PanelToolbar({ children }) {
  return (
    <div className="flex items-center gap-3.5 px-[22px] py-4 border-b border-line-soft">
      {children}
    </div>
  )
}

/* Search box with a leading magnifier; controlled via value/onChange. */
export function SearchInput({ value, onChange, placeholder }) {
  return (
    <div className="relative flex-1 max-w-[360px]">
      <svg className="w-4 h-4 absolute left-3 top-1/2 -translate-y-1/2 text-ink-mut pointer-events-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="11" cy="11" r="7" /><path d="m21 21-4.3-4.3" /></svg>
      <input value={value} onChange={e => onChange(e.target.value)} placeholder={placeholder}
        className="w-full text-[13.5px] pl-9 pr-3 py-[9px] bg-surface border border-line rounded-[10px] outline-none text-ink focus:border-blue-600 focus:ring-3 focus:ring-blue-600/10 transition-colors" />
    </div>
  )
}

/* Primary toolbar action; right-aligned by default. */
export function ToolbarButton({ onClick, children, className = '' }) {
  return (
    <button onClick={onClick}
      className={`ml-auto inline-flex items-center gap-1.5 text-[13.5px] font-semibold text-white bg-blue-600 hover:bg-blue-700 border-0 px-[18px] py-[9px] rounded-[10px] cursor-pointer transition-colors ${className}`}>
      {children}
    </button>
  )
}
