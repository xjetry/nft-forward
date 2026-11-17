/* 设计令牌的可读性约束。

   index.css 的注释声明了若干对比度性质（例如“ink-mut 可以放心用于 12px 的
   辅助文字”）。这些是从色值算出来的结论，色值一改就可能不再成立，所以在
   这里从 index.css 的实际色值重新计算并断言，而不是相信注释。

   口径：WCAG 2.x 相对亮度与对比度。半透明色先叠到它实际所在的底色上
   再计算。 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'index.css'), 'utf8')

/** 取出某个规则块（`@theme {` 或 `.dark {`）里声明的全部 --color-* 。 */
function colorsIn(blockStart) {
  const start = css.indexOf(blockStart)
  assert.notEqual(start, -1, `index.css 里找不到 ${blockStart}`)
  // 找到与起始花括号配对的结束花括号（@theme 里嵌着 @keyframes）。
  let depth = 0
  let end = start
  for (let i = css.indexOf('{', start); i < css.length; i++) {
    if (css[i] === '{') depth++
    else if (css[i] === '}' && --depth === 0) { end = i; break }
  }
  const out = {}
  for (const m of css.slice(start, end).matchAll(/--color-([a-z-]+):\s*([^;]+);/g)) out[m[1]] = m[2].trim()
  return out
}

const light = colorsIn('@theme {')
const dark = { ...light, ...colorsIn('\n.dark {') }

/** 解析 #rrggbb 或 rgb(r g b / a)，返回 [r, g, b, a]。 */
function parse(value) {
  const hex = value.match(/^#([0-9a-f]{6})$/i)
  if (hex) return [0, 2, 4].map(i => parseInt(hex[1].slice(i, i + 2), 16)).concat(1)
  const rgb = value.match(/^rgb\((\d+)\s+(\d+)\s+(\d+)(?:\s*\/\s*([\d.]+))?\)$/)
  if (rgb) return [Number(rgb[1]), Number(rgb[2]), Number(rgb[3]), rgb[4] == null ? 1 : Number(rgb[4])]
  throw new Error(`无法解析颜色：${value}`)
}

/** 把前景色按其 alpha 叠到不透明的底色上。 */
function over(fg, bg) {
  const [r, g, b, a] = fg
  return [r * a + bg[0] * (1 - a), g * a + bg[1] * (1 - a), b * a + bg[2] * (1 - a), 1]
}

function luminance([r, g, b]) {
  const lin = c => { const s = c / 255; return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4 }
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)
}

function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

/**
 * 断言：文字色 fg 画在 bg 上（bg 若半透明，则它本身铺在 base 上）对比度不低于 min。
 */
function assertReadable(theme, name, fg, bg, min, base = 'surface') {
  const tokens = name === 'light' ? light : dark
  for (const key of [fg, bg, base]) assert.ok(tokens[key], `${name} 主题缺少 --color-${key}`)
  const baseColor = parse(tokens[base])
  assert.equal(baseColor[3], 1, `--color-${base} 必须不透明`)
  const bgColor = over(parse(tokens[bg]), baseColor)
  const fgColor = over(parse(tokens[fg]), bgColor)
  const ratio = contrast(fgColor, bgColor)
  assert.ok(ratio >= min, `${name}: ${fg} 在 ${bg} 上的对比度是 ${ratio.toFixed(2)}，低于 ${min}`)
}

for (const name of ['light', 'dark']) {
  test(`${name}: 三级文字色在各层表面上都满足正文对比度 4.5`, () => {
    for (const fg of ['ink', 'ink-soft', 'ink-mut']) {
      for (const bg of ['app', 'surface', 'subtle', 'overlay']) assertReadable(name, name, fg, bg, 4.5)
    }
  })

  test(`${name}: 强调色与危险色填充上的文字满足 4.5`, () => {
    assertReadable(name, name, 'on-accent', 'accent', 4.5)
    assertReadable(name, name, 'on-accent', 'accent-hover', 4.5)
    assertReadable(name, name, 'on-accent', 'danger', 4.5)
    assertReadable(name, name, 'on-accent', 'danger-hover', 4.5)
  })

  test(`${name}: 链接色与状态文字色在表面和各自的浅底上都满足 4.5`, () => {
    for (const tone of ['accent', 'ok', 'warn', 'danger']) {
      for (const bg of ['surface', 'app', 'subtle']) assertReadable(name, name, `${tone}-ink`, bg, 4.5)
      // 浅底（-soft）出现在卡片（surface）上，也出现在弹窗（overlay）上。
      assertReadable(name, name, `${tone}-ink`, `${tone}-soft`, 4.5, 'surface')
      assertReadable(name, name, `${tone}-ink`, `${tone}-soft`, 4.5, 'overlay')
    }
  })

  test(`${name}: 反色气泡（toast / tooltip：surface 色文字画在 ink 底上）满足 4.5`, () => {
    assertReadable(name, name, 'surface', 'ink', 4.5)
  })

  test(`${name}: 状态圆点与焦点环在表面上满足非文字对比度 3`, () => {
    for (const fg of ['accent', 'ok', 'warn', 'danger']) assertReadable(name, name, fg, 'surface', 3)
  })
}

test('深色主题只改值、不新增令牌', () => {
  const extra = Object.keys(colorsIn('\n.dark {')).filter(k => !(k in light))
  assert.deepEqual(extra, [])
})
