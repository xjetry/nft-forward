# 落地出口流量限额 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 管理员分配给用户的落地节点全部成为"特殊出口"，按 (用户, host:port) 配置独立流量限额；账本与现有用户/授权配额互不相干（纯增量第二本账），超额只停打到该出口的规则。

**Architecture:** 新表 `user_landing_exits` 同时承担物化落地集合与限额账本；计量在 `applyCounters` 末跳按原始字节累加；执行复用"下发排除 + re-dispatch"三件套（`ActiveRuleHopsForPush` 第五个 NOT EXISTS）；同步依赖新的错误感知解析入口，订阅失败保留旧集合。零 agent/协议改动。

**Tech Stack:** Go (chi + modernc sqlite)、React 19 + Tailwind（web/ 目录，embed 进二进制）。

**Spec:** `docs/superpowers/specs/2026-07-02-landing-exit-quota-design.md`（实现遇到歧义以 spec 为准）。

## Global Constraints

- **注释/commit 禁止过程元信息**：不写任务编号、方案代号、审阅轮次（如 "Task 3"、"per spec"）；注释只写 WHY 与不变量。派发 subagent 时必须在 prompt 中传达此规则。
- 流量单位字节、限速 MB/s；UI 输入用 GB（×1073741824）。
- 用户可见文案一律中文；audit action 命名沿用 `user.set_node_quota` 风格。
- 出口账本按**原始字节**（上+下行），不乘 rate_multiplier/billing_rate、不受 unidirectional 影响。
- host:port 字符串精确匹配；host 存裸主机名（IPv6 不带方括号），内存索引键用 `net.JoinHostPort`。
- `SyncUserLandingExits` 不触碰 quota/used；订阅解析失败整轮不同步。
- 每个任务收尾：`gofmt -l internal/ cmd/` 输出为空；`go build ./...` 通过。
- Commit 用 conventional 风格（feat/fix/docs + scope），信息描述行为而非步骤。

---

### Task 1: 周期重置后无条件重推（既有缺口修复，独立先行）

现状：周期重置只在用户因"流量超额"被全局禁用时才重推节点；per-grant 配额压制的规则在计数清零后要等某次无关下发才恢复。配额排除只在 push 时求值，重置必须主动重推。

**Files:**
- Modify: `internal/server/hub.go:668-677`（applyCounters 内联重置分支）
- Modify: `internal/server/server.go:109-121`（cycleResetEnforcer）
- Test: `internal/server/cycle_reset_redispatch_test.go`（新建）

**Interfaces:**
- Consumes: `db.CheckAndResetTrafficCycle`、`db.DistinctUserNodes`、`hub.Redispatch`（均已存在）
- Produces: 行为变更——`reset==true` 时无条件重推该用户全部 hop 节点

- [ ] **Step 1: 写失败测试**

新建 `internal/server/cycle_reset_redispatch_test.go`：

```go
package server

import (
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// A per-grant quota overrun never disables the user, so the old reset path
// (which only re-dispatched disabled users) left suppressed rules dead until
// an unrelated push. The cycle rollover itself must trigger the re-push.
func TestCycleResetRedispatchesWithoutDisable(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "cr1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 1000)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)

	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1.ID)
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=?, last_traffic_reset_at=0 WHERE id=?`,
		time.Now().Unix()-31*86400, uid)

	hub := NewHub(d)
	got := make(chan []int64, 1)
	hub.Redispatch = func(nodes []int64) { got <- nodes }

	port := getHopPort(t, d, ruleID, n1.ID)
	hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesUp: 1}})

	select {
	case nodes := <-got:
		found := false
		for _, n := range nodes {
			if n == n1.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("redispatch nodes %v missing %d", nodes, n1.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cycle reset must redispatch even when the user is not disabled")
	}
	g, _ := db.GetNodeGrant(d, uid, n1.ID)
	if g.TrafficUsedBytes != 1 {
		t.Fatalf("grant counter should be reset then accumulate the sample, got %d", g.TrafficUsedBytes)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestCycleResetRedispatchesWithoutDisable -v`
Expected: FAIL（超时——现状不重推未禁用用户）

- [ ] **Step 3: 改 hub.go 内联分支**

`internal/server/hub.go` 把 668-677 行的：

```go
				if u != nil {
					if reset, _ := db.CheckAndResetTrafficCycle(h.DB, u); reset {
						if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
							_ = db.SetUserDisabled(h.DB, userID, false, "")
							if nodes, err := db.DistinctUserNodes(h.DB, userID); err == nil && h.Redispatch != nil {
								go h.Redispatch(nodes)
							}
						}
					}
				}
```

改为：

```go
				if u != nil {
					if reset, _ := db.CheckAndResetTrafficCycle(h.DB, u); reset {
						if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
							_ = db.SetUserDisabled(h.DB, userID, false, "")
						}
						// Quota exclusions are evaluated at push time only; a
						// fresh cycle must re-push or suppressed rules stay dead.
						if nodes, err := db.DistinctUserNodes(h.DB, userID); err == nil && h.Redispatch != nil {
							go h.Redispatch(nodes)
						}
					}
				}
```

- [ ] **Step 4: 改 server.go cycleResetEnforcer**

`internal/server/server.go` 把 109-121 行的：

```go
				if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
					if err := db.SetUserDisabled(s.DB, u.ID, false, ""); err != nil {
						log.Printf("cycle: re-enable user %d: %v", u.ID, err)
						continue
					}
					if nodes, err := db.DistinctUserNodes(s.DB, u.ID); err == nil {
						for _, n := range nodes {
							if err := s.dispatchToNode(n); err != nil {
								log.Printf("cycle: re-dispatch node %d for user %d: %v", n, u.ID, err)
							}
						}
					}
				}
```

改为：

```go
				if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
					if err := db.SetUserDisabled(s.DB, u.ID, false, ""); err != nil {
						log.Printf("cycle: re-enable user %d: %v", u.ID, err)
						continue
					}
				}
				// Quota exclusions are evaluated at push time only; a fresh
				// cycle must re-push or suppressed rules stay dead.
				if nodes, err := db.DistinctUserNodes(s.DB, u.ID); err == nil {
					for _, n := range nodes {
						if err := s.dispatchToNode(n); err != nil {
							log.Printf("cycle: re-dispatch node %d for user %d: %v", n, u.ID, err)
						}
					}
				}
```

同时更新 cycleResetEnforcer 函数头注释（76-83 行）末段，说明重推现在无条件执行。

- [ ] **Step 5: 跑测试确认通过 + 回归**

Run: `go test ./internal/server/ -run 'TestCycleReset|TestPerNodeQuota|TestApplyCounters' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/server/hub.go internal/server/server.go internal/server/cycle_reset_redispatch_test.go
git commit -m "fix(server): re-dispatch on traffic cycle reset regardless of disable state"
```

---

### Task 2: 迁移 0028 + db 层账本模型与全部查询助手

**Files:**
- Create: `internal/db/migrations/0028_user_landing_exits.sql`
- Create: `internal/db/landing_exits.go`
- Modify: `internal/db/traffic.go`（CheckAndResetTrafficCycle、ResetAllUserTraffic 追加清零）
- Test: `internal/db/landing_exits_test.go`（新建）

**Interfaces:**
- Consumes: `now()`（db 包内已有）、`queryInt64s`
- Produces（后续任务全部依赖，签名以此为准）:
  - `type LandingExit struct { UserID int64; Host string; Port int; Name, Protocol, URI string; Present bool; QuotaBytes, UsedBytes, UpdatedAt int64 }`（json 见下，URI 为 `json:"-"`）
  - `type LandingExitInput struct { Host string; Port int; Name, Protocol, URI string }`
  - `type LandingExitKey struct { Host string; Port int }`
  - `type UserExitKey struct { UserID int64; Host string; Port int }`
  - `SyncUserLandingExits(d *sql.DB, userID int64, exits []LandingExitInput, srcSubURL, srcURIs string) (flipped []LandingExitKey, synced bool, err error)`
  - `ListUserLandingExits(d *sql.DB, userID int64) ([]*LandingExit, error)`
  - `PresentLandingExitsForUser(d *sql.DB, userID int64) ([]*LandingExit, error)`
  - `PresentLandingExitSet(d *sql.DB, userIDs []int64) (map[UserExitKey]bool, error)`
  - `MaxHopPositions(d *sql.DB, ruleIDs []int64) (map[int64]int, error)`
  - `SetUserLandingExitQuota(d *sql.DB, userID int64, host string, port int, quota int64) (updated, present bool, err error)`
  - `ResetUserLandingExitTraffic(d *sql.DB, userID int64, host string, port int) (updated, present bool, err error)`
  - `DeleteUserLandingExit(d *sql.DB, userID int64, host string, port int) (status string, err error)` — status ∈ `"deleted" | "notfound" | "present"`
  - `ExitsExceedingQuota(d *sql.DB, userID int64) ([]LandingExitKey, error)`
  - `NodesForUserExit(d *sql.DB, userID int64, host string, port int) ([]int64, error)`

- [ ] **Step 1: 写迁移**

`internal/db/migrations/0028_user_landing_exits.sql`：

```sql
-- Per-user landing-exit traffic ledger keyed by destination host:port. One
-- table carries both the materialized landing set (name/protocol/uri/present,
-- driving exit classification) and the quota ledger (quota_bytes/used_bytes,
-- 0 quota = unlimited). Sync never deletes rows: exits that drop out of the
-- subscription are flagged present=0 so a returning exit resumes its ledger.
CREATE TABLE user_landing_exits (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host        TEXT    NOT NULL,
  port        INTEGER NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',
  protocol    TEXT    NOT NULL DEFAULT '',
  uri         TEXT    NOT NULL DEFAULT '',
  present     INTEGER NOT NULL DEFAULT 1,
  quota_bytes INTEGER NOT NULL DEFAULT 0,
  used_bytes  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, host, port)
);
```

- [ ] **Step 2: 写失败测试**

`internal/db/landing_exits_test.go`（沿用 `traffic_test.go` 的 `openTestDB`/`createTestUser` 助手）：

```go
package db

import "testing"

func inputs(hosts ...string) []LandingExitInput {
	out := make([]LandingExitInput, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, LandingExitInput{Host: h, Port: 443, Name: "n-" + h, Protocol: "vless", URI: "vless://x@" + h + ":443"})
	}
	return out
}

func TestSyncUserLandingExitsLifecycle(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)

	// initial sync materializes present=1 rows
	_, synced, err := SyncUserLandingExits(d, uid, inputs("a.com", "b.com"), "", "")
	if err != nil || !synced {
		t.Fatalf("sync: synced=%v err=%v", synced, err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if len(exits) != 2 || !exits[0].Present {
		t.Fatalf("want 2 present rows, got %+v", exits)
	}

	// quota/used survive a re-sync and a disappearance
	if _, _, err := SetUserLandingExitQuota(d, uid, "a.com", 443, 1000); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=? AND host='a.com'`, uid)
	_, synced, _ = SyncUserLandingExits(d, uid, inputs("b.com"), "", "")
	if !synced {
		t.Fatal("second sync should apply")
	}
	rows, _ := ListUserLandingExits(d, uid)
	var a *LandingExit
	for _, e := range rows {
		if e.Host == "a.com" {
			a = e
		}
	}
	if a == nil || a.Present || a.QuotaBytes != 1000 || a.UsedBytes != 500 {
		t.Fatalf("a.com should be present=0 with ledger kept, got %+v", a)
	}

	// returning exit resumes the same ledger
	SyncUserLandingExits(d, uid, inputs("a.com", "b.com"), "", "")
	rows, _ = ListUserLandingExits(d, uid)
	for _, e := range rows {
		if e.Host == "a.com" && (!e.Present || e.UsedBytes != 500) {
			t.Fatalf("returning exit lost ledger: %+v", e)
		}
	}
}

func TestSyncDiscardsStaleSource(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	d.Exec(`UPDATE users SET landing_sub_url='https://new.example/sub' WHERE id=?`, uid)
	_, synced, err := SyncUserLandingExits(d, uid, inputs("a.com"), "https://old.example/sub", "")
	if err != nil {
		t.Fatal(err)
	}
	if synced {
		t.Fatal("sync resolved from a stale source must be discarded")
	}
	if exits, _ := ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatalf("no rows expected, got %d", len(exits))
	}
}

func TestSyncReturnsFlippedOverQuotaKeys(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	SetUserLandingExitQuota(d, uid, "a.com", 443, 100)
	d.Exec(`UPDATE user_landing_exits SET used_bytes=100 WHERE user_id=?`, uid)

	// present 1→0 on an exhausted exit lifts its push exclusion
	flipped, _, _ := SyncUserLandingExits(d, uid, nil, "", "")
	if len(flipped) != 1 || flipped[0].Host != "a.com" {
		t.Fatalf("want a.com flipped, got %+v", flipped)
	}
	// present 0→1 re-imposes it
	flipped, _, _ = SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	if len(flipped) != 1 {
		t.Fatalf("want flip back reported, got %+v", flipped)
	}
	// steady state reports nothing
	flipped, _, _ = SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	if len(flipped) != 0 {
		t.Fatalf("no flip expected, got %+v", flipped)
	}
}

func TestExitQuotaHelpers(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")

	if updated, present, _ := SetUserLandingExitQuota(d, uid, "a.com", 443, 100); !updated || !present {
		t.Fatal("quota update on present row")
	}
	if updated, _, _ := SetUserLandingExitQuota(d, uid, "nope.com", 443, 100); updated {
		t.Fatal("missing row must report updated=false")
	}
	d.Exec(`UPDATE user_landing_exits SET used_bytes=150 WHERE user_id=?`, uid)
	keys, _ := ExitsExceedingQuota(d, uid)
	if len(keys) != 1 || keys[0].Host != "a.com" {
		t.Fatalf("want a.com exceeding, got %+v", keys)
	}
	if _, _, err := ResetUserLandingExitTraffic(d, uid, "a.com", 443); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ExitsExceedingQuota(d, uid); len(keys) != 0 {
		t.Fatal("reset should clear the overrun")
	}

	// delete is restricted to residual rows
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "present" {
		t.Fatalf("present row must refuse delete, got %q", st)
	}
	SyncUserLandingExits(d, uid, nil, "", "")
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "deleted" {
		t.Fatalf("residual row should delete, got %q", st)
	}
	if st, _ := DeleteUserLandingExit(d, uid, "a.com", 443); st != "notfound" {
		t.Fatalf("gone row is notfound, got %q", st)
	}
}

func TestCycleResetClearsExitLedger(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=?`, uid)
	d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=strftime('%s','now')-31*86400, last_traffic_reset_at=0 WHERE id=?`, uid)
	u, _ := GetUserByID(d, uid)
	if reset, err := CheckAndResetTrafficCycle(d, u); err != nil || !reset {
		t.Fatalf("reset=%v err=%v", reset, err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatalf("cycle reset must clear the exit ledger, got %d", exits[0].UsedBytes)
	}

	SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
	d.Exec(`UPDATE user_landing_exits SET used_bytes=500 WHERE user_id=?`, uid)
	if err := ResetAllUserTraffic(d, uid); err != nil {
		t.Fatal(err)
	}
	exits, _ = ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatal("manual full reset must clear the exit ledger too")
	}
}
```

注意：删掉文件顶部占位的 `seedExit`（上面代码块中已标注，最终文件不包含它）。

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/db/ -run 'TestSync|TestExitQuota|TestCycleResetClears' -v`
Expected: FAIL（编译错误：类型未定义）

- [ ] **Step 4: 实现 `internal/db/landing_exits.go`**

```go
package db

import (
	"database/sql"
	"strings"
)

// LandingExit is one row of a user's materialized landing-exit set plus its
// traffic ledger. Present=false rows are exits that dropped out of the landing
// source; their quota/used are kept so a returning exit resumes seamlessly.
// URI is server-internal (relay-URI rewriting); it never serializes into
// admin-facing JSON.
type LandingExit struct {
	UserID     int64  `json:"user_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	URI        string `json:"-"`
	Present    bool   `json:"present"`
	QuotaBytes int64  `json:"quota_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	UpdatedAt  int64  `json:"updated_at"`
}

// LandingExitInput is a deduplicated landing node destined for the
// materialized set (a plain struct so this package stays decoupled from the
// landing parser).
type LandingExitInput struct {
	Host     string
	Port     int
	Name     string
	Protocol string
	URI      string
}

// LandingExitKey addresses one exit within a user's set.
type LandingExitKey struct {
	Host string
	Port int
}

// UserExitKey addresses one exit ledger row across users.
type UserExitKey struct {
	UserID int64
	Host   string
	Port   int
}

// SyncUserLandingExits materializes a successfully resolved landing set.
// Inputs must already be deduplicated by host:port (first wins, manual URIs
// preceding subscription nodes). Rows missing from the input flip to present=0 —
// never deleted — and quota/used are never touched here. srcSubURL/srcURIs
// are the source values the resolution ran against: if the users row no
// longer matches (the admin changed the source during a slow subscription
// fetch), the stale result is discarded with synced=false. The returned keys
// flipped presence while at/over quota — their push-exclusion state changed,
// so the caller must re-dispatch the rules pointed at them.
func SyncUserLandingExits(d *sql.DB, userID int64, exits []LandingExitInput, srcSubURL, srcURIs string) (flipped []LandingExitKey, synced bool, err error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var curSub, curURIs string
	if err := tx.QueryRow(`SELECT landing_sub_url, landing_uris FROM users WHERE id=?`, userID).Scan(&curSub, &curURIs); err != nil {
		return nil, false, err
	}
	if curSub != srcSubURL || curURIs != srcURIs {
		return nil, false, nil
	}

	type rowState struct {
		present   bool
		overQuota bool
	}
	existing := map[LandingExitKey]rowState{}
	rows, err := tx.Query(`SELECT host, port, present, quota_bytes, used_bytes FROM user_landing_exits WHERE user_id=?`, userID)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var k LandingExitKey
		var present int
		var quota, used int64
		if err := rows.Scan(&k.Host, &k.Port, &present, &quota, &used); err != nil {
			rows.Close()
			return nil, false, err
		}
		existing[k] = rowState{present: present == 1, overQuota: quota > 0 && used >= quota}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()

	nowTs := now()
	inInput := map[LandingExitKey]bool{}
	for _, e := range exits {
		k := LandingExitKey{Host: e.Host, Port: e.Port}
		if inInput[k] {
			continue
		}
		inInput[k] = true
		if _, err := tx.Exec(`INSERT INTO user_landing_exits(user_id, host, port, name, protocol, uri, present, updated_at)
			VALUES (?,?,?,?,?,?,1,?)
			ON CONFLICT(user_id, host, port) DO UPDATE SET name=excluded.name, protocol=excluded.protocol, uri=excluded.uri, present=1, updated_at=excluded.updated_at`,
			userID, e.Host, e.Port, e.Name, e.Protocol, e.URI, nowTs); err != nil {
			return nil, false, err
		}
		if st, ok := existing[k]; ok && !st.present && st.overQuota {
			flipped = append(flipped, k)
		}
	}
	for k, st := range existing {
		if inInput[k] || !st.present {
			continue
		}
		if _, err := tx.Exec(`UPDATE user_landing_exits SET present=0, updated_at=? WHERE user_id=? AND host=? AND port=?`,
			nowTs, userID, k.Host, k.Port); err != nil {
			return nil, false, err
		}
		if st.overQuota {
			flipped = append(flipped, k)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return flipped, true, nil
}

const landingExitCols = `user_id, host, port, name, protocol, uri, present, quota_bytes, used_bytes, updated_at`

func scanLandingExit(r rowScanner) (*LandingExit, error) {
	e := &LandingExit{}
	var present int
	if err := r.Scan(&e.UserID, &e.Host, &e.Port, &e.Name, &e.Protocol, &e.URI, &present, &e.QuotaBytes, &e.UsedBytes, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Present = present == 1
	return e, nil
}

// ListUserLandingExits returns the user's full materialized set, present rows
// first, for the admin quota card.
func ListUserLandingExits(d *sql.DB, userID int64) ([]*LandingExit, error) {
	return queryAll(d, `SELECT `+landingExitCols+` FROM user_landing_exits WHERE user_id=? ORDER BY present DESC, name, host, port`,
		scanLandingExit, userID)
}

// PresentLandingExitsForUser returns only the rows that drive classification,
// metering and push exclusion.
func PresentLandingExitsForUser(d *sql.DB, userID int64) ([]*LandingExit, error) {
	return queryAll(d, `SELECT `+landingExitCols+` FROM user_landing_exits WHERE user_id=? AND present=1 ORDER BY name, host, port`,
		scanLandingExit, userID)
}

// PresentLandingExitSet returns the present (user, host, port) triples for the
// given users — the per-batch lookup applyCounters classifies samples against.
func PresentLandingExitSet(d *sql.DB, userIDs []int64) (map[UserExitKey]bool, error) {
	out := map[UserExitKey]bool{}
	if len(userIDs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(userIDs)-1) + "?"
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}
	rows, err := d.Query(`SELECT user_id, host, port FROM user_landing_exits WHERE present=1 AND user_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k UserExitKey
		if err := rows.Scan(&k.UserID, &k.Host, &k.Port); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// MaxHopPositions returns each rule's final hop position. Only the final hop
// meters into the exit ledger: middle hops target system relay addresses,
// which must never be mistaken for the user's destination.
func MaxHopPositions(d *sql.DB, ruleIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(ruleIDs)-1) + "?"
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
	}
	rows, err := d.Query(`SELECT rule_id, MAX(position) FROM rule_hops WHERE rule_id IN (`+ph+`) GROUP BY rule_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			return nil, err
		}
		out[id] = pos
	}
	return out, rows.Err()
}

// exitRowPresent reports whether the row exists and is present. found=false
// means no such row.
func exitRowPresent(d *sql.DB, userID int64, host string, port int) (found, present bool, err error) {
	var p int
	err = d.QueryRow(`SELECT present FROM user_landing_exits WHERE user_id=? AND host=? AND port=?`, userID, host, port).Scan(&p)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, p == 1, nil
}

// SetUserLandingExitQuota updates one exit's quota (0 = unlimited). present
// tells the caller whether a re-dispatch is warranted — present=0 residual
// rows sit outside the push exclusion.
func SetUserLandingExitQuota(d *sql.DB, userID int64, host string, port int, quota int64) (updated, present bool, err error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET quota_bytes=?, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		quota, now(), userID, host, port)
	return err == nil, present, err
}

// ResetUserLandingExitTraffic zeroes one exit's ledger.
func ResetUserLandingExitTraffic(d *sql.DB, userID int64, host string, port int) (updated, present bool, err error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET used_bytes=0, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		now(), userID, host, port)
	return err == nil, present, err
}

// DeleteUserLandingExit removes a residual (present=0) row. In-set rows are
// managed by sync and refuse deletion.
func DeleteUserLandingExit(d *sql.DB, userID int64, host string, port int) (string, error) {
	found, present, err := exitRowPresent(d, userID, host, port)
	if err != nil {
		return "", err
	}
	if !found {
		return "notfound", nil
	}
	if present {
		return "present", nil
	}
	if _, err := d.Exec(`DELETE FROM user_landing_exits WHERE user_id=? AND host=? AND port=?`, userID, host, port); err != nil {
		return "", err
	}
	return "deleted", nil
}

// ExitsExceedingQuota returns the user's present exits whose ledger reached
// quota. Quota 0 (unlimited) never exceeds.
func ExitsExceedingQuota(d *sql.DB, userID int64) ([]LandingExitKey, error) {
	rows, err := d.Query(`SELECT host, port FROM user_landing_exits
		WHERE user_id=? AND present=1 AND quota_bytes>0 AND used_bytes>=quota_bytes`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LandingExitKey
	for rows.Next() {
		var k LandingExitKey
		if err := rows.Scan(&k.Host, &k.Port); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// NodesForUserExit returns the distinct physical hop nodes of the user's rules
// that exit to host:port. Composite entries are already expanded into physical
// hops in rule_hops; composite virtual nodes have no agent connection and must
// never enter a dispatch set.
func NodesForUserExit(d *sql.DB, userID int64, host string, port int) ([]int64, error) {
	return queryInt64s(d, `
		SELECT DISTINCT rh.node_id
		FROM rule_hops rh
		JOIN rules r ON r.id = rh.rule_id
		WHERE r.owner_id=? AND r.exit_host=? AND r.exit_port=?`, userID, host, port)
}
```

- [ ] **Step 5: 追加周期/手动重置清零**

`internal/db/traffic.go` `CheckAndResetTrafficCycle` 在 `UPDATE user_nodes ...`（113 行）之后、`tx.Commit()` 之前追加：

```go
	if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = 0 WHERE user_id=?`, u.ID); err != nil {
		return false, err
	}
```

`ResetAllUserTraffic` 在 `UPDATE user_nodes ...`（32 行）之后、`tx.Commit()` 之前追加：

```go
	if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = 0 WHERE user_id=?`, userID); err != nil {
		return err
	}
```

两处函数注释中"Both must be cleared together"相应改为覆盖三本账。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/db/ -v`
Expected: PASS（含既有回归）

- [ ] **Step 7: Commit**

```bash
git add internal/db/migrations/0028_user_landing_exits.sql internal/db/landing_exits.go internal/db/traffic.go internal/db/landing_exits_test.go
git commit -m "feat(db): per-user landing-exit ledger with sync, quota and lookup helpers"
```

---

### Task 3: 错误感知解析入口 + 同步接线（保存来源 / 用户详情预览）

**Files:**
- Modify: `internal/server/landing.go`
- Modify: `internal/server/api.go:1641-1646`（apiGetUser 的 landing_nodes 预览）
- Test: `internal/server/landing_sync_test.go`（新建）

**Interfaces:**
- Consumes: `db.SyncUserLandingExits`、`db.NodesForUserExit`（Task 2）
- Produces:
  - `(s *Server) resolveLandingExits(u *db.User, force bool) ([]landing.Node, bool)`
  - `(s *Server) syncLandingExits(u *db.User, nodes []landing.Node)`
  - `(s *Server) redispatchUserExit(userID int64, host string, port int)`
  - `dedupLandingNodes(nodes []landing.Node) []landing.Node`

- [ ] **Step 1: 写失败测试**

`internal/server/landing_sync_test.go`：

```go
package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestResolveLandingExitsManualOnly(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	nodes, ok := s.resolveLandingExits(u, false)
	if !ok || len(nodes) != 1 || nodes[0].Host != "1.2.3.4" {
		t.Fatalf("manual-only resolution must succeed, ok=%v nodes=%+v", ok, nodes)
	}
}

// The SSRF guard refuses non-public targets, so a loopback subscription URL is
// a deterministic fetch failure — exactly the case that must not flip the
// materialized set.
func TestResolveLandingExitsSubFailure(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "http://127.0.0.1:1/sub", "vless://u@1.2.3.4:443#HK")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	if _, ok := s.resolveLandingExits(u, true); ok {
		t.Fatal("subscription fetch failure must report ok=false")
	}
}

func TestSyncLandingExitsMaterializes(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK\nvless://u@1.2.3.4:443#DUP")
	u, _ := db.GetUserByID(d, uid)
	s, _ := New(d)

	nodes, ok := s.resolveLandingExits(u, false)
	if !ok {
		t.Fatal("resolve")
	}
	s.syncLandingExits(u, nodes)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 || exits[0].Name != "HK" {
		t.Fatalf("dedup keeps the first node per host:port, got %+v", exits)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run 'TestResolveLanding|TestSyncLanding' -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 实现 landing.go 新函数**

在 `internal/server/landing.go` 的 `landingNodesFor` 之后追加：

```go
// resolveLandingExits resolves the user's admin-assigned landing set for
// materialization. Unlike landingNodesFor — display-oriented, silently
// degrading to manual URIs when the subscription fetch fails — it reports
// failure, because syncing a partial set would flip the subscription's exits
// to present=0 and shift billing classification on a network blip.
func (s *Server) resolveLandingExits(u *db.User, force bool) ([]landing.Node, bool) {
	var nodes []landing.Node
	if uris := strings.TrimSpace(u.LandingURIs); uris != "" {
		nodes = append(nodes, landing.ParseURIs(strings.Split(uris, "\n"))...)
	}
	if url := strings.TrimSpace(u.LandingSubURL); url != "" {
		subNodes, err := s.Landing.Subscription(url, force)
		if err != nil {
			return nil, false
		}
		nodes = append(nodes, subNodes...)
	}
	return nodes, true
}

// dedupLandingNodes keeps the first node per host:port — manual URIs precede
// subscription nodes, so they win a collision — as the materialized shape.
func dedupLandingNodes(nodes []landing.Node) []landing.Node {
	seen := make(map[string]bool, len(nodes))
	out := make([]landing.Node, 0, len(nodes))
	for _, n := range nodes {
		key := net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// syncLandingExits materializes a successfully resolved landing set and
// re-pushes any exits whose push-exclusion state flipped with presence —
// excluded rules generate no traffic, so no counters-driven path would ever
// revive them.
func (s *Server) syncLandingExits(u *db.User, nodes []landing.Node) {
	deduped := dedupLandingNodes(nodes)
	exits := make([]db.LandingExitInput, 0, len(deduped))
	for _, n := range deduped {
		exits = append(exits, db.LandingExitInput{Host: n.Host, Port: n.Port, Name: n.Name, Protocol: n.Protocol, URI: n.URI})
	}
	flipped, synced, err := db.SyncUserLandingExits(s.DB, u.ID, exits, u.LandingSubURL, u.LandingURIs)
	if err != nil {
		log.Printf("landing: sync exits for user %d: %v", u.ID, err)
		return
	}
	if !synced || len(flipped) == 0 {
		return
	}
	go func() {
		for _, k := range flipped {
			s.redispatchUserExit(u.ID, k.Host, k.Port)
		}
	}()
}

// redispatchUserExit re-pushes every node carrying rules that exit to the
// given landing exit so a changed ledger/quota state reaches the data plane.
func (s *Server) redispatchUserExit(userID int64, host string, port int) {
	nodes, err := db.NodesForUserExit(s.DB, userID, host, port)
	if err != nil {
		log.Printf("landing: nodes for user %d exit %s:%d: %v", userID, host, port, err)
		return
	}
	for _, n := range nodes {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("landing: re-dispatch node %d for exit %s:%d: %v", n, host, port, err)
		}
	}
}
```

`landing.go` import 增加 `"log"`（`net`/`strconv`/`strings` 已有）。

- [ ] **Step 4: 接线 apiSetUserLanding 与 apiGetUser**

`internal/server/landing.go` `apiSetUserLanding` 尾部（84-87 行）改为：

```go
	// Return a fresh preview so the admin sees what the source resolved to,
	// and materialize it while the resolution is known-good.
	target, _ := db.GetUserByID(s.DB, id)
	if nodes, ok := s.resolveLandingExits(target, true); ok {
		s.syncLandingExits(target, nodes)
	}
	nodes := s.landingNodesFor(target, false)
	jsonOK(w, map[string]any{"ok": true, "landing_nodes": nodes})
```

（force 解析已把结果写进 Fetcher 缓存，随后的 landingNodesFor 命中缓存，不产生第二次网络请求。）

`internal/server/api.go` `apiGetUser`（1635-1646 行）把 `"landing_nodes": s.landingNodesFor(target, false),` 前的响应构建改为：

```go
	// The landing_nodes preview doubles as a sync point: any successful
	// resolution keeps the materialized set fresh without waiting for the
	// background pass.
	landingPreview, lok := s.resolveLandingExits(target, false)
	if lok {
		s.syncLandingExits(target, landingPreview)
	} else {
		landingPreview = s.landingNodesFor(target, false)
	}
	jsonOK(w, map[string]any{
		"user": apiUserFullView(target), "nodes": grantedNodes,
		"grants": grants, "all_nodes": allNodes,
		"rules":         rules,
		"landing_nodes": landingPreview,
	})
```

- [ ] **Step 5: 跑测试确认通过 + 回归**

Run: `go test ./internal/server/ -run 'TestResolveLanding|TestSyncLanding|TestLanding|TestClassify' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/server/landing.go internal/server/api.go internal/server/landing_sync_test.go
git commit -m "feat(server): error-aware landing resolution with materialized exit sync"
```

---

### Task 4: 下发排除——出口超额的规则从 push 集消失

**Files:**
- Modify: `internal/db/queries.go:614-643`（ActiveRuleHopsForPush）
- Test: `internal/server/landing_exit_enforcement_test.go`（新建）

**Interfaces:**
- Consumes: `user_landing_exits` 表（Task 2）
- Produces: `ActiveRuleHopsForPush` 排除条件扩展（签名不变）

- [ ] **Step 1: 写失败测试**

`internal/server/landing_exit_enforcement_test.go`：

```go
package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
)

// seedLandingExit materializes one present exit row with the given ledger.
func seedLandingExit(t *testing.T, d *sql.DB, uid int64, host string, port int, quota, used int64) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO user_landing_exits(user_id, host, port, present, quota_bytes, used_bytes) VALUES (?,?,?,1,?,?)`,
		uid, host, port, quota, used); err != nil {
		t.Fatal(err)
	}
}

func TestExitQuotaExclusion(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "ex1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)

	// both rules exit to 8.8.8.8:443 (the test helper's fixed exit)
	createTestRuleDirectNode(t, d, uid, n1.ID)
	createTestRuleDirectNode(t, d, uid, n1.ID)

	// exhausted ledger on the exit both rules point at
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 1000, 1000)
	if hops, _ := db.ActiveRuleHopsForPush(d, n1.ID); len(hops) != 0 {
		t.Fatalf("rules to an exhausted exit must be excluded, got %d hops", len(hops))
	}

	// quota=0 (unlimited) never excludes, whatever the ledger reads
	d.Exec(`UPDATE user_landing_exits SET quota_bytes=0 WHERE user_id=?`, uid)
	if hops, _ := db.ActiveRuleHopsForPush(d, n1.ID); len(hops) != 2 {
		t.Fatalf("unlimited exit must not exclude, got %d hops", len(hops))
	}

	// present=0 lifts the exclusion (rule reverts to ordinary billing)
	d.Exec(`UPDATE user_landing_exits SET quota_bytes=1000, present=0 WHERE user_id=?`, uid)
	if hops, _ := db.ActiveRuleHopsForPush(d, n1.ID); len(hops) != 2 {
		t.Fatalf("absent exit must not exclude, got %d hops", len(hops))
	}
}

func TestExitQuotaExclusionScopedToOwnerAndExit(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	other, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "ex2", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, other, n1.ID, 10, 0)

	mine := createTestRuleDirectNode(t, d, uid, n1.ID)
	theirs := createTestRuleDirectNode(t, d, other, n1.ID)
	// a rule of mine to a different destination
	otherExit := createTestRuleDirectNode(t, d, uid, n1.ID)
	d.Exec(`UPDATE rules SET exit_host='9.9.9.9' WHERE id=?`, otherExit)
	d.Exec(`UPDATE rule_hops SET target_host='9.9.9.9' WHERE rule_id=?`, otherExit)

	seedLandingExit(t, d, uid, "8.8.8.8", 443, 1000, 1000)

	hops, _ := db.ActiveRuleHopsForPush(d, n1.ID)
	ruleIDs := map[int64]bool{}
	for _, h := range hops {
		ruleIDs[h.RuleID] = true
	}
	if ruleIDs[mine] {
		t.Fatal("my rule to the exhausted exit must be excluded")
	}
	if !ruleIDs[theirs] {
		t.Fatal("another user's rule to the same host:port must stay active")
	}
	if !ruleIDs[otherExit] {
		t.Fatal("my rule to a different exit must stay active")
	}
}

// A chain rule whose middle hop targets the exit's host:port must still be
// excluded exactly once at the rule level (exclusion keys on rules.exit_*).
func TestExitQuotaExclusionChainRule(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "ex3", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "ex4", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)

	seedLandingExit(t, d, uid, "8.8.8.8", 443, 500, 500)
	for _, nid := range []int64{n1.ID, n2.ID} {
		hops, _ := db.ActiveRuleHopsForPush(d, nid)
		for _, h := range hops {
			if h.RuleID == ruleID {
				t.Fatalf("chain hop on node %d should be excluded", nid)
			}
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestExitQuota -v`
Expected: FAIL（规则未被排除）

- [ ] **Step 3: 扩展 ActiveRuleHopsForPush**

`internal/db/queries.go` 在第四个 NOT EXISTS（634-640 行）之后、`ORDER BY` 之前插入：

```go
		AND NOT EXISTS (
		  SELECT 1 FROM rules r4
		  JOIN user_landing_exits ule ON ule.user_id = r4.owner_id
		    AND ule.host = r4.exit_host AND ule.port = r4.exit_port
		  WHERE r4.id = rh.rule_id
		    AND ule.present = 1
		    AND ule.quota_bytes > 0
		    AND ule.used_bytes >= ule.quota_bytes
		)
```

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/server/ -run 'TestExitQuota|TestPerNodeQuota|TestChainExcluded' -v && go test ./internal/db/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/db/queries.go internal/server/landing_exit_enforcement_test.go
git commit -m "feat(db): exclude rules pointed at exhausted landing exits from push"
```

---

### Task 5: applyCounters 末跳计量入出口账本

**Files:**
- Modify: `internal/server/hub.go:588-770`（applyCounters）
- Test: `internal/server/landing_exit_metering_test.go`（新建）

**Interfaces:**
- Consumes: `db.PresentLandingExitSet`、`db.MaxHopPositions`、`db.UserExitKey`（Task 2）
- Produces: 行为——末跳原始字节累入 `user_landing_exits.used_bytes`，并置 touched 触发 OnTrafficUpdate

- [ ] **Step 1: 写失败测试**

`internal/server/landing_exit_metering_test.go`（复用 Task 4 的 `seedLandingExit`）：

```go
package server

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

func exitUsed(t *testing.T, d *sql.DB, uid int64) int64 {
	t.Helper()
	var used int64
	if err := d.QueryRow(`SELECT used_bytes FROM user_landing_exits WHERE user_id=?`, uid).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

// Chain rule n1→n2, exit 8.8.8.8:443. Only the final hop's raw bytes reach the
// exit ledger; user/grant ledgers keep their existing (weighted, every-hop)
// accounting untouched.
func TestExitLedgerCountsFinalHopOnly(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "m2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	p1 := getHopPort(t, d, ruleID, n1.ID)
	p2 := getHopPort(t, d, ruleID, n2.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p1, BytesUp: 300, BytesDown: 700}})
	s.Hub.applyCounters(n2.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p2, BytesUp: 400, BytesDown: 600}})

	if used := exitUsed(t, d, uid); used != 1000 {
		t.Fatalf("exit ledger wants the final hop's 1000 raw bytes, got %d", used)
	}
	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 2000 {
		t.Fatalf("user ledger must keep every-hop accounting (2000), got %d", u.TrafficUsedBytes)
	}
}

// A middle hop whose target coincides with the exit host:port must not meter:
// final-hop detection keys on position, not on target matching.
func TestExitLedgerIgnoresRelayCollision(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m3", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "m4", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 0)
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)
	// forge the middle hop's target to collide with the exit
	d.Exec(`UPDATE rule_hops SET target_host='8.8.8.8', target_port=443 WHERE rule_id=? AND position=0`, ruleID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	p1 := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: p1, BytesUp: 500, BytesDown: 500}})

	if used := exitUsed(t, d, uid); used != 0 {
		t.Fatalf("middle hop must not meter into the exit ledger, got %d", used)
	}
}

// Unidirectional nodes bill uplink only, but the exit ledger records real
// traffic to the destination — and its growth alone must still trigger the
// quota callback (weighted is 0 for a downlink-only batch).
func TestExitLedgerUnidirectionalAndTouch(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m5", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	d.Exec(`UPDATE nodes SET unidirectional=1 WHERE id=?`, n1.ID)
	db.GrantNode(d, uid, n1.ID, 10, 0)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)

	s, _ := New(d)
	touched := make(chan struct{}, 1)
	s.Hub.OnTrafficUpdate = func(userID, nodeID int64) {
		select {
		case touched <- struct{}{}:
		default:
		}
	}
	port := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesDown: 800}})

	if used := exitUsed(t, d, uid); used != 800 {
		t.Fatalf("exit ledger ignores unidirectional billing, want 800 got %d", used)
	}
	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("unidirectional downlink must not bill the user, got %d", u.TrafficUsedBytes)
	}
	select {
	case <-touched:
	case <-time.After(2 * time.Second):
		t.Fatal("exit ledger growth must trigger OnTrafficUpdate")
	}
}

func TestExitLedgerSkipsAbsentAndForeign(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "m6", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	ruleID := createTestRuleDirectNode(t, d, uid, n1.ID)
	// present=0: rule reverts to ordinary accounting only
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)
	d.Exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=?`, uid)

	s, _ := New(d)
	port := getHopPort(t, d, ruleID, n1.ID)
	s.Hub.applyCounters(n1.ID, []wsproto.CounterSample{{Proto: "tcp", ListenPort: port, BytesUp: 100, BytesDown: 100}})

	if used := exitUsed(t, d, uid); used != 0 {
		t.Fatalf("absent exit must not meter, got %d", used)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestExitLedger -v`
Expected: FAIL（used_bytes 恒为 0）

- [ ] **Step 3: 改造 applyCounters**

`internal/server/hub.go`，在 `ruleMap` 加载（609-612 行）之后追加批内辅助数据：

```go
	// Landing-exit ledger lookups for this batch: which (owner, host, port)
	// triples are present landing exits, and each rule's final hop position —
	// the only hop whose bytes reach the exit ledger, since middle hops target
	// system relay addresses. On a load error the batch skips exit metering
	// entirely (under-counting beats mis-counting).
	ownerSet := map[int64]bool{}
	for _, r := range ruleMap {
		if r.OwnerID.Valid {
			ownerSet[r.OwnerID.Int64] = true
		}
	}
	ownerIDs := make([]int64, 0, len(ownerSet))
	for id := range ownerSet {
		ownerIDs = append(ownerIDs, id)
	}
	exitSet, err := db.PresentLandingExitSet(h.DB, ownerIDs)
	if err != nil {
		log.Printf("hub: node %d load landing exit set: %v", nodeID, err)
		exitSet = nil
	}
	maxPos, err := db.MaxHopPositions(h.DB, ruleIDs)
	if err != nil {
		log.Printf("hub: node %d load hop positions: %v", nodeID, err)
		exitSet = nil
	}
```

累加器声明（634 行 `userAdds := map[int64]int64{}` 之后）：

```go
	exitAdds := map[db.UserExitKey]int64{}
```

样本循环内，`r := ruleMap[rh.RuleID]`（653 行）之后插入：

```go
		// Exit ledger: final hop only, raw and unweighted — it records real
		// traffic to the destination, independent of billing multipliers and
		// the node's unidirectional setting. Growth must mark the pair touched
		// itself: a downlink-only batch on a unidirectional node bills 0 and
		// would otherwise never reach the quota callback.
		if r != nil && r.OwnerID.Valid && totalDelta > 0 && len(exitSet) > 0 && rh.Position == maxPos[rh.RuleID] {
			key := db.UserExitKey{UserID: r.OwnerID.Int64, Host: r.ExitHost, Port: r.ExitPort}
			if exitSet[key] {
				exitAdds[key] += totalDelta
				touched[userNode{key.UserID, nodeID}] = true
			}
		}
```

flush 条件（728 行）改为：

```go
	if len(hopWrites) > 0 || len(userNodeAdds) > 0 || len(userAdds) > 0 || len(exitAdds) > 0 {
```

flush 事务内 `userAdds` 循环之后追加：

```go
			for k, delta := range exitAdds {
				if !ok {
					break
				}
				// A zero-row hit means the row was flipped absent and deleted
				// between load and flush; dropping one batch is the intent of
				// that deletion.
				if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = used_bytes + ?, updated_at = ? WHERE user_id=? AND host=? AND port=?`,
					delta, time.Now().Unix(), k.UserID, k.Host, k.Port); err != nil {
					log.Printf("hub: user %d exit %s:%d ledger add: %v", k.UserID, k.Host, k.Port, err)
					ok = false
					break
				}
			}
```

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/server/ -run 'TestExitLedger|TestApplyCounters|TestCycleReset' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/hub.go internal/server/landing_exit_metering_test.go
git commit -m "feat(server): meter final-hop raw bytes into landing-exit ledgers"
```

---

### Task 6: enforceExitQuota 接入 OnTrafficUpdate

**Files:**
- Modify: `internal/server/server.go:41-44`（OnTrafficUpdate 接线）+ 文件尾部新增函数
- Test: `internal/server/landing_exit_enforcement_test.go`（追加用例）

**Interfaces:**
- Consumes: `db.ExitsExceedingQuota`、`s.redispatchUserExit`（Task 2/3）
- Produces: `(s *Server) enforceExitQuota(userID int64)`

- [ ] **Step 1: 追加失败测试**

`internal/server/landing_exit_enforcement_test.go` 追加：

```go
// enforceExitQuota is a smoke path: the exclusion itself is covered above;
// here we assert the callback wiring finds the exceeded exit and survives
// dispatching to unconnected nodes.
func TestEnforceExitQuota(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)
	n1, _ := db.CreateNode(d, "eq1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)
	createTestRuleDirectNode(t, d, uid, n1.ID)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 100, 100)

	s, _ := New(d)
	s.enforceExitQuota(uid)

	keys, _ := db.ExitsExceedingQuota(d, uid)
	if len(keys) != 1 {
		t.Fatalf("exceeded exit expected, got %+v", keys)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestEnforceExitQuota -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 实现并接线**

`internal/server/server.go` 在 `enforcePerNodeQuota` 之后追加：

```go
// enforceExitQuota re-pushes the nodes carrying rules whose landing-exit
// ledger reached quota, so ActiveRuleHopsForPush drops exactly the rules
// pointed at the exhausted exit.
func (s *Server) enforceExitQuota(userID int64) {
	exceeded, err := db.ExitsExceedingQuota(s.DB, userID)
	if err != nil {
		log.Printf("quota: exit check user %d: %v", userID, err)
		return
	}
	for _, k := range exceeded {
		s.redispatchUserExit(userID, k.Host, k.Port)
	}
}
```

`New()` 中 OnTrafficUpdate（41-44 行）改为：

```go
	hub.OnTrafficUpdate = func(userID int64, nodeID int64) {
		s.enforcePerNodeQuota(userID, nodeID)
		s.enforceUserQuota(userID)
		s.enforceExitQuota(userID)
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run 'TestEnforceExitQuota|TestGlobalQuota' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/landing_exit_enforcement_test.go
git commit -m "feat(server): enforce landing-exit quotas on traffic updates"
```

---

### Task 7: 后台同步循环 + 启动回填

**Files:**
- Modify: `internal/server/server.go`（Server struct、New、新增 enforcer）
- Test: `internal/server/landing_sync_test.go`（追加用例）

**Interfaces:**
- Consumes: `resolveLandingExits`、`syncLandingExits`、`hasLandingSource`、`hasDynamicSource`
- Produces: `(s *Server) landingSyncPass(includeManualOnly bool)`、goroutine `landingSyncEnforcer`

- [ ] **Step 1: 追加失败测试**

`internal/server/landing_sync_test.go` 追加：

```go
func TestLandingSyncPassBackfillsManualUsers(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)

	s.landingSyncPass(true)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 {
		t.Fatalf("startup pass must backfill manual-URI users, got %d rows", len(exits))
	}

	// the periodic pass skips manual-only users (their set changes on save only)
	d.Exec(`DELETE FROM user_landing_exits`)
	s.landingSyncPass(false)
	if exits, _ := db.ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatal("periodic pass must skip manual-only users")
	}
}

func TestLandingSyncPassKeepsSetOnSubFailure(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)
	s.landingSyncPass(true)

	// switching to a failing subscription must not flip the existing rows
	db.SetUserLandingSource(d, uid, "http://127.0.0.1:1/sub", "vless://u@1.2.3.4:443#HK")
	s.landingSyncPass(true)
	exits, _ := db.ListUserLandingExits(d, uid)
	if len(exits) != 1 || !exits[0].Present {
		t.Fatalf("failed resolution must leave the set untouched, got %+v", exits)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestLandingSyncPass -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 实现**

`internal/server/server.go`：Server struct 加字段 `stopLandingSync chan struct{}`（stopCycle 之后）；`New()` 初始化处加 `stopLandingSync: make(chan struct{})`，`go s.cycleResetEnforcer()` 之后加 `go s.landingSyncEnforcer()`。文件内追加：

```go
// landingSyncEnforcer keeps materialized landing-exit sets in step with
// subscription content when no page load resolves them. The first pass runs
// immediately and includes manual-URI users, backfilling existing deployments
// right after upgrade; the table then persists, so later restarts have no
// empty-set window.
func (s *Server) landingSyncEnforcer() {
	s.landingSyncPass(true)
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopLandingSync:
			return
		case <-ticker.C:
			s.landingSyncPass(false)
		}
	}
}

// landingSyncPass syncs every user with a landing source. includeManualOnly
// widens the pass to users without a subscription — their set only changes on
// save, so the periodic pass skips them.
func (s *Server) landingSyncPass(includeManualOnly bool) {
	users, err := db.ListUsers(s.DB)
	if err != nil {
		log.Printf("landing: sync pass list users: %v", err)
		return
	}
	for _, u := range users {
		if !hasLandingSource(u) {
			continue
		}
		if !includeManualOnly && !hasDynamicSource(u) {
			continue
		}
		nodes, ok := s.resolveLandingExits(u, false)
		if !ok {
			continue
		}
		s.syncLandingExits(u, nodes)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestLandingSyncPass -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/landing_sync_test.go
git commit -m "feat(server): periodic landing-exit sync with startup backfill"
```

---

### Task 8: 管理端 API（列表 / 设限额 / 重置 / 删除）

**Files:**
- Modify: `internal/server/landing.go`（四个 handler）
- Modify: `internal/server/server.go:426`（admin 路由组追加四条）
- Test: `internal/server/landing_exit_api_test.go`（新建）

**Interfaces:**
- Consumes: Task 2 的 db 助手、Task 3 的 `resolveLandingExits`/`syncLandingExits`/`redispatchUserExit`
- Produces: 路由 `GET /api/users/{id}/landing-exits`、`POST .../landing-exits/{quota|reset|delete}`

- [ ] **Step 1: 写失败测试**

`internal/server/landing_exit_api_test.go`（沿用 `pernode_quota_api_test.go` 的 `openDB`/`loginAsUser`/`loginAsAdmin`/`itoa` 模式）：

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

func adminPost(t *testing.T, s *Server, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAPILandingExitQuotaLifecycle(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless"}}, "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	// list
	req := httptest.NewRequest("GET", "/api/users/"+itoa(uid)+"/landing-exits", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Exits []db.LandingExit `json:"exits"`
	}
	json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Exits) != 1 || listResp.Exits[0].Host != "1.2.3.4" {
		t.Fatalf("exits = %+v", listResp.Exits)
	}

	// set quota
	rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "1.2.3.4", "port": 443, "quota_bytes": 1073741824})
	if rec.Code != http.StatusOK {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body.String())
	}
	exits, _ := db.ListUserLandingExits(d, uid)
	if exits[0].QuotaBytes != 1073741824 {
		t.Fatalf("quota not stored: %+v", exits[0])
	}

	// negative quota rejected; unknown exit 404
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "1.2.3.4", "port": 443, "quota_bytes": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative quota: %d", rec.Code)
	}
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/quota",
		map[string]any{"host": "nope", "port": 1, "quota_bytes": 1}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown exit: %d", rec.Code)
	}

	// reset
	d.Exec(`UPDATE user_landing_exits SET used_bytes=999 WHERE user_id=?`, uid)
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/reset",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	exits, _ = db.ListUserLandingExits(d, uid)
	if exits[0].UsedBytes != 0 {
		t.Fatalf("reset did not zero: %+v", exits[0])
	}

	// delete refuses present rows, accepts residual ones
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/delete",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusConflict {
		t.Fatalf("delete present: %d", rec.Code)
	}
	db.SyncUserLandingExits(d, uid, nil, "", "")
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/delete",
		map[string]any{"host": "1.2.3.4", "port": 443}); rec.Code != http.StatusOK {
		t.Fatalf("delete residual: %d %s", rec.Code, rec.Body.String())
	}
	if exits, _ = db.ListUserLandingExits(d, uid); len(exits) != 0 {
		t.Fatalf("row should be gone, got %+v", exits)
	}
}

func TestAPILandingExitsRequireAdmin(t *testing.T) {
	d := openDB(t)
	uid, userCookie := loginAsUser(t, d, 10)
	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/users/"+itoa(uid)+"/landing-exits", nil)
	req.AddCookie(userCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("non-admin must be rejected")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestAPILandingExit -v`
Expected: FAIL（404，路由不存在）

- [ ] **Step 3: 实现 handler**

`internal/server/landing.go` 追加（import 增加 `"fmt"`）：

```go
// apiListUserLandingExits returns the user's materialized landing-exit set
// for the admin quota card. ?refresh=1 re-resolves the source first; a failed
// resolution silently serves the existing snapshot.
func (s *Server) apiListUserLandingExits(w http.ResponseWriter, r *http.Request) {
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	target, err := db.GetUserByID(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		if nodes, ok := s.resolveLandingExits(target, true); ok {
			s.syncLandingExits(target, nodes)
		}
	}
	exits, err := db.ListUserLandingExits(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"exits": exits})
}

// exitBody is the shared request shape addressing one exit row.
type exitBody struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	QuotaBytes int64  `json:"quota_bytes"`
}

func (s *Server) apiSetLandingExitQuota(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.QuotaBytes < 0 {
		jsonErr(w, http.StatusBadRequest, "字节数无效")
		return
	}
	updated, present, err := db.SetUserLandingExitQuota(s.DB, id, body.Host, body.Port, body.QuotaBytes)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	// A lowered quota may start excluding immediately; a raised/cleared one
	// lifts the exclusion. Residual rows sit outside the exclusion — no push.
	if present {
		go s.redispatchUserExit(id, body.Host, body.Port)
	}
	db.WriteAudit(s.DB, u.ID, "user.set_exit_quota", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d bytes=%d", body.Host, body.Port, body.QuotaBytes))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiResetLandingExitTraffic(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	updated, present, err := db.ResetUserLandingExitTraffic(s.DB, id, body.Host, body.Port)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	if present {
		go s.redispatchUserExit(id, body.Host, body.Port)
	}
	db.WriteAudit(s.DB, u.ID, "user.reset_exit_traffic", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d", body.Host, body.Port))
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) apiDeleteLandingExit(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body exitBody
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	status, err := db.DeleteUserLandingExit(s.DB, id, body.Host, body.Port)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch status {
	case "notfound":
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	case "present":
		jsonErr(w, http.StatusConflict, "在册出口由同步维护，不可删除")
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.delete_exit", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d", body.Host, body.Port))
	jsonOK(w, map[string]any{"ok": true})
}
```

- [ ] **Step 4: 注册路由**

`internal/server/server.go` admin 组内 `r.Post("/users/{id}/nodes/{nodeID}/reset-traffic", ...)`（426 行）之后追加：

```go
			r.Get("/users/{id}/landing-exits", s.apiListUserLandingExits)
			r.Post("/users/{id}/landing-exits/quota", s.apiSetLandingExitQuota)
			r.Post("/users/{id}/landing-exits/reset", s.apiResetLandingExitTraffic)
			r.Post("/users/{id}/landing-exits/delete", s.apiDeleteLandingExit)
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestAPILandingExit -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/server/landing.go internal/server/server.go internal/server/landing_exit_api_test.go
git commit -m "feat(server): admin endpoints for landing-exit quotas"
```

---

### Task 9: /my/landing-nodes 携带账本 + stale 回退

**Files:**
- Modify: `internal/server/landing.go`（apiMyLandingNodes 重写）
- Test: `internal/server/landing_sync_test.go`（追加用例）

**Interfaces:**
- Consumes: `resolveLandingExits`、`syncLandingExits`、`db.ListUserLandingExits`、`db.PresentLandingExitsForUser`
- Produces: 响应 nodes 元素追加 `quota_bytes`/`used_bytes`/`exceeded`，响应顶层追加 `stale`

- [ ] **Step 1: 追加失败测试**

`internal/server/landing_sync_test.go` 追加：

```go
func TestMyLandingNodesCarriesLedger(t *testing.T) {
	d := openDB(t)
	uid, cookie := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)

	req := httptest.NewRequest("GET", "/api/my/landing-nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []struct {
			Host       string `json:"host"`
			QuotaBytes int64  `json:"quota_bytes"`
			UsedBytes  int64  `json:"used_bytes"`
			Exceeded   bool   `json:"exceeded"`
		} `json:"nodes"`
		Stale bool `json:"stale"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Nodes) != 1 || resp.Stale {
		t.Fatalf("resp = %+v", resp)
	}

	// exhausted ledger surfaces as exceeded
	d.Exec(`UPDATE user_landing_exits SET quota_bytes=100, used_bytes=100 WHERE user_id=?`, uid)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Nodes[0].Exceeded || resp.Nodes[0].UsedBytes != 100 {
		t.Fatalf("ledger not joined: %+v", resp.Nodes[0])
	}
}

func TestMyLandingNodesStaleFallback(t *testing.T) {
	d := openDB(t)
	uid, cookie := loginAsUser(t, d, 10)
	// materialize while healthy, then break the subscription
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless", URI: "vless://u@1.2.3.4:443#HK"}}, "", "")
	d.Exec(`UPDATE users SET landing_sub_url='http://127.0.0.1:1/sub' WHERE id=?`, uid)
	s, _ := New(d)

	req := httptest.NewRequest("GET", "/api/my/landing-nodes?refresh=1", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var resp struct {
		Nodes []struct {
			Host string `json:"host"`
			URI  string `json:"uri"`
		} `json:"nodes"`
		Stale bool `json:"stale"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Stale || len(resp.Nodes) != 1 || resp.Nodes[0].Host != "1.2.3.4" {
		t.Fatalf("stale fallback should serve the snapshot, got %+v", resp)
	}
}
```

（文件 import 需补 `"encoding/json"`、`"net/http/httptest"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestMyLandingNodes -v`
Expected: FAIL

- [ ] **Step 3: 重写 apiMyLandingNodes**

`internal/server/landing.go` 替换 apiMyLandingNodes：

```go
// myLandingNodeView is a landing node plus its exit-ledger fields for the
// user's landing page.
type myLandingNodeView struct {
	landing.Node
	QuotaBytes int64 `json:"quota_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	Exceeded   bool  `json:"exceeded"`
}

// apiMyLandingNodes returns the current user's landing nodes for the
// create-rule picker and the landing-nodes nav page. ?refresh=1 bypasses the
// subscription cache. A failed resolution serves the last materialized
// snapshot (stale=true) — billing classification runs on the snapshot, so the
// list the user sees should match it rather than silently shrink.
func (s *Server) apiMyLandingNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	force := r.URL.Query().Get("refresh") == "1"
	nodes, ok := s.resolveLandingExits(u, force)
	stale := false
	if ok {
		s.syncLandingExits(u, nodes)
	} else {
		stale = true
		if exits, err := db.PresentLandingExitsForUser(s.DB, u.ID); err == nil {
			nodes = make([]landing.Node, 0, len(exits))
			for _, e := range exits {
				nodes = append(nodes, landing.Node{Name: e.Name, Protocol: e.Protocol, Host: e.Host, Port: e.Port, URI: e.URI})
			}
		}
	}
	ledger := map[string]*db.LandingExit{}
	if exits, err := db.ListUserLandingExits(s.DB, u.ID); err == nil {
		for _, e := range exits {
			ledger[net.JoinHostPort(e.Host, strconv.Itoa(e.Port))] = e
		}
	}
	views := make([]myLandingNodeView, 0, len(nodes))
	for _, n := range nodes {
		v := myLandingNodeView{Node: n}
		if e := ledger[net.JoinHostPort(n.Host, strconv.Itoa(n.Port))]; e != nil {
			v.QuotaBytes = e.QuotaBytes
			v.UsedBytes = e.UsedBytes
			v.Exceeded = e.QuotaBytes > 0 && e.UsedBytes >= e.QuotaBytes
		}
		views = append(views, v)
	}
	jsonOK(w, map[string]any{
		"nodes":       views,
		"has_source":  hasLandingSource(u),
		"has_dynamic": hasDynamicSource(u),
		"stale":       stale,
	})
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run 'TestMyLandingNodes|TestResolveLanding|TestSyncLanding' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/landing.go internal/server/landing_sync_test.go
git commit -m "feat(server): landing-nodes response carries exit ledger with stale fallback"
```

---

### Task 10: classifyExit 改由物化表驱动

**Files:**
- Modify: `internal/server/landing.go`（新增 landingIndexFromDB）
- Modify: `internal/server/api.go:1216`、`api.go:1381`、`api.go:2137`、`api.go:2206`
- Test: `internal/server/landing_sync_test.go`（追加用例）

**Interfaces:**
- Consumes: `db.PresentLandingExitsForUser`
- Produces: `(s *Server) landingIndexFromDB(userID int64) map[string]landing.Node`

- [ ] **Step 1: 追加失败测试**

```go
func TestLandingIndexFromDB(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{
		{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless", URI: "vless://u@1.2.3.4:443#HK"},
	}, "", "")
	// absent rows must not classify
	db.SyncUserLandingExits(d, uid, nil, "", "")
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{
		{Host: "5.6.7.8", Port: 8443, Name: "TW", Protocol: "trojan", URI: "trojan://u@5.6.7.8:8443#TW"},
	}, "", "")
	s, _ := New(d)

	idx := s.landingIndexFromDB(uid)
	if _, ok := idx["1.2.3.4:443"]; ok {
		t.Fatal("absent exit must not be in the index")
	}
	n, ok := idx["5.6.7.8:8443"]
	if !ok || n.Name != "TW" || n.URI == "" {
		t.Fatalf("present exit missing or incomplete: %+v", n)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestLandingIndexFromDB -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 实现并替换调用点**

`internal/server/landing.go` 追加：

```go
// landingIndexFromDB builds the exit-classification index from the user's
// materialized landing set — the same table that drives metering and push
// exclusion, so the badge can never disagree with billing. No subscription
// fetch happens on this path.
func (s *Server) landingIndexFromDB(userID int64) map[string]landing.Node {
	exits, err := db.PresentLandingExitsForUser(s.DB, userID)
	if err != nil {
		return nil
	}
	m := make(map[string]landing.Node, len(exits))
	for _, e := range exits {
		key := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		if _, ok := m[key]; !ok {
			m[key] = landing.Node{Name: e.Name, Protocol: e.Protocol, Host: e.Host, Port: e.Port, URI: e.URI}
		}
	}
	return m
}
```

四处调用点替换：
- `api.go:1214-1217`（admin 规则列表 ownerIndex 闭包）：`if u := byID[ownerID]; u != nil { idx = landingIndex(s.landingNodesFor(u, false)) }` → `idx = s.landingIndexFromDB(ownerID)`（闭包内不再需要 byID 查找，保留 map 缓存结构）
- `api.go:1381`（admin 规则详情）：`idx = landingIndex(s.landingNodesFor(u, false))` → `idx = s.landingIndexFromDB(rl.OwnerID.Int64)`
- `api.go:2137`（my 规则列表）：`idx := landingIndex(s.landingNodesFor(u, false))` → `idx := s.landingIndexFromDB(u.ID)`
- `api.go:2206`（my 规则详情）：同上

替换后若 `landingIndex` 仅剩 `apiMyLandingNodes` 相关使用（Task 3 的 dedup 已独立），检查是否仍有调用者；若无调用者则删除该函数及其注释（`go build` 会报未使用）。api.go:1206-1208 的旧注释（"resolved once per owner, subscriptions are cached"）改为说明索引来自物化表。

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

Run: `go test ./internal/server/ -v`
Expected: PASS（classifyExit 单元测试 `landing_test.go` 不受影响——它直接构造索引 map）

- [ ] **Step 5: Commit**

```bash
git add internal/server/landing.go internal/server/api.go internal/server/landing_sync_test.go
git commit -m "feat(server): drive exit classification from the materialized landing set"
```

---

### Task 11: 管理端 UI——落地出口限额卡片

**Files:**
- Modify: `web/src/pages/users/Detail.jsx`（新增 LandingExitQuotaCard + ExitQuotaForm，挂在 LandingSourceForm 之后）

**Interfaces:**
- Consumes: Task 8 的四个端点；`fmtTrafficGB`（`web/src/lib/fmt.js`）；`Badge`/`SensText`（`web/src/components/ui`，Detail.jsx 已 import）
- Produces: 无（叶子 UI）

- [ ] **Step 1: 实现组件**

`web/src/pages/users/Detail.jsx`：确认顶部 import 含 `fmtTrafficGB`（`from '../../lib/fmt'`，缺则补）。在 `LandingSourceForm` 组件定义之后追加：

```jsx
function ExitQuotaForm({ userId, exit, onDone }) {
  const [gb, setGb] = useState(String(Number(((exit.quota_bytes || 0) / 1073741824).toFixed(2))))
  const toast = useToast()
  const submit = async (e) => {
    e.preventDefault()
    const bytes = Math.max(0, Math.round((Number(gb) || 0) * 1073741824))
    try {
      await api.post(`/users/${userId}/landing-exits/quota`, { host: exit.host, port: exit.port, quota_bytes: bytes })
      toast('已设置')
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  return (
    <form onSubmit={submit} className="inline-flex items-center gap-1.5">
      <input className="input-field font-mono" type="number" min="0" step="0.1" value={gb}
        onChange={e => setGb(e.target.value)} style={{ width: 80 }} title="0 = 不限" />
      <span className="text-xs text-ink-mut">GB</span>
      <button type="submit" className="btn-secondary text-xs">设限额</button>
    </form>
  )
}

function LandingExitQuotaCard({ userId, blurred }) {
  const [exits, setExits] = useState(null)
  const [refreshing, setRefreshing] = useState(false)
  const toast = useToast()
  const load = (refresh = false) => {
    if (refresh) setRefreshing(true)
    api.get(`/users/${userId}/landing-exits${refresh ? '?refresh=1' : ''}`)
      .then(d => setExits(d?.exits || []))
      .catch(err => toast(err.message, 'error'))
      .finally(() => setRefreshing(false))
  }
  useEffect(() => { load(false) }, [userId])

  const reset = async (e) => {
    try {
      await api.post(`/users/${userId}/landing-exits/reset`, { host: e.host, port: e.port })
      toast('已重置'); load()
    } catch (err) { toast(err.message, 'error') }
  }
  const del = async (e) => {
    try {
      await api.post(`/users/${userId}/landing-exits/delete`, { host: e.host, port: e.port })
      toast('已删除'); load()
    } catch (err) { toast(err.message, 'error') }
  }

  if (exits === null) return null
  return (
    <div className="card mb-5">
      <div className="card-header">
        <h3 className="text-sm font-bold">落地出口限额</h3>
        <button onClick={() => load(true)} disabled={refreshing} className="btn-secondary text-xs">
          {refreshing ? '刷新中…' : '刷新'}
        </button>
      </div>
      {exits.length === 0 ? (
        <div className="p-5 text-xs text-ink-mut">暂无落地出口——先在上方配置落地节点来源。</div>
      ) : (
        <div className="tbl-scroll">
          <table className="tbl">
            <thead><tr><th>名称</th><th>协议</th><th>地址</th><th>限额</th><th>已用</th><th className="text-right">操作</th></tr></thead>
            <tbody>
              {exits.map((e, i) => {
                const exceeded = e.quota_bytes > 0 && e.used_bytes >= e.quota_bytes
                return (
                  <tr key={i} className={e.present ? '' : 'opacity-50'}>
                    <td className="font-semibold">
                      {e.name || '(未命名)'}
                      {!e.present && <Badge color="gray">已不在来源</Badge>}
                    </td>
                    <td className="font-mono text-xs text-ink-soft">{e.protocol}</td>
                    <td className="font-mono text-xs"><SensText blurred={blurred}>{e.host}:{e.port}</SensText></td>
                    <td><ExitQuotaForm userId={userId} exit={e} onDone={load} /></td>
                    <td className="font-mono text-xs">
                      {fmtTrafficGB(e.used_bytes, e.quota_bytes)}
                      {exceeded && <Badge color="red">已超额</Badge>}
                    </td>
                    <td className="text-right">
                      <button onClick={() => reset(e)} className="text-blue-600 text-xs font-semibold">重置</button>
                      {!e.present && <button onClick={() => del(e)} className="text-red-600 text-xs font-semibold ml-3">删除</button>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
```

在页面 JSX 中 `<LandingSourceForm ... />`（约 129 行）之后插入：

```jsx
        <LandingExitQuotaCard userId={id} blurred={blurred} />
```

若 `Badge` 未在 Detail.jsx import，则从 `'../../components/ui'` 补。

- [ ] **Step 2: 构建验证**

Run: `cd web && npm run build`
Expected: 构建成功无错误

- [ ] **Step 3: Commit**

```bash
git add web/src/pages/users/Detail.jsx
git commit -m "feat(web): admin landing-exit quota card"
```

---

### Task 12: 用户端 UI——已用/总量列、stale 提示、侧边栏入口

**Files:**
- Modify: `web/src/pages/my/LandingNodes.jsx`
- Modify: `web/src/components/Layout.jsx:143-144`（用户导航）

**Interfaces:**
- Consumes: Task 9 响应字段 `quota_bytes`/`used_bytes`/`exceeded`/`stale`；`fmtTrafficGB`
- Produces: 无（叶子 UI）

- [ ] **Step 1: 改 LandingNodes.jsx**

import 增加：

```jsx
import { fmtTrafficGB } from '../../lib/fmt'
```

state 增加（`hasDynamic` 之后）：

```jsx
  const [stale, setStale] = useState(false)
```

`load` 的 then 回调改为：

```jsx
      .then(d => {
        setServerNodes((d?.nodes || []).map(n => ({ ...n, source: 'admin' })))
        setHasDynamic(!!d?.has_dynamic)
        setStale(!!d?.stale)
      })
```

PanelToolbar 内、刷新按钮之前插入 stale 提示：

```jsx
          {stale && <span className="text-xs text-amber-600 ml-2">订阅刷新失败，显示上次结果</span>}
```

表头（73 行）在「地址」与「来源」之间插入「已用/总量」：

```jsx
            <thead><tr><th>名称</th><th>协议</th><th>地址</th><th>已用/总量</th><th>来源</th><th className="text-right">操作</th></tr></thead>
```

行内对应位置（地址 td 之后）插入：

```jsx
                  <td className="font-mono text-xs">
                    {n.source === 'local' ? '—' : (
                      <>
                        {fmtTrafficGB(n.used_bytes, n.quota_bytes)}
                        {n.exceeded && <Badge color="red">已超额</Badge>}
                      </>
                    )}
                  </td>
```

（`Badge` 已在该文件 import。）

- [ ] **Step 2: 加侧边栏入口**

`web/src/components/Layout.jsx` 用户导航区，`<SideLink to="/my/rules" ...>我的规则</SideLink>`（143 行）之后插入：

```jsx
                  {(hasLocalProxies(user.username) || user.has_landing_source) && <SideLink to="/my/landing" icon={<IconProxy />}>落地节点</SideLink>}
```

（显示条件与 144 行「我的代理」一致；`IconProxy` 已定义。）

- [ ] **Step 3: 构建验证**

Run: `cd web && npm run build`
Expected: 构建成功

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/my/LandingNodes.jsx web/src/components/Layout.jsx
git commit -m "feat(web): landing page shows exit usage with sidebar entry"
```

---

### Task 13: 全量回归收尾

**Files:** 无新改动（只验证；发现问题就地修）

- [ ] **Step 1: 全量后端测试**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部 PASS；vet 无告警

- [ ] **Step 2: gofmt 检查**

Run: `gofmt -l internal/ cmd/`
Expected: 无输出

- [ ] **Step 3: 前端构建**

Run: `cd web && npm run build`
Expected: 成功

- [ ] **Step 4: 行为抽查（可选但推荐）**

本地起 server（`go run ./cmd/nft-server`），管理员配置一个含手动 URI 的用户落地来源，确认：用户详情出现「落地出口限额」卡片；设 1GB 限额后 `user_landing_exits` 有值；`/my/landing` 显示「已用/总量」列与侧边栏入口。

- [ ] **Step 5: 若有修复则提交**

```bash
git add -A && git commit -m "fix: regression fixes surfaced by full test sweep"
```

（无修复则跳过。）
