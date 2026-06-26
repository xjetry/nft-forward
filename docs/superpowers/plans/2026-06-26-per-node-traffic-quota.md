# Per-Node 流量配额 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将流量配额从全局共享扩展为可按节点单独配置，支持节点倍率和自动周期重置。

**Architecture:** 在 `nodes` 表加 `traffic_multiplier`，在 `user_nodes` 表加 `traffic_quota_bytes` / `traffic_used_bytes`，在 `users` 表加 `traffic_reset_days`。`applyCounters` 改为按倍率累加全局 + 按原始字节累加 per-node。`ActiveRuleHopsForPush` 增加 per-node 超额过滤。超额执行分两层：per-node 超额只禁相关规则，全局超额禁用用户。

**Tech Stack:** Go, SQLite, chi router

## Global Constraints

- `nodeCols` / `scanNode` / `grants.go` inline scan 三处必须保持对齐（参照 queries.go:237 注释）
- `traffic_quota_bytes = 0` 表示无限制（回退全局）
- `traffic_reset_days = 0` 表示永不自动重置
- `traffic_multiplier` 默认 1.0
- 迁移文件编号从 0013 开始

---

### Task 1: 数据库迁移 + struct 更新

**Files:**
- Create: `internal/db/migrations/0013_per_node_traffic_quota.sql`
- Modify: `internal/db/queries.go:32-56` (Node struct, nodeCols, scanNode)
- Modify: `internal/db/queries.go:111-121` (User struct, userCols, scanUser)
- Modify: `internal/db/grants.go:12-17` (UserNode struct)
- Modify: `internal/db/grants.go:55-59` (GrantNode)
- Modify: `internal/db/grants.go:76-82` (scanUserNode)
- Modify: `internal/db/grants.go:86-142` (ListNodesForUser inline scan)
- Modify: `internal/db/grants.go:150-182` (ListUsersForNode)
- Modify: `internal/db/grants.go:184-186` (ListAllGrants)

**Interfaces:**
- Produces: `Node.TrafficMultiplier float64`, `UserNode.TrafficQuotaBytes int64`, `UserNode.TrafficUsedBytes int64`, `User.TrafficResetDays int`, `User.LastTrafficResetAt int64`
- Produces: `GrantNode(d *sql.DB, userID, nodeID int64, maxForwards int, trafficQuotaBytes int64) error`

- [ ] **Step 1: 创建迁移文件**

```sql
-- internal/db/migrations/0013_per_node_traffic_quota.sql
ALTER TABLE nodes ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0;
ALTER TABLE user_nodes ADD COLUMN traffic_quota_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_nodes ADD COLUMN traffic_used_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN traffic_reset_days INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN last_traffic_reset_at INTEGER NOT NULL DEFAULT 0;
```

- [ ] **Step 2: 更新 Node struct 和 nodeCols/scanNode**

`internal/db/queries.go` — Node struct 末尾加字段：

```go
type Node struct {
	// ... existing fields ...
	LastUpgradeError   string        `json:"last_upgrade_error,omitempty"`
	TrafficMultiplier  float64       `json:"traffic_multiplier"`
}
```

nodeCols 末尾追加 `,traffic_multiplier`：

```go
const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,online,agent_version,agent_sha,last_seen,last_apply_at,last_error,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error,hidden,sort_order,traffic_multiplier`
```

scanNode 的 Scan 调用末尾追加 `&n.TrafficMultiplier`：

```go
if err := r.Scan(
	&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
	&n.RelayHost, &n.Online, &agentVersion, &n.AgentSHA,
	&lastSeen, &n.LastApplyAt, &n.LastError,
	&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
	&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
	&hidden, &n.SortOrder,
	&n.TrafficMultiplier,
); err != nil {
```

- [ ] **Step 3: 更新 User struct / userCols / scanUser**

`internal/db/queries.go` — User struct 加字段：

```go
type User struct {
	// ... existing fields ...
	TrafficUsedBytes    int64          `json:"traffic_used_bytes"`
	TrafficResetDays    int            `json:"traffic_reset_days"`
	LastTrafficResetAt  int64          `json:"last_traffic_reset_at"`
	ExpiresAt           sql.NullInt64  `json:"expires_at"`
	// ... rest ...
}
```

userCols 追加 `traffic_reset_days, last_traffic_reset_at`（放在 `traffic_used_bytes` 之后，`expires_at` 之前）：

```go
const userCols = `id, username, pw_hash, role, disabled, disable_reason, max_forwards, traffic_quota_bytes, traffic_used_bytes, traffic_reset_days, last_traffic_reset_at, expires_at, landing_sub_url, landing_uris`
```

scanUser 的 Scan 相应追加 `&u.TrafficResetDays, &u.LastTrafficResetAt`：

```go
if err := r.Scan(&u.ID, &u.Username, &u.PwHash, &u.Role, &disabled, &u.DisableReason, &u.MaxForwards, &u.TrafficQuotaBytes, &u.TrafficUsedBytes, &u.TrafficResetDays, &u.LastTrafficResetAt, &u.ExpiresAt, &u.LandingSubURL, &u.LandingURIs); err != nil {
```

- [ ] **Step 4: 更新 UserNode struct / scanUserNode / GrantNode**

`internal/db/grants.go` — UserNode struct：

```go
type UserNode struct {
	UserID            int64 `json:"user_id"`
	NodeID            int64 `json:"node_id"`
	MaxForwards       int   `json:"max_forwards"`
	TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64 `json:"traffic_used_bytes"`
	GrantedAt         int64 `json:"granted_at"`
}
```

scanUserNode 更新：

```go
func scanUserNode(r rowScanner) (*UserNode, error) {
	g := &UserNode{}
	if err := r.Scan(&g.UserID, &g.NodeID, &g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
```

GrantNode 增加 trafficQuotaBytes 参数：

```go
func GrantNode(d *sql.DB, userID, nodeID int64, maxForwards int, trafficQuotaBytes int64) error {
	_, err := d.Exec(`INSERT INTO user_nodes(user_id, node_id, max_forwards, traffic_quota_bytes, granted_at) VALUES (?,?,?,?,?)
		ON CONFLICT(user_id, node_id) DO UPDATE SET max_forwards=excluded.max_forwards, traffic_quota_bytes=excluded.traffic_quota_bytes`,
		userID, nodeID, maxForwards, trafficQuotaBytes, now())
	return err
}
```

ListAllGrants 更新查询列：

```go
func ListAllGrants(d *sql.DB) ([]*UserNode, error) {
	return queryAll(d, `SELECT user_id, node_id, max_forwards, traffic_quota_bytes, traffic_used_bytes, granted_at FROM user_nodes`, scanUserNode)
}
```

- [ ] **Step 5: 更新 ListNodesForUser inline scan**

`internal/db/grants.go` ListNodesForUser — SELECT 追加 grant 字段，Scan 追加：

```go
rows, err := d.Query(`
	SELECT `+nodeCols+`,
	       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.granted_at
	FROM nodes n JOIN user_nodes g ON g.node_id = n.id
	WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
```

Scan 中追加（在现有 `&g.MaxForwards, &g.GrantedAt` 之间插入）：

```go
if err := rows.Scan(
	// ... node fields including &n.TrafficMultiplier at end ...
	&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.GrantedAt,
); err != nil {
```

- [ ] **Step 6: 更新 ListUsersForNode**

`internal/db/grants.go` ListUsersForNode — 返回类型和查询加上新字段：

```go
func ListUsersForNode(d *sql.DB, nodeID int64) ([]struct {
	UserID            int64  `json:"user_id"`
	Username          string `json:"username"`
	MaxForwards       int    `json:"max_forwards"`
	TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
	TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
	GrantedAt         int64  `json:"granted_at"`
}, error) {
	rows, err := d.Query(`SELECT g.user_id, u.username, g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.granted_at
		FROM user_nodes g JOIN users u ON u.id = g.user_id
		WHERE g.node_id = ? ORDER BY g.granted_at`, nodeID)
```

对应的 Scan 和 struct 定义都加上 `TrafficQuotaBytes` 和 `TrafficUsedBytes`。

- [ ] **Step 7: 修复所有 GrantNode 调用方**

全局搜索 `db.GrantNode(` 并在现有的 `maxForwards` 参数后追加 `, 0`（默认无专有配额）。涉及文件：
- `internal/server/api.go` (apiGrantNode)
- 所有测试文件中的 `db.GrantNode` 调用

- [ ] **Step 8: 运行测试确认编译通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/...`
Expected: 全部 PASS

- [ ] **Step 9: Commit**

```bash
git add internal/db/migrations/0013_per_node_traffic_quota.sql internal/db/queries.go internal/db/grants.go internal/server/
git commit -m "$(cat <<'EOF'
feat: add per-node traffic quota schema and struct fields

Add traffic_multiplier to nodes, traffic_quota_bytes/traffic_used_bytes
to user_nodes, and traffic_reset_days to users. Update all scan/cols
lockstep points. No behavior change yet — all defaults preserve existing
semantics.
EOF
)"
```

---

### Task 2: per-node 流量计数 DB 函数 + 周期重置

**Files:**
- Modify: `internal/db/queries.go` (新增函数)
- Create: `internal/db/traffic.go` (流量相关函数集中管理)
- Test: `internal/db/traffic_test.go`

**Interfaces:**
- Consumes: `User.TrafficResetDays int`, `UserNode.TrafficQuotaBytes int64`, `UserNode.TrafficUsedBytes int64`, `Node.TrafficMultiplier float64`
- Produces:
  - `AddUserNodeTraffic(d *sql.DB, userID, nodeID, delta int64) error`
  - `ResetUserNodeTraffic(d *sql.DB, userID, nodeID int64) error`
  - `ResetAllUserTraffic(d *sql.DB, userID int64) error` — 清零全局 + 所有 per-node
  - `NodeMultipliers(d *sql.DB) (map[int64]float64, error)`
  - `CheckAndResetTrafficCycle(d *sql.DB, u *User) (bool, error)` — 比较 `last_traffic_reset_at` 与当前周期起点，返回是否发生了重置
  - `NodesExceedingQuota(d *sql.DB, userID int64) ([]int64, error)`
  - `RulesAffectedByNode(d *sql.DB, userID, nodeID int64) ([]int64, error)` — 返回受影响的 rule_hop node IDs

- [ ] **Step 1: 编写 traffic_test.go 中的测试**

```go
// internal/db/traffic_test.go
package db

import (
	"testing"
)

func TestAddUserNodeTraffic(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "n1")
	grantNode(t, d, uid, nid)

	if err := AddUserNodeTraffic(d, uid, nid, 1000); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.TrafficUsedBytes != 1000 {
		t.Fatalf("want 1000, got %d", g.TrafficUsedBytes)
	}

	// accumulate
	if err := AddUserNodeTraffic(d, uid, nid, 500); err != nil {
		t.Fatal(err)
	}
	g, _ = GetNodeGrant(d, uid, nid)
	if g.TrafficUsedBytes != 1500 {
		t.Fatalf("want 1500, got %d", g.TrafficUsedBytes)
	}
}

func TestResetAllUserTraffic(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	n1 := createTestNode(t, d, "a")
	n2 := createTestNode(t, d, "b")
	grantNode(t, d, uid, n1)
	grantNode(t, d, uid, n2)

	_ = AddUserTraffic(d, uid, 5000)
	_ = AddUserNodeTraffic(d, uid, n1, 2000)
	_ = AddUserNodeTraffic(d, uid, n2, 3000)

	if err := ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}
	u, _ := GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global not reset: %d", u.TrafficUsedBytes)
	}
	g1, _ := GetNodeGrant(d, uid, n1)
	g2, _ := GetNodeGrant(d, uid, n2)
	if g1.TrafficUsedBytes != 0 || g2.TrafficUsedBytes != 0 {
		t.Fatalf("per-node not reset: %d, %d", g1.TrafficUsedBytes, g2.TrafficUsedBytes)
	}
}

func TestNodeMultipliers(t *testing.T) {
	d := openTestDB(t)
	n1 := createTestNode(t, d, "x")
	n2 := createTestNode(t, d, "y")
	d.Exec(`UPDATE nodes SET traffic_multiplier=0.5 WHERE id=?`, n2)

	m, err := NodeMultipliers(d)
	if err != nil {
		t.Fatal(err)
	}
	if m[n1] != 1.0 {
		t.Fatalf("n1 want 1.0, got %f", m[n1])
	}
	if m[n2] != 0.5 {
		t.Fatalf("n2 want 0.5, got %f", m[n2])
	}
}

func TestCheckAndResetTrafficCycle(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "n")
	grantNode(t, d, uid, nid)

	// reset_days=0 means never reset
	u, _ := GetUserByID(d, uid)
	reset, _ := CheckAndResetTrafficCycle(d, u)
	if reset {
		t.Fatal("reset_days=0 should not trigger reset")
	}

	// set reset_days=30, created_at=31 days ago, add traffic
	past := now() - 31*86400
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=? WHERE id=?`, past, uid)
	_ = AddUserTraffic(d, uid, 9999)
	_ = AddUserNodeTraffic(d, uid, nid, 8888)

	u, _ = GetUserByID(d, uid)
	reset, _ = CheckAndResetTrafficCycle(d, u)
	if !reset {
		t.Fatal("should have reset after 31 days with 30-day cycle")
	}
	u, _ = GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global not reset: %d", u.TrafficUsedBytes)
	}
	g, _ := GetNodeGrant(d, uid, nid)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("per-node not reset: %d", g.TrafficUsedBytes)
	}

	// calling again in the same cycle should not reset
	_ = AddUserTraffic(d, uid, 100)
	u, _ = GetUserByID(d, uid)
	reset, _ = CheckAndResetTrafficCycle(d, u)
	if reset {
		t.Fatal("should not reset again in same cycle")
	}
	u, _ = GetUserByID(d, uid)
	if u.TrafficUsedBytes != 100 {
		t.Fatalf("traffic should remain at 100, got %d", u.TrafficUsedBytes)
	}
}

func TestNodesExceedingQuota(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	n1 := createTestNode(t, d, "q1")
	n2 := createTestNode(t, d, "q2")
	grantNode(t, d, uid, n1)
	grantNode(t, d, uid, n2)

	// set n1 quota=1000, used=1000 (exactly at limit = exceeded)
	d.Exec(`UPDATE user_nodes SET traffic_quota_bytes=1000, traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1)
	// n2 no quota (0)
	exceeded, err := NodesExceedingQuota(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(exceeded) != 1 || exceeded[0] != n1 {
		t.Fatalf("want [%d], got %v", n1, exceeded)
	}
}

// --- test helpers ---

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func createTestUser(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	id, err := CreateUser(d, "testuser-"+RandToken(4), "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createTestNode(t *testing.T, d *sql.DB, name string) int64 {
	t.Helper()
	n, err := CreateNode(d, name+"-"+RandToken(4), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func grantNode(t *testing.T, d *sql.DB, uid, nid int64) {
	t.Helper()
	if err := GrantNode(d, uid, nid, 10, 0); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/db/ -run 'TestAddUserNodeTraffic|TestResetAllUserTraffic|TestNodeMultipliers|TestCheckAndResetTrafficCycle|TestNodesExceedingQuota' -v`
Expected: FAIL — 函数未定义

- [ ] **Step 3: 实现 traffic.go**

```go
// internal/db/traffic.go
package db

import (
	"database/sql"
	"time"
)

func AddUserNodeTraffic(d *sql.DB, userID, nodeID, delta int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + ? WHERE user_id=? AND node_id=?`,
		delta, userID, nodeID)
	return err
}

func ResetUserNodeTraffic(d *sql.DB, userID, nodeID int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

func ResetAllUserTraffic(d *sql.DB, userID int64) error {
	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes = 0 WHERE id=?`, userID); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=?`, userID)
	return err
}

func NodeMultipliers(d *sql.DB) (map[int64]float64, error) {
	rows, err := d.Query(`SELECT id, traffic_multiplier FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]float64)
	for rows.Next() {
		var id int64
		var mult float64
		if err := rows.Scan(&id, &mult); err != nil {
			return nil, err
		}
		m[id] = mult
	}
	return m, rows.Err()
}

func CheckAndResetTrafficCycle(d *sql.DB, u *User) (bool, error) {
	if u.TrafficResetDays <= 0 {
		return false, nil
	}
	nowTs := time.Now().Unix()
	period := int64(u.TrafficResetDays) * 86400
	elapsed := nowTs - u.CreatedAt
	if elapsed < 0 {
		return false, nil
	}
	cycleStart := u.CreatedAt + (elapsed/period)*period
	if u.LastTrafficResetAt >= cycleStart {
		return false, nil
	}
	if err := ResetAllUserTraffic(d, u.ID); err != nil {
		return false, err
	}
	if _, err := d.Exec(`UPDATE users SET last_traffic_reset_at=? WHERE id=?`, nowTs, u.ID); err != nil {
		return false, err
	}
	return true, nil
}

func NodesExceedingQuota(d *sql.DB, userID int64) ([]int64, error) {
	return queryInt64s(d, `SELECT node_id FROM user_nodes WHERE user_id=? AND traffic_quota_bytes > 0 AND traffic_used_bytes >= traffic_quota_bytes`, userID)
}

func RulesAffectedByNode(d *sql.DB, userID, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `SELECT DISTINCT rh.node_id FROM rule_hops rh JOIN rules r ON r.id = rh.rule_id WHERE r.owner_id=? AND rh.rule_id IN (SELECT rh2.rule_id FROM rule_hops rh2 WHERE rh2.node_id=?)`, userID, nodeID)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/db/ -run 'TestAddUserNodeTraffic|TestResetAllUserTraffic|TestNodeMultipliers|TestCheckAndResetTrafficCycle|TestNodesExceedingQuota' -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/db/traffic.go internal/db/traffic_test.go
git commit -m "$(cat <<'EOF'
feat: per-node traffic accounting and cycle reset DB functions

AddUserNodeTraffic, ResetAllUserTraffic, NodeMultipliers,
CheckAndResetTrafficCycle, NodesExceedingQuota, RulesAffectedByNode.
EOF
)"
```

---

### Task 3: applyCounters 改造 — 倍率计费 + per-node 累计

**Files:**
- Modify: `internal/server/hub.go:34-37` (OnTrafficUpdate 签名)
- Modify: `internal/server/hub.go:432-480` (applyCounters)
- Modify: `internal/server/server.go:39` (callback 赋值)
- Modify: `internal/db/queries.go:368-386` (可删除 EntryRuleHopIDs 或保留不用)
- Test: `internal/server/traffic_accounting_test.go`

**Interfaces:**
- Consumes: `db.NodeMultipliers`, `db.AddUserTraffic`, `db.AddUserNodeTraffic`, `db.RuleHopMapByNode`, `db.RulesByID`, `db.CheckAndResetTrafficCycle`, `db.GetUserByID`
- Produces: `Hub.OnTrafficUpdate func(userID, nodeID int64)` (签名变更)

- [ ] **Step 1: 编写测试**

```go
// internal/server/traffic_accounting_test.go
package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

func TestApplyCountersMultiplier(t *testing.T) {
	d := openDB(t)
	// create user
	uid, _ := loginAsUser(t, d, 100)

	// create two nodes with different multipliers
	n1, _ := db.CreateNode(d, "entry", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	d.Exec(`UPDATE nodes SET traffic_multiplier=1.0 WHERE id=?`, n1.ID)

	n2, _ := db.CreateNode(d, "relay", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	d.Exec(`UPDATE nodes SET traffic_multiplier=0.5 WHERE id=?`, n2.ID)

	// grant nodes and set per-node quota on n1
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)

	// create a rule with hops on both nodes
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)
	_ = ruleID

	s, _ := New(d)

	// simulate counters from n1: 1000 bytes
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: getHopPort(t, d, ruleID, n1.ID), BytesDelta: 1000},
	})
	// simulate counters from n2: 1000 bytes
	s.Hub.applyCounters(n2.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: getHopPort(t, d, ruleID, n2.ID), BytesDelta: 1000},
	})

	u, _ := db.GetUserByID(d, uid)
	// global: 1000*1.0 + 1000*0.5 = 1500
	if u.TrafficUsedBytes != 1500 {
		t.Fatalf("global traffic want 1500, got %d", u.TrafficUsedBytes)
	}

	// per-node: raw bytes
	g1, _ := db.GetNodeGrant(d, uid, n1.ID)
	if g1.TrafficUsedBytes != 1000 {
		t.Fatalf("n1 per-node want 1000, got %d", g1.TrafficUsedBytes)
	}
	g2, _ := db.GetNodeGrant(d, uid, n2.ID)
	if g2.TrafficUsedBytes != 1000 {
		t.Fatalf("n2 per-node want 1000, got %d", g2.TrafficUsedBytes)
	}
}

func TestApplyCountersZeroMultiplier(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "free-relay", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "3.3.3.3")
	d.Exec(`UPDATE nodes SET traffic_multiplier=0 WHERE id=?`, n1.ID)
	db.GrantNode(d, uid, n1.ID, 10, 0)

	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)
	s, _ := New(d)

	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: getHopPort(t, d, ruleID, n1.ID), BytesDelta: 5000},
	})

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("multiplier=0 should not add to global, got %d", u.TrafficUsedBytes)
	}
	g, _ := db.GetNodeGrant(d, uid, n1.ID)
	if g.TrafficUsedBytes != 5000 {
		t.Fatalf("per-node should still track raw bytes, got %d", g.TrafficUsedBytes)
	}
}
```

注意：`createTestRuleWithHops`、`createTestRuleDirectNode`、`getHopPort` 是需要在测试文件中创建的 helper，它们直接插入 rules + rule_hops 行以便测试 applyCounters 的逻辑。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestApplyCountersMultiplier|TestApplyCountersZeroMultiplier' -v`
Expected: FAIL

- [ ] **Step 3: 修改 Hub.OnTrafficUpdate 签名**

`internal/server/hub.go:34-37`：

```go
OnTrafficUpdate func(userID int64, nodeID int64)
```

`internal/server/hub.go:476`（回调调用处）修改为传 `(tid, nodeID)`，但需要在循环中跟踪 touched 的 (userID, nodeID) 对。将 `touched` 从 `map[int64]bool` 改为收集 `(userID, nodeID)` 对。

- [ ] **Step 4: 改造 applyCounters**

```go
func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) {
	hopMap, err := db.RuleHopMapByNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load rule hop map for counters: %v", nodeID, err)
		return
	}
	multipliers, err := db.NodeMultipliers(h.DB)
	if err != nil {
		log.Printf("hub: load node multipliers: %v", err)
		multipliers = map[int64]float64{}
	}
	ruleMap, _ := db.RulesByID(h.DB)
	if ruleMap == nil {
		ruleMap = map[int64]*db.Rule{}
	}

	type userNode struct{ userID, nodeID int64 }
	touched := map[userNode]bool{}

	for _, s := range samples {
		key := fmt.Sprintf("%s/%d", s.Proto, s.ListenPort)
		rh, ok := hopMap[key]
		if !ok {
			log.Printf("hub: node %d counters sample for %s/%d matched no rule_hop row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
			continue
		}
		if _, err := h.DB.Exec(`UPDATE rule_hops SET last_bytes=?, total_bytes=total_bytes+? WHERE id=?`,
			s.BytesDelta, s.BytesDelta, rh.ID); err != nil {
			log.Printf("hub: node %d counters update for %s/%d: %v", nodeID, s.Proto, s.ListenPort, err)
			continue
		}
		r := ruleMap[rh.RuleID]
		if r == nil || !r.OwnerID.Valid || s.BytesDelta <= 0 {
			continue
		}
		userID := r.OwnerID.Int64

		// per-node: raw bytes on the physical node
		if err := db.AddUserNodeTraffic(h.DB, userID, nodeID, s.BytesDelta); err != nil {
			log.Printf("hub: user %d node %d per-node traffic add: %v", userID, nodeID, err)
		}

		// composite node per-node quota: if this rule belongs to a
		// composite node (r.NodeID != nodeID), only the entry hop
		// (position=0) contributes to avoid counting the same traffic
		// once per physical hop in the chain.
		if r.NodeID != nodeID && rh.Position == 0 {
			if err := db.AddUserNodeTraffic(h.DB, userID, r.NodeID, s.BytesDelta); err != nil {
				log.Printf("hub: user %d composite node %d per-node traffic add: %v", userID, r.NodeID, err)
			}
		}

		// global: weighted by multiplier
		mult := multipliers[nodeID]
		weighted := int64(math.Round(float64(s.BytesDelta) * mult))
		if weighted > 0 {
			if err := db.AddUserTraffic(h.DB, userID, weighted); err != nil {
				log.Printf("hub: user %d traffic add: %v", userID, err)
				continue
			}
		}

		touched[userNode{userID, nodeID}] = true
	}

	if h.OnTrafficUpdate != nil {
		for un := range touched {
			h.OnTrafficUpdate(un.userID, un.nodeID)
		}
	}
}
```

在文件顶部确保 `import "math"` 已存在。

- [ ] **Step 5: 更新 server.go 的 callback 赋值**

`internal/server/server.go:39`：

```go
hub.OnTrafficUpdate = func(userID int64, nodeID int64) {
	s.enforceUserQuota(userID)
}
```

（nodeID 参数暂时不用，Task 4 会用到。）

- [ ] **Step 6: 运行全部测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server/hub.go internal/server/server.go internal/server/traffic_accounting_test.go
git commit -m "$(cat <<'EOF'
feat: multiplier-based traffic accounting in applyCounters

Replace entry-hop-only billing with per-node multiplier. Each hop's raw
bytes accumulate to per-node used, weighted bytes accumulate to global
used. OnTrafficUpdate now passes (userID, nodeID).
EOF
)"
```

---

### Task 4: 两层超额执行 + ActiveRuleHopsForPush 改造

**Files:**
- Modify: `internal/server/server.go:39` (callback 加 per-node 检查)
- Modify: `internal/server/server.go:73-97` (enforceUserQuota)
- Modify: `internal/db/rules.go:458-472` (ActiveRuleHopsForPush)
- Test: `internal/server/pernode_quota_enforcement_test.go`

**Interfaces:**
- Consumes: `db.NodesExceedingQuota`, `db.RulesAffectedByNode`, `db.ActiveRuleHopsForPush`, `db.CheckAndResetTrafficCycle`, `db.GetUserByID`
- Produces: `Server.enforcePerNodeQuota(userID, nodeID int64)`, modified `ActiveRuleHopsForPush`

- [ ] **Step 1: 编写测试**

```go
// internal/server/pernode_quota_enforcement_test.go
package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestPerNodeQuotaExclusion(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "pn1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "pn2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")

	db.GrantNode(d, uid, n1.ID, 10, 1000)  // 1000 byte quota on n1
	db.GrantNode(d, uid, n2.ID, 10, 0)     // no per-node quota on n2

	r1 := createTestRuleDirectNode(t, d, uid, n1.ID)
	r2 := createTestRuleDirectNode(t, d, uid, n2.ID)

	// before exceeding: both rules should be pushed
	hops1, _ := db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ := db.ActiveRuleHopsForPush(d, n2.ID)
	if len(hops1) == 0 {
		t.Fatal("r1 hops should be active before exceeding quota")
	}
	if len(hops2) == 0 {
		t.Fatal("r2 hops should be active")
	}

	// exceed n1 quota
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1.ID)

	// n1 rules excluded, n2 rules still active
	hops1, _ = db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ = db.ActiveRuleHopsForPush(d, n2.ID)
	if len(hops1) != 0 {
		t.Fatalf("r1 hops should be excluded after n1 quota exceeded, got %d", len(hops1))
	}
	if len(hops2) == 0 {
		t.Fatal("r2 hops should still be active — n2 has no per-node quota")
	}
	_ = r1
	_ = r2
}

func TestChainExcludedWhenOneHopExceedsQuota(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "ch1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "ch2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")

	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 500) // quota on n2

	// chain rule: n1 → n2
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)

	// exceed n2 quota
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=500 WHERE user_id=? AND node_id=?`, uid, n2.ID)

	// both n1 and n2 hops for this rule should be excluded
	hops1, _ := db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ := db.ActiveRuleHopsForPush(d, n2.ID)
	for _, h := range hops1 {
		if h.RuleID == ruleID {
			t.Fatal("chain rule hop on n1 should be excluded because n2 exceeded quota")
		}
	}
	for _, h := range hops2 {
		if h.RuleID == ruleID {
			t.Fatal("chain rule hop on n2 should be excluded")
		}
	}
}

func TestGlobalQuotaStillDisablesUser(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "gq1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)

	// set global quota
	d.Exec(`UPDATE users SET traffic_quota_bytes=2000, traffic_used_bytes=2000 WHERE id=?`, uid)

	s, _ := New(d)
	s.enforceUserQuota(uid)

	u, _ := db.GetUserByID(d, uid)
	if !u.Disabled {
		t.Fatal("user should be disabled when global quota exceeded")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestPerNodeQuotaExclusion|TestChainExcludedWhenOneHopExceedsQuota|TestGlobalQuotaStillDisablesUser' -v`
Expected: FAIL

- [ ] **Step 3: 改造 ActiveRuleHopsForPush**

`internal/db/rules.go:458-472`：

```go
func ActiveRuleHopsForPush(d *sql.DB, nodeID int64) ([]*RuleHop, error) {
	q := `SELECT ` + ruleHopCols + ` FROM rule_hops rh
		WHERE rh.node_id=?
		AND NOT EXISTS (
		  SELECT 1 FROM rules r
		  WHERE r.id = rh.rule_id AND r.disabled = 1
		)
		AND NOT EXISTS (
		  SELECT 1 FROM rules r JOIN users u ON u.id = r.owner_id
		  WHERE r.id = rh.rule_id
		  AND (u.disabled = 1 OR (u.expires_at IS NOT NULL AND u.expires_at > 0 AND u.expires_at < strftime('%s','now')))
		)
		AND NOT EXISTS (
		  SELECT 1 FROM rule_hops rh2
		  JOIN rules r2 ON r2.id = rh2.rule_id
		  JOIN user_nodes un ON un.user_id = r2.owner_id AND un.node_id = rh2.node_id
		  WHERE rh2.rule_id = rh.rule_id
		    AND un.traffic_quota_bytes > 0
		    AND un.traffic_used_bytes >= un.traffic_quota_bytes
		)
		ORDER BY rh.listen_port`
	return queryAll(d, q, scanRuleHop, nodeID)
}
```

- [ ] **Step 4: 添加 enforcePerNodeQuota 并更新 callback**

`internal/server/server.go`：

```go
func (s *Server) enforcePerNodeQuota(userID int64, nodeID int64) {
	exceeded, err := db.NodesExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: per-node check user %d: %v", userID, err)
		return
	}
	for _, excNode := range exceeded {
		affectedNodes, err := db.RulesAffectedByNode(s.DB, userID, excNode)
		if err != nil {
			log.Printf("quota: affected nodes for user %d node %d: %v", userID, excNode, err)
			continue
		}
		for _, n := range affectedNodes {
			if err := s.dispatchToNode(n); err != nil {
				log.Printf("quota: re-dispatch node %d after per-node quota user %d: %v", n, userID, err)
			}
		}
	}
}
```

更新 callback（server.go:39）：

```go
hub.OnTrafficUpdate = func(userID int64, nodeID int64) {
	s.enforcePerNodeQuota(userID, nodeID)
	s.enforceUserQuota(userID)
}
```

- [ ] **Step 5: 在 applyCounters 中插入周期重置检查**

在 `applyCounters` 的流量累加循环之前，对每个涉及的 userID 检查周期重置。在 `touched` 循环之前加一个去重的 user 集合做检查：

```go
// 在 for _, s := range samples 循环之前加:
checkedCycle := map[int64]bool{}

// 在 r == nil || !r.OwnerID.Valid 检查之后、per-node 累加之前加:
if !checkedCycle[userID] {
	checkedCycle[userID] = true
	u, err := db.GetUserByID(h.DB, userID)
	if err == nil {
		if reset, _ := db.CheckAndResetTrafficCycle(h.DB, u); reset {
			log.Printf("hub: user %d traffic cycle reset", userID)
			if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
				_ = db.SetUserDisabled(h.DB, userID, false, "")
			}
		}
	}
}
```

- [ ] **Step 6: 运行全部测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server/server.go internal/db/rules.go internal/server/pernode_quota_enforcement_test.go internal/server/hub.go
git commit -m "$(cat <<'EOF'
feat: two-tier quota enforcement with per-node exclusion

ActiveRuleHopsForPush now excludes rules where any hop's per-node quota
is exceeded. enforcePerNodeQuota re-dispatches only affected nodes.
Global quota still disables the user entirely. Cycle reset runs
inline before traffic accumulation.
EOF
)"
```

---

### Task 5: API 接口 — 节点倍率 + per-node 配额 + 重置周期

**Files:**
- Modify: `internal/server/server.go:280-326` (路由注册)
- Modify: `internal/server/api.go` (新增 handler + 修改现有 handler)
- Test: `internal/server/pernode_quota_api_test.go`

**Interfaces:**
- Consumes: `db.GrantNode` (新签名), `db.ResetAllUserTraffic`, `db.ResetUserNodeTraffic`, `db.NodesExceedingQuota`, `db.RulesAffectedByNode`
- Produces: 新 API endpoint handlers

- [ ] **Step 1: 编写 API 测试**

```go
// internal/server/pernode_quota_api_test.go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestAPISetNodeMultiplier(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "mnode", "", "")
	s, _ := New(d)
	_, cookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"traffic_multiplier": 0.5})
	req := httptest.NewRequest("POST", "/api/nodes/"+itoa(n.ID)+"/multiplier", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	node, _ := db.GetNode(d, n.ID)
	if node.TrafficMultiplier != 0.5 {
		t.Fatalf("want 0.5, got %f", node.TrafficMultiplier)
	}
}

func TestAPISetPerNodeQuota(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "qnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	s, _ := New(d)
	_, adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"traffic_quota_bytes": 1073741824})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/quota", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficQuotaBytes != 1073741824 {
		t.Fatalf("want 1073741824, got %d", g.TrafficQuotaBytes)
	}
}

func TestAPIResetPerNodeTraffic(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	db.AddUserNodeTraffic(d, uid, n.ID, 5000)
	s, _ := New(d)
	_, adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/reset-traffic", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("want 0, got %d", g.TrafficUsedBytes)
	}
}

func TestAPIResetTrafficClearsPerNode(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rtnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	db.AddUserTraffic(d, uid, 3000)
	db.AddUserNodeTraffic(d, uid, n.ID, 2000)
	s, _ := New(d)
	_, adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/reset-traffic", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global want 0, got %d", u.TrafficUsedBytes)
	}
	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("per-node want 0, got %d", g.TrafficUsedBytes)
	}
}

func TestAPISetResetDays(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	s, _ := New(d)
	_, adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"traffic_reset_days": 30})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/reset-days", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficResetDays != 30 {
		t.Fatalf("want 30, got %d", u.TrafficResetDays)
	}
}
```

注意：需要 `loginAsAdmin` 和 `itoa` helper。`loginAsAdmin` 类似 `loginAsUser` 但 role="admin"。`itoa` 就是 `strconv.FormatInt`。查看现有测试文件中是否已有这些 helper，如果没有则在此测试文件中创建。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestAPISetNodeMultiplier|TestAPISetPerNodeQuota|TestAPIResetPerNodeTraffic|TestAPIResetTrafficClearsPerNode|TestAPISetResetDays' -v`
Expected: FAIL

- [ ] **Step 3: 注册新路由**

`internal/server/server.go` 路由部分追加：

```go
r.Post("/nodes/{id}/multiplier", s.apiSetNodeMultiplier)
r.Post("/users/{id}/nodes/{nodeID}/quota", s.apiSetPerNodeQuota)
r.Post("/users/{id}/nodes/{nodeID}/reset-traffic", s.apiResetPerNodeTraffic)
r.Post("/users/{id}/reset-days", s.apiSetResetDays)
```

- [ ] **Step 4: 实现 API handler**

`internal/server/api.go` 追加：

```go
func (s *Server) apiSetNodeMultiplier(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TrafficMultiplier float64 `json:"traffic_multiplier"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficMultiplier < 0 {
		jsonErr(w, http.StatusBadRequest, "倍率不能为负")
		return
	}
	if _, err := s.DB.Exec(`UPDATE nodes SET traffic_multiplier=? WHERE id=?`, body.TrafficMultiplier, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_multiplier", strconv.FormatInt(id, 10), fmt.Sprintf("%.2f", body.TrafficMultiplier))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetPerNodeQuota(w http.ResponseWriter, r *http.Request) {
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
		TrafficQuotaBytes int64 `json:"traffic_quota_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficQuotaBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "字节数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET traffic_quota_bytes=? WHERE user_id=? AND node_id=?`,
		body.TrafficQuotaBytes, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_quota", strconv.FormatInt(userID, 10), fmt.Sprintf("node=%d bytes=%d", nodeID, body.TrafficQuotaBytes))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetPerNodeTraffic(w http.ResponseWriter, r *http.Request) {
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
	if err := db.ResetUserNodeTraffic(s.DB, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// re-dispatch in case this node was previously excluded due to quota
	affected, _ := db.RulesAffectedByNode(s.DB, userID, nodeID)
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_node_traffic", strconv.FormatInt(userID, 10), strconv.FormatInt(nodeID, 10))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiSetResetDays(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		TrafficResetDays int `json:"traffic_reset_days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.TrafficResetDays < 0 {
		jsonErr(w, http.StatusBadRequest, "天数无效")
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET traffic_reset_days=? WHERE id=?`, body.TrafficResetDays, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.set_reset_days", strconv.FormatInt(id, 10), strconv.Itoa(body.TrafficResetDays))
	jsonOK(w, map[string]any{"ok": true})
}
```

- [ ] **Step 5: 修改 apiResetUserTraffic 使用 ResetAllUserTraffic**

```go
func (s *Server) apiResetUserTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := db.ResetAllUserTraffic(s.DB, id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// re-dispatch to re-enable rules that were excluded by per-node quota
	if nodes, err := db.DistinctUserNodes(s.DB, id); err == nil {
		for _, n := range nodes {
			_ = s.dispatchToNode(n)
		}
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_traffic", strconv.FormatInt(id, 10), "")
	jsonOK(w, map[string]any{"ok": true})
}
```

- [ ] **Step 6: 修改 apiGrantNode 接受 traffic_quota_bytes**

```go
var body struct {
	NodeID            int64   `json:"node_id"`
	NodeIDs           []int64 `json:"node_ids"`
	MaxForwards       int     `json:"max_forwards"`
	TrafficQuotaBytes int64   `json:"traffic_quota_bytes"`
}
```

调用处改为 `db.GrantNode(s.DB, userID, nid, body.MaxForwards, body.TrafficQuotaBytes)`。

- [ ] **Step 7: 修改 apiUserFullView 返回新字段**

```go
m["traffic_reset_days"] = u.TrafficResetDays
```

- [ ] **Step 8: 运行全部测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v`
Expected: 全部 PASS

- [ ] **Step 9: Commit**

```bash
git add internal/server/api.go internal/server/server.go internal/server/pernode_quota_api_test.go
git commit -m "$(cat <<'EOF'
feat: API endpoints for node multiplier, per-node quota, and reset cycle

New: /nodes/{id}/multiplier, /users/{id}/nodes/{nodeID}/quota,
/users/{id}/nodes/{nodeID}/reset-traffic, /users/{id}/reset-days.
Modified: reset-traffic now clears per-node usage, grant-nodes accepts
traffic_quota_bytes, user full-view returns traffic_reset_days.
EOF
)"
```

---

### Task 6: 整合验证 + 清理

**Files:**
- Possibly modify: `internal/db/queries.go` (删除不再需要的 `EntryRuleHopIDs`)
- Test: 运行完整测试套件 + 手动验证

**Interfaces:**
- Consumes: 所有前序 task 的产出

- [ ] **Step 1: 检查 EntryRuleHopIDs 是否仍有其他调用方**

Run: `grep -rn 'EntryRuleHopIDs' /Users/xjetry/work/vibe/nft-forward/internal/`

如果只在旧的 `applyCounters` 中使用（已移除），则删除该函数。

- [ ] **Step 2: 运行完整测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/... -v -count=1`
Expected: 全部 PASS

- [ ] **Step 3: 运行 go vet**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go vet ./...`
Expected: 无错误

- [ ] **Step 4: 验证迁移文件可以在现有数据库上运行**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/db/ -run TestOpenDB -v`（或者使用 openDB 的测试，它会触发所有迁移）

- [ ] **Step 5: Commit 清理**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore: remove unused EntryRuleHopIDs after multiplier migration
EOF
)"
```

