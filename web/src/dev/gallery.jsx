/* 组件陈列页（仅开发用）。

   入口是 web/ui-gallery.html，dev server 下访问 /ui-gallery.html。它不在生产
   构建的入口里（vite build 只打 index.html），所以不会进面板的内嵌产物。

   用途：脱离业务页面单独核对每个原子在各状态、各尺寸、明暗主题下的样子，
   尤其是“相邻控件等高”“嵌套圆角同心”这类只有并排摆出来才看得出的约束。
   新增或修改 components/ui 里的原子时，在这里补一格。 */
import { StrictMode, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import '../index.css'
import {
  Alert, Badge, Button, Card, CardHeader, CardSection, Checkbox, CodeBlock, ConfirmProvider, CopyText, DescItem, DescList,
  Empty, ErrorState, Field, IconButton, IconPlus, IconRefresh, IconTrash, Input, Loading, Menu, Meter, Modal, ModalActions,
  Segmented, Select, Spinner, Stat, StatusDot, Switch, Tag, Textarea, ToastProvider, ToggleChip, Tooltip, useConfirm, useToast,
} from '../components/ui'
import { Page, PageHeader, SearchInput, SortableTh, TableBox, Toolbar, nextSort } from '../components/page'
import { ModeTag, NodeStackTags, NodeStatus, NodeTypeTag, ProtoTag } from '../components/net'

function Section({ title, children }) {
  return (
    <Card>
      <CardHeader title={title} />
      <CardSection className="flex flex-col gap-4">{children}</CardSection>
    </Card>
  )
}

function Row({ label, children }) {
  return (
    <div className="flex flex-wrap items-center gap-3">
      <span className="w-24 flex-none text-caption text-ink-mut">{label}</span>
      {children}
    </div>
  )
}

const NODE_OPTIONS = [
  { value: 1, label: 'hk-01' }, { value: 2, label: 'hk-02' }, { value: 3, label: 'jp-tokyo-01' },
  { heading: '以下节点不可用' }, { value: 4, label: 'us-lax-01', disabled: true },
]

function Gallery() {
  const toast = useToast()
  const confirm = useConfirm()
  const [dark, setDark] = useState(() => document.documentElement.classList.contains('dark'))
  const [text, setText] = useState('hk-01')
  const [node, setNode] = useState('2')
  const [nodes, setNodes] = useState(['1'])
  const [seg, setSeg] = useState('single')
  const [proto, setProto] = useState('tcp')
  const [on, setOn] = useState(true)
  const [checked, setChecked] = useState(false)
  const [chip, setChip] = useState(true)
  const [modal, setModal] = useState(false)
  const [sort, setSort] = useState({ col: 'traffic', dir: 'desc' })
  const [q, setQ] = useState('')

  const flip = () => {
    document.documentElement.classList.toggle('dark', !dark)
    setDark(!dark)
  }

  return (
    <div className="min-h-screen bg-app p-6">
      <Page>
        <PageHeader
          title="组件陈列"
          meta="components/ui 的全部原子"
          actions={<>
            <Button onClick={flip}>{dark ? '切到浅色' : '切到深色'}</Button>
            <Button variant="primary" icon={<IconPlus />}>主操作</Button>
          </>}
        />

        <Section title="按钮">
          <Row label="层级">
            <Button variant="primary">保存</Button>
            <Button>取消</Button>
            <Button variant="ghost">更多</Button>
            <Button variant="primary" icon={<IconPlus />}>添加节点</Button>
            <Button icon={<IconRefresh />}>重新同步</Button>
          </Row>
          <Row label="破坏性">
            <Button variant="primary" danger>确认删除</Button>
            <Button danger icon={<IconTrash />}>删除节点</Button>
            <Button variant="ghost" danger>移除</Button>
          </Row>
          <Row label="状态">
            <Button variant="primary" loading>保存中</Button>
            <Button loading>同步中</Button>
            <Button variant="primary" disabled>不可用</Button>
            <Button disabled>不可用</Button>
          </Row>
          <Row label="小尺寸">
            <Button size="sm" variant="primary">保存</Button>
            <Button size="sm">取消</Button>
            <Button size="sm" variant="ghost">更多</Button>
            <Button size="sm" danger>撤销</Button>
            <IconButton size="sm" label="刷新"><IconRefresh size={14} /></IconButton>
            <IconButton size="sm" variant="secondary" label="删除" danger><IconTrash size={14} /></IconButton>
          </Row>
          <Row label="图标按钮">
            <IconButton label="刷新"><IconRefresh /></IconButton>
            <IconButton variant="secondary" label="刷新"><IconRefresh /></IconButton>
            <IconButton label="删除" danger><IconTrash /></IconButton>
            <Menu items={[
              { label: '编辑', onSelect: () => toast('编辑') },
              { label: '复制为新规则', onSelect: () => toast('复制') },
              { label: '删除', danger: true, onSelect: () => toast('删除', 'error') },
            ]} />
          </Row>
        </Section>

        <Section title="同一行控件等高">
          <Row label="默认 36px">
            <Input value={text} onChange={setText} className="w-40" />
            <Select value={node} onChange={setNode} options={NODE_OPTIONS} className="w-40" />
            <Segmented value={proto} onChange={setProto} options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
            <Button variant="primary">保存</Button>
            <IconButton variant="secondary" label="刷新"><IconRefresh /></IconButton>
          </Row>
          <Row label="紧凑 28px">
            <Input size="sm" value={text} onChange={setText} className="w-32" />
            <Select size="sm" value={node} onChange={setNode} options={NODE_OPTIONS} className="w-32" />
            <Segmented size="sm" value={seg} onChange={setSeg} options={[{ value: 'single', label: '单点', count: 7 }, { value: 'composite', label: '组合', count: 1 }]} />
            <ToggleChip pressed={chip} onChange={setChip}>入口</ToggleChip>
            <ToggleChip pressed={!chip} onChange={v => setChip(!v)}>中转</ToggleChip>
            <Button size="sm">应用</Button>
          </Row>
        </Section>

        <Section title="表单">
          <div className="grid gap-5 md:grid-cols-2">
            <div className="flex flex-col gap-4">
              <Field label="名称" hint="用于在列表里识别这个节点">
                <Input value={text} onChange={setText} placeholder="例如 hk-1" />
              </Field>
              <Field label="端口范围" error="起始端口必须小于结束端口">
                <Input mono value="20000-10001" onChange={() => {}} />
              </Field>
              <Field label="流量配额" optional>
                <Input value="200" onChange={() => {}} suffix="GB" className="w-40" />
              </Field>
              <Field label="入口节点">
                <Select searchable value={node} onChange={setNode} options={NODE_OPTIONS} />
              </Field>
              <Field label="授权用户" optional>
                <Select multiple searchable selectAll value={nodes} onChange={setNodes} options={NODE_OPTIONS.filter(o => !o.heading)} placeholder="不授权" />
              </Field>
              <Field label="已失效的选项">
                <Select value="99" onChange={() => {}} options={NODE_OPTIONS} />
              </Field>
            </div>
            <div className="flex flex-col gap-4">
              <Field label="面板名称" layout="row">
                <Input value="nft-forward" onChange={() => {}} />
              </Field>
              <Field label="显示倍率" layout="row" hint="向普通用户展示节点与链路的计费倍率">
                <div className="flex min-h-control items-center"><Switch checked={on} onChange={setOn} label="显示倍率" /></div>
              </Field>
              <Field label="备注" layout="row">
                <Textarea value="" onChange={() => {}} placeholder="可选" rows={3} />
              </Field>
              <Row label="开关 / 勾选">
                <Switch checked={on} onChange={setOn}>使用 gh-proxy</Switch>
                <Checkbox checked={checked} onChange={setChecked}>覆盖已有设置</Checkbox>
                <Switch checked={false} onChange={() => {}} disabled label="不可用" />
              </Row>
              <Row label="不可用">
                <Input value="只读" onChange={() => {}} disabled className="w-32" />
                <Select value="1" onChange={() => {}} options={NODE_OPTIONS} disabled className="w-32" />
              </Row>
            </div>
          </div>
        </Section>

        <Section title="状态与类别">
          <Row label="状态 Badge">
            <Badge tone="ok" dot>在线</Badge>
            <Badge tone="warn" dot>待同步</Badge>
            <Badge tone="danger" dot>错误</Badge>
            <Badge tone="neutral" dot>离线</Badge>
            <Badge tone="accent">新</Badge>
            <StatusDot tone="ok">在线</StatusDot>
            <StatusDot tone="neutral">离线</StatusDot>
          </Row>
          <Row label="类别 Tag">
            <ProtoTag proto="tcp" /><ProtoTag proto="udp" /><ProtoTag proto="tcp+udp" />
            <ModeTag mode="kernel" /><ModeTag mode="userspace" />
            <NodeTypeTag type="single" /><NodeTypeTag type="composite" /><NodeTypeTag type="self" />
            <Tag>入口</Tag><Tag>中转</Tag>
          </Row>
          <Row label="节点">
            <NodeStatus node={{ online: 1, last_apply_at: { Valid: true } }} />
            <NodeStatus node={{ online: 1 }} />
            <NodeStatus node={{ online: 0, last_error: { Valid: true, String: 'node 2 not connected' } }} />
            <NodeStatus node={{ disabled: true }} />
            <NodeStackTags node={{ relay_host: '1.1.1.1', relay_host_v6: '::1' }} />
            <NodeStackTags node={{ node_type: 'composite', entry_relay_host: '1.1.1.1', entry_relay_host_v6: '::1' }} />
          </Row>
          <Alert>本地 URI 只保存在当前浏览器。</Alert>
          <Alert tone="accent">新版本 agent 可用。</Alert>
          <Alert tone="ok">订阅已更新，共 12 个节点。</Alert>
          <Alert tone="warn">尚未设置面板地址，安装命令使用了你当前访问的域名。</Alert>
          <Alert tone="danger">node 2 not connected</Alert>
        </Section>

        <Card>
          <CardHeader title="表格" description="表头 36px · 数据行 44px" actions={<Button size="sm">导出</Button>} />
          <Toolbar>
            <SearchInput value={q} onChange={setQ} placeholder="搜索规则名称…" />
            <Segmented size="sm" value={seg} onChange={setSeg} options={[{ value: 'single', label: '单点', count: 7 }, { value: 'composite', label: '组合', count: 1 }]} />
          </Toolbar>
          <TableBox>
            <table className="tbl">
              <thead>
                <tr>
                  <th>名称</th><th>入口</th><th>协议</th><th>状态</th>
                  <SortableTh col="traffic" sort={sort} onSort={c => setSort(s => nextSort(s, c))} align="right">流量</SortableTh>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {[['web-hk', 'hk-01:14220', 'tcp', 1, '1.2 GB'], ['game-udp', 'hk-01:17619', 'udp', 0, '820.4 MB'], ['jp-media', 'jp-tokyo-01:16050', 'tcp+udp', 1, '11.7 GB']].map(([name, entry, p, up, traffic]) => (
                  <tr key={name}>
                    <td className="font-medium">{name}</td>
                    <td className="font-mono"><CopyText text={entry} /></td>
                    <td><ProtoTag proto={p} /></td>
                    <td><NodeStatus node={{ online: up, last_apply_at: { Valid: true } }} /></td>
                    <td className="num">{traffic}</td>
                    <td className="num">
                      <div className="flex justify-end gap-1">
                        <IconButton size="sm" label="测试"><IconRefresh size={14} /></IconButton>
                        <Menu size="sm" items={[{ label: '编辑', onSelect: () => {} }, { label: '删除', danger: true, onSelect: () => {} }]} />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </TableBox>
        </Card>

        <div className="grid gap-5 md:grid-cols-2">
          <Card>
            <CardHeader title="数值" />
            <CardSection className="grid grid-cols-2 gap-5">
              <Stat label="在线节点" value="7" unit="/ 8" hint="de-fra-01 离线" />
              <Stat label="本周期流量" value="128.4" unit="GB" hint="配额 200 GB">
                <Meter value={128.4} max={200} label="流量用量" />
              </Stat>
            </CardSection>
            <CardSection>
              <DescList>
                <DescItem label="连接 IP">203.0.113.11</DescItem>
                <DescItem label="Agent 版本"><span className="font-mono">v0.68.1</span></DescItem>
                <DescItem label="最近心跳">12 秒前</DescItem>
              </DescList>
            </CardSection>
          </Card>
          <Card>
            <CardHeader title="反馈" />
            <CardSection className="flex flex-col gap-4">
              <Row label="浮层">
                <Button onClick={() => setModal(true)}>打开弹窗</Button>
                <Button onClick={async () => toast((await confirm({ title: '删除节点', message: '确认删除节点 hk-01？此操作会清空节点上的转发。', confirmText: '删除', danger: true })) ? '已确认' : '已取消')}>确认框</Button>
                <Button onClick={() => toast('已保存')}>成功提示</Button>
                <Button onClick={() => toast('端口范围无效：起始端口必须小于结束端口', 'error')}>错误提示</Button>
                <Tooltip content="node 2 not connected"><Badge tone="danger" dot>错误</Badge></Tooltip>
              </Row>
              <CodeBlock text="curl -fsSL https://panel.example.com/install.sh | bash -s -- --token 6d24439c887ebbedf5f05828c6dd6439fd53f11a83eaab0f30091b2fcae e4d9f" />
              <Row label="加载"><Spinner /><span className="text-small text-ink-mut">测试中…</span></Row>
            </CardSection>
          </Card>
        </div>

        <div className="grid gap-5 md:grid-cols-3">
          <Card><Loading /></Card>
          <Card><Empty title="还没有规则" desc="创建一条规则，把入口端口转发到目标地址。"><Button variant="primary" icon={<IconPlus />}>创建规则</Button></Empty></Card>
          <Card><ErrorState message="服务暂时不可用（502），请稍后重试" onRetry={() => toast('重试')} /></Card>
        </div>
      </Page>

      {modal && (
        <Modal
          title="添加节点"
          onClose={() => setModal(false)}
          footer={<ModalActions note="添加后会生成 token 与安装命令。">
            <Button danger onClick={async () => { if (await confirm({ title: '放弃修改', message: '放弃已填写的内容？', confirmText: '放弃', danger: true })) setModal(false) }}>放弃</Button>
            <Button onClick={() => setModal(false)}>取消</Button>
            <Button variant="primary" onClick={() => setModal(false)}>添加节点</Button>
          </ModalActions>}
        >
          <div className="flex flex-col gap-4">
            <Field label="名称"><Input value={text} onChange={setText} data-autofocus /></Field>
            <Field label="入口节点"><Select searchable value={node} onChange={setNode} options={NODE_OPTIONS} /></Field>
            <Field label="计费方向">
              <Segmented value={seg} onChange={setSeg} options={[{ value: 'single', label: '双向' }, { value: 'composite', label: '单向' }]} />
            </Field>
          </div>
        </Modal>
      )}
    </div>
  )
}

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <BrowserRouter>
      <ToastProvider>
        <ConfirmProvider>
          <Gallery />
        </ConfirmProvider>
      </ToastProvider>
    </BrowserRouter>
  </StrictMode>,
)
