#!/usr/bin/env node
/* 检查 .jsx / .js 源码里两类 `tsc --noEmit` 不报的问题：

   1. 引用了不存在的东西：导入了模块没有导出的名字、导入的模块找不到、用了
      没有声明的标识符（典型：JSX 里写了没导入的组件）。其中“未声明的
      标识符”连 vite build 也不报，只在运行到那一行时抛 ReferenceError。
   2. 声明了却没用到的导入与局部变量。它们本身无害，但通常意味着一处改动
      只做了一半（删了用法没删导入，或算了一个值忘了用）。

   .jsx 文件不做完整的类型检查（tsconfig 里 checkJs 关闭）。这里借用类型
   检查器的名字解析，只保留这几类诊断，其余（隐式 any 等）忽略。 */
import { spawnSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')

/* TS2304 找不到名字；TS2552 找不到名字（附拼写建议）；TS2305 模块没有这个
   导出；TS2724 同上（附拼写建议）；TS2614 把默认导出当具名导出；
   TS2307 找不到模块；TS2613 模块没有默认导出；TS2459 模块里有这个名字但
   没有导出；
   TS6133 声明了但没用到；TS6192 整条 import 都没用到；TS6196 类型声明没用到。 */
const CODES = /error TS(2304|2552|2305|2724|2614|2307|2613|2459|6133|6192|6196):/

const tsc = join(root, 'node_modules', '.bin', 'tsc')
const res = spawnSync(tsc, ['--noEmit', '--checkJs', '--noUnusedLocals', '--pretty', 'false'], { cwd: root, encoding: 'utf8' })
if (res.error) {
  console.error(`check-refs: 无法运行 tsc：${res.error.message}`)
  process.exit(2)
}
const lines = (res.stdout + res.stderr).split('\n').filter(l => CODES.test(l))
for (const l of lines) console.log(l)
if (lines.length) {
  console.error(`\ncheck-refs: ${lines.length} 处无法解析的引用或未使用的声明`)
  process.exit(1)
}
console.log('check-refs: 没有无法解析的引用，也没有未使用的声明')
