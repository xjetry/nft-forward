# 节点用途双栏改造 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把节点详情的「节点角色」卡片重构为入口 / 中间层左右两栏、各自独立开关的「节点用途」卡片，并把下游关系从只读升级为可在上游侧直接编辑。

**Architecture:** 绑定边 `node_bindings{upstream, downstream, mode}` 不变，新增对称的 upstream 端 db 读写与 HTTP 接口，使一条边可从上游侧或下游侧任一侧编辑其相关子集。前端 `RolesCard` 拆成两栏：入口栏懒加载并编辑「以本节点为上游」的下游边，中间层栏沿用「以本节点为下游」的上游边。存量节点一次性迁移为同时具备两用途。

**Tech Stack:** Go + chi 路由 + SQLite（`internal/db`, `internal/server`）；React + Vite（`web/`）。

## Global Constraints

- roles bitmask：entry = `1<<0`（`db.NodeRoleEntry`），via = `1<<1`（`db.NodeRoleVia`）；至少保留一位。
- 绑定边 mode ∈ {`kernel`, `userspace`}，空值默认 `userspace`（勿落入 `NormalizeForwardMode` 的 kernel 全局回退）。
- 代码注释、KDoc、commit message 一律不得出现执行过程信息（任务/步骤编号、方案代号、审阅轮次等）；注释只解释 WHY 与不变量。
- migration 文件放 `internal/db/migrations/`，四位序号递增，当前最新为 `0037_node_no_direct_exit.sql`。
- 前端无单测框架，前端任务以 `cd web && npm run build`（vite build）通过 + 手动验证为准。

---

### Task 1: 存量迁移 + 新建节点默认双用途

**Files:**
- Create: `internal/db/migrations/0038_default_node_roles_dual.sql`
- Modify: `internal/db/queries.go`（`CreateNode` 的 INSERT，约 321 行）
- Test: `internal/db/queries_test.go`（默认角色断言，约 99 行）

**Interfaces:**
- Consumes: `db.NodeRoleEntry`, `db.NodeRoleVia`（`internal/db/queries.go:13-14`）。
- Produces: 新建 agent 节点默认 `roles = 3`；存量非 self 节点迁移为 `roles = 3`。

- [ ] **Step 1: 改默认断言测试为双用途**

把 `internal/db/queries_test.go` 中新建节点默认角色的断言改成 entry|via：

```go
	if n.Roles != NodeRoleEntry|NodeRoleVia {
		t.Fatalf("default roles want %d, got %d", NodeRoleEntry|NodeRoleVia, n.Roles)
	}
```

（原断言为 `if n.Roles != NodeRoleEntry`，见 `queries_test.go:99-101`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/db/ -run TestNodeRoles -v`
Expected: FAIL —— 当前 `CreateNode` 走列默认 `roles=1`，断言 3 不成立。
（若该测试函数名不同，用 `go test ./internal/db/ -run Roles -v` 定位。）

- [ ] **Step 3: `CreateNode` 显式写入双用途默认**

把 `internal/db/queries.go` 的 `CreateNode` INSERT 显式带上 `roles` 列（原 SQL 不含该列，靠 migration 0031 的列默认 `1`）：

```go
	// New agent nodes default to both roles (entry|via = 3) so a freshly
	// registered node can be picked as an entry or bound as a middle layer
	// without an extra edit; the numeric literal mirrors NodeRoleEntry|NodeRoleVia.
	res, err := d.Exec(`INSERT INTO nodes(name, address, secret, roles, sort_order, created_at)
		VALUES (?,?,?,3, (SELECT COALESCE(MAX(sort_order),0)+1 FROM nodes), ?)`,
		name, address, secret, now())
```

（`UpsertSelfNode` 不动——self 节点保持列默认 `roles=1`，不获得 via。）

- [ ] **Step 4: 写存量迁移（排除 self）**

创建 `internal/db/migrations/0038_default_node_roles_dual.sql`：

```sql
-- Grant both roles (entry|via = 3) to every real agent node so existing nodes
-- can serve as an entry and as a middle layer without a manual toggle. The
-- panel's built-in self node is excluded: it never acts as a middle layer, and
-- a via bit would only surface it in binding candidate lists.
UPDATE nodes SET roles = 3 WHERE node_type != 'self';
```

- [ ] **Step 5: 运行 db 测试全绿**

Run: `go test ./internal/db/`
Expected: ok（默认断言通过；若其它用例假设新建节点仅 entry 而失败，按新语义修正断言——新建节点现在同时具备 entry|via）。

- [ ] **Step 6: 运行 server 测试全绿**

Run: `go test ./internal/server/`
Expected: ok。`handlers_admin_test.go` 里显式 `UpdateNodeRoles(... NodeRoleEntry)` 的用例仍有效；若有用例依赖新建节点默认无 via，改为显式设置角色。

- [ ] **Step 7: Commit**

```bash
git add internal/db/migrations/0038_default_node_roles_dual.sql internal/db/queries.go internal/db/queries_test.go
git commit -m "feat(db): default nodes to both entry and middle-layer roles

New agent nodes are created with roles=entry|via and existing non-self nodes
are migrated to the same, so any node can be an entry or a bound middle layer
without a manual toggle. The self node is excluded to keep it out of binding
candidate lists."
```

---

### Task 2: db 层 upstream 侧绑定读写

**Files:**
- Modify: `internal/db/bindings.go`
- Test: `internal/db/bindings_test.go`

**Interfaces:**
- Consumes: `NodeBinding{UpstreamNodeID, DownstreamNodeID, Mode}`、`NormalizeForwardMode`、`bindingCols`、`scanNodeBinding`、`queryAll`（均在 `internal/db/bindings.go`）。
- Produces:
  - `ListBindingsForUpstream(d *sql.DB, upstreamID int64) ([]*NodeBinding, error)` —— 列出以 upstreamID 为上游的全部边。
  - `ReplaceBindingsForUpstream(d *sql.DB, upstreamID int64, bindings []NodeBinding) error` —— 整体替换以 upstreamID 为上游的边集。

- [ ] **Step 1: 写失败测试**

在 `internal/db/bindings_test.go` 追加：

```go
func TestUpstreamBindingsReplace(t *testing.T) {
	d := openTestDB(t)
	up, _ := CreateNode(d, "entry", "", "")
	a, _ := CreateNode(d, "mid-a", "", "")
	b, _ := CreateNode(d, "mid-b", "", "")

	// Seed an unrelated edge that lists `a` behind a different upstream; the
	// upstream-side replace on `up` must never touch it.
	if err := ReplaceBindingsForDownstream(d, a.ID, []NodeBinding{
		{UpstreamNodeID: b.ID, DownstreamNodeID: a.ID, Mode: "kernel"},
	}); err != nil {
		t.Fatal(err)
	}

	// Replace `up`'s downstream set with edges to a and b.
	if err := ReplaceBindingsForUpstream(d, up.ID, []NodeBinding{
		{UpstreamNodeID: up.ID, DownstreamNodeID: a.ID, Mode: "userspace"},
		{UpstreamNodeID: up.ID, DownstreamNodeID: b.ID, Mode: "kernel"},
	}); err != nil {
		t.Fatal(err)
	}
	ls, _ := ListBindingsForUpstream(d, up.ID)
	if len(ls) != 2 {
		t.Fatalf("want 2 downstream edges from up, got %d", len(ls))
	}

	// The unrelated edge (b -> a) survives the upstream-side replace on `up`.
	if _, err := GetNodeBinding(d, b.ID, a.ID); err != nil {
		t.Fatalf("unrelated edge b->a must survive, got %v", err)
	}

	// Replacing `up` down to a single edge drops the other.
	if err := ReplaceBindingsForUpstream(d, up.ID, []NodeBinding{
		{UpstreamNodeID: up.ID, DownstreamNodeID: b.ID, Mode: "kernel"},
	}); err != nil {
		t.Fatal(err)
	}
	ls, _ = ListBindingsForUpstream(d, up.ID)
	if len(ls) != 1 || ls[0].DownstreamNodeID != b.ID {
		t.Fatalf("want only up->b left, got %+v", ls)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/db/ -run TestUpstreamBindingsReplace -v`
Expected: FAIL —— `ListBindingsForUpstream` / `ReplaceBindingsForUpstream` 未定义（编译错误）。

- [ ] **Step 3: 实现两个函数**

在 `internal/db/bindings.go` 的 `ListBindingsForDownstream` 之后追加：

```go
func ListBindingsForUpstream(d *sql.DB, upstreamID int64) ([]*NodeBinding, error) {
	return queryAll(d, `SELECT `+bindingCols+` FROM node_bindings WHERE upstream_node_id=? ORDER BY downstream_node_id`, scanNodeBinding, upstreamID)
}
```

并在 `ReplaceBindingsForDownstream` 之后追加对称的 upstream 版：

```go
// ReplaceBindingsForUpstream swaps the upstream node's full downstream edge set
// in one transaction. It touches only edges where this node is the upstream, so
// it never disturbs a downstream node's other upstream edges — the mirror image
// of ReplaceBindingsForDownstream.
func ReplaceBindingsForUpstream(d *sql.DB, upstreamID int64, bindings []NodeBinding) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_bindings WHERE upstream_node_id=?`, upstreamID); err != nil {
		return err
	}
	for _, b := range bindings {
		mode := NormalizeForwardMode(b.Mode)
		if _, err := tx.Exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?,?)`,
			upstreamID, b.DownstreamNodeID, mode); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/db/ -run TestUpstreamBindingsReplace -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/db/bindings.go internal/db/bindings_test.go
git commit -m "feat(db): add upstream-side binding read and whole-set replace

Mirrors the downstream-side helpers so a binding edge can be edited from the
upstream node too, touching only edges where the node is the upstream."
```

---

### Task 3: server 层 downstream-bindings 接口与路由

**Files:**
- Modify: `internal/server/api.go`（在 `apiUpdateNodeBindings` 之后新增两个 handler）
- Modify: `internal/server/server.go`（在现有 bindings 路由旁注册）
- Test: `internal/server/handlers_admin_test.go`

**Interfaces:**
- Consumes: `db.ListBindingsForUpstream`, `db.ReplaceBindingsForUpstream`（Task 2）；`db.GetNode`, `db.NodeRoleEntry`, `db.NodeRoleVia`, `db.NodeBinding`, `urlParamInt64`, `decodeJSON`, `jsonOK`, `jsonErr`, `userFromCtx`, `db.WriteAudit`（现有）。
- Produces:
  - Route `GET  /nodes/{id}/downstream-bindings` → `apiListNodeDownstreamBindings`
  - Route `POST /nodes/{id}/downstream-bindings` → `apiUpdateNodeDownstreamBindings`
  - POST body：`{"bindings":[{"downstream_node_id":<int>,"mode":"kernel|userspace"}]}`
  - GET 返回：`{"bindings":[{upstream_node_id, downstream_node_id, mode}...]}`

- [ ] **Step 1: 写失败测试**

在 `internal/server/handlers_admin_test.go` 追加。它建 up（上游）与两个 via 中间层，从 up 侧写下游边并覆盖校验分支：

```go
func TestDownstreamBindingsFromUpstream(t *testing.T) {
	d := openDB(t)
	up, _ := db.CreateNode(d, "up", "", "")
	m1, _ := db.CreateNode(d, "m1", "", "")
	m2, _ := db.CreateNode(d, "m2", "", "")
	// New nodes default to entry|via, so m1/m2 already qualify as downstreams.
	cookie := loginAsAdmin(t, d)
	s, _ := New(d)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		return w
	}

	// Write two downstream edges from the upstream side.
	body := fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d,"mode":"kernel"},{"downstream_node_id":%d}]}`, m1.ID, m2.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != 200 {
		t.Fatalf("set downstream: %d %s", w.Code, w.Body.String())
	}
	// Omitted mode persists as userspace.
	bs, _ := db.ListBindingsForUpstream(d, up.ID)
	var m2mode string
	for _, b := range bs {
		if b.DownstreamNodeID == m2.ID {
			m2mode = b.Mode
		}
	}
	if len(bs) != 2 || m2mode != "userspace" {
		t.Fatalf("want 2 edges with m2 userspace, got %+v", bs)
	}
	// GET reflects them.
	if w := do("GET", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), ""); w.Code != 200 ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"mode":"kernel"`)) {
		t.Fatalf("list downstream: %d %s", w.Code, w.Body.String())
	}

	// A downstream lacking the via role is rejected.
	_ = db.UpdateNodeRoles(d, m1.ID, db.NodeRoleEntry)
	body = fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d,"mode":"kernel"}]}`, m1.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("downstream without via: want 400, got %d", w.Code)
	}

	// Self-binding is rejected.
	body = fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d}]}`, up.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("self-bind: want 400, got %d", w.Code)
	}

	// Duplicate downstream is rejected.
	body = fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d},{"downstream_node_id":%d}]}`, m2.ID, m2.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate downstream: want 400, got %d", w.Code)
	}

	// Invalid mode is rejected.
	body = fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d,"mode":"bogus"}]}`, m2.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: want 400, got %d", w.Code)
	}

	// An upstream lacking both roles cannot host downstreams. A roleless node
	// is impossible via the /roles API, so force it in DB to cover the branch.
	_ = db.UpdateNodeRoles(d, up.ID, 0)
	body = fmt.Sprintf(`{"bindings":[{"downstream_node_id":%d}]}`, m2.ID)
	if w := do("POST", fmt.Sprintf("/api/nodes/%d/downstream-bindings", up.ID), body); w.Code != http.StatusBadRequest {
		t.Fatalf("roleless upstream: want 400, got %d", w.Code)
	}
}
```

> 注：`UpdateNodeRoles(up.ID, 0)` 直接写 DB 制造「无任何角色」的上游以覆盖该分支；HTTP `/roles` 接口本身会拒绝 0。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/ -run TestDownstreamBindingsFromUpstream -v`
Expected: FAIL —— 路由不存在，POST/GET 返回 404。

- [ ] **Step 3: 实现两个 handler**

在 `internal/server/api.go` 的 `apiUpdateNodeBindings` 之后追加：

```go
// apiListNodeDownstreamBindings lists the edges where this node is the upstream
// (the nodes cascading in behind it). Admin only.
func (s *Server) apiListNodeDownstreamBindings(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	bs, err := db.ListBindingsForUpstream(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"bindings": bs})
}

// apiUpdateNodeDownstreamBindings replaces the whole set of edges where this
// node is the upstream. The node must be able to act as an upstream (entry or
// via), and every named downstream must carry the via role so it can sit behind
// this one as a middle layer. Admin only.
func (s *Server) apiUpdateNodeDownstreamBindings(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "node not found")
		return
	}
	if node.Roles&(db.NodeRoleEntry|db.NodeRoleVia) == 0 {
		jsonErr(w, http.StatusBadRequest, "node cannot host downstreams; enable the entry or middle layer role first")
		return
	}
	var body struct {
		Bindings []struct {
			DownstreamNodeID int64  `json:"downstream_node_id"`
			Mode             string `json:"mode"`
		} `json:"bindings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request format")
		return
	}
	bs := make([]db.NodeBinding, len(body.Bindings))
	seen := make(map[int64]bool, len(body.Bindings))
	for i, b := range body.Bindings {
		if b.DownstreamNodeID == id {
			jsonErr(w, http.StatusBadRequest, "cannot bind to self")
			return
		}
		if seen[b.DownstreamNodeID] {
			jsonErr(w, http.StatusBadRequest, "下游节点重复")
			return
		}
		seen[b.DownstreamNodeID] = true
		down, err := db.GetNode(s.DB, b.DownstreamNodeID)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "downstream node not found")
			return
		}
		if down.Roles&db.NodeRoleVia == 0 {
			jsonErr(w, http.StatusBadRequest, "下游节点需先开启中间层角色")
			return
		}
		// Match the downstream-side default: an omitted mode is userspace, not
		// NormalizeForwardMode's kernel fallback.
		mode := strings.ToLower(strings.TrimSpace(b.Mode))
		if mode == "" {
			mode = "userspace"
		}
		if mode != "kernel" && mode != "userspace" {
			jsonErr(w, http.StatusBadRequest, "转发模式必须为 kernel 或 userspace")
			return
		}
		bs[i] = db.NodeBinding{UpstreamNodeID: id, DownstreamNodeID: b.DownstreamNodeID, Mode: mode}
	}
	if err := db.ReplaceBindingsForUpstream(s.DB, id, bs); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.downstream_bindings", strconv.FormatInt(id, 10), fmt.Sprintf("%d edges", len(bs)))
	jsonOK(w, map[string]any{"ok": true})
}
```

- [ ] **Step 4: 注册路由**

在 `internal/server/server.go` 现有 bindings 路由旁（约 468-470 行）新增：

```go
			r.Get("/nodes/{id}/bindings", s.apiListNodeBindings)
			r.Post("/nodes/{id}/bindings", s.apiUpdateNodeBindings)
			r.Get("/nodes/{id}/downstream-bindings", s.apiListNodeDownstreamBindings)
			r.Post("/nodes/{id}/downstream-bindings", s.apiUpdateNodeDownstreamBindings)
			r.Get("/node-bindings", s.apiListAllNodeBindings)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/server/ -run TestDownstreamBindingsFromUpstream -v`
Expected: PASS

- [ ] **Step 6: 全量后端测试**

Run: `go test ./...`
Expected: ok（全部包）。

- [ ] **Step 7: Commit**

```bash
git add internal/server/api.go internal/server/server.go internal/server/handlers_admin_test.go
git commit -m "feat(server): edit downstream bindings from the upstream node

Adds GET/POST /nodes/{id}/downstream-bindings to list and whole-set replace the
edges where the node is the upstream. Validates the upstream can host (entry or
via) and each downstream carries the via role, mirroring the downstream-side
mode default and duplicate/self checks."
```

---

### Task 4: 前端 RolesCard 双栏重构 + 下游可编辑

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx`（替换 `RolesCard`，约 670-879 行；新增内部组件 `EdgeEditor`）

**Interfaces:**
- Consumes: `GET/POST /nodes/{id}/bindings`（上游边）、`GET/POST /nodes/{id}/downstream-bindings`（下游边，Task 3）、`POST /nodes/{id}/roles`、`GET /nodes`；`Select`, `NodeTypeIcon`, `useToast`, `api`, `card`（`Detail.jsx` 现有 import）。
- Produces: 双栏「节点用途」卡片。移除原来独立的只读「已绑定的下游」区块与 `/node-bindings` 只读拉取。

- [ ] **Step 1: 用双栏版整体替换 `RolesCard`**

把 `internal/`…前端文件 `web/src/pages/nodes/Detail.jsx` 中从注释块 `// A node's role bitmask...`（约 670 行）到 `RolesCard` 函数结束（约 879 行）的整段，替换为下面的 `EdgeEditor` + `RolesCard`：

```jsx
// A node's role bitmask controls where it can appear in a rule chain: entry
// (bit0) lets a rule start here, via (bit1) lets other nodes bind behind it as
// a middle layer. Both bits can be set at once; at least one must stay set or
// the node becomes unreachable from any rule.
//
// Each role owns one column and one edge set. The entry column owns downstream
// edges — nodes that cascade in behind this one, so this node is their upstream
// — editable from here via the upstream-side API. The via column owns upstream
// edges — nodes this one sits behind, so this node is their downstream. Each
// editor loads lazily when its role is checked; a failed load keeps rows null
// (nothing to save) with a retry, so one transient blip can never overwrite a
// stored edge set with an empty one on the next save.
function EdgeEditor({ title, hint, rows, err, onRetry, candidates, idKey, nodeById,
                      onPick, onMode, onRemove, onAddAll, placeholder }) {
  return (
    <div>
      <div className="flex items-baseline gap-2.5 mb-1.5">
        <h3 className="m-0 text-[13.5px] font-bold">{title}</h3>
        <span className="text-[12.5px] text-ink-mut">{rows ? `${rows.length} 条` : err ? '' : '加载中…'}</span>
      </div>
      <p className="text-[12.5px] text-ink-mut mb-2.5">{hint}</p>
      {err && (
        <div className="text-[12.5px] text-red-600 flex items-center gap-2.5">
          已有绑定加载失败，为避免误覆盖，修好前不会保存绑定。
          <button type="button" onClick={onRetry} className="btn-secondary text-xs">重试</button>
        </div>
      )}
      {rows && (
        <>
          <div className="flex items-center gap-2 mb-2">
            <Select multiple searchable className="flex-1" placeholder={placeholder}
              value={rows.map(r => String(r[idKey]))} onChange={onPick}
              options={candidates.map(n => ({ value: n.id, label: n.name, icon: <NodeTypeIcon type={n.node_type} /> }))} />
            <button type="button" onClick={onAddAll} className="btn-secondary flex-none text-xs">全选</button>
          </div>
          {rows.length > 0 && (
            <div className="space-y-2">
              {rows.map((r, i) => {
                const n = nodeById[Number(r[idKey])]
                return (
                  <div key={r[idKey]} className="flex items-center gap-2 bg-raised rounded-lg px-3 py-2">
                    <NodeTypeIcon type={n?.node_type} />
                    <span className="flex-1 min-w-0 truncate text-[13.5px]">{n?.name || `#${r[idKey]}`}</span>
                    <Select value={r.mode} onChange={v => onMode(i, v)} style={{ width: 120 }}
                      options={[{ value: 'kernel', label: 'kernel' }, { value: 'userspace', label: 'userspace' }]} />
                    <button type="button" onClick={() => onRemove(i)} className="btn-danger-sm text-xs px-1.5">×</button>
                  </div>
                )
              })}
            </div>
          )}
        </>
      )}
    </div>
  )
}

function RolesCard({ node, onDone }) {
  const [roles, setRoles] = useState(node.roles ?? 1)
  const [saving, setSaving] = useState(false)
  const [allNodes, setAllNodes] = useState([])
  // Upstream edges: nodes this one sits behind (this node = downstream).
  const [upRows, setUpRows] = useState(null)          // [{upstream_node_id, mode}]
  const [savedUpRows, setSavedUpRows] = useState(null)
  const [upErr, setUpErr] = useState(false)
  // Downstream edges: nodes that cascade in behind this one (this node = upstream).
  const [downRows, setDownRows] = useState(null)      // [{downstream_node_id, mode}]
  const [savedDownRows, setSavedDownRows] = useState(null)
  const [downErr, setDownErr] = useState(false)
  const toast = useToast()

  const entryChecked = (roles & 1) !== 0
  const viaChecked = (roles & 2) !== 0

  // NodeDetail stays mounted when only the :id param changes, so re-seed roles
  // and drop both edge sets whenever the node identity changes.
  useEffect(() => setRoles(node.roles ?? 1), [node.id, node.roles])
  useEffect(() => {
    setUpRows(null); setSavedUpRows(null); setUpErr(false)
    setDownRows(null); setSavedDownRows(null); setDownErr(false)
  }, [node.id])

  // The candidate roster and both editors' name lookups share one fetch of the
  // full node list, made once either role is checked.
  const needNodes = entryChecked || viaChecked
  useEffect(() => {
    if (!needNodes) return
    let stale = false
    api.get('/nodes').then(d => { if (!stale) setAllNodes(d.nodes || []) }).catch(() => { if (!stale) setAllNodes([]) })
    return () => { stale = true }
  }, [needNodes, node.id])

  // Lazy-load upstream edges when 中间层 is checked. stale drops superseded
  // responses; an error keeps rows null so a save never overwrites stored edges.
  useEffect(() => {
    if (!viaChecked || upRows !== null || upErr) return
    let stale = false
    api.get(`/nodes/${node.id}/bindings`)
      .then(d => {
        if (stale) return
        const rs = (d.bindings || []).map(b => ({ upstream_node_id: b.upstream_node_id, mode: b.mode }))
        setUpRows(rs); setSavedUpRows(rs)
      })
      .catch(() => { if (!stale) setUpErr(true) })
    return () => { stale = true }
  }, [viaChecked, upRows, node.id, upErr])

  // Lazy-load downstream edges when 入口 is checked, mirroring the upstream path.
  useEffect(() => {
    if (!entryChecked || downRows !== null || downErr) return
    let stale = false
    api.get(`/nodes/${node.id}/downstream-bindings`)
      .then(d => {
        if (stale) return
        const rs = (d.bindings || []).map(b => ({ downstream_node_id: b.downstream_node_id, mode: b.mode }))
        setDownRows(rs); setSavedDownRows(rs)
      })
      .catch(() => { if (!stale) setDownErr(true) })
    return () => { stale = true }
  }, [entryChecked, downRows, node.id, downErr])

  const nodeById = Object.fromEntries(allNodes.map(n => [n.id, n]))
  const toggle = (bit) => setRoles(r => r ^ bit)

  // Upstream candidates: any other node (an upstream may be entry or via).
  const upCandidates = allNodes.filter(n => n.id !== node.id)
  // Downstream candidates: other nodes carrying the via role, since a downstream
  // must be able to sit behind this one as a middle layer.
  const downCandidates = allNodes.filter(n => n.id !== node.id && (n.roles & 2) !== 0)

  const pickUp = (next) => setUpRows(rs => {
    const keep = rs.filter(r => next.includes(String(r.upstream_node_id)))
    const have = new Set(keep.map(r => String(r.upstream_node_id)))
    const added = next.filter(v => !have.has(v)).map(v => ({ upstream_node_id: Number(v), mode: 'userspace' }))
    return [...keep, ...added]
  })
  const setUpMode = (i, v) => setUpRows(rs => rs.map((r, j) => j === i ? { ...r, mode: v } : r))
  const removeUp = (i) => setUpRows(rs => rs.filter((_, j) => j !== i))
  const addAllUp = () => setUpRows(rs => upCandidates.map(n =>
    rs.find(r => Number(r.upstream_node_id) === n.id) || { upstream_node_id: n.id, mode: 'userspace' }))

  const pickDown = (next) => setDownRows(rs => {
    const keep = rs.filter(r => next.includes(String(r.downstream_node_id)))
    const have = new Set(keep.map(r => String(r.downstream_node_id)))
    const added = next.filter(v => !have.has(v)).map(v => ({ downstream_node_id: Number(v), mode: 'userspace' }))
    return [...keep, ...added]
  })
  const setDownMode = (i, v) => setDownRows(rs => rs.map((r, j) => j === i ? { ...r, mode: v } : r))
  const removeDown = (i) => setDownRows(rs => rs.filter((_, j) => j !== i))
  const addAllDown = () => setDownRows(rs => downCandidates.map(n =>
    rs.find(r => Number(r.downstream_node_id) === n.id) || { downstream_node_id: n.id, mode: 'userspace' }))

  const rolesDirty = roles !== (node.roles ?? 1)
  const edgesDirty = (rows, saved) => rows !== null && saved !== null && (
    rows.length !== saved.length ||
    rows.some((r, i) => {
      const s = saved[i]
      return !s || JSON.stringify(r) !== JSON.stringify(s)
    })
  )
  // Hidden edits are not dirty: an unchecked role's editor won't be saved.
  const upDirty = viaChecked && edgesDirty(upRows, savedUpRows)
  const downDirty = entryChecked && edgesDirty(downRows, savedDownRows)
  const dirty = rolesDirty || upDirty || downDirty

  // The three POSTs are not one transaction; onDone (a silent refresh) runs on
  // failure too so the dirty baseline realigns with what actually persisted,
  // without remounting the card and dropping the still-edited rows.
  const save = async () => {
    if (!roles) { toast('至少保留一个用途', 'error'); return }
    setSaving(true)
    try {
      if (rolesDirty) await api.post(`/nodes/${node.id}/roles`, { roles })
      if (upDirty) {
        const bs = upRows.map(r => ({ upstream_node_id: Number(r.upstream_node_id), mode: r.mode }))
        await api.post(`/nodes/${node.id}/bindings`, { bindings: bs })
        setSavedUpRows(bs)
      }
      if (downDirty) {
        const bs = downRows.map(r => ({ downstream_node_id: Number(r.downstream_node_id), mode: r.mode }))
        await api.post(`/nodes/${node.id}/downstream-bindings`, { bindings: bs })
        setSavedDownRows(bs)
      }
      toast('已保存')
    } catch (err) { toast(err.message, 'error') } finally { setSaving(false); onDone() }
  }

  const entryCls = 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-900/30 dark:text-emerald-400 dark:border-emerald-700'
  const viaCls = 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700'
  const roleBtn = (bit, label, cls) => (
    <button type="button" onClick={() => toggle(bit)}
      className={`px-3 py-1 text-[12.5px] font-semibold rounded-md border transition-colors ${
        (roles & bit) !== 0 ? cls : 'bg-transparent border-line text-ink-mut/40 hover:text-ink-mut'
      }`}>{label}</button>
  )

  return (
    <section className={`${card} px-[26px] pt-[22px] pb-[18px]`}>
      <div className="flex items-center gap-2 mb-1.5">
        <h2 className="m-0 text-[15px] font-bold">节点用途</h2>
        <button onClick={save} disabled={saving || !dirty} className="btn-primary ml-auto">保存</button>
      </div>
      <p className="text-[12.5px] text-ink-mut mb-3">
        入口：可被规则选为入口，并绑定下游。中间层：可绑定到上游之后供规则级联。至少保留一个。
      </p>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-x-6 gap-y-4">
        <div className="md:pr-6 md:border-r border-line-soft">
          <div className="flex items-center gap-1.5 mb-3">{roleBtn(1, '入口', entryCls)}</div>
          {entryChecked && (
            <EdgeEditor title="已绑定的下游" placeholder="选择下游节点…"
              hint="选中的下游（须为中间层）可在选中本节点的规则里级联接入本节点之后。模式作用于衔接段；修改对此后新建的规则生效。"
              rows={downRows} err={downErr} onRetry={() => setDownErr(false)}
              candidates={downCandidates} idKey="downstream_node_id" nodeById={nodeById}
              onPick={pickDown} onMode={setDownMode} onRemove={removeDown} onAddAll={addAllDown} />
          )}
        </div>
        <div>
          <div className="flex items-center gap-1.5 mb-3">{roleBtn(2, '中间层', viaCls)}</div>
          {viaChecked && (
            <EdgeEditor title="已绑定的上游" placeholder="选择上游节点…"
              hint="绑定后，选中这些上游（入口或中间层）的规则可以级联接入本节点。模式作用于衔接段；修改对此后新建的规则生效。"
              rows={upRows} err={upErr} onRetry={() => setUpErr(false)}
              candidates={upCandidates} idKey="upstream_node_id" nodeById={nodeById}
              onPick={pickUp} onMode={setUpMode} onRemove={removeUp} onAddAll={addAllUp} />
          )}
        </div>
      </div>
    </section>
  )
}
```

- [ ] **Step 2: 若 `ModeBadge` 变为未使用则移除其 import**

原只读下游区块用到 `ModeBadge`。替换后若 `Detail.jsx` 内 `ModeBadge` 不再被引用，从第 6 行 import 列表删掉它（vite build 不因未用 import 报错，但保持整洁）。用编辑器搜索 `ModeBadge` 确认无其它引用后再删。

- [ ] **Step 3: 构建前端**

Run: `cd web && npm run build`
Expected: 构建成功，无 JSX 语法 / 未定义引用错误。

- [ ] **Step 4: 手动验证（executing 时人工确认）**

- 入口栏开关切换 entry 位；关闭时下游编辑器隐藏，开启时懒加载已绑定下游。
- 下游多选下拉只列出「已开启中间层角色」的其它节点；每条下游行有 mode 选择器与删除。
- 中间层栏行为与改造前一致（上游多选 + mode + 全选）。
- 两开关全部关闭时点保存提示「至少保留一个用途」。
- 保存后，从对方节点的详情页可见同一条边（同一 mode）。

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/nodes/Detail.jsx
git commit -m "feat(web): split node usage into entry/middle-layer columns

Each role owns one column with its own toggle. The entry column now edits
downstream bindings from the upstream side (multi-select restricted to
middle-layer-capable nodes, per-edge mode), replacing the old read-only list;
the middle-layer column keeps the existing upstream binding editor."
```

---

## Self-Review 记录

- **Spec 覆盖**：数据模型/迁移 → Task 1；upstream 端 db 读写 → Task 2；对称 HTTP 接口与校验 → Task 3；双栏前端 + 下游可编辑 + 语义文案 → Task 4。上游/下游候选非对称 → Task 4 `upCandidates`/`downCandidates`。self 排除 → Task 1 migration + `CreateNode`。
- **无占位符**：所有步骤含实际代码与命令。
- **类型一致**：db `ListBindingsForUpstream`/`ReplaceBindingsForUpstream`（Task 2）↔ server handler 调用（Task 3）↔ 前端 `downstream-bindings` 路由（Task 4）签名与路径一致；边字段 `downstream_node_id`/`upstream_node_id`/`mode` 三处一致。
