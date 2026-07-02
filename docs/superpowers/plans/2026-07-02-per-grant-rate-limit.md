# （用户+节点）共享限速 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 限速从规则级迁移到授权（grant）级：同一 grant 下的所有规则共享一个 X MB/s 总带宽桶（双向合计），移除规则级限速入口。

**Architecture:** `user_nodes` 新增 `rate_limit_mbytes` 列；`buildRules` 按规则所属面板节点的 grant 把 `shape_group`（grant rowid）+ `rate_mbytes` 填到 `nft.Rule` 随规则下发；agent 侧 userspace 用按组共享的 `rate.Limiter`、kernel 用 connmark（首包打标+每包恢复）把整组双向流量分进同一 tc HTB class。旧 agent 靠 server 同步填充的旧字段 `bandwidth_mbps` 降级为每规则近似限速。

**Tech Stack:** Go（modernc sqlite、chi、nftables/tc 外呼、golang.org/x/time/rate）、React/Vite 前端。

**Spec:** `docs/superpowers/specs/2026-07-02-per-grant-rate-limit-design.md`

## Global Constraints

- 限速单位一律 **MB/s**，1 MB = 1048576 字节；任何面向用户的字段/文案不得出现 bit 单位（Mbps）。
- `rate_limit_mbytes = 0` 表示不限速；注意与 `traffic_quota_bytes = 0`（回退全局配额）语义不同，UI 文案区分。
- 组 mark 方案唯一来源是 `nft.GroupShapeMark`（0x10000 | shape_group）；有效组条件 = `ShapeGroup > 0 && RateMBytes > 0 && ShapeGroup <= 0xFFFF`，kernel 与 userspace 判断必须一致（userspace 无 16 位限制，条件为 `ShapeGroup > 0 && RateMBytes > 0`）。
- 兼容不变量：`shape_group` 有效时 agent 忽略 `bandwidth_mbps`；仅有 `bandwidth_mbps`（旧 server）时保留既有每规则、仅上行的限速行为。
- 代码注释、KDoc、commit message 中禁止出现任务/步骤编号、方案代号、审阅轮次等过程性元信息；注释只解释 WHY 和 invariant。
- 每个 Task 结束时 `go build ./... && go test ./internal/...` 必须通过再 commit。

---

### Task 1: DB 迁移与 grant 数据层

**Files:**
- Create: `internal/db/migrations/0027_per_grant_rate_limit.sql`
- Modify: `internal/db/grants.go`
- Test: `internal/db/grants_rate_test.go`（新建）

**Interfaces:**
- Produces: `db.UserNode.RateLimitMBytes int64`（json `rate_limit_mbytes`）；`db.GrantShape{GrantID, RateLimitMBytes int64}`；`db.GrantShapes(d *sql.DB) (map[[2]int64]GrantShape, error)`（key = `[2]int64{userID, nodeID}`，只含 rate>0 的 grant）。
- 注意：`db.GrantNode` 签名**不变**（约 30 处测试调用），其 upsert 不触碰 `rate_limit_mbytes`（新列 INSERT 走 DEFAULT 0，ON CONFLICT 不覆盖）——限速只经专用 UPDATE 路径写入。

- [ ] **Step 1: 写迁移文件**

`internal/db/migrations/0027_per_grant_rate_limit.sql`：

```sql
-- Per-grant (user+node) shared rate limit in MB/s (0 = unlimited). Shaping
-- policy lives on the grant, like the traffic quota: all rules priced by one
-- grant share a single bucket. rules.bandwidth_mbps stays as a dead column
-- (dropping needs a table rebuild) and is zeroed so stale per-rule caps cannot
-- leak back through old code paths.
ALTER TABLE user_nodes ADD COLUMN rate_limit_mbytes INTEGER NOT NULL DEFAULT 0;
UPDATE rules SET bandwidth_mbps = 0;
```

（迁移由 `//go:embed migrations/*.sql` 自动发现，无需注册。）

- [ ] **Step 2: 写失败测试**

`internal/db/grants_rate_test.go`：

```go
package db

import "testing"

func TestGrantRateLimitRoundTrip(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "rl")
	grantNode(t, d, uid, nid)

	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=10 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.RateLimitMBytes != 10 {
		t.Fatalf("rate = %d, want 10", g.RateLimitMBytes)
	}

	shapes, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := shapes[[2]int64{uid, nid}]
	if !ok || s.RateLimitMBytes != 10 || s.GrantID <= 0 {
		t.Fatalf("shape = %+v ok=%v, want rate 10 with positive grant id", s, ok)
	}

	// GrantNode upsert must not touch the rate and must keep the rowid stable
	// (the rowid is the shaping group id; churning it would orphan connmarks).
	if err := GrantNode(d, uid, nid, 5, 0); err != nil {
		t.Fatal(err)
	}
	shapes2, _ := GrantShapes(d)
	s2 := shapes2[[2]int64{uid, nid}]
	if s2.GrantID != s.GrantID || s2.RateLimitMBytes != 10 {
		t.Fatalf("after upsert shape = %+v, want unchanged %+v", s2, s)
	}
}

func TestGrantShapesSkipsUnlimited(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "rl0")
	grantNode(t, d, uid, nid)

	shapes, err := GrantShapes(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shapes[[2]int64{uid, nid}]; ok {
		t.Fatal("rate 0 grant must not appear in GrantShapes")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/db/ -run TestGrantRateLimit -v`
Expected: FAIL（`RateLimitMBytes`/`GrantShapes` 未定义，编译错误）

- [ ] **Step 4: 实现**

`internal/db/grants.go`：

UserNode 加字段（在 `TrafficUsedBytes` 后）：

```go
	// RateLimitMBytes is the per-grant shared rate limit in MB/s; all of the
	// user's rules on this node share one bucket. 0 = unlimited (unlike the
	// quota, there is no user-level fallback).
	RateLimitMBytes int64 `json:"rate_limit_mbytes"`
```

同步所有 SELECT/Scan（该文件多处内联 scan，漏改会静默错位）：

- `GetNodeGrant`：SELECT 列表在 `traffic_used_bytes` 后加 `rate_limit_mbytes`；Scan 在 `&g.TrafficUsedBytes` 后加 `&g.RateLimitMBytes`。
- `scanUserNode`：Scan 同上（`ListAllGrants` 的 SQL 也要加列，顺序与 scanUserNode 一致）。
- `ListNodesForUser`：SELECT 的 `g.traffic_used_bytes` 后加 `g.rate_limit_mbytes`；rows.Scan 的 `&g.TrafficUsedBytes` 后加 `&g.RateLimitMBytes`（在 `&g.GrantedAt` 前）。
- `ListUsersForNode`：三处匿名 struct 各加 `RateLimitMBytes int64 \`json:"rate_limit_mbytes"\``（放 `TrafficUsedBytes` 后），SQL 加 `g.rate_limit_mbytes`，Scan 加 `&r.RateLimitMBytes`。

文件末尾新增：

```go
// GrantShape carries the shaping identity and limit of one rate-limited grant.
// GrantID is the user_nodes rowid: stable across upserts, so agents can use it
// as the connmark-backed shaping group id. A revoke+regrant creates a new row
// and therefore a new group — existing connections fall back to the default
// class until they reconnect, which is the intended semantics for a new grant.
type GrantShape struct {
	GrantID         int64
	RateLimitMBytes int64
}

// GrantShapes returns every rate-limited grant keyed by {user_id, node_id}.
// Unlimited grants (rate 0) are omitted so callers can treat absence as "no
// shaping".
func GrantShapes(d *sql.DB) (map[[2]int64]GrantShape, error) {
	rows, err := d.Query(`SELECT rowid, user_id, node_id, rate_limit_mbytes FROM user_nodes WHERE rate_limit_mbytes > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]GrantShape{}
	for rows.Next() {
		var gs GrantShape
		var uid, nid int64
		if err := rows.Scan(&gs.GrantID, &uid, &nid, &gs.RateLimitMBytes); err != nil {
			return nil, err
		}
		out[[2]int64{uid, nid}] = gs
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/db/ -v`
Expected: 全部 PASS（含既有测试——scan 对齐错误会在这里暴露）

- [ ] **Step 6: Commit**

```bash
git add internal/db/migrations/0027_per_grant_rate_limit.sql internal/db/grants.go internal/db/grants_rate_test.go
git commit -m "feat(db): per-grant rate limit column and shaping-group lookup"
```

---

### Task 2: 服务端 API——新增 grant 限速端点，移除规则限速端点

**Files:**
- Modify: `internal/server/server.go`（路由：删 `/rules/{id}/bandwidth`，加 `/users/{id}/nodes/{nodeID}/rate-limit`）
- Modify: `internal/server/api.go`（删 `apiSetRuleBandwidth`；加 `apiSetPerNodeRateLimit`；`apiBatchApplyGrants` 扩展）
- Modify: `internal/db/queries.go`（删 `SetRuleBandwidth` 及其注释）
- Delete+Create: `internal/server/bandwidth_test.go` → 删除旧测试；新建 `internal/server/grant_rate_api_test.go`

**Interfaces:**
- Consumes: Task 1 的 `rate_limit_mbytes` 列。
- Produces: `POST /api/users/{id}/nodes/{nodeID}/rate-limit`，body `{"rate_limit_mbytes": int64}`，admin 权限（路由放在与 `/quota` 相同的 admin 组内，server.go:415 旁）；`/grants/batch-apply` 的 grants 元素接受 `rate_limit_mbytes`。
- 注意：`db.Rule.BandwidthMbps` 字段与 `buildRules` 里对它的引用（server.go:303）**本 Task 不动**（Task 3 一并处理，保证每个 Task 可编译）。

- [ ] **Step 1: 写失败测试**

`internal/server/grant_rate_api_test.go`：

```go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestAPISetPerNodeRateLimit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rlnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"rate_limit_mbytes": 10})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/rate-limit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.RateLimitMBytes != 10 {
		t.Fatalf("want 10, got %d", g.RateLimitMBytes)
	}

	// negative is rejected
	body, _ = json.Marshal(map[string]any{"rate_limit_mbytes": -1})
	req = httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/rate-limit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative rate: want 400, got %d", rec.Code)
	}
}

func TestAPIRuleBandwidthEndpointRemoved(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/rules/1/bandwidth", bytes.NewReader([]byte(`{"bandwidth_mbps":5}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for removed endpoint, got %d", rec.Code)
	}
}

func TestAPIBatchApplyGrantsRateLimit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rlbatch", "", "")
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	payload := map[string]any{
		"user_ids": []int64{uid},
		"grants": []map[string]any{
			{"node_name": "rlbatch", "max_forwards": 5, "traffic_quota_bytes": 0, "rate_limit_mbytes": 7},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/api/grants/batch-apply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	g, err := db.GetNodeGrant(d, uid, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.RateLimitMBytes != 7 {
		t.Fatalf("want 7, got %d", g.RateLimitMBytes)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestAPISetPerNodeRateLimit|TestAPIRuleBandwidthEndpointRemoved|TestAPIBatchApplyGrantsRateLimit' -v`
Expected: FAIL（新端点 404、旧端点仍 200、batch 忽略 rate 字段）

- [ ] **Step 3: 实现**

`internal/server/server.go`：删除 `r.Post("/rules/{id}/bandwidth", s.apiSetRuleBandwidth)`（约 400 行）；在 `r.Post("/users/{id}/nodes/{nodeID}/quota", s.apiSetPerNodeQuota)`（约 415 行）后加：

```go
			r.Post("/users/{id}/nodes/{nodeID}/rate-limit", s.apiSetPerNodeRateLimit)
```

`internal/server/api.go`：整体删除 `apiSetRuleBandwidth`（约 1555-1583 行，连同其注释）；在 `apiSetPerNodeQuota` 之后加：

```go
// apiSetPerNodeRateLimit sets the grant's shared rate limit (MB/s, 0 =
// unlimited) and re-dispatches every node carrying the grant's rule hops so
// the data plane picks up the new shaping.
func (s *Server) apiSetPerNodeRateLimit(w http.ResponseWriter, r *http.Request) {
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
		RateLimitMBytes int64 `json:"rate_limit_mbytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.RateLimitMBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "限速不能为负")
		return
	}
	if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`,
		body.RateLimitMBytes, userID, nodeID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	affected, _ := db.RulesAffectedByNode(s.DB, userID, nodeID)
	for _, n := range affected {
		_ = s.dispatchToNode(n)
	}
	db.WriteAudit(s.DB, u.ID, "user.set_node_rate_limit", strconv.FormatInt(userID, 10),
		fmt.Sprintf("node=%d mbytes=%d", nodeID, body.RateLimitMBytes))
	jsonOK(w, map[string]any{"ok": true})
}
```

`apiBatchApplyGrants`（约 2860-2908 行）：grants 元素 struct 加字段：

```go
			RateLimitMBytes   int64  `json:"rate_limit_mbytes"`
```

内层 `db.GrantNode` 成功后（WriteAudit 之前）加：

```go
			mb := g.RateLimitMBytes
			if mb < 0 {
				mb = 0
			}
			if _, err := s.DB.Exec(`UPDATE user_nodes SET rate_limit_mbytes=? WHERE user_id=? AND node_id=?`, mb, uid, nid); err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
```

循环结束后、`jsonOK` 之前，对涉及节点重下发（批量授权常在建规则之前，多数是 no-op，但改已有 grant 的限速必须立即生效）：

```go
	affected := map[int64]bool{}
	for _, g := range body.Grants {
		nid, ok := nameToID[g.NodeName]
		if !ok {
			continue
		}
		for _, uid := range body.UserIDs {
			ns, err := db.RulesAffectedByNode(s.DB, uid, nid)
			if err != nil {
				continue
			}
			for _, n := range ns {
				affected[n] = true
			}
		}
	}
	nodeIDs := make([]int64, 0, len(affected))
	for n := range affected {
		nodeIDs = append(nodeIDs, n)
	}
	s.apiDispatchFanout(nodeIDs)
```

`internal/db/queries.go`：整体删除 `SetRuleBandwidth`（约 575-586 行，连同注释）。

删除文件 `internal/server/bandwidth_test.go`（其测试对象已不存在；buildRules 的替代测试在 Task 3 建立）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go build ./... && go test ./internal/server/ ./internal/db/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A internal/server internal/db/queries.go
git commit -m "feat(server): grant-level rate limit endpoint replaces per-rule bandwidth"
```

---

### Task 3: 下发协议——nft.Rule 分组字段与 buildRules

**Files:**
- Modify: `internal/nft/nft.go`（Rule 加字段 + `GroupShapeMark` 辅助）
- Modify: `internal/server/server.go`（`buildRules` 改填分组字段）
- Modify: `internal/db/queries.go`（`ruleCols`/`scanRule` 去掉 `bandwidth_mbps`；删 `Rule.BandwidthMbps` 字段，位于 queries.go:94-96）
- Test: `internal/server/grant_rate_buildrules_test.go`（新建）、`internal/wsproto/messages_test.go`（追加）、`internal/nft/nft_test.go`（追加 GroupShapeMark 用例）

**Interfaces:**
- Consumes: `db.GrantShapes`（Task 1）。
- Produces: `nft.Rule.ShapeGroup int64`（json `shape_group,omitempty`）、`nft.Rule.RateMBytes int`（json `rate_mbytes,omitempty`）、`nft.GroupShapeMark(r Rule) uint32`（无效组返回 0）。wire 上 `bandwidth_mbps` 字段保留（`nft.Rule.BandwidthMbps` 不删），由 buildRules 用等效 Mbit 值填充。

- [ ] **Step 1: 写失败测试**

`internal/nft/nft_test.go` 追加：

```go
func TestGroupShapeMark(t *testing.T) {
	cases := []struct {
		r    Rule
		want uint32
	}{
		{Rule{ShapeGroup: 5, RateMBytes: 10}, 0x10005},
		{Rule{ShapeGroup: 0, RateMBytes: 10}, 0},
		{Rule{ShapeGroup: 5, RateMBytes: 0}, 0},
		{Rule{ShapeGroup: 0x10000, RateMBytes: 10}, 0}, // minor is 16-bit; oversize falls back to legacy shaping
	}
	for i, c := range cases {
		if got := GroupShapeMark(c.r); got != c.want {
			t.Errorf("case %d: mark = %#x, want %#x", i, got, c.want)
		}
	}
}
```

`internal/wsproto/messages_test.go` 追加：

```go
func TestApplyRulesetShapeFieldsRoundtrip(t *testing.T) {
	ar := ApplyRuleset{
		Rev: "r1",
		Rules: []nft.Rule{
			{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, ShapeGroup: 7, RateMBytes: 12},
		},
	}
	b, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	var got ApplyRuleset
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Rules[0].ShapeGroup != 7 || got.Rules[0].RateMBytes != 12 {
		t.Fatalf("shape fields roundtrip mismatch: %+v", got.Rules[0])
	}

	// omitempty: unshaped rules must not emit the new keys, keeping payloads
	// byte-identical for old panels/agents.
	b2, _ := json.Marshal(ApplyRuleset{Rules: []nft.Rule{{Proto: "tcp", SrcPort: 1, DestIP: "1.1.1.1", DestPort: 1}}})
	if strings.Contains(string(b2), "shape_group") || strings.Contains(string(b2), "rate_mbytes") {
		t.Fatalf("zero-value rule leaked shape keys: %s", b2)
	}
}
```

（`messages_test.go` 需要 import `strings`。）

`internal/server/grant_rate_buildrules_test.go`：

```go
package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
)

// The grant's rate limit reaches the data plane: every rule priced by the
// grant carries the shaping group + MB/s rate, plus the legacy Mbit mirror for
// pre-group agents. Ownerless rules stay unshaped.
func TestGrantRateLimitPropagatesToRules(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rl-node", "https://p", "s")
	db.GrantNode(d, uid, n.ID, 10, 0)
	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=10 WHERE user_id=? AND node_id=?`, uid, n.ID); err != nil {
		t.Fatal(err)
	}
	owned, _ := createStandaloneRuleHop(t, d, n.ID, "tcp", 0, "10.0.0.9", 9000, sql.NullInt64{Int64: uid, Valid: true})
	orphan, _ := createStandaloneRuleHop(t, d, n.ID, "tcp", 0, "10.0.0.9", 9001, sql.NullInt64{})

	ruleHops, _ := db.ActiveRuleHopsForPush(d, n.ID)
	rules := buildRules(d, ruleHops)

	var foundOwned, foundOrphan bool
	for _, r := range rules {
		switch r.RuleID {
		case owned:
			foundOwned = true
			if r.ShapeGroup <= 0 || r.RateMBytes != 10 {
				t.Errorf("owned rule shape = group %d rate %d, want positive group rate 10", r.ShapeGroup, r.RateMBytes)
			}
			// 10 MB/s (2^20 bytes) ≈ 84 Mbit/s for the legacy mirror.
			if r.BandwidthMbps != 84 {
				t.Errorf("legacy mirror = %d Mbit, want 84", r.BandwidthMbps)
			}
		case orphan:
			foundOrphan = true
			if r.ShapeGroup != 0 || r.RateMBytes != 0 || r.BandwidthMbps != 0 {
				t.Errorf("ownerless rule must be unshaped, got %+v", r)
			}
		}
	}
	if !foundOwned || !foundOrphan {
		t.Fatalf("rules missing from built set: owned=%v orphan=%v", foundOwned, foundOrphan)
	}
}

// Shape fields are data plane state: changing the rate must change the rev so
// reconnecting agents are not skipped by the rev short-circuit.
func TestComputeRevIncludesShapeFields(t *testing.T) {
	base := []nft.Rule{{Proto: "tcp", SrcPort: 1, DestIP: "1.1.1.1", DestPort: 1}}
	shaped := []nft.Rule{{Proto: "tcp", SrcPort: 1, DestIP: "1.1.1.1", DestPort: 1, ShapeGroup: 3, RateMBytes: 10}}
	if computeRev(base) == computeRev(shaped) {
		t.Fatal("rev must differ when shape fields differ")
	}
}
```

（该文件需要 import `nft-forward/internal/nft`；`createStandaloneRuleHop` 的返回值/签名以 `shared_test.go` 中的定义为准，若第二返回值不是 error 需相应调整。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/ ./internal/wsproto/ ./internal/server/ -run 'GroupShapeMark|ShapeFields|GrantRateLimitPropagates' -v`
Expected: FAIL（字段未定义，编译错误）

- [ ] **Step 3: 实现**

`internal/nft/nft.go`，Rule struct 在 `BandwidthMbps` 之后加：

```go
	// ShapeGroup/RateMBytes carry the per-grant shared rate limit: every rule
	// in the same group (one user's rules on one panel node, priced by one
	// grant) shares a single RateMBytes MB/s bucket, both directions combined.
	// ShapeGroup is the panel-side grant id; 0 = no group. When the group is
	// valid the data plane ignores the legacy per-rule BandwidthMbps, which
	// new panels still fill so pre-group agents degrade to an approximate
	// per-rule cap.
	ShapeGroup int64 `json:"shape_group,omitempty"`
	RateMBytes int   `json:"rate_mbytes,omitempty"`
```

同文件（EffectiveMode 之后）加：

```go
// GroupShapeMark returns the fwmark for a validly group-shaped rule, or 0.
// The 0x10000 offset keeps group marks disjoint from legacy per-port marks
// (ports are ≤ 0xFFFF). Groups whose id exceeds 16 bits cannot become a tc
// class minor and fall back to legacy shaping — callers must treat 0 as "not
// group-shaped", never as "unshaped".
func GroupShapeMark(r Rule) uint32 {
	if r.ShapeGroup > 0 && r.RateMBytes > 0 && r.ShapeGroup <= 0xFFFF {
		return uint32(0x10000 | r.ShapeGroup)
	}
	return 0
}
```

`internal/server/server.go` `buildRules`：函数开头预加载处（`hopCounts` 之后）加：

```go
	shapes, _ := db.GrantShapes(d)
```

把 `if r := ruleMap[rh.RuleID]; r != nil { ... }` 块改为：

```go
		if r := ruleMap[rh.RuleID]; r != nil {
			rule.RuleID = r.ID
			rule.RuleName = r.Name
			if r.OwnerID.Valid {
				if u := users[r.OwnerID.Int64]; u != nil {
					rule.OwnerName = u.Username
				}
				// Shaping is priced by the grant on the rule's panel node —
				// the same node the quota is tracked on — then applied at
				// every hop.
				if gs, ok := shapes[[2]int64{r.OwnerID.Int64, r.NodeID}]; ok {
					rule.ShapeGroup = gs.GrantID
					rule.RateMBytes = int(gs.RateLimitMBytes)
					// Legacy mirror so pre-group agents still shape
					// (per rule, approximate): MB/s (2^20 bytes) → Mbit/s.
					rule.BandwidthMbps = int((gs.RateLimitMBytes*8388608 + 500000) / 1000000)
				}
			}
		}
```

（原 `rule.BandwidthMbps = r.BandwidthMbps` 行删除。）

`internal/db/queries.go`：删除 `Rule` struct 的 `BandwidthMbps` 字段（94-96 行）；`ruleCols` 去掉 `bandwidth_mbps`；`scanRule` 去掉 `&rl.BandwidthMbps`。`ruleCols` 上方注释补一句：

```go
// bandwidth_mbps is likewise dead (shaping moved to the per-grant rate limit
// on user_nodes) and stays out of the projection.
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go build ./... && go test ./internal/...`
Expected: PASS（若有其他测试引用 `db.Rule.BandwidthMbps`，一并删除该引用）

- [ ] **Step 5: Commit**

```bash
git add internal/nft/nft.go internal/server internal/db/queries.go internal/wsproto/messages_test.go
git commit -m "feat(proto): rules carry grant shaping group and MB/s rate"
```

---

### Task 4: userspace 组限速——共享令牌桶、双向合计

**Files:**
- Modify: `internal/forward/relay.go`（burst 计算辅助）
- Modify: `internal/forward/userspace.go`（listener 加 `limDown`；backend 加组注册表）
- Test: `internal/forward/userspace_test.go`（追加）

**Interfaces:**
- Consumes: `nft.Rule.ShapeGroup`/`RateMBytes`（Task 3）。
- Produces: `userspaceBackend.groups map[int64]*rate.Limiter`（跨 Reconcile 稳定，SetLimit 热更）；`(b *userspaceBackend) setLimits(l *listener, r nft.Rule)`。语义：组内所有 listener 上下行共用一个桶；legacy（无组、有 `BandwidthMbps`）保持每 listener 独立桶、仅上行。

- [ ] **Step 1: 写失败测试**

`internal/forward/userspace_test.go` 追加（沿用文件内既有的 `echoServer`/`freePort` 辅助）：

```go
// Two listeners in one shape group share a single bidirectional bucket: the
// echoed traffic of both ports combined is paced by one 1 MB/s limiter. With
// independent buckets the transfer would finish several times faster.
func TestUserspace_GroupSharedBucketPacesAggregate(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	p1, p2 := freePort(t), freePort(t)
	be := newUserspaceBackend()
	defer be.Close()

	rules := []nft.Rule{
		{ID: "g1", Proto: "tcp", SrcPort: p1, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace, ShapeGroup: 9, RateMBytes: 1},
		{ID: "g2", Proto: "tcp", SrcPort: p2, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace, ShapeGroup: 9, RateMBytes: 1},
	}
	if err := be.Reconcile(rules); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 1 MB payload per port; echo doubles it: 4 MB total through a 1 MB/s
	// bucket with a 1 MB burst → ≥ ~3s. Assert a loose lower bound.
	const per = 1 << 20
	start := time.Now()
	var wg sync.WaitGroup
	for _, p := range []int{p1, p2} {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
			if err != nil {
				t.Errorf("dial %d: %v", port, err)
				return
			}
			defer conn.Close()
			go conn.Write(make([]byte, per))
			if _, err := io.ReadFull(conn, make([]byte, per)); err != nil {
				t.Errorf("read back %d: %v", port, err)
			}
		}(p)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed < 2*time.Second {
		t.Fatalf("aggregate transfer too fast for a shared 1 MB/s bucket: %v", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("transfer absurdly slow: %v", elapsed)
	}
}

// A rate change hot-updates the shared bucket without restarting listeners:
// an established connection keeps working across Reconcile.
func TestUserspace_GroupRateHotUpdate(t *testing.T) {
	upstreamAddr, stop := echoServer(t)
	defer stop()
	host, portStr, _ := net.SplitHostPort(upstreamAddr)
	upPort, _ := strconv.Atoi(portStr)

	listen := freePort(t)
	be := newUserspaceBackend()
	defer be.Close()

	mk := func(mb int) []nft.Rule {
		return []nft.Rule{{ID: "g", Proto: "tcp", SrcPort: listen, DestIP: host, DestPort: upPort, Mode: nft.ModeUserspace, ShapeGroup: 3, RateMBytes: mb}}
	}
	if err := be.Reconcile(mk(1)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listen), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := echoOnce(conn); err != nil {
		t.Fatalf("echo before update: %v", err)
	}
	if err := be.Reconcile(mk(50)); err != nil {
		t.Fatalf("reconcile update: %v", err)
	}
	if err := echoOnce(conn); err != nil {
		t.Fatalf("echo after update (listener must not restart): %v", err)
	}
}
```

若文件里没有 `echoOnce` 辅助则一并添加：

```go
// echoOnce asserts the relay still forwards by writing a small payload and
// reading the echo back on the same connection.
func echoOnce(conn net.Conn) error {
	msg := []byte("ping")
	if _, err := conn.Write(msg); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf, msg) {
		return fmt.Errorf("echo mismatch: %q", buf)
	}
	return nil
}
```

（需要 import `bytes`、`sync`，若尚未引入。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/forward/ -run TestUserspace_Group -v`
Expected: FAIL（`ShapeGroup` 规则被当作 legacy 处理，聚合测试过快完成）

- [ ] **Step 3: 实现**

`internal/forward/relay.go`，`makeLimiter` 后加：

```go
// groupBurst sizes a shared bucket's burst: one second of quota, floored at
// the copy buffer so a single WaitN can never exceed the burst.
func groupBurst(bytesPerSec float64) int {
	burst := int(bytesPerSec)
	if burst < relayBufSize {
		burst = relayBufSize
	}
	return burst
}
```

`internal/forward/userspace.go`：

listener struct 的 `lim` 后加：

```go
	// limDown mirrors lim for the upstream→client direction. Group-shaped
	// rules point both at the same shared limiter (the cap is the combined
	// two-way total); legacy per-rule caps leave it nil (download unshaped).
	limDown atomic.Pointer[rate.Limiter]
```

`handle()` 的回程改为：

```go
	go func() {
		relayCopy(l.ctx, client, upstream, &l.limDown, &l.bytesDown)
		halfCloseWrite(client)
		done <- struct{}{}
	}()
```

（注释 `// Return path (upstream→client): counted but unshaped.` 改为 `// Return path (upstream→client): counted; shaped only under a group bucket.`）

`openListener` 中删除 `l.lim.Store(makeLimiter(r.BandwidthMbps))`（limiter 统一由 Reconcile 的 setLimits 写入）。

backend struct 加字段并初始化：

```go
type userspaceBackend struct {
	mu        sync.Mutex
	listeners map[int]*listener
	// groups holds one shared limiter per shape group, stable across
	// Reconcile calls: rate changes SetLimit the existing limiter instead of
	// replacing it, so bucket state (accumulated debt) survives a re-apply.
	groups   map[int64]*rate.Limiter
	poolSize int
}

func newUserspaceBackend() *userspaceBackend {
	return &userspaceBackend{listeners: map[int]*listener{}, groups: map[int64]*rate.Limiter{}, poolSize: envPoolSize()}
}
```

`Reconcile` 在构建 `desired` map 之后、开监听循环之前加组同步：

```go
	desiredGroups := map[int64]int{}
	for _, r := range rules {
		if r.ShapeGroup > 0 && r.RateMBytes > 0 {
			desiredGroups[r.ShapeGroup] = r.RateMBytes
		}
	}
	for sg, mb := range desiredGroups {
		bytesPerSec := float64(mb) * 1048576
		if lim, ok := b.groups[sg]; ok {
			lim.SetLimit(rate.Limit(bytesPerSec))
			lim.SetBurst(groupBurst(bytesPerSec))
		} else {
			b.groups[sg] = rate.NewLimiter(rate.Limit(bytesPerSec), groupBurst(bytesPerSec))
		}
	}
	for sg := range b.groups {
		if _, ok := desiredGroups[sg]; !ok {
			delete(b.groups, sg)
		}
	}
```

热更新循环里把 `l.lim.Store(makeLimiter(r.BandwidthMbps))` 替换为 `b.setLimits(l, r)`，并新增方法：

```go
// setLimits points a listener's limiters at the right bucket: group-shaped
// rules share the group's bidirectional limiter; legacy per-rule caps (from
// pre-group panels) keep their historical semantics — a private bucket, upload
// only.
func (b *userspaceBackend) setLimits(l *listener, r nft.Rule) {
	if r.ShapeGroup > 0 && r.RateMBytes > 0 {
		g := b.groups[r.ShapeGroup]
		l.lim.Store(g)
		l.limDown.Store(g)
		return
	}
	l.lim.Store(makeLimiter(r.BandwidthMbps))
	l.limDown.Store(nil)
}
```

`Close()` 末尾加 `b.groups = map[int64]*rate.Limiter{}`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/forward/ -v`
Expected: 全部 PASS（既有 `TestUserspace_RateLimitPaces` 等 legacy 测试必须仍绿）

- [ ] **Step 5: Commit**

```bash
git add internal/forward
git commit -m "feat(forward): shared bidirectional group buckets in the userspace relay"
```

---

### Task 5: kernel nft 渲染——connmark 首包打标 + 每包恢复

**Files:**
- Modify: `internal/nft/nft.go`（`RenderRuleset`）
- Test: `internal/nft/nft_test.go`（追加）

**Interfaces:**
- Consumes: `GroupShapeMark`（Task 3）。
- Produces: 组规则的 DNAT 行带前缀 `meta mark set 0x1XXXX ct mark set meta mark `；存在有效组时渲染 `restore_mark` 链（filter/prerouting/mangle，`ct mark != 0 meta mark set ct mark`）。legacy 规则维持 `meta mark set <port> `。

- [ ] **Step 1: 写失败测试**

`internal/nft/nft_test.go` 追加：

```go
func TestRenderRuleset_GroupShaping(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
	})
	// First packet: stamp the packet and store the mark on the conntrack
	// entry in one DNAT rule.
	if !strings.Contains(out, "meta mark set 0x10005 ct mark set meta mark dnat ip to 10.0.0.2:80") {
		t.Fatalf("missing group mark on DNAT rule:\n%s", out)
	}
	// Every later packet, both directions: restore the mark before routing so
	// tc's egress fw filter classifies the whole connection.
	if !strings.Contains(out, "chain restore_mark") ||
		!strings.Contains(out, "type filter hook prerouting priority mangle; policy accept;") ||
		!strings.Contains(out, "ct mark != 0 meta mark set ct mark") {
		t.Fatalf("missing restore_mark chain:\n%s", out)
	}
}

func TestRenderRuleset_LegacyPortMark(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, BandwidthMbps: 50},
	})
	if !strings.Contains(out, "meta mark set 8080 dnat ip to 10.0.0.2:80") {
		t.Fatalf("legacy per-port mark missing:\n%s", out)
	}
	if strings.Contains(out, "restore_mark") {
		t.Fatalf("legacy-only ruleset must not emit the restore chain:\n%s", out)
	}
}

func TestRenderRuleset_GroupOverridesLegacyMirror(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10, BandwidthMbps: 84},
	})
	if strings.Contains(out, "meta mark set 8080 ") {
		t.Fatalf("group-shaped rule must not also carry the legacy port mark:\n%s", out)
	}
}

func TestRenderRuleset_OversizeGroupFallsBackToLegacy(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 0x10000, RateMBytes: 10, BandwidthMbps: 84},
	})
	if !strings.Contains(out, "meta mark set 8080 ") {
		t.Fatalf("oversize group must fall back to the legacy port mark:\n%s", out)
	}
	if strings.Contains(out, "restore_mark") {
		t.Fatalf("no valid group → no restore chain:\n%s", out)
	}
}
```

（`nft_test.go` 需要 import `strings`，若尚未引入。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/nft/ -run TestRenderRuleset -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`internal/nft/nft.go` `RenderRuleset`：mark 生成改为：

```go
		mark := ""
		if m := GroupShapeMark(r); m != 0 {
			// Stamp the first packet and its conntrack entry in one go; the
			// restore_mark chain re-stamps every later packet (both
			// directions) so the whole connection lands in the group's tc
			// class. nat prerouting only sees a connection's first packet,
			// which is why the mark must be persisted via ct mark.
			mark = fmt.Sprintf("meta mark set 0x%x ct mark set meta mark ", m)
		} else if r.BandwidthMbps > 0 {
			mark = fmt.Sprintf("meta mark set %d ", r.SrcPort)
		}
```

在 prerouting 链渲染完成后（`b.WriteString("\t}\n")` 之后、postrouting 之前）加：

```go
	hasGroup := false
	for _, r := range rules {
		if GroupShapeMark(r) != 0 {
			hasGroup = true
			break
		}
	}
	if hasGroup {
		b.WriteString("\tchain restore_mark {\n")
		b.WriteString("\t\ttype filter hook prerouting priority mangle; policy accept;\n")
		b.WriteString("\t\tct mark != 0 meta mark set ct mark\n")
		b.WriteString("\t}\n")
	}
```

（`hasGroup` 的探测放到函数开头与 `hasLoopback` 并列亦可，保持一次遍历。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/nft/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/nft
git commit -m "feat(nft): connmark-backed group marks so shaping sees every packet both ways"
```

---

### Task 6: tc——每组一个 HTB class

**Files:**
- Modify: `internal/tc/tc.go`
- Test: `internal/tc/tc_test.go`（追加）

**Interfaces:**
- Consumes: `nft.GroupShapeMark`。
- Produces: `planClasses(rules []nft.Rule) []shapeClass`（纯函数，`shapeClass{ClassID, Rate, Handle string}`，按 ClassID 排序去重）；`Apply` 消费它。组 class：`1:<shape_group hex>`、rate=`<MB/s×8388608>bit`、handle=`0x1XXXX`；legacy class 维持 `1:<port hex>`、`<N>mbit`、handle=`0x<port hex>`；组占用的 minor 与 legacy 端口 minor 冲突时组优先。

- [ ] **Step 1: 写失败测试**

`internal/tc/tc_test.go` 追加：

```go
func TestPlanClasses(t *testing.T) {
	rules := []nft.Rule{
		// Two rules in one group produce exactly one class.
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
		{Proto: "tcp", SrcPort: 8081, DestIP: "10.0.0.3", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
		// Legacy per-port cap from a pre-group panel.
		{Proto: "tcp", SrcPort: 9000, DestIP: "10.0.0.4", DestPort: 80, BandwidthMbps: 50},
		// Group-shaped rules must not leak a legacy class from the mirror value.
		{Proto: "tcp", SrcPort: 9001, DestIP: "10.0.0.5", DestPort: 80, ShapeGroup: 6, RateMBytes: 2, BandwidthMbps: 17},
		// Unshaped.
		{Proto: "tcp", SrcPort: 9002, DestIP: "10.0.0.6", DestPort: 80},
	}
	got := planClasses(rules)
	// Sorted lexicographically by ClassID ("1:2328" < "1:5" < "1:6").
	want := []shapeClass{
		{ClassID: "1:2328", Rate: "50mbit", Handle: "0x2328"},
		{ClassID: "1:5", Rate: "83886080bit", Handle: "0x10005"},
		{ClassID: "1:6", Rate: "16777216bit", Handle: "0x10006"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planClasses:\n got %+v\nwant %+v", got, want)
	}
}

func TestPlanClasses_GroupWinsMinorCollision(t *testing.T) {
	// A legacy port whose hex minor equals a group id would collide in the
	// class-id space; the group keeps the minor, the legacy class is dropped.
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 5, DestIP: "10.0.0.2", DestPort: 80, BandwidthMbps: 50},
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.3", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
	}
	got := planClasses(rules)
	want := []shapeClass{
		{ClassID: "1:5", Rate: "83886080bit", Handle: "0x10005"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planClasses:\n got %+v\nwant %+v", got, want)
	}
}
```

（`tc_test.go` 需要 import `reflect` 与 `nft-forward/internal/nft`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tc/ -v`
Expected: FAIL（planClasses 未定义）

- [ ] **Step 3: 实现**

`internal/tc/tc.go`：

```go
// shapeClass is one HTB leaf: a class and the fw-mark filter feeding it.
type shapeClass struct {
	ClassID string // "1:<minor hex>"
	Rate    string // tc rate expression, rate == ceil
	Handle  string // fw mark the filter matches, "0x..."
}

// planClasses derives the HTB leaves from the ruleset: one class per shape
// group (minor = group id, mark carries the 0x10000 offset) plus one class per
// legacy per-port cap from pre-group panels. Group-shaped rules never spawn a
// legacy class from their mirror BandwidthMbps. When a legacy port's minor
// collides with a group id the group wins — the group is current policy, the
// port cap is a compatibility remnant. Output is sorted for determinism.
func planClasses(rules []nft.Rule) []shapeClass {
	groups := map[int64]int{}
	legacy := map[int]int{}
	for _, r := range rules {
		if nft.GroupShapeMark(r) != 0 {
			groups[r.ShapeGroup] = r.RateMBytes
		} else if r.BandwidthMbps > 0 {
			legacy[r.SrcPort] = r.BandwidthMbps
		}
	}
	out := make([]shapeClass, 0, len(groups)+len(legacy))
	for sg, mb := range groups {
		out = append(out, shapeClass{
			ClassID: fmt.Sprintf("1:%x", sg),
			// MB/s (2^20 bytes) expressed in exact bits so tc's own unit
			// parsing cannot skew the cap.
			Rate:   fmt.Sprintf("%dbit", int64(mb)*8388608),
			Handle: fmt.Sprintf("0x%x", 0x10000|sg),
		})
	}
	for port, mbps := range legacy {
		if _, taken := groups[int64(port)]; taken {
			continue
		}
		out = append(out, shapeClass{
			ClassID: fmt.Sprintf("1:%x", port),
			Rate:    fmt.Sprintf("%dmbit", mbps),
			Handle:  fmt.Sprintf("0x%x", port),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClassID < out[j].ClassID })
	return out
}
```

`Apply` 重写为消费 planClasses（保留签名与现有注释框架，注释更新为组语义）：

```go
func Apply(rules []nft.Rule, iface string) error {
	if iface == "" {
		return nil
	}
	classes := planClasses(rules)
	// Always tear down to keep state deterministic.
	_ = runIgnore("tc", "qdisc", "del", "dev", iface, "root")
	if len(classes) == 0 {
		return nil
	}

	if err := run("tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	// Default class — huge ceiling so unmarked traffic isn't throttled.
	if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", "1:1", "htb", "rate", "100gbit"); err != nil {
		return err
	}
	for _, c := range classes {
		if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", c.ClassID,
			"htb", "rate", c.Rate, "ceil", c.Rate); err != nil {
			return fmt.Errorf("class %s: %w", c.ClassID, err)
		}
		for _, proto := range []string{"ip", "ipv6"} {
			if err := run("tc", "filter", "add", "dev", iface, "parent", "1:", "protocol", proto,
				"handle", c.Handle, "fw", "classid", c.ClassID); err != nil {
				return fmt.Errorf("filter %s/%s: %w", proto, c.Handle, err)
			}
		}
	}
	return nil
}
```

（顶部注释块的 Layout 部分同步改写：`class 1:<group> — 每个 shape group 一个，rate=ceil；class 1:<port> — legacy per-rule cap`。import 加 `sort`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tc/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tc
git commit -m "feat(tc): one HTB class per shaping group, legacy per-port classes kept"
```

---

### Task 7: daemon standalone 降级清空分组字段

**Files:**
- Modify: `internal/daemon/daemon.go`（约 138-147 行的降级循环）
- Test: `internal/daemon/state_test.go`（追加）

**Interfaces:**
- Produces: `downgradePanelRule(r nft.Rule) nft.Rule`（从降级循环中提取的纯函数，daemon 包内私有）。

- [ ] **Step 1: 写失败测试**

`internal/daemon/state_test.go` 追加：

```go
// Standalone downgrade strips panel bindings. Group shaping is panel policy
// (priced by a grant), so it is dropped too; the legacy per-rule cap survives
// because it is self-contained.
func TestDowngradePanelRuleStripsPanelState(t *testing.T) {
	in := nft.Rule{
		ID: "p1", Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 8443,
		RuleID: 7, RuleName: "r", OwnerName: "alice", HopCount: 2,
		ShapeGroup: 5, RateMBytes: 10, BandwidthMbps: 84,
	}
	out := downgradePanelRule(in)
	if out.ID == "" || out.ID == in.ID {
		t.Fatalf("downgrade must assign a fresh local id, got %q", out.ID)
	}
	if out.RuleID != 0 || out.RuleName != "" || out.OwnerName != "" || out.HopCount != 0 {
		t.Fatalf("panel metadata must be cleared: %+v", out)
	}
	if out.ShapeGroup != 0 || out.RateMBytes != 0 {
		t.Fatalf("shape group fields must be cleared: %+v", out)
	}
	if out.BandwidthMbps != 84 {
		t.Fatalf("legacy cap must survive downgrade, got %d", out.BandwidthMbps)
	}
	if out.Proto != "tcp" || out.SrcPort != 443 || out.DestIP != "10.0.0.1" || out.DestPort != 8443 {
		t.Fatalf("forwarding fields must survive: %+v", out)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run TestDowngradePanelRule -v`
Expected: FAIL（函数未定义）

- [ ] **Step 3: 实现**

`internal/daemon/daemon.go`：新增函数（放在降级代码块附近）：

```go
// downgradePanelRule turns a panel-pushed rule into a standalone tui rule:
// fresh local id, panel metadata cleared. Group shaping is dropped with it —
// the limit is priced by a panel-side grant that no longer governs this
// daemon — while the self-contained legacy per-rule cap is kept.
func downgradePanelRule(r nft.Rule) nft.Rule {
	r.ID = nft.NewRuleID()
	r.RuleID = 0
	r.RuleName = ""
	r.OwnerName = ""
	r.HopCount = 0
	r.ShapeGroup = 0
	r.RateMBytes = 0
	return r
}
```

降级循环体改为：

```go
	if d.connectURL == "" && len(owners["panel"]) > 0 {
		downgraded := make([]nft.Rule, len(owners["panel"]))
		for i, r := range owners["panel"] {
			downgraded[i] = downgradePanelRule(r)
		}
		...
```

（其余逻辑不变。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/daemon/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "fix(daemon): standalone downgrade drops grant-priced shaping state"
```

---

### Task 8: Web UI——grant 限速编辑、粘贴格式、用户侧展示、移除规则限速

**Files:**
- Modify: `web/src/pages/users/Detail.jsx`（PerNodeRateForm + 表格列 + copyGrants）
- Modify: `web/src/pages/users/PasteGrantsModal.jsx`（解析/预览/提交）
- Modify: `web/src/pages/my/Dashboard.jsx`（只读限速列）
- Modify: `web/src/pages/rules/Detail.jsx`（删 BandwidthForm、限速卡片、信息区限速行）

**Interfaces:**
- Consumes: `POST /users/{uid}/nodes/{nid}/rate-limit {rate_limit_mbytes}`（Task 2）；grant JSON 的 `rate_limit_mbytes`（Task 1 自动带出）。
- Produces: 粘贴文本格式扩展为 `节点名 | max=N | quota=XGB | rate=N`（N 为整数 MB/s，缺省 0）。

- [ ] **Step 1: users/Detail.jsx**

`PerNodeQuotaForm` 之后加：

```jsx
function PerNodeRateForm({ userId, nodeId, rateMBytes, onDone }) {
  const [mb, setMb] = useState(String(rateMBytes || 0))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const n = Math.max(0, Math.round(Number(mb) || 0))
    try {
      await api.post(`/users/${userId}/nodes/${nodeId}/rate-limit`, { rate_limit_mbytes: n })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" value={mb}
        onChange={e => setMb(e.target.value)} style={{ width: 64 }} title="0 = 不限，同节点所有规则共享" />
      <span className="text-xs text-ink-mut">MB/s</span>
      <button type="submit" className="btn-secondary text-xs">设限速</button>
    </form>
  )
}
```

GrantedNodesCard 表头（`<th ...>流量配额</th>` 与 `<th ...>已用</th>` 之间）加：

```jsx
<th className="px-3 py-2.5 text-left text-xs font-semibold text-ink-soft">限速</th>
```

行内（PerNodeQuotaForm 单元格之后、已用流量单元格之前）加：

```jsx
<td className="px-3 py-2">
  <PerNodeRateForm userId={userId} nodeId={n.id} rateMBytes={grantByNode[n.id]?.rate_limit_mbytes} onDone={onDone} />
</td>
```

`copyGrants` 在 `parts.push(\`quota=${gb}GB\`)` 之后加：

```jsx
parts.push(`rate=${g?.rate_limit_mbytes || 0}`)
```

- [ ] **Step 2: PasteGrantsModal.jsx**

`parseGrantText`：`let quotaGB = 0` 后加 `let rateMBytes = 0`；循环内 `qMatch` 分支后加：

```jsx
const rMatch = p.match(/^rate=(\d+)$/i)
if (rMatch) { rateMBytes = Number(rMatch[1]); continue }
```

return 对象加 `rateMBytes`。

`submit` 的 grants 映射加：

```jsx
rate_limit_mbytes: applySettings ? p.rateMBytes : 0,
```

预览表头 `<th>流量配额</th>` 后加 `<th>限速</th>`；行内配额单元格后加：

```jsx
<td className="font-mono text-sm">{applySettings && p.rateMBytes ? `${p.rateMBytes}MB/s` : '不限'}</td>
```

textarea placeholder 改为 `'gateway-hk | max=10 | quota=5GB | rate=10\nrelay-jp | max=20'`。

- [ ] **Step 3: my/Dashboard.jsx**

桌面表头 `<th>已用流量</th>` 与 `<th>本节点上限</th>` 之间加 `<th>限速</th>`；对应行内（已用流量单元格后）加：

```jsx
<td className="font-mono text-xs">{g?.rate_limit_mbytes > 0 ? `${g.rate_limit_mbytes} MB/s` : '不限'}</td>
```

移动卡片的 `fmtTrafficGB` span 之后加：

```jsx
{g?.rate_limit_mbytes > 0 && <>
  <span className="text-ink-mut">·</span>
  <span className="font-mono">{g.rate_limit_mbytes} MB/s</span>
</>}
```

- [ ] **Step 4: rules/Detail.jsx**

删除：信息区的两行限速展示（`<span ...>限速</span>` 与其后的 `<span ...>{rule.bandwidth_mbps > 0 ? ... }</span>`）、整个 `{/* Bandwidth limit */}` 卡片块、整个 `BandwidthForm` 组件定义。确认文件内不再有 `bandwidth` 字样。

- [ ] **Step 5: 构建验证**

Run: `cd web && npm run build`
Expected: 构建成功，无未定义引用告警

- [ ] **Step 6: Commit**

```bash
git add web/src
git commit -m "feat(web): grant-level rate limit editing and display, per-rule entry removed"
```

---

### Task 9: 全量验证

**Files:** 无新增（验证性任务）

- [ ] **Step 1: 全量测试**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS

- [ ] **Step 2: 前端构建产物**

Run: `cd web && npm run build`
Expected: 成功（若 `web/dist` 属 git 跟踪产物，按仓库惯例一并提交）

- [ ] **Step 3: 语义抽查（代码走读断言）**

- `grep -rn "bandwidth_mbps" internal/ web/src/` 仅应命中：`nft.Rule.BandwidthMbps`（wire 兼容）、迁移 SQL、tc/userspace 的 legacy 路径与相关测试；不应命中任何 server API、db 投影或前端。
- `grep -rn "Mbps\|mbit" web/src/` 无命中（面向用户无 bit 单位）。

- [ ] **Step 4: Commit（如有收尾修正）**

```bash
git add -A
git commit -m "chore: per-grant rate limit follow-ups"
```
