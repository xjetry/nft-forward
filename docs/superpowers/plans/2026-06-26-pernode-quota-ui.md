# Per-Node 配额 UI + 倍率迁移 + 端口范围 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将流量倍率从 nodes 迁移到 node_hops，添加 per-node 配额/重置/周期的前端 UI，添加节点端口范围配置。

**Architecture:** 后端迁移 traffic_multiplier 到 node_hops 表并更新 applyCounters；前端在用户详情页扩展授权节点表格（配额/用量/重置），在组合节点页加倍率输入，在添加节点对话框加端口范围。

**Tech Stack:** Go, SQLite, React (JSX), Vite, chi router

## Global Constraints

- `nodeCols` / `scanNode` / `grants.go` inline scan 三处必须保持对齐
- 前端遵循现有模式：inline form + api.post + toast + load()
- 迁移文件编号 0014
- 不在代码/commit 中包含过程信息

---

### Task 1: 倍率迁移 — node_hops.traffic_multiplier + 后端改造

**Files:**
- Create: `internal/db/migrations/0014_hop_multiplier.sql`
- Modify: `internal/db/queries.go` (Node struct 移除 TrafficMultiplier, nodeCols, scanNode)
- Modify: `internal/db/grants.go` (NodeHop struct 加 TrafficMultiplier, scanNodeHop, ListNodeHops, ListAllNodeHops, ListNodesForUser inline scan)
- Modify: `internal/db/traffic.go` (NodeMultipliers → HopMultipliers)
- Modify: `internal/server/hub.go` (applyCounters 用 HopMultipliers)
- Modify: `internal/server/api.go` (apiUpdateNodeHops 接受 traffic_multiplier, apiGetNode 返回 traffic_multiplier)
- Test: `internal/db/traffic_test.go` (TestHopMultipliers)

**Interfaces:**
- Produces: `HopMultipliers(d *sql.DB) (map[int64]map[int64]float64, error)` — map[compositeNodeID][physicalNodeID]float64
- Produces: `NodeHop.TrafficMultiplier float64`

- [ ] **Step 1: 创建迁移文件**

```sql
-- internal/db/migrations/0014_hop_multiplier.sql
ALTER TABLE node_hops ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0;
```

- [ ] **Step 2: 更新 NodeHop struct + scanNodeHop**

`internal/db/grants.go` — NodeHop struct 加字段：

```go
type NodeHop struct {
	NodeID            int64   `json:"node_id"`
	Position          int     `json:"position"`
	HopNodeID         int64   `json:"hop_node_id"`
	Mode              string  `json:"mode"`
	TrafficMultiplier float64 `json:"traffic_multiplier"`
}
```

scanNodeHop 更新：

```go
func scanNodeHop(r rowScanner) (*NodeHop, error) {
	h := &NodeHop{}
	if err := r.Scan(&h.NodeID, &h.Position, &h.HopNodeID, &h.Mode, &h.TrafficMultiplier); err != nil {
		return nil, err
	}
	return h, nil
}
```

ListNodeHops 和 ListAllNodeHops 的 SELECT 追加 `traffic_multiplier`：

```go
func ListNodeHops(d *sql.DB, nodeID int64) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode, traffic_multiplier FROM node_hops WHERE node_id=? ORDER BY position`, scanNodeHop, nodeID)
}

func ListAllNodeHops(d *sql.DB) ([]*NodeHop, error) {
	return queryAll(d, `SELECT node_id, position, hop_node_id, mode, traffic_multiplier FROM node_hops ORDER BY node_id, position`, scanNodeHop)
}
```

- [ ] **Step 3: Node struct 移除 TrafficMultiplier**

`internal/db/queries.go` — 从 Node struct 删除 `TrafficMultiplier float64` 字段。nodeCols 移除 `,traffic_multiplier`。scanNode 移除 `&n.TrafficMultiplier`。

`internal/db/grants.go` — ListNodesForUser inline scan 同步移除 `&n.TrafficMultiplier`。

- [ ] **Step 4: 替换 NodeMultipliers 为 HopMultipliers**

`internal/db/traffic.go` — 删除 `NodeMultipliers`，新增：

```go
func HopMultipliers(d *sql.DB) (map[int64]map[int64]float64, error) {
	rows, err := d.Query(`SELECT node_id, hop_node_id, traffic_multiplier FROM node_hops`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]map[int64]float64)
	for rows.Next() {
		var compositeID, hopID int64
		var mult float64
		if err := rows.Scan(&compositeID, &hopID, &mult); err != nil {
			return nil, err
		}
		if m[compositeID] == nil {
			m[compositeID] = make(map[int64]float64)
		}
		m[compositeID][hopID] = mult
	}
	return m, rows.Err()
}
```

- [ ] **Step 5: 更新 applyCounters**

`internal/server/hub.go` — 替换 multipliers 加载和使用：

```go
// 替换:
// multipliers, err := db.NodeMultipliers(h.DB)
hopMultipliers, err := db.HopMultipliers(h.DB)
if err != nil {
	log.Printf("hub: load hop multipliers: %v", err)
	hopMultipliers = map[int64]map[int64]float64{}
}
```

替换倍率查找逻辑：

```go
// 替换:
// mult := multipliers[nodeID]
mult := 1.0
if r.NodeID != nodeID {
	if hm, ok := hopMultipliers[r.NodeID]; ok {
		if m, ok := hm[nodeID]; ok {
			mult = m
		}
	}
}
```

- [ ] **Step 6: 更新 apiUpdateNodeHops 接受 traffic_multiplier**

`internal/server/api.go` — apiUpdateNodeHops 的请求体 struct 中，每个 hop 追加 TrafficMultiplier：

当前请求体是 `hops: [{mode: "kernel"}, ...]`。扩展为 `hops: [{mode: "kernel", traffic_multiplier: 1.0}, ...]`。

在构建 NodeHop 数组时设置 TrafficMultiplier（默认 1.0）：

```go
type hopUpdate struct {
	Mode              string  `json:"mode"`
	TrafficMultiplier float64 `json:"traffic_multiplier"`
}
```

构建时：

```go
mult := hu.TrafficMultiplier
if mult <= 0 {
	mult = 1.0
}
newHops = append(newHops, db.NodeHop{
	NodeID:            nodeID,
	Position:          i,
	HopNodeID:         existing[i].HopNodeID,
	Mode:              mode,
	TrafficMultiplier: mult,
})
```

- [ ] **Step 7: 更新 CreateNodeHops 写入 traffic_multiplier**

`internal/db/grants.go` — CreateNodeHops 的 INSERT 追加 traffic_multiplier：

```go
func CreateNodeHops(d DBTX, nodeID int64, hops []NodeHop) error {
	for _, h := range hops {
		mult := h.TrafficMultiplier
		if mult == 0 {
			mult = 1.0
		}
		if _, err := d.Exec(`INSERT INTO node_hops(node_id, position, hop_node_id, mode, traffic_multiplier) VALUES (?,?,?,?,?)`,
			nodeID, h.Position, h.HopNodeID, h.Mode, mult); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 8: 更新 traffic_test.go**

替换 `TestNodeMultipliers` 为 `TestHopMultipliers`：

```go
func TestHopMultipliers(t *testing.T) {
	d := openTestDB(t)
	// Create a composite node with 2 hops
	n1 := createTestNode(t, d, "entry")
	n2 := createTestNode(t, d, "relay")
	comp, err := CreateNode(d, "composite-"+RandToken(4), "", "")
	if err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, comp.ID)
	CreateNodeHops(d, comp.ID, []NodeHop{
		{NodeID: comp.ID, Position: 0, HopNodeID: n1, Mode: "kernel", TrafficMultiplier: 1.0},
		{NodeID: comp.ID, Position: 1, HopNodeID: n2, Mode: "userspace", TrafficMultiplier: 0.5},
	})

	m, err := HopMultipliers(d)
	if err != nil {
		t.Fatal(err)
	}
	if m[comp.ID][n1] != 1.0 {
		t.Fatalf("entry hop want 1.0, got %f", m[comp.ID][n1])
	}
	if m[comp.ID][n2] != 0.5 {
		t.Fatalf("relay hop want 0.5, got %f", m[comp.ID][n2])
	}
}
```

- [ ] **Step 9: 运行全部测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v -count=1`
Expected: 全部 PASS

- [ ] **Step 10: Commit**

```bash
git add internal/db/migrations/0014_hop_multiplier.sql internal/db/queries.go internal/db/grants.go internal/db/traffic.go internal/db/traffic_test.go internal/server/hub.go internal/server/api.go
git commit -m "$(cat <<'EOF'
feat: move traffic_multiplier from nodes to node_hops

Each physical node can have a different multiplier in different composite
chains. Direct (non-composite) rules use a fixed 1.0 multiplier.
HopMultipliers replaces NodeMultipliers with a nested map keyed by
(compositeNodeID, physicalNodeID).
EOF
)"
```

---

### Task 2: 前端 — 组合节点跳序倍率 UI

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx` (CompositeHopsCard 加倍率输入)

**Interfaces:**
- Consumes: POST `/nodes/{id}/hops` body 扩展为 `{hops: [{mode, traffic_multiplier}, ...]}`
- Consumes: GET `/nodes/{id}` 返回的 node_hops 中含 `traffic_multiplier`

- [ ] **Step 1: 更新 CompositeHopsCard**

`web/src/pages/nodes/Detail.jsx` — CompositeHopsCard 组件中：

在 state 初始化时同时提取 multipliers（和 modes 并列）：

```jsx
const [mults, setMults] = useState(hops.map(h => String(h.traffic_multiplier ?? 1)))
const setMult = (i, v) => setMults(prev => prev.map((m, j) => j === i ? v : m))
```

save 函数中把 multiplier 一起发送：

```jsx
const save = async () => {
  setSaving(true)
  try {
    await api.post(`/nodes/${nodeId}/hops`, {
      hops: modes.map((m, i) => ({ mode: m, traffic_multiplier: Number(mults[i]) || 1 }))
    })
    toast('已保存')
    onDone()
  } catch (err) { toast(err.message) } finally { setSaving(false) }
}
```

表格头追加"倍率"列，每跳行追加倍率 input：

```jsx
<th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft w-24">倍率</th>
```

```jsx
<td className="px-3 py-2">
  <input className="input-field font-mono" type="number" min="0" step="0.1"
    value={mults[i]} onChange={e => setMult(i, e.target.value)}
    style={{ width: 80 }} title="全局流量计费倍率" />
</td>
```

- [ ] **Step 2: 手动验证**

打开组合节点详情页，确认：
- 每跳显示倍率输入框，默认值 1.0
- 修改倍率后点保存，刷新后值保持

- [ ] **Step 3: Commit**

```bash
git add web/src/pages/nodes/Detail.jsx
git commit -m "feat: add traffic multiplier input to composite node hops UI"
```

---

### Task 3: 前端 — 用户详情页 per-node 配额

**Files:**
- Modify: `web/src/pages/users/Detail.jsx` (GrantedNodesCard 扩展 + ResetDaysForm + PerNodeQuotaForm)

**Interfaces:**
- Consumes: GET `/users/{id}` 返回的 grants 中含 `traffic_quota_bytes`, `traffic_used_bytes`
- Consumes: POST `/users/{id}/nodes/{nodeId}/quota` body `{traffic_quota_bytes}`
- Consumes: POST `/users/{id}/nodes/{nodeId}/reset-traffic`
- Consumes: POST `/users/{id}/reset-days` body `{traffic_reset_days}`
- Consumes: GET `/users/{id}` 返回 `traffic_reset_days`

- [ ] **Step 1: 添加 ResetDaysForm 组件**

`web/src/pages/users/Detail.jsx` — 在 QuotaForm 后面追加：

```jsx
function ResetDaysForm({ userId, resetDays, onDone }) {
  const [val, setVal] = useState(String(resetDays || 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const days = Math.max(0, Math.round(Number(val) || 0))
    try {
      await api.post(`/users/${userId}/reset-days`, { traffic_reset_days: days })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="1" value={val}
        onChange={e => setVal(e.target.value)} style={{ width: 80 }} title="0 = 永不重置" />
      <span className="text-xs text-ink-mut">天</span>
      <button type="submit" className="btn-secondary text-xs">设周期</button>
    </form>
  )
}
```

在 action 按钮行中追加渲染：

```jsx
{isRegularUser && <ResetDaysForm userId={id} resetDays={user.traffic_reset_days} onDone={load} />}
```

- [ ] **Step 2: 流量显示增加周期信息**

在流量 InfoRow 中追加周期显示：

```jsx
<span className="font-mono">
  {fmtTrafficGB(user.traffic_used_bytes, user.traffic_quota_bytes)}
  {user.traffic_quota_bytes > 0 && ` (${pct(user.traffic_used_bytes, user.traffic_quota_bytes)}%)`}
  {user.traffic_reset_days > 0 && <span className="text-ink-mut text-xs ml-1">每{user.traffic_reset_days}天重置</span>}
</span>
```

- [ ] **Step 3: 扩展 GrantedNodesCard 表格**

在授权节点表格头追加列（在 max_forwards 列之后）：

```jsx
<th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">流量配额</th>
<th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">已用</th>
<th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft w-16"></th>
```

在每行中追加（g 是 grant 对象）：

```jsx
<td className="px-3 py-2">
  <PerNodeQuotaForm userId={userId} nodeId={n.id} quotaBytes={g.traffic_quota_bytes} onDone={onDone} />
</td>
<td className="px-3 py-2 font-mono text-sm">
  {fmtTrafficGB(g.traffic_used_bytes, g.traffic_quota_bytes)}
</td>
<td className="px-3 py-2">
  {g.traffic_used_bytes > 0 && (
    <button onClick={() => resetNodeTraffic(n.id)} className="btn-danger-sm text-xs">重置</button>
  )}
</td>
```

- [ ] **Step 4: 添加 PerNodeQuotaForm 和 resetNodeTraffic**

```jsx
function PerNodeQuotaForm({ userId, nodeId, quotaBytes, onDone }) {
  const [gb, setGb] = useState(String(Number(((quotaBytes || 0) / 1073741824).toFixed(2))))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.round((Number(gb) || 0) * 1073741824))
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/quota`, { traffic_quota_bytes: bytes })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message) }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="0.1" value={gb}
        onChange={e => setGb(e.target.value)} style={{ width: 80 }} title="0 = 不限" />
      <span className="text-xs text-ink-mut">GB</span>
      <button type="submit" className="btn-secondary text-xs">设配额</button>
    </form>
  )
}
```

GrantedNodesCard 内加 resetNodeTraffic 函数：

```jsx
const resetNodeTraffic = async (nodeId) => {
  if (!(await confirm({ title: '重置节点流量', message: '清零该用户在此节点上的已用流量？', confirmText: '清零', danger: true }))) return
  try { await api.post(`/users/${userId}/nodes/${nodeId}/reset-traffic`); toast('已重置'); onDone() } catch (err) { toast(err.message) }
}
```

- [ ] **Step 5: 手动验证**

打开用户详情页，确认：
- 重置周期表单显示，0=永不
- 授权节点表格显示配额/用量/重置列
- 设配额、重置流量操作正常

- [ ] **Step 6: Commit**

```bash
git add web/src/pages/users/Detail.jsx
git commit -m "feat: per-node traffic quota UI on user detail page"
```

---

### Task 4: 前端 — 添加节点时配置端口范围

**Files:**
- Modify: `web/src/pages/nodes/List.jsx` (AddNodeModal 和 CompositeNodeModal 加端口范围输入)
- Modify: `web/src/pages/nodes/Detail.jsx` (安装脚本加 --port-range)
- Modify: `internal/server/api.go` (apiCreateNode 接受 port_range)

**Interfaces:**
- Consumes: POST `/nodes` body 中新增 `port_range` 字段
- Consumes: install.sh 已支持 `--port-range` 参数（需确认）

- [ ] **Step 1: 检查 install.sh 是否支持 --port-range**

Run: `grep -n 'port.range\|port_range' /Users/xjetry/work/vibe/nft-forward/install.sh`

如不支持，需添加。如已支持，确认参数格式。

- [ ] **Step 2: 更新 AddNodeModal 加端口范围输入**

`web/src/pages/nodes/List.jsx` — AddNodeModal 中追加两个输入框：

```jsx
const [portStart, setPortStart] = useState('10000')
const [portEnd, setPortEnd] = useState('19999')
```

在 API 调用中传入 port_range：

```jsx
const portRange = `${portStart || '10000'}-${portEnd || '19999'}`
await api.post('/nodes', { name, secret: secret || undefined, port_range: portRange })
```

在 name/token 输入框之后追加：

```jsx
<div className="flex gap-2">
  <div className="flex-1">
    <label className="fl">起始端口</label>
    <input className="input-field w-full font-mono" type="number" min="1" max="65535"
      value={portStart} onChange={e => setPortStart(e.target.value)} placeholder="10000" />
  </div>
  <div className="flex-1">
    <label className="fl">结束端口</label>
    <input className="input-field w-full font-mono" type="number" min="1" max="65535"
      value={portEnd} onChange={e => setPortEnd(e.target.value)} placeholder="19999" />
  </div>
</div>
```

- [ ] **Step 3: 更新后端 apiCreateNode 接受 port_range**

`internal/server/api.go` — apiCreateNode 请求体加 `PortRange string` 字段。创建节点后如果 port_range 非空则更新：

```go
if body.PortRange != "" {
	if _, err := s.DB.Exec(`UPDATE nodes SET port_range=? WHERE id=?`, body.PortRange, n.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
}
```

- [ ] **Step 4: 更新安装脚本生成**

`web/src/pages/nodes/Detail.jsx` — installCmd 中，如果 port_range 不是默认值（`10001-20000`），追加 `--port-range`：

```jsx
const portRangePart = node.port_range && node.port_range !== '10001-20000'
  ? ` \\\n  --port-range ${node.port_range}`
  : ''
const installCmd = `curl -fsSL ${proxyPrefix}https://raw.githubusercontent.com/xjetry/nft-forward/main/install.sh | bash -s agent \\\n  --panel-url ${panel_url} \\\n  --token ${node.secret}${portRangePart}${proxyPrefix ? ` \\\n  --gh-proxy ${proxyPrefix}` : ''}`
```

- [ ] **Step 5: 运行后端测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -count=1`
Expected: PASS

- [ ] **Step 6: 手动验证**

- 添加节点对话框中显示端口范围输入
- 创建节点后端口范围正确保存
- 节点详情页安装脚本包含 --port-range（非默认时）

- [ ] **Step 7: Commit**

```bash
git add web/src/pages/nodes/List.jsx web/src/pages/nodes/Detail.jsx internal/server/api.go
git commit -m "feat: port range config on node creation and install script"
```

---

### Task 5: 整合验证

**Files:**
- 运行完整测试套件

- [ ] **Step 1: 运行全部后端测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v -count=1`
Expected: 全部 PASS

- [ ] **Step 2: go vet**

Run: `go vet ./...`
Expected: 无错误

- [ ] **Step 3: 重建前端确认编译通过**

Run: `cd web && npm run build && cd ..`
Expected: 构建成功

- [ ] **Step 4: Commit（如有清理）**

```bash
git add -A && git commit -m "chore: cleanup after per-node quota UI integration"
```
