import { createContext, useContext, useId } from 'react'
import { cx } from './cx'

/* Field 把自己生成的 id 通过 context 交给内部控件：label 的 htmlFor、控件的
   id、提示/错误文字的 aria-describedby 因此总是成对出现，调用方不需要手工
   起 id 再四处传。控件脱离 Field 单独使用时 context 为 null，行为退化为
   普通受控输入。 */
const FieldCtx = createContext(null)

/** 控件侧读取所在 Field 的 id / 描述 / 校验态；不在 Field 内时返回空对象。 */
export function useField() {
  return useContext(FieldCtx) || {}
}

/**
 * 表单项：标签 + 控件 + 提示或错误。
 *
 * - `layout="stack"`（默认）标签在上，适合弹窗与窄列；
 *   `layout="row"` 标签在左、固定 10rem 宽，适合设置页这类宽表单。
 * - `error` 存在时取代 `hint`，并把内部控件标记为 aria-invalid。
 *
 * @param {{
 *   label: import('react').ReactNode,
 *   hint?: import('react').ReactNode,
 *   error?: import('react').ReactNode,
 *   optional?: boolean,
 *   layout?: 'stack' | 'row',
 *   className?: string,
 *   children: import('react').ReactNode,
 * }} props
 */
export function Field({ label, hint, error, optional = false, layout = 'stack', className, children }) {
  const id = useId()
  const descId = error ? `${id}-error` : hint ? `${id}-hint` : undefined
  return (
    <FieldCtx.Provider value={{ id, describedBy: descId, invalid: !!error }}>
      <div className={cx(
        layout === 'row'
          ? 'grid grid-cols-1 gap-x-4 gap-y-1.5 sm:grid-cols-[10rem_minmax(0,1fr)]'
          : 'flex flex-col gap-1.5',
        className,
      )}>
        {/* row 布局下标签占一个控件高度并垂直居中，文字因此与右侧控件里的
            文字落在同一条视觉中线上，而不是贴着控件顶边。self-start 让它
            不被网格拉伸到整行高度：右侧还有提示文字时，行比控件高，拉伸后
            标签会对着“控件 + 提示”的中线而不是控件的中线。 */}
        <label htmlFor={id} className={cx(
          'text-small font-medium text-ink-soft',
          layout === 'row' && 'sm:flex sm:min-h-control sm:items-center sm:self-start',
        )}>
          {label}
          {optional && <span className="ml-1.5 font-normal text-ink-mut">可选</span>}
        </label>
        <div className="flex min-w-0 flex-col gap-1.5">
          {children}
          {error
            ? <p id={descId} role="alert" className="text-caption text-danger-ink">{error}</p>
            : hint
              ? <p id={descId} className="text-caption text-ink-mut">{hint}</p>
              : null}
        </div>
      </div>
    </FieldCtx.Provider>
  )
}

/* 输入类控件共用的外框。焦点、校验、禁用三种状态都画在外框上（内部
   <input> 无边框），这样前后缀与输入区始终处在同一个边界里。 */
const BOX = 'flex items-center rounded-control border bg-surface text-ink has-[:disabled]:bg-subtle has-[:disabled]:text-ink-mut'
const BOX_STATE = {
  normal: 'border-line-strong focus-within:border-accent focus-within:ring-2 focus-within:ring-accent/20',
  invalid: 'border-danger focus-within:ring-2 focus-within:ring-danger/20',
}
const BOX_SIZE = {
  md: 'h-control text-body',
  sm: 'h-control-sm text-small',
}
const PAD = { md: 'px-3', sm: 'px-2.5' }

/**
 * 单行输入框。高度来自高度令牌，与同 size 的 Button / Select 等高。
 *
 * - 外框是块级 flex：默认撑满所在列；需要定宽时给 `className` 传 `w-*`。
 * - `mono` 用于地址、端口、token 等机器字面量。
 * - `prefix` / `suffix` 是框内的静态附加物（搜索图标、单位），不可交互。
 * - `onChange` 直接给出字符串值，而不是事件对象。
 *
 * @param {{
 *   value: string | number,
 *   onChange?: (value: string) => void,
 *   size?: 'md' | 'sm',
 *   mono?: boolean,
 *   invalid?: boolean,
 *   prefix?: import('react').ReactNode,
 *   suffix?: import('react').ReactNode,
 *   className?: string,
 * } & Record<string, unknown>} props
 */
export function Input({ value, onChange, size = 'md', mono = false, invalid, prefix, suffix, className, id, ...rest }) {
  const field = useField()
  const bad = invalid ?? field.invalid
  return (
    <div className={cx(BOX, BOX_SIZE[size], BOX_STATE[bad ? 'invalid' : 'normal'], className)}>
      {prefix != null && <span className={cx('flex flex-none items-center text-ink-mut', size === 'sm' ? 'pl-2' : 'pl-2.5')}>{prefix}</span>}
      <input
        id={id ?? field.id}
        value={value}
        onChange={onChange ? e => onChange(e.target.value) : undefined}
        aria-invalid={bad || undefined}
        aria-describedby={field.describedBy}
        className={cx(
          'h-full min-w-0 flex-1 bg-transparent outline-none placeholder:text-ink-mut',
          PAD[size],
          prefix != null && 'pl-2',
          suffix != null && 'pr-2',
          mono && 'font-mono text-small',
        )}
        {...rest}
      />
      {suffix != null && <span className={cx('flex flex-none items-center text-caption text-ink-mut', size === 'sm' ? 'pr-2' : 'pr-3')}>{suffix}</span>}
    </div>
  )
}

/**
 * 多行输入框。
 * @param {{
 *   value: string,
 *   onChange?: (value: string) => void,
 *   mono?: boolean,
 *   invalid?: boolean,
 *   rows?: number,
 *   className?: string,
 * } & Record<string, unknown>} props
 */
export function Textarea({ value, onChange, mono = false, invalid, rows = 4, className, id, ...rest }) {
  const field = useField()
  const bad = invalid ?? field.invalid
  return (
    <textarea
      id={id ?? field.id}
      value={value}
      rows={rows}
      onChange={onChange ? e => onChange(e.target.value) : undefined}
      aria-invalid={bad || undefined}
      aria-describedby={field.describedBy}
      className={cx(
        'block w-full resize-y rounded-control border bg-surface px-3 py-2 text-ink outline-none placeholder:text-ink-mut',
        'disabled:bg-subtle disabled:text-ink-mut',
        bad
          ? 'border-danger focus:ring-2 focus:ring-danger/20'
          : 'border-line-strong focus:border-accent focus:ring-2 focus:ring-accent/20',
        mono ? 'font-mono text-small' : 'text-body',
        className,
      )}
      {...rest}
    />
  )
}

/**
 * 复选框 + 文字标签。`onChange` 直接给出布尔值。
 * @param {{
 *   checked: boolean,
 *   onChange: (checked: boolean) => void,
 *   disabled?: boolean,
 *   className?: string,
 *   children?: import('react').ReactNode,
 * } & Record<string, unknown>} props
 */
export function Checkbox({ checked, onChange, disabled = false, className, children, ...rest }) {
  return (
    <label className={cx('inline-flex items-center gap-2 text-body text-ink select-none', disabled && 'opacity-50', className)}>
      <input type="checkbox" checked={checked} disabled={disabled} onChange={e => onChange(e.target.checked)} {...rest} />
      {children}
    </label>
  )
}

/**
 * 开关：表达“立即生效的二元状态”。需要随表单一起提交的二选一用 Segmented
 * 或 Checkbox，不用开关。
 *
 * 轨道 36×20、滑块 16、四周留 2px —— 滑块圆角与轨道圆角同心。滑块位移是
 * 这里唯一的动画：它说明状态从哪一侧切到了哪一侧。
 *
 * @param {{
 *   checked: boolean,
 *   onChange: (checked: boolean) => void,
 *   disabled?: boolean,
 *   label?: string,
 *   children?: import('react').ReactNode,
 * }} props `label` 是无可见文字时的无障碍名称；有 children 时以 children 为名。
 */
export function Switch({ checked, onChange, disabled = false, label, children }) {
  const field = useField()
  const control = (
    <button
      type="button"
      role="switch"
      id={field.id}
      aria-checked={checked}
      aria-label={children ? undefined : label}
      aria-describedby={field.describedBy}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        'relative inline-flex h-5 w-9 flex-none items-center rounded-full disabled:opacity-50',
        checked ? 'bg-accent' : 'bg-line-strong',
      )}
    >
      <span className={cx(
        'size-4 rounded-full bg-on-accent transition-transform duration-150 ease-out',
        checked ? 'translate-x-4.5' : 'translate-x-0.5',
      )} />
    </button>
  )
  if (!children) return control
  return (
    <label className={cx('inline-flex items-center gap-2.5 text-body text-ink select-none', disabled && 'opacity-50')}>
      {control}
      <span>{children}</span>
    </label>
  )
}

/**
 * 分段控件：在 2–4 个互斥选项里单选，所有选项始终可见。用于视图切换与
 * 表单里的小枚举（协议、转发模式、计费方向）。
 *
 * 外框圆角 control(6) = 分段圆角 chip(4) + 2px 内边距，两层圆角同心。
 * 语义是 radiogroup：左右方向键在选项间移动并选中。
 *
 * @template {string | number} V
 * @param {{
 *   value: V,
 *   onChange: (value: V) => void,
 *   options: Array<{ value: V, label: import('react').ReactNode, count?: number, title?: string, disabled?: boolean }>,
 *   size?: 'md' | 'sm',
 *   label?: string,
 *   disabled?: boolean,
 *   className?: string,
 * }} props
 */
export function Segmented({ value, onChange, options, size = 'md', label, disabled = false, className }) {
  const field = useField()
  const move = (e, index) => {
    const dir = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0
    if (!dir) return
    e.preventDefault()
    const enabled = options.map((o, i) => (o.disabled ? -1 : i)).filter(i => i >= 0)
    const at = enabled.indexOf(index)
    const next = enabled[(at + dir + enabled.length) % enabled.length]
    onChange(options[next].value)
    e.currentTarget.parentElement?.children[next]?.focus()
  }
  return (
    <div
      role="radiogroup"
      id={field.id}
      aria-label={label}
      aria-describedby={field.describedBy}
      className={cx(
        'inline-flex flex-none items-center gap-0.5 rounded-control bg-subtle p-0.5',
        size === 'sm' ? 'h-control-sm' : 'h-control',
        disabled && 'opacity-50',
        className,
      )}
    >
      {options.map((o, i) => {
        const selected = o.value === value
        return (
          <button
            key={String(o.value)}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={selected ? 0 : -1}
            title={o.title}
            disabled={disabled || o.disabled}
            onClick={() => onChange(o.value)}
            onKeyDown={e => move(e, i)}
            className={cx(
              'inline-flex h-full flex-1 items-center justify-center gap-1.5 rounded-chip border px-3 font-medium whitespace-nowrap',
              size === 'sm' ? 'text-caption' : 'text-small',
              selected ? 'border-line bg-overlay text-ink' : 'border-transparent text-ink-soft hover:text-ink',
            )}
          >
            {o.label}
            {o.count != null && <span className="font-normal text-ink-mut">{o.count}</span>}
          </button>
        )
      })}
    </div>
  )
}

/**
 * 可叠加的筛选开关（多个可同时按下）。互斥的选择用 Segmented。
 * @param {{
 *   pressed: boolean,
 *   onChange: (pressed: boolean) => void,
 *   title?: string,
 *   children: import('react').ReactNode,
 * }} props
 */
export function ToggleChip({ pressed, onChange, title, children }) {
  return (
    <button
      type="button"
      aria-pressed={pressed}
      title={title}
      onClick={() => onChange(!pressed)}
      className={cx(
        'inline-flex h-control-sm flex-none items-center gap-1.5 rounded-control border px-2.5 text-small font-medium whitespace-nowrap',
        pressed
          ? 'border-accent-line bg-accent-soft text-accent-ink'
          : 'border-line-strong bg-surface text-ink-soft hover:bg-subtle hover:text-ink',
      )}
    >
      {children}
    </button>
  )
}
