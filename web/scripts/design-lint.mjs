#!/usr/bin/env node
/* 设计约束检查：把“页面只通过设计令牌与 components/ui 的原子来表达界面”
   这条约定变成可执行的检查。

   为什么需要它：index.css 清空了 Tailwind 的默认调色板 / 字号阶 / 圆角阶 /
   阴影阶，绕开令牌的类名不会生成任何 CSS —— 这类写法不会报错，只会静默
   失效（元素没有颜色、没有圆角）。静默失效靠肉眼很难发现，所以由脚本
   逐条找出来。

   分区：
   - 原子区（components/ui、components/page.jsx、dev/）：负责实现原子，允许
     使用原生交互元素、任意值、内联 style、过渡动画。
   - 业务区（其余全部）：只能组合原子。

   用法：node scripts/design-lint.mjs [--summary]
   退出码：有任何发现为 1，否则为 0。 */
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const PALETTE = 'red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|neutral|stone|white|black'
const COLOR_PREFIX = 'bg|text|border|ring|outline|fill|stroke|from|via|to|accent|divide|decoration|placeholder|caret|shadow'

/** 对所有源码生效：这些类名在当前令牌表下不生成任何 CSS。 */
const EVERYWHERE = [
  {
    id: 'raw-palette',
    re: new RegExp(`(?<![\\w-])(?:[a-z-]+:)*(?:${COLOR_PREFIX})-(?:${PALETTE})(?:-\\d{2,3})?(?:/\\d+)?(?![\\w-])`, 'g'),
    why: '调色板颜色已清空，改用语义色（ink / accent / ok / warn / danger…）',
  },
  {
    id: 'default-text-scale',
    re: /(?<![\w-])(?:[a-z-]+:)*text-(?:xs|sm|base|lg|xl|[2-9]xl)(?![\w-])/g,
    why: '默认字号阶已清空，改用 text-caption / small / body / title / heading / display',
  },
  {
    id: 'default-radius-scale',
    re: /(?<![\w-])(?:[a-z-]+:)*rounded(?:-(?:[trblse]|tl|tr|bl|br|ss|se|es|ee))?(?:-(?:none|xs|sm|md|lg|xl|[2-4]xl))?(?![\w-])/g,
    why: '默认圆角阶已清空，改用 rounded-chip / control / popover / card / modal（或 rounded-full）',
  },
  {
    id: 'default-shadow-scale',
    re: /(?<![\w-])(?:[a-z-]+:)*shadow(?:-(?:2xs|xs|sm|md|lg|xl|2xl|inner|none))?(?![\w-])/g,
    why: '默认阴影阶已清空，只有 shadow-popover / shadow-modal 两级',
  },
  {
    id: 'default-font-weight',
    re: /(?<![\w-])(?:[a-z-]+:)*font-(?:thin|extralight|light|bold|extrabold|black)(?![\w-])/g,
    why: '字重只有 font-normal / medium / semibold 三级',
  },
]

/** 允许使用任意值的工具类前缀：它们表达的是布局约束，不是设计令牌。 */
const ARBITRARY_OK = /^(?:grid-cols|grid-rows|col-span|row-span|max-w|min-w|max-h|min-h|basis)-\[/

/** 只对业务区生效。 */
const FEATURE_ONLY = [
  {
    id: 'arbitrary-value',
    // 末尾的 (?!:) 排除任意值“变体”（has-[:disabled]:、data-[state=open]:），
    // 它们是选择器而不是取值。
    re: /(?<![\w-])(?:[a-z-]+:)*-?[a-z][a-z-]*-\[[^\]\s]+\](?!:)/g,
    accept: m => ARBITRARY_OK.test(m.replace(/^(?:[a-z-]+:)*-?/, '')),
    why: '任意值绕开了令牌；字号 / 间距 / 圆角 / 颜色用令牌或标准刻度，确需新值先加令牌',
  },
  {
    id: 'inline-style',
    re: /\bstyle=\{\{/g,
    why: '内联 style 绕开了令牌；动态尺寸交给原子（如 Meter），固定尺寸用工具类',
  },
  {
    id: 'raw-element',
    re: /<(?:button|input|select|textarea|a)(?=[\s>/])/g,
    why: '业务区不直接写原生交互元素，用 Button / IconButton / TextLink / Input / Select / Textarea / Checkbox / Switch',
  },
  {
    id: 'raw-link',
    re: /<Link(?=[\s>/])/g,
    why: '站内链接用 TextLink、<Button to>、ListItem 或 PageHeader 的 back，链接样式才只有一种',
  },
  {
    id: 'motion',
    re: /(?<![\w-])(?:[a-z-]+:)*(?:transition(?:-[a-z]+)?|animate-[a-z-]+|duration-\d+|ease-[a-z-]+)(?![\w-])/g,
    why: '动效由原子统一持有；业务区不自行添加过渡或动画',
  },
  {
    id: 'uppercase',
    re: /(?<![\w-])(?:[a-z-]+:)*uppercase(?![\w-])/g,
    why: '标签不使用全大写',
  },
  {
    id: 'native-dialog',
    re: /\b(?:window\.)?(?:confirm|alert|prompt)\(/g,
    accept: (m, line) => /await confirm\(|useConfirm\(/.test(line) && !m.startsWith('window.'),
    why: '不使用浏览器原生对话框，确认用 useConfirm()',
  },
  {
    id: 'legacy-class',
    re: /(?<![\w-])(?:btn-(?:primary|secondary|danger(?:-sm)?)|icon-btn(?:-danger)?|input-field|card-header|stat-card|stat-sub|seg-label|seg-span|mobile-card|tbl-scroll|copy-panel[a-z-]*|detail-(?:grid|item|label|value)|route-(?:stack|line|name|node)|proxy-[a-z-]+|user-[a-z]+(?:-[a-z]+)*|admin-[a-z]+(?:-[a-z]+)*|settings-[a-z]+(?:-[a-z]+)*|sidebar-[a-z]+(?:-[a-z]+)*|app-(?:sidebar|main|content|topbar|panel|table-scroll)|panel-(?:toolbar|filterbar)|page-header|search-input(?:-field)?|toolbar-button[a-z-]*|mini-metric[a-z-]*|donut-[a-z]+|dot-grid|tag-(?:tcp|udp|tcpudp|kernel|user)|animate-(?:in|toast|fadeout))(?![\w-])/g,
    why: 'index.css 里没有这个类，写上去不会有任何样式',
  },
  {
    // card / fl 是普通英文单词，只在 className 的字面量里才当作类名看待。
    id: 'legacy-class',
    re: /className=\{?[`"'][^`"']*?(?<![\w-])(?:card|fl)(?![\w-])/g,
    why: 'index.css 里没有 .card / .fl，用 <Card> 与 <Field>',
  },
]

const PRIMITIVE_ZONE = [
  ['components', 'ui'],
  ['components', 'page.jsx'],
  ['dev'],
]

/** @param {string} relPath 相对 src 的路径（任意分隔符） */
export function isPrimitiveZone(relPath) {
  const parts = relPath.split(/[\\/]/)
  return PRIMITIVE_ZONE.some(prefix => prefix.every((seg, i) => parts[i] === seg))
}

/* 去掉注释再检查：注释里会出现被禁写法的例子（比如这条规则自己的说明）。
   保留换行，行号才对得上。不处理字符串里的 // —— URL 里的 // 后面跟着的
   是地址而不是类名，误删不影响检查结果。 */
function stripComments(text) {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, m => m.replace(/[^\n]/g, ' '))
    .replace(/(^|[^:"'`])\/\/[^\n]*/g, (m, lead) => lead + ' '.repeat(m.length - lead.length))
}

/**
 * 检查一份源码。
 * @param {string} relPath 相对 src 的路径，用于判断所属分区
 * @param {string} text 源码
 * @returns {Array<{ file: string, line: number, rule: string, match: string, why: string }>}
 */
export function lintSource(relPath, text) {
  const rules = isPrimitiveZone(relPath) ? EVERYWHERE : [...EVERYWHERE, ...FEATURE_ONLY]
  const lines = stripComments(text).split('\n')
  const findings = []
  lines.forEach((line, i) => {
    for (const rule of rules) {
      rule.re.lastIndex = 0
      for (const m of line.matchAll(rule.re)) {
        if (rule.accept?.(m[0], line)) continue
        findings.push({ file: relPath, line: i + 1, rule: rule.id, match: m[0], why: rule.why })
      }
    }
  })
  return findings
}

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) walk(full, out)
    else if (/\.(jsx?|tsx?)$/.test(name) && !/\.test\.[jt]sx?$/.test(name)) out.push(full)
  }
  return out
}

/** @param {string} srcDir */
export function lintTree(srcDir) {
  return walk(srcDir).flatMap(file => lintSource(relative(srcDir, file).split(sep).join('/'), readFileSync(file, 'utf8')))
}

function main() {
  const srcDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')
  const findings = lintTree(srcDir)
  const summary = process.argv.includes('--summary')

  if (findings.length === 0) {
    console.log('design-lint: 通过')
    return 0
  }
  if (summary) {
    const byFile = new Map()
    for (const f of findings) byFile.set(f.file, (byFile.get(f.file) || 0) + 1)
    for (const [file, n] of [...byFile].sort((a, b) => b[1] - a[1])) console.log(`${String(n).padStart(5)}  ${file}`)
    const byRule = new Map()
    for (const f of findings) byRule.set(f.rule, (byRule.get(f.rule) || 0) + 1)
    console.log('')
    for (const [rule, n] of [...byRule].sort((a, b) => b[1] - a[1])) console.log(`${String(n).padStart(5)}  ${rule}`)
  } else {
    for (const f of findings) console.log(`src/${f.file}:${f.line}  [${f.rule}] ${f.match}  — ${f.why}`)
  }
  console.log(`\ndesign-lint: ${findings.length} 处不符合设计约束`)
  return 1
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main())
}
