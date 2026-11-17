#!/usr/bin/env node
/* 检查源码里“引用了不存在的东西”：导入了模块没有导出的名字、导入的模块
   找不到、用了没有声明的标识符（典型：JSX 里写了没导入的组件）。

   .jsx 文件不做完整的类型检查（tsconfig 里 checkJs 关闭），所以这类错误
   不会被 `tsc --noEmit` 报出来；其中“未声明的标识符”连 vite build 也不报，
   只在运行到那一行时抛 ReferenceError。这里借用类型检查器的名字解析，只
   保留这几类诊断，其余（隐式 any 等）忽略。 */
import { spawnSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')

/* TS2304 找不到名字；TS2552 找不到名字（附拼写建议）；TS2305 模块没有这个
   导出；TS2724 同上（附拼写建议）；TS2614 把默认导出当具名导出；
   TS2307 找不到模块；TS2613 模块没有默认导出。 */
const CODES = /error TS(2304|2552|2305|2724|2614|2307|2613):/

const tsc = join(root, 'node_modules', '.bin', 'tsc')
const res = spawnSync(tsc, ['--noEmit', '--checkJs', '--pretty', 'false'], { cwd: root, encoding: 'utf8' })
if (res.error) {
  console.error(`check-refs: 无法运行 tsc：${res.error.message}`)
  process.exit(2)
}
const lines = (res.stdout + res.stderr).split('\n').filter(l => CODES.test(l))
for (const l of lines) console.log(l)
if (lines.length) {
  console.error(`\ncheck-refs: ${lines.length} 处引用了不存在的导出、模块或标识符`)
  process.exit(1)
}
console.log('check-refs: 未发现无法解析的引用')
