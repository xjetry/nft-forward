import { test } from 'node:test'
import assert from 'node:assert/strict'
import { isPrimitiveZone, lintSource } from './design-lint.mjs'

/** 在业务区路径下检查一段源码，返回命中的规则 id 列表。 */
const rulesHit = (code, path = 'pages/Example.jsx') => lintSource(path, code).map(f => f.rule)

test('分区：原子区只有 components/ui、components/page.jsx、dev', () => {
  assert.equal(isPrimitiveZone('components/ui/Button.jsx'), true)
  assert.equal(isPrimitiveZone('components/page.jsx'), true)
  assert.equal(isPrimitiveZone('dev/gallery.jsx'), true)
  assert.equal(isPrimitiveZone('components/Layout.jsx'), false)
  assert.equal(isPrimitiveZone('components/net.jsx'), false)
  assert.equal(isPrimitiveZone('pages/nodes/List.jsx'), false)
})

test('合规写法不产生任何发现', () => {
  const ok = `
    <Card className="flex flex-col gap-4 rounded-card border border-line bg-surface p-5">
      <span className="text-small font-medium text-ink-soft hover:text-accent-ink">名称</span>
      <div className="grid grid-cols-[7rem_minmax(0,1fr)] max-w-[40rem] rounded-full shadow-popover" />
      <Button variant="primary" onClick={async () => { if (!(await confirm({ message: '删除？' }))) return }}>删除</Button>
    </Card>`
  assert.deepEqual(rulesHit(ok), [])
})

test('调色板颜色：带变体、带透明度都能识别', () => {
  assert.deepEqual(rulesHit('<i className="text-blue-600" />'), ['raw-palette'])
  assert.deepEqual(rulesHit('<i className="dark:hover:bg-red-900/10" />'), ['raw-palette'])
  assert.deepEqual(rulesHit('<i className="text-white" />'), ['raw-palette'])
  // 语义色的名字里含 ink / accent 等，不应被当成调色板
  assert.deepEqual(rulesHit('<i className="text-danger-ink bg-accent-soft border-ok-line" />'), [])
})

test('已清空的默认刻度：字号、圆角、阴影、字重', () => {
  assert.deepEqual(rulesHit('<i className="text-sm" />'), ['default-text-scale'])
  assert.deepEqual(rulesHit('<i className="sm:text-2xl" />'), ['default-text-scale'])
  assert.deepEqual(rulesHit('<i className="rounded-lg" />'), ['default-radius-scale'])
  assert.deepEqual(rulesHit('<i className="rounded" />'), ['default-radius-scale'])
  assert.deepEqual(rulesHit('<i className="rounded-t-md" />'), ['default-radius-scale'])
  assert.deepEqual(rulesHit('<i className="shadow-lg" />'), ['default-shadow-scale'])
  assert.deepEqual(rulesHit('<i className="shadow" />'), ['default-shadow-scale'])
  assert.deepEqual(rulesHit('<i className="font-bold" />'), ['default-font-weight'])
  // 语义刻度不受影响
  assert.deepEqual(rulesHit('<i className="text-body rounded-t-card rounded-control shadow-modal font-semibold" />'), [])
})

test('任意值：令牌类属性禁止，布局约束允许，任意值变体不算', () => {
  assert.deepEqual(rulesHit('<i className="text-[13px]" />'), ['arbitrary-value'])
  assert.deepEqual(rulesHit('<i className="px-[22px]" />'), ['arbitrary-value'])
  assert.deepEqual(rulesHit('<i className="w-[110px]" />'), ['arbitrary-value'])
  assert.deepEqual(rulesHit('<i className="md:rounded-[9px]" />'), ['arbitrary-value'])
  assert.deepEqual(rulesHit('<i className="grid-cols-[1fr_auto] min-h-[60px] max-w-[200px]" />'), [])
  assert.deepEqual(rulesHit('<i className="has-[:disabled]:opacity-50" />'), [])
})

test('业务区不写原生交互元素、裸 Link、内联 style', () => {
  assert.deepEqual(rulesHit('<button onClick={f}>x</button>'), ['raw-element'])
  assert.deepEqual(rulesHit('<input value={v} />'), ['raw-element'])
  assert.deepEqual(rulesHit('<select>'), ['raw-element'])
  assert.deepEqual(rulesHit('<textarea />'), ['raw-element'])
  assert.deepEqual(rulesHit('<a href="/x">x</a>'), ['raw-element'])
  assert.deepEqual(rulesHit('<Link to="/x">x</Link>'), ['raw-link'])
  assert.deepEqual(rulesHit('<div style={{ width: 100 }} />'), ['inline-style'])
  // 名字只是以这些标签开头的元素 / 组件不算
  assert.deepEqual(rulesHit('<aside /><article /><Button /><TextLink to="/x" /><NavLink to="/x" />'), [])
})

test('同样的写法在原子区是允许的', () => {
  const code = '<button style={{ width: 1 }} className="w-[3px] transition-transform duration-150"><input /></button>'
  assert.deepEqual(rulesHit(code, 'components/ui/Field.jsx'), [])
  // 但原子区同样不能用已清空的刻度与调色板
  assert.deepEqual(rulesHit('<i className="text-sm bg-blue-500" />', 'components/ui/Field.jsx').sort(), ['default-text-scale', 'raw-palette'])
})

test('动效、全大写、原生对话框', () => {
  assert.deepEqual(rulesHit('<i className="transition-colors" />'), ['motion'])
  assert.deepEqual(rulesHit('<i className="animate-spin" />'), ['motion'])
  assert.deepEqual(rulesHit('<i className="uppercase" />'), ['uppercase'])
  assert.deepEqual(rulesHit('if (window.confirm("删除？")) go()'), ['native-dialog'])
  assert.deepEqual(rulesHit('alert("x")'), ['native-dialog'])
  // useConfirm 返回的 confirm 是允许的
  assert.deepEqual(rulesHit('const confirm = useConfirm()\nif (!(await confirm({ message: "x" }))) return'), [])
  // startTransition 之类的标识符不是类名
  assert.deepEqual(rulesHit('startTransition(() => setX(1))'), [])
})

test('没有样式定义的类名', () => {
  assert.deepEqual(rulesHit('<i className="btn-primary" />'), ['legacy-class'])
  assert.deepEqual(rulesHit('<i className="input-field font-mono" />'), ['legacy-class'])
  assert.deepEqual(rulesHit('<i className="user-hero-main" />'), ['legacy-class'])
  assert.deepEqual(rulesHit('<i className="admin-node-panel" />'), ['legacy-class'])
  assert.deepEqual(rulesHit('<i className="card p-5" />'), ['legacy-class'])
  assert.deepEqual(rulesHit('<label className="fl">x</label>'), ['legacy-class'])
  // 仍然有效的类与普通单词
  assert.deepEqual(rulesHit('<table className="tbl" />'), [])
  assert.deepEqual(rulesHit('const card = getCard(); // not a class'), [])
})

test('注释里的示例不算', () => {
  assert.deepEqual(rulesHit('/* 不要写 text-blue-600 或 <button> */\n// 也不要写 rounded-lg\n<i className="text-ink" />'), [])
})

test('发现带有可定位的行号', () => {
  const [f] = lintSource('pages/Example.jsx', 'const a = 1\n\n<i className="text-sm" />')
  assert.equal(f.line, 3)
  assert.equal(f.match, 'text-sm')
})
