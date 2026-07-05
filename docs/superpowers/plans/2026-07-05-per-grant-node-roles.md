# 授权级节点角色掩码 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「用户能把某节点当作入口还是中间层」按每一条授权独立决定，从而把纯中间层节点定向开放为特定用户的规则起点，而不改变节点全局角色、不暴露给其他被授权者。

**Architecture:** 新增授权级角色掩码 `user_nodes.roles`（`0`=继承节点掩码，非 `0`=覆盖）。引入唯一判定原语「有效角色 = 授权掩码覆盖值，否则节点掩码」。所有 per-user 的角色判定（my 路径入口/via 候选与校验）改读有效角色；节点能力/拓扑判定（绑定图）仍读节点掩码。计费口径不变。

**Tech Stack:** Go（`modernc.org/sqlite`，标准 `database/sql`，chi 路由，`net/http/httptest` 测试）；React 19 + Vite 前端（无测试框架）。

## Global Constraints

- 设计文档：`docs/superpowers/specs/2026-07-05-per-grant-node-roles-design.md`（本计划的权威来源）。
- 角色位常量（`internal/db/queries.go:13-14`）：`NodeRoleEntry int64 = 1 << 0`，`NodeRoleVia int64 = 1 << 1`。
- 授权掩码哨兵：`0` = 继承节点掩码；合法覆盖值 ∈ `{1, 2, 3}`。写入时校验 `roles &^ (NodeRoleEntry|NodeRoleVia) == 0`。
- `user_nodes` 所有 `SELECT` 的列顺序统一为：`... rate_limit_mbytes, roles, granted_at`（`roles` 紧跟 `rate_limit_mbytes`）。加列必须同步每一处 scan，漏一处会静默错位读授权字段。
- `GrantNode` 签名与行为**不改**（50+ 调用点依赖 4 参数签名）；授权掩码通过独立写路径设置，新建授权默认继承（列默认 `0`）。
- 代码注释只写 WHY 与不变量，不得出现任务编号、方案代号、审阅轮次等过程信息。
- 提交只 `git add` 本任务涉及的文件；仓库工作区已有的 `internal/db/landing_exits.go` 等既存改动不要一并提交。
- Go 测试运行：`go test ./internal/db/...`、`go test ./internal/server/...`。前端仅 `cd web && npm run build` 验证可构建。

---

### Task 1: `user_nodes.roles` 列与所有读取路径对齐

**Files:**
- Create: `internal/db/migrations/0038_grant_roles.sql`
- Modify: `internal/db/grants.go`（`UserNode` struct、`scanUserNode`、`GetNodeGrant`、`ListNodesForUser`、`ListUsersForNode`、`ListAllGrants` 的 SELECT）
- Test: `internal/db/grant_roles_test.go`

**Interfaces:**
- Produces: `UserNode.Roles int64`（授权掩码，`0`=继承）；所有返回 `*UserNode` / grant 元数据的查询都填充 `Roles`。

- [ ] **Step 1: 写迁移文件**

Create `internal/db/migrations/0038_grant_roles.sql`:

```sql
-- Grant-level role mask overriding the node's roles for one user's use of the
-- node: 0 = inherit the node's mask, non-zero = use this mask instead (any
-- combination of entry=1 / via=2, independent of the node mask). Lets a
-- middle-layer node be opened as a rule entry for specific grantees without
-- changing its global role or exposing it to everyone else granted the node.
-- Existing grants inherit (0), so behavior is unchanged.
ALTER TABLE user_nodes ADD COLUMN roles INTEGER NOT NULL DEFAULT 0;
```

- [ ] **Step 2: `UserNode` struct 加 `Roles` 字段**

In `internal/db/grants.go`, the `UserNode` struct (lines 13-25) currently ends:

```go
	RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	GrantedAt       int64 `json:"granted_at"`
}
```

Replace with:

```go
	RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	// Roles overrides the node's role mask for this user: 0 = inherit the
	// node's roles, non-zero = use this mask (entry/via combination). Lets a
	// node be an entry for one grantee while staying via-only for others,
	// independent of the node's global roles.
	Roles     int64 `json:"roles"`
	GrantedAt int64 `json:"granted_at"`
}
```

- [ ] **Step 3: `scanUserNode` 加 `Roles`**

In `internal/db/grants.go`, replace `scanUserNode`:

```go
func scanUserNode(r rowScanner) (*UserNode, error) {
	g := &UserNode{}
	if err := r.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
```

with:

```go
func scanUserNode(r rowScanner) (*UserNode, error) {
	g := &UserNode{}
	if err := r.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
```

- [ ] **Step 4: `GetNodeGrant` 的 SELECT 与 inline scan 加 `roles`**

Replace `GetNodeGrant`:

```go
func GetNodeGrant(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	row := d.QueryRow(`SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, granted_at FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	g := &UserNode{}
	if err := row.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
```

with:

```go
func GetNodeGrant(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
	row := d.QueryRow(`SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, roles, granted_at FROM user_nodes WHERE user_id=? AND node_id=?`, userID, nodeID)
	g := &UserNode{}
	if err := row.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
```

- [ ] **Step 5: `ListNodesForUser` 的 SELECT 与 inline scan 加 `g.roles`**

In `internal/db/grants.go`, the query in `ListNodesForUser` (around line 99-103) has:

```go
		SELECT `+nodeCols+`,
		       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
```

Change the grant columns line to include `g.roles`:

```go
		SELECT `+nodeCols+`,
		       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.roles, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
```

Then in the same function's `rows.Scan(...)` block (the grant tail, around line 126) change:

```go
			&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.GrantedAt,
```

to:

```go
			&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.RateLimitMBytes, &g.Roles, &g.GrantedAt,
```

- [ ] **Step 6: `ListUsersForNode` 加 `roles`（anonymous struct 三处 + SELECT + scan）**

In `internal/db/grants.go`, `ListUsersForNode` declares the same anonymous struct three times (return signature, `out` slice, per-row `r`). Add a `Roles int64 \`json:"roles"\`` field **after** `RateLimitMBytes` in **all three**. For example the return signature:

```go
func ListUsersForNode(d *sql.DB, nodeID int64) ([]struct {
	UserID            int64  `json:"user_id"`
	Username          string `json:"username"`
	MaxForwards       int    `json:"max_forwards"`
	TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
	RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
	Roles             int64  `json:"roles"`
	GrantedAt         int64  `json:"granted_at"`
}, error) {
```

Apply the identical field insertion to the `var out []struct {...}` declaration and the `var r struct {...}` declaration lower in the function. Change the SELECT:

```go
	rows, err := d.Query(`SELECT g.user_id, u.username, g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.granted_at
		FROM user_nodes g JOIN users u ON u.id = g.user_id
		WHERE g.node_id = ? ORDER BY g.granted_at`, nodeID)
```

to include `g.roles`:

```go
	rows, err := d.Query(`SELECT g.user_id, u.username, g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.rate_limit_mbytes, g.roles, g.granted_at
		FROM user_nodes g JOIN users u ON u.id = g.user_id
		WHERE g.node_id = ? ORDER BY g.granted_at`, nodeID)
```

And change the row scan from:

```go
		if err := rows.Scan(&r.UserID, &r.Username, &r.MaxForwards, &r.TrafficQuotaBytes, &r.TrafficUsedBytes, &r.RateLimitMBytes, &r.GrantedAt); err != nil {
```

to:

```go
		if err := rows.Scan(&r.UserID, &r.Username, &r.MaxForwards, &r.TrafficQuotaBytes, &r.TrafficUsedBytes, &r.RateLimitMBytes, &r.Roles, &r.GrantedAt); err != nil {
```

- [ ] **Step 7: `ListAllGrants` 的 SELECT 加 `roles`**

Replace `ListAllGrants`:

```go
func ListAllGrants(d *sql.DB) ([]*UserNode, error) {
	return queryAll(d, `SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, granted_at FROM user_nodes`, scanUserNode)
}
```

with:

```go
func ListAllGrants(d *sql.DB) ([]*UserNode, error) {
	return queryAll(d, `SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, rate_limit_mbytes, roles, granted_at FROM user_nodes`, scanUserNode)
}
```

- [ ] **Step 8: 写读取对齐测试**

Create `internal/db/grant_roles_test.go`:

```go
package db

import "testing"

// A new grant inherits (roles = 0) so existing behavior is unchanged.
func TestGrantRolesDefaultInherit(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != 0 {
		t.Fatalf("new grant roles = %d, want 0 (inherit)", g.Roles)
	}
}

// Every user_nodes read path must return the same override — one misaligned
// scan would silently shift the grant columns.
func TestGrantRolesReadAlignment(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)
	if _, err := d.Exec(`UPDATE user_nodes SET roles=1 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != 1 {
		t.Fatalf("GetNodeGrant roles = %d, want 1", g.Roles)
	}

	_, grants, err := ListNodesForUser(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Roles != 1 {
		t.Fatalf("ListNodesForUser roles = %v, want [1]", grants)
	}

	all, err := ListAllGrants(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Roles != 1 {
		t.Fatalf("ListAllGrants roles = %v, want 1", all)
	}

	users, err := ListUsersForNode(d, nid)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Roles != 1 {
		t.Fatalf("ListUsersForNode roles = %v, want 1", users)
	}
}
```

- [ ] **Step 9: 运行测试**

Run: `go test ./internal/db/ -run 'TestGrantRoles' -v`
Expected: PASS（两个用例）。若任一 grant 列 scan 未对齐，`TestGrantRolesReadAlignment` 会读到错位值而失败。

- [ ] **Step 10: 全量回归**

Run: `go test ./internal/db/...`
Expected: PASS（列顺序变更不影响既有测试）。

- [ ] **Step 11: Commit**

```bash
git add internal/db/migrations/0038_grant_roles.sql internal/db/grants.go internal/db/grant_roles_test.go
git commit -m "feat(db): add grant-level roles column with read-path alignment"
```

---

### Task 2: 有效角色原语与授权掩码写入

**Files:**
- Modify: `internal/db/queries.go`（新增 `EffectiveNodeRoles`）
- Modify: `internal/db/grants.go`（新增 `SetGrantRoles`）
- Test: `internal/db/grant_roles_test.go`（追加）

**Interfaces:**
- Consumes: `NodeRoleEntry`, `NodeRoleVia`, `UserNode.Roles`（Task 1）。
- Produces:
  - `func EffectiveNodeRoles(nodeRoles, grantRoles int64) int64` —— `grantRoles==0` 返回 `nodeRoles`，否则返回 `grantRoles`。
  - `func SetGrantRoles(d *sql.DB, userID, nodeID, roles int64) error` —— 覆盖一条已存在授权的掩码。

- [ ] **Step 1: 写有效角色合并的失败测试**

Append to `internal/db/grant_roles_test.go`:

```go
func TestEffectiveNodeRoles(t *testing.T) {
	// grant 0 inherits the node mask
	if got := EffectiveNodeRoles(NodeRoleVia, 0); got != NodeRoleVia {
		t.Fatalf("inherit = %d, want %d", got, NodeRoleVia)
	}
	// override may add a bit the node lacks (via node opened as entry)
	if got := EffectiveNodeRoles(NodeRoleVia, NodeRoleEntry); got != NodeRoleEntry {
		t.Fatalf("override-add = %d, want %d", got, NodeRoleEntry)
	}
	// override may drop a bit the node has (entry+via node narrowed to via)
	if got := EffectiveNodeRoles(NodeRoleEntry|NodeRoleVia, NodeRoleVia); got != NodeRoleVia {
		t.Fatalf("override-narrow = %d, want %d", got, NodeRoleVia)
	}
}

func TestSetGrantRoles(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	if err := SetGrantRoles(d, uid, nid, NodeRoleEntry); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Roles != NodeRoleEntry {
		t.Fatalf("roles = %d, want %d", g.Roles, NodeRoleEntry)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/db/ -run 'TestEffectiveNodeRoles|TestSetGrantRoles' -v`
Expected: FAIL/编译错误（`EffectiveNodeRoles`、`SetGrantRoles` 未定义）。

- [ ] **Step 3: 实现 `EffectiveNodeRoles`**

In `internal/db/queries.go`, immediately after the role constants block (lines 12-15, ending `)`), insert:

```go
// EffectiveNodeRoles resolves how a specific grant may use a node: the grant's
// own mask when set, otherwise the node's mask. grantRoles == 0 means inherit;
// any other value overrides the node mask entirely — it may add or drop bits,
// the two masks are independent, not subset-constrained.
func EffectiveNodeRoles(nodeRoles, grantRoles int64) int64 {
	if grantRoles == 0 {
		return nodeRoles
	}
	return grantRoles
}
```

- [ ] **Step 4: 实现 `SetGrantRoles`**

In `internal/db/grants.go`, after `RevokeNode` (around line 77), insert:

```go
// SetGrantRoles overrides the role mask for one existing grant (0 = inherit the
// node's mask). It targets an existing (user, node) row; if the user was never
// granted the node the UPDATE affects nothing.
func SetGrantRoles(d *sql.DB, userID, nodeID, roles int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET roles=? WHERE user_id=? AND node_id=?`, roles, userID, nodeID)
	return err
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/db/ -run 'TestEffectiveNodeRoles|TestSetGrantRoles' -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/db/queries.go internal/db/grants.go internal/db/grant_roles_test.go
git commit -m "feat(db): effective-role resolver and grant role setter"
```

---

### Task 3: `hopsForChain` 按有效角色判定入口/中间层

**Files:**
- Modify: `internal/server/api.go`（新增 `grantRoleOverrides`；`hopsForChain` 签名与入口/via 校验；四个调用点 1541、1862、2729、2890）
- Test: `internal/server/grant_role_entry_test.go`

**Interfaces:**
- Consumes: `db.EffectiveNodeRoles`, `db.SetGrantRoles`, `db.ListNodesForUser`（含 `Roles`）。
- Produces:
  - `func (s *Server) grantRoleOverrides(userID int64) map[int64]int64` —— 返回该用户所有 `roles != 0` 的授权覆盖，键为节点 id。
  - `hopsForChain(entryID int64, vias []int64, singleMode, exitMode string, effRoles map[int64]int64)` —— 新增末位参数 `effRoles`（可为 nil）。

- [ ] **Step 1: 写失败测试**

Create `internal/server/grant_role_entry_test.go`:

```go
package server

import (
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// A via-only node whose grant overrides to entry becomes a usable rule start
// for that user: a single-hop rule (no via) dials straight to the exit.
func TestGrantEntryOverrideAllowsViaNodeAsEntry(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeRoles(d, m.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}

	uid, cookie := loginAsUser(t, d, 20)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	if err := db.SetGrantRoles(d, uid, m.ID, db.NodeRoleEntry); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, m.ID, nil, "start-at-middle")
	if rec.Code != http.StatusOK {
		t.Fatalf("create with entry override: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 || rules[0].NodeID != m.ID {
		t.Fatalf("want one rule entering at middle node, got %+v", rules)
	}
}

// Without the override the same via-only node is rejected as an entry — the
// grant alone doesn't confer entry usability.
func TestViaNodeWithoutOverrideRejectedAsEntry(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeRoles(d, m.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}

	uid, cookie := loginAsUser(t, d, 21)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, m.ID, nil, "should-fail")
	if rec.Code == http.StatusOK {
		t.Fatalf("via-only node without override must not be a valid entry; got 200")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/ -run 'TestGrantEntryOverride|TestViaNodeWithoutOverride' -v`
Expected: `TestGrantEntryOverride...` FAIL（当前入口校验按节点掩码，via-only 被拒 → 非 200）。`TestViaNodeWithoutOverride...` 可能已 PASS（当前就会拒）。

- [ ] **Step 3: 新增 `grantRoleOverrides` helper**

In `internal/server/api.go`, immediately before `hopsForChain` (before line 1723 `func (s *Server) hopsForChain`), insert:

```go
// grantRoleOverrides returns a user's per-grant role overrides keyed by node
// id, including only grants that actually override (roles != 0). hopsForChain
// consults it so a node's usability as entry/via is judged by the user's
// effective role, not the node's global mask. nil/absent entries fall back to
// the node mask via db.EffectiveNodeRoles.
func (s *Server) grantRoleOverrides(userID int64) map[int64]int64 {
	_, grants, err := db.ListNodesForUser(s.DB, userID)
	if err != nil {
		return nil
	}
	out := map[int64]int64{}
	for _, g := range grants {
		if g.Roles != 0 {
			out[g.NodeID] = g.Roles
		}
	}
	return out
}
```

- [ ] **Step 4: `hopsForChain` 签名加 `effRoles`，入口/via 校验改读有效角色**

In `internal/server/api.go`, change the signature (line 1723):

```go
func (s *Server) hopsForChain(entryID int64, vias []int64, singleMode, exitMode string) ([]db.HopInput, bool, error) {
```

to:

```go
func (s *Server) hopsForChain(entryID int64, vias []int64, singleMode, exitMode string, effRoles map[int64]int64) ([]db.HopInput, bool, error) {
```

Change the entry-role check (currently lines 1728-1730):

```go
	if entryNode.Roles&db.NodeRoleEntry == 0 {
		return nil, false, fmt.Errorf("节点 %s 不是入口", entryNode.Name)
	}
```

to:

```go
	if db.EffectiveNodeRoles(entryNode.Roles, effRoles[entryID])&db.NodeRoleEntry == 0 {
		return nil, false, fmt.Errorf("节点 %s 不是入口", entryNode.Name)
	}
```

Change the via-role check (currently lines 1742-1744):

```go
		if viaNode.Roles&db.NodeRoleVia == 0 {
			return nil, true, fmt.Errorf("节点 %s 不是中间层", viaNode.Name)
		}
```

to:

```go
		if db.EffectiveNodeRoles(viaNode.Roles, effRoles[viaID])&db.NodeRoleVia == 0 {
			return nil, true, fmt.Errorf("节点 %s 不是中间层", viaNode.Name)
		}
```

（`effRoles` 为 nil 时 `effRoles[id]` 得 `0`，`EffectiveNodeRoles` 回退节点掩码——admin create 传 nil 即保持现状。）

- [ ] **Step 5: 更新四个调用点**

**5a — `apiMyCreateRule`（line 2729）** — 当前用户上下文 `u`：

```go
	hops, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode)
```

→

```go
	hops, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode, s.grantRoleOverrides(u.ID))
```

**5b — `apiMyUpdateRule`（line 2890）** — 当前用户上下文 `u`：

```go
	hops, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode)
```

→

```go
	hops, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode, s.grantRoleOverrides(u.ID))
```

**5c — `apiCreateRule`（admin create，line 1541）** — admin 建规则无 owner，传 nil 回退节点掩码：

```go
		derived, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode)
```

→

```go
		derived, _, derr := s.hopsForChain(body.NodeID, vias, body.Mode, body.ExitMode, nil)
```

**5d — `apiUpdateRule`（admin update，line 1862）** — 按被编辑规则的 owner 判定（无 owner 回退节点掩码）。The block currently reads:

```go
		derived, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode)
```

Replace with (insert the override lookup on the lines just above the call, then pass it):

```go
		var ownerOverrides map[int64]int64
		if rl.OwnerID.Valid {
			ownerOverrides = s.grantRoleOverrides(rl.OwnerID.Int64)
		}
		derived, composite, derr := s.hopsForChain(entryID, vias, body.Mode, body.ExitMode, ownerOverrides)
```

（`rl` 是该 handler 已加载的规则变量，`internal/server/api.go:1790`。）

- [ ] **Step 6: 运行目标测试确认通过**

Run: `go test ./internal/server/ -run 'TestGrantEntryOverride|TestViaNodeWithoutOverride' -v`
Expected: PASS（两个用例）。

- [ ] **Step 7: 服务端全量回归**

Run: `go test ./internal/server/...`
Expected: PASS（既有 via/组合链测试的 `hopsForChain` 调用经四个改点后仍传入正确的有效角色；纯 entry / entry|via 节点不受影响）。

- [ ] **Step 8: Commit**

```bash
git add internal/server/api.go internal/server/grant_role_entry_test.go
git commit -m "feat(server): judge entry/via usability by per-grant effective role"
```

---

### Task 4: my 响应把节点角色替换为有效角色

**Files:**
- Modify: `internal/server/api.go`（新增 `applyEffectiveRoles`；`apiMyListRules`、`apiMyGetRule` 合并）
- Test: `internal/server/grant_role_entry_test.go`（追加）

**Interfaces:**
- Consumes: `db.EffectiveNodeRoles`, `db.ListNodesForUser`（含 `Roles`）。
- Produces: `func applyEffectiveRoles(nodes []*db.Node, grants []*db.UserNode)` —— 就地把每个节点的 `Roles` 覆写为该授权的有效角色，供 my 规则表单按用户实际可用性过滤入口/via。

- [ ] **Step 1: 写失败测试**

Append the following to `internal/server/grant_role_entry_test.go`（确保该文件 import 块含 `"encoding/json"` 与 `"net/http/httptest"`，缺则补上）:

```go
// The my rule-form node list reports the grantee's effective role: a via-only
// node with an entry override surfaces as entry-capable (bit 0 set).
func TestMyNodesReportEffectiveRoles(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	_ = db.UpdateNodeRelayHost(d, m.ID, "2.2.2.2")
	if err := db.UpdateNodeRoles(d, m.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}
	uid, cookie := loginAsUser(t, d, 22)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	if err := db.SetGrantRoles(d, uid, m.ID, db.NodeRoleEntry); err != nil {
		t.Fatal(err)
	}

	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/my/rules", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my rules: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []struct {
			ID    int64 `json:"id"`
			Roles int64 `json:"roles"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range resp.Nodes {
		if n.ID == m.ID {
			found = true
			if n.Roles&db.NodeRoleEntry == 0 {
				t.Fatalf("middle node effective roles = %d, want entry bit set", n.Roles)
			}
		}
	}
	if !found {
		t.Fatalf("granted node %d missing from my nodes", m.ID)
	}
}
```

Add `"encoding/json"` and `"net/http/httptest"` to this test file's import block if not already present.

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run 'TestMyNodesReportEffectiveRoles' -v`
Expected: FAIL（`middle` 节点 roles 仍是节点掩码 `NodeRoleVia`，entry 位未置）。

- [ ] **Step 3: 新增 `applyEffectiveRoles` helper**

In `internal/server/api.go`, immediately before `apiMyListRules` (before line 2539), insert:

```go
// applyEffectiveRoles overwrites each granted node's Roles with the grantee's
// effective role (grant override if set, else the node's own mask) so the
// my-side rule form filters entry/via candidates by what this user may actually
// do with the node. nodes and grants are index-aligned as ListNodesForUser
// returns them.
func applyEffectiveRoles(nodes []*db.Node, grants []*db.UserNode) {
	for i, n := range nodes {
		if i < len(grants) && grants[i] != nil {
			n.Roles = db.EffectiveNodeRoles(n.Roles, grants[i].Roles)
		}
	}
}
```

- [ ] **Step 4: 两处 my handler 合并有效角色（一次 replace_all）**

`apiMyListRules`（line 2544）与 `apiMyGetRule`（line 2613）是**仅有**的两处 `grantedNodes, _, _ := db.ListNodesForUser(s.DB, u.ID)`（其余调用点用 `grantedNodes, grants, _`，字符串不同，不会误伤）。对 `internal/server/api.go` 用 `replace_all` 把:

```go
	grantedNodes, _, _ := db.ListNodesForUser(s.DB, u.ID)
```

全部替换为:

```go
	grantedNodes, grants, _ := db.ListNodesForUser(s.DB, u.ID)
	applyEffectiveRoles(grantedNodes, grants)
```

- [ ] **Step 5: 核对两处均已覆盖**

Run: `grep -c "applyEffectiveRoles(grantedNodes, grants)" internal/server/api.go`
Expected: `2`（`apiMyListRules` 与 `apiMyGetRule` 各一处）。

- [ ] **Step 6: 运行确认通过**

Run: `go test ./internal/server/ -run 'TestMyNodesReportEffectiveRoles' -v`
Expected: PASS。

- [ ] **Step 7: 服务端全量回归**

Run: `go test ./internal/server/...`
Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/server/api.go internal/server/grant_role_entry_test.go
git commit -m "feat(server): my node list reports per-grant effective roles"
```

---

### Task 5: 授权掩码写端点与已授权用户展示

**Files:**
- Modify: `internal/server/api.go`（新增 `apiSetPerNodeRoles`）
- Modify: `internal/server/server.go`（注册路由）
- Test: `internal/server/grant_role_entry_test.go`（追加）

**Interfaces:**
- Consumes: `db.SetGrantRoles`, `NodeRoleEntry|NodeRoleVia`。
- Produces: `POST /api/users/{id}/nodes/{nodeID}/roles`，body `{"roles": <int>}`（`0`=继承，`1|2|3` 合法）。

- [ ] **Step 1: 写失败测试**

Append to `internal/server/grant_role_entry_test.go`:

```go
// The per-node roles endpoint writes the override; an illegal bit is rejected.
func TestSetPerNodeRolesEndpoint(t *testing.T) {
	d := openDB(t)
	m, _ := db.CreateNode(d, "middle", "", "")
	uid, _ := loginAsUser(t, d, 23)
	_ = db.GrantNode(d, uid, m.ID, 5, 0)
	adminCookie := loginAsAdmin(t, d)

	s, _ := New(d)

	setRoles := func(val int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"roles": val})
		req := httptest.NewRequest("POST",
			"/api/users/"+strconv.FormatInt(uid, 10)+"/nodes/"+strconv.FormatInt(m.ID, 10)+"/roles",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	if rec := setRoles(db.NodeRoleEntry); rec.Code != http.StatusOK {
		t.Fatalf("set entry override: %d %s", rec.Code, rec.Body.String())
	}
	g, _ := db.GetNodeGrant(d, uid, m.ID)
	if g.Roles != db.NodeRoleEntry {
		t.Fatalf("stored roles = %d, want %d", g.Roles, db.NodeRoleEntry)
	}

	if rec := setRoles(4); rec.Code == http.StatusOK {
		t.Fatalf("illegal role bit must be rejected; got 200")
	}
}
```

Add `"bytes"` and `"strconv"` to the import block if not already present. `loginAsAdmin` is an existing server test helper (used across the package).

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run 'TestSetPerNodeRolesEndpoint' -v`
Expected: FAIL（路由 404 / handler 未定义）。

- [ ] **Step 3: 实现 `apiSetPerNodeRoles`**

In `internal/server/api.go`, immediately after `apiSetPerNodeMaxForwards` (ends around line 3135), insert:

```go
func (s *Server) apiSetPerNodeRoles(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	userID, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	nodeID, err := urlParamInt64(r, "nodeID")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad node id")
		return
	}
	var body struct {
		Roles int64 `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// 0 = inherit the node mask; any other value must be a legal entry/via
	// combination. This only changes what the grantee may do with the node, so
	// there is no need to constrain it to a subset of the node's own mask.
	if body.Roles&^(db.NodeRoleEntry|db.NodeRoleVia) != 0 {
		jsonErr(w, http.StatusBadRequest, "roles invalid")
		return
	}
	if err := db.SetGrantRoles(s.DB, userID, nodeID, body.Roles); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_roles", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d roles=%d", nodeID, body.Roles))
	jsonOK(w, map[string]any{"ok": true})
}
```

- [ ] **Step 4: 注册路由**

In `internal/server/server.go`, after the per-node rate-limit route (line 496):

```go
			r.Post("/users/{id}/nodes/{nodeID}/rate-limit", s.apiSetPerNodeRateLimit)
```

add:

```go
			r.Post("/users/{id}/nodes/{nodeID}/roles", s.apiSetPerNodeRoles)
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/server/ -run 'TestSetPerNodeRolesEndpoint' -v`
Expected: PASS。

- [ ] **Step 6: 服务端全量回归**

Run: `go test ./internal/server/...`
Expected: PASS。

- [ ] **Step 7: Commit**

```bash
git add internal/server/api.go internal/server/server.go internal/server/grant_role_entry_test.go
git commit -m "feat(server): per-node grant roles endpoint"
```

---

### Task 6: 前端授权掩码编辑与展示

前端无测试框架，本任务用 `npm run build` 验证可构建 + 手动验证清单。有效角色已由服务端注入 my 节点列表，`RuleFormModal` 的入口/via 过滤（`RuleFormModal.jsx:105`、`:132`）**无需改动**。

**Files:**
- Modify: `web/src/pages/users/Detail.jsx`（新增 `PerNodeRolesForm`；已授权表格加「用途」列）
- Modify: `web/src/pages/nodes/Detail.jsx`（已授权用户表格加「用途」列）

**Interfaces:**
- Consumes: `POST /api/users/{id}/nodes/{nodeID}/roles`（Task 5）；`grantByNode[n.id].roles`（`users/Detail` 的 `grants` 数组已含 `roles`，Task 1）；`g.roles`（`nodes/Detail` 的 `granted_users`，Task 1）；节点自身掩码 `n.roles`（用于「继承」时显示）。

- [ ] **Step 1: `users/Detail.jsx` 新增 `PerNodeRolesForm` 组件**

In `web/src/pages/users/Detail.jsx`, after `PerNodeRateForm` (ends around line 346), add a component that edits the grant mask. `0`=继承（跟随节点掩码），`1`=入口，`2`=中间层，`3`=两者：

```jsx
function PerNodeRolesForm({ userId, nodeId, roles, onDone }) {
  // 0 = 继承节点掩码；其余是覆盖值（入口=1 / 中间层=2 的组合）。
  const [val, setVal] = useState(String(roles ?? 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/roles`, { roles: Number(val) })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <select className="input-field" value={val} onChange={e => setVal(e.target.value)} style={{ width: 108 }}>
        <option value="0">跟随节点</option>
        <option value="1">仅入口</option>
        <option value="2">仅中间层</option>
        <option value="3">入口+中间层</option>
      </select>
      <button type="submit" className="btn-secondary text-xs">设用途</button>
    </form>
  )
}
```

- [ ] **Step 2: `users/Detail.jsx` 已授权表格加「用途」列**

In the granted-nodes table (`web/src/pages/users/Detail.jsx`, header around line 715, body around line 726), add a `<th>用途</th>` header (e.g. right after the 限速 column header) and a matching cell in the row body:

Header — insert after the 限速 `<th>`:

```jsx
            <th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">用途</th>
```

Row cell — insert after the `PerNodeRateForm` `<td>`:

```jsx
                <td className="px-3 py-2">
                  <PerNodeRolesForm userId={userId} nodeId={n.id} roles={grantByNode[n.id]?.roles} onDone={onDone} />
                </td>
```

- [ ] **Step 3: `nodes/Detail.jsx` 已授权用户表格显示用途**

In `web/src/pages/nodes/Detail.jsx`, the granted-users table (header around line 457, body around line 462) exposes `g.roles`. Add a read-only 用途 column. Header — insert after the 单节点配额 `<th>`:

```jsx
                <th>用途</th>
```

Row cell — insert after the `max_forwards` `<td>` (`<td className="font-mono">{g.max_forwards}</td>`):

```jsx
                    <td className="text-xs">{g.roles === 1 ? '仅入口' : g.roles === 2 ? '仅中间层' : g.roles === 3 ? '入口+中间层' : '跟随节点'}</td>
```

- [ ] **Step 4: 构建验证**

Run: `cd web && npm run build`
Expected: 构建成功，无语法/引用错误。

- [ ] **Step 5: 手动验证清单**

在开发环境（`go run ./cmd/...` 起服务 + `cd web && npm run dev`，或构建后整体运行）验证：

1. 建一个纯中间层节点 M（管理端节点角色只勾「中间层」，`nodes.roles = 2`）。
2. 把 M 授权给用户甲；在 `users/{甲}` 授权表格里把 M 的「用途」设为「入口+中间层」（或「仅入口」）。
3. 以甲登录 → 建规则：入口选择器应出现 M；选 M、不选中间层、填目标，创建成功（M 单跳直落）。
4. 另建用户乙，授权 M 但「用途」保持「跟随节点」。以乙登录建规则：入口选择器**不**出现 M（M 节点掩码仅中间层）。
5. 回到管理端 `nodes/{M}` 的已授权用户表：甲一行「用途」显示「入口+中间层」，乙显示「跟随节点」。

- [ ] **Step 6: Commit**

```bash
git add web/src/pages/users/Detail.jsx web/src/pages/nodes/Detail.jsx
git commit -m "feat(web): per-grant node role (usage) editor and display"
```

---

## 完成后

- 全量回归：`go test ./...`，`cd web && npm run build`。
- 该功能不改动任何计费路径；中间层节点被当作入口时按其 `rate_multiplier` 计一次全局用量（既有入口段口径），无需额外验证计费数字。
