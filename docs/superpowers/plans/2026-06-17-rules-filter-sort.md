# Rules List: User Filter + Column Sort Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在管理员转发规则列表页新增用户多选过滤（API 下推）、列排序（前端三态），并将「路径」列替换为「出口」列。

**Architecture:** 后端在 `db` 层新增支持多 `owner_id` 过滤的查询函数，API handler 解析 `?owner_ids=` 参数并在响应中始终返回全量 `users` 列表供前端构建筛选器。前端维护已选用户 ID 集合，变更时携参重新请求；排序状态独立于过滤，对当前已返回数据做前端排序（null → asc → desc → null 三态循环）。

**Tech Stack:** Go (database/sql, SQLite), React 18 + Vite, Tailwind CSS

---

## File Map

| 文件 | 改动 |
|------|------|
| `internal/db/rules.go` | 新增 `ListRulesByOwnerIDs` |
| `internal/server/api.go` | 修改 `apiListRules`：解析 `owner_ids` 参数、返回 `users` 字段 |
| `web/src/pages/rules/List.jsx` | 移除路径列、增加出口列、用户 chip 过滤、列排序 |

---

### Task 1: db 层 — ListRulesByOwnerIDs

**Files:**
- Modify: `internal/db/rules.go`（在 `ListRulesByOwnerIDs` 之后、`ListAllRules` 之前插入）

- [ ] **Step 1: 在 `ListRulesByUser` 后（约 264 行）插入新函数**

```go
// ListRulesByOwnerIDs returns rules whose owner_id matches any of the given IDs.
// If ids is empty it falls back to returning all rules.
func ListRulesByOwnerIDs(d *sql.DB, ids []int64) ([]*Rule, error) {
	if len(ids) == 0 {
		return ListAllRules(d)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return listRulesWhere(d, "owner_id IN ("+strings.Join(placeholders, ",")+")", args...)
}
```

- [ ] **Step 2: 编译验证**

```bash
cd /Users/xjetry/work/vibe/nft-forward && go build ./...
```

Expected: 无输出（编译通过）

- [ ] **Step 3: Commit**

```bash
git add internal/db/rules.go
git commit -m "feat: add ListRulesByOwnerIDs for multi-owner rule filtering"
```

---

### Task 2: API 层 — apiListRules 支持 owner_ids 过滤并返回 users

**Files:**
- Modify: `internal/server/api.go`，替换 `apiListRules` 函数（约第 634–650 行）

- [ ] **Step 1: 确认当前函数范围**

```bash
grep -n "func (s \*Server) apiListRules\|^func " /Users/xjetry/work/vibe/nft-forward/internal/server/api.go | head -20
```

找到 `apiListRules` 的起止行。

- [ ] **Step 2: 替换整个函数体**

将：
```go
func (s *Server) apiListRules(w http.ResponseWriter, r *http.Request) {
	rules, _ := db.ListAllRules(s.DB)
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	users, _ := db.UsersByID(s.DB)
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		oname := ""
		if rl.OwnerID.Valid {
			if u := users[rl.OwnerID.Int64]; u != nil {
				oname = u.Username
			}
		}
		views = append(views, s.buildRuleListItem(rl, oname))
	}
	jsonOK(w, map[string]any{"rules": views, "nodes": nodes})
}
```

替换为：
```go
func (s *Server) apiListRules(w http.ResponseWriter, r *http.Request) {
	var rules []*db.Rule
	if raw := r.URL.Query().Get("owner_ids"); raw != "" {
		var ids []int64
		for _, part := range strings.Split(raw, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		rules, _ = db.ListRulesByOwnerIDs(s.DB, ids)
	} else {
		rules, _ = db.ListAllRules(s.DB)
	}
	db.FillRuleTraffic(s.DB, rules)
	nodes, _ := db.ListNodes(s.DB)
	allUsers, _ := db.ListUsers(s.DB)
	byID := make(map[int64]*db.User, len(allUsers))
	for _, u := range allUsers {
		byID[u.ID] = u
	}
	views := make([]ruleListItem, 0, len(rules))
	for _, rl := range rules {
		oname := ""
		if rl.OwnerID.Valid {
			if u := byID[rl.OwnerID.Int64]; u != nil {
				oname = u.Username
			}
		}
		views = append(views, s.buildRuleListItem(rl, oname))
	}
	userList := make([]map[string]any, 0, len(allUsers))
	for _, u := range allUsers {
		userList = append(userList, map[string]any{"id": u.ID, "username": u.Username})
	}
	jsonOK(w, map[string]any{"rules": views, "nodes": nodes, "users": userList})
}
```

- [ ] **Step 3: 确认 `strconv` 已在 api.go 导入**

```bash
grep "strconv" /Users/xjetry/work/vibe/nft-forward/internal/server/api.go | head -3
```

Expected: 有 `"strconv"` 导入行。若无则在 import block 中补充。

- [ ] **Step 4: 编译**

```bash
cd /Users/xjetry/work/vibe/nft-forward && go build ./...
```

Expected: 无输出

- [ ] **Step 5: 手动验证（可选，需服务在运行）**

```bash
# 无过滤，检查 users 字段存在
curl -s -b <cookie> http://localhost:8080/api/rules | python3 -m json.tool | grep -A5 '"users"'

# 按 owner_ids 过滤（把 1 换成实际存在的 user id）
curl -s -b <cookie> "http://localhost:8080/api/rules?owner_ids=1" | python3 -m json.tool | grep owner_name
```

- [ ] **Step 6: Commit**

```bash
git add internal/server/api.go
git commit -m "feat: filter rules by owner_ids query param; always return users list"
```

---

### Task 3: 前端 — 替换路径列为出口列

**Files:**
- Modify: `web/src/pages/rules/List.jsx`

- [ ] **Step 1: 移除路径 `<th>` 和对应 `<td>`，在入口列之后添加出口列**

表头：将
```jsx
<th>路径</th>
<th>入口</th>
```
改为：
```jsx
<th>入口</th>
<th>出口</th>
```

表格行：将
```jsx
<td className="font-mono text-xs text-gray-500 max-w-[200px] truncate"><SensText blurred={blurred}>{r.path || '--'}</SensText></td>
<td className="font-mono text-xs" onClick={e => e.stopPropagation()}>
  {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
</td>
```
改为：
```jsx
<td className="font-mono text-xs" onClick={e => e.stopPropagation()}>
  {r.entry ? <CopyText text={r.entry}><SensText blurred={blurred}>{r.entry}</SensText></CopyText> : '--'}
</td>
<td className="font-mono text-xs text-gray-500">
  <SensText blurred={blurred}>{r.exit_host && r.exit_port ? `${r.exit_host}:${r.exit_port}` : '--'}</SensText>
</td>
```

- [ ] **Step 2: 启动开发服务器，目视确认列变更**

```bash
cd /Users/xjetry/work/vibe/nft-forward/web && npm run dev
```

打开 http://localhost:5173/rules，确认：
- 「路径」列已消失
- 「入口」列之后出现「出口」列，内容格式为 `host:port`

- [ ] **Step 3: Commit**

```bash
git add web/src/pages/rules/List.jsx
git commit -m "feat: replace path column with exit column in rules list"
```

---

### Task 4: 前端 — 用户多选 chip 过滤器

**Files:**
- Modify: `web/src/pages/rules/List.jsx`

- [ ] **Step 1: 增加 `selectedOwners` 状态并重写 `load` 函数**

在组件顶部现有 state 声明之后插入：
```jsx
const [users, setUsers] = useState([])
const [selectedOwners, setSelectedOwners] = useState(new Set())
```

将现有 `load` 函数替换为：
```jsx
const load = (ownerIDs) => {
  const ids = ownerIDs ?? selectedOwners
  setLoading(true)
  const params = ids.size > 0 ? `?owner_ids=${[...ids].join(',')}` : ''
  api.get(`/rules${params}`)
    .then(d => {
      setData(d)
      if (d.users?.length) setUsers(d.users)
    })
    .catch(console.error)
    .finally(() => setLoading(false))
}
```

- [ ] **Step 2: 添加 chip 过滤 UI（放在 `card-header` 的 `</div>` 之后、`<table>` 之前）**

```jsx
{users.length > 0 && (
  <div className="flex flex-wrap gap-1.5 px-4 py-2 border-b border-gray-100">
    <button
      onClick={() => { const next = new Set(); setSelectedOwners(next); load(next) }}
      className={`px-2 py-0.5 rounded text-xs border transition-colors ${
        selectedOwners.size === 0
          ? 'bg-blue-500 text-white border-blue-500'
          : 'bg-white text-gray-600 border-gray-200 hover:border-gray-400'
      }`}
    >全部</button>
    {users.map(u => (
      <button
        key={u.id}
        onClick={() => {
          const next = new Set(selectedOwners)
          if (next.has(u.id)) next.delete(u.id)
          else next.add(u.id)
          setSelectedOwners(next)
          load(next)
        }}
        className={`px-2 py-0.5 rounded text-xs border transition-colors ${
          selectedOwners.has(u.id)
            ? 'bg-blue-500 text-white border-blue-500'
            : 'bg-white text-gray-600 border-gray-200 hover:border-gray-400'
        }`}
      >{u.username}</button>
    ))}
  </div>
)}
```

- [ ] **Step 3: 目视验证过滤功能**

在浏览器 http://localhost:5173/rules：
- chips 行展示所有用户名
- 点某用户 chip → chip 变蓝，表格只显示该用户的规则
- 多选多个 chip → 显示多用户的规则叠加
- 点「全部」chip → 恢复全量显示

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/rules/List.jsx
git commit -m "feat: multi-select user filter chips with API-backed filtering"
```

---

### Task 5: 前端 — 列排序（节点 / 所有者，三态）

**Files:**
- Modify: `web/src/pages/rules/List.jsx`

- [ ] **Step 1: 增加排序状态和工具函数**

在 `users` / `selectedOwners` state 之后插入：
```jsx
const [sort, setSort] = useState({ col: null, dir: null })

const cycleSort = (col) => {
  setSort(s => {
    if (s.col !== col) return { col, dir: 'asc' }
    if (s.dir === 'asc') return { col, dir: 'desc' }
    return { col: null, dir: null }
  })
}
```

- [ ] **Step 2: 在 `rules` 解构下方插入排序逻辑**

现有：
```jsx
const { rules = [], nodes = [] } = data || {}
const nodeMap = {}
nodes.forEach(n => { nodeMap[n.id] = n })
```

之后插入：
```jsx
const sortedRules = sort.col
  ? [...rules].sort((a, b) => {
      const va = sort.col === 'node'
        ? (nodeMap[a.node_id]?.name || '')
        : (a.owner_name || '')
      const vb = sort.col === 'node'
        ? (nodeMap[b.node_id]?.name || '')
        : (b.owner_name || '')
      return sort.dir === 'asc' ? va.localeCompare(vb) : vb.localeCompare(va)
    })
  : rules
```

- [ ] **Step 3: 让表头「节点」和「所有者」可点击，并显示排序箭头**

将：
```jsx
<th>节点</th>
```
改为：
```jsx
<th className="cursor-pointer select-none" onClick={() => cycleSort('node')}>
  节点{sort.col === 'node' ? (sort.dir === 'asc' ? ' ↑' : ' ↓') : ' ↕'}
</th>
```

将：
```jsx
<th>所有者</th>
```
改为：
```jsx
<th className="cursor-pointer select-none" onClick={() => cycleSort('owner')}>
  所有者{sort.col === 'owner' ? (sort.dir === 'asc' ? ' ↑' : ' ↓') : ' ↕'}
</th>
```

- [ ] **Step 4: 将 `rules.map` 改为 `sortedRules.map`**

将：
```jsx
{rules.map(r => {
```
改为：
```jsx
{sortedRules.map(r => {
```

- [ ] **Step 5: 目视验证排序**

在浏览器 http://localhost:5173/rules：
- 点「节点」列头：→ ↑ 升序 → ↓ 降序 → ↕ 取消（三态循环）
- 点「所有者」列头：同上
- 过滤后排序：先选某用户 chip，再点列头，排序作用于过滤后的结果

- [ ] **Step 6: Commit**

```bash
git add web/src/pages/rules/List.jsx
git commit -m "feat: three-state column sort for node and owner in rules list"
```
