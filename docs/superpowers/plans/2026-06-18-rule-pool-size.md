# Per-Rule Connection-Pool Prewarm Size Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the admin set a per-rule userspace connection-pool prewarm size from the web panel; the value rides the existing `apply_ruleset` push and takes effect on the node without a daemon restart.

**Architecture:** Store a nullable `pool_size` on the rule (NULL=use node default, 0=disable prewarm, N=prewarm N). Carry it on `nft.Rule` so `buildRules` injects it into every hop of the rule and `computeRev` includes it (so a change re-pushes). The daemon's userspace backend computes the effective pool size per listener and recreates the pool on change. UI: a field in the admin create modal and a dedicated pool-size editor on the rule detail page.

**Deviation from spec:** The spec proposed editing via the rule update form. During planning we found `apiUpdateRule` (PUT `/rules/{id}`) rejects an empty `hops` array, and the web `EditRuleCard` does not send hops — so that path is unusable for an isolated pool-size edit. Instead we add a dedicated `POST /rules/{id}/pool-size` endpoint (mirroring the existing `reallocate` endpoint), which is better isolated and avoids the fragile regenerate path.

**Tech Stack:** Go (chi router, database/sql + go-sqlite3, embedded SQL migrations), React 19 + Vite + Tailwind v4.

---

## File Structure

- `internal/db/migrations/0007_rule_pool_size.sql` — **create**: adds nullable `pool_size` column.
- `internal/db/queries.go` — **modify**: `Rule.PoolSize` field, `ruleCols`, `scanRule`.
- `internal/db/rules.go` — **modify**: `CreateRule` insert, new `SetRulePoolSize`, `nptrToNull` helper.
- `internal/db/rules_pool_test.go` — **create**: db round-trip tests.
- `internal/nft/nft.go` — **modify**: `Rule.PoolSize *int` field.
- `internal/server/server.go` — **modify**: `buildRules` sets `PoolSize`; admin route for the new endpoint.
- `internal/server/api.go` — **modify**: `apiCreateRule` accepts `pool_size`; new `apiSetRulePoolSize`.
- `internal/server/poolsize_test.go` — **create**: buildRules / computeRev / endpoint tests.
- `internal/forward/relay.go` — **modify**: `effectivePoolSize` helper.
- `internal/forward/userspace.go` — **modify**: per-listener pool size in `openListener` call + `Reconcile` hot-update.
- `internal/forward/userspace_poolsize_test.go` — **create**: per-rule pool size + hot-update tests.
- `web/src/pages/rules/List.jsx` — **modify**: create-modal pool-size field.
- `web/src/pages/rules/Detail.jsx` — **modify**: pool-size editor card.

---

## Task 1: DB layer — store per-rule pool size

**Files:**
- Create: `internal/db/migrations/0007_rule_pool_size.sql`
- Modify: `internal/db/queries.go` (Rule struct ~line 50, `ruleCols` line 366, `scanRule` ~line 368)
- Modify: `internal/db/rules.go` (`CreateRule` line 250)
- Test: `internal/db/rules_pool_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/db/rules_pool_test.go`:

```go
package db

import "testing"

func TestRulePoolSizeRoundTrip(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Default: no pool_size set -> nil.
	id, err := CreateRule(d, &Rule{NodeID: 1, Name: "r", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	rl, err := GetRule(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if rl.PoolSize != nil {
		t.Fatalf("want nil pool_size, got %v", *rl.PoolSize)
	}

	// Set to 0 (disable) and read back.
	zero := int64(0)
	if err := SetRulePoolSize(d, id, &zero); err != nil {
		t.Fatal(err)
	}
	rl, _ = GetRule(d, id)
	if rl.PoolSize == nil || *rl.PoolSize != 0 {
		t.Fatalf("want pool_size 0, got %v", rl.PoolSize)
	}

	// Set to 8, then clear back to NULL.
	eight := int64(8)
	if err := SetRulePoolSize(d, id, &eight); err != nil {
		t.Fatal(err)
	}
	rl, _ = GetRule(d, id)
	if rl.PoolSize == nil || *rl.PoolSize != 8 {
		t.Fatalf("want pool_size 8, got %v", rl.PoolSize)
	}
	if err := SetRulePoolSize(d, id, nil); err != nil {
		t.Fatal(err)
	}
	rl, _ = GetRule(d, id)
	if rl.PoolSize != nil {
		t.Fatalf("want nil pool_size after clear, got %v", *rl.PoolSize)
	}

	// CreateRule that supplies a value persists it.
	four := int64(4)
	id2, _ := CreateRule(d, &Rule{NodeID: 1, Name: "r2", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, PoolSize: &four})
	rl2, _ := GetRule(d, id2)
	if rl2.PoolSize == nil || *rl2.PoolSize != 4 {
		t.Fatalf("want pool_size 4 on create, got %v", rl2.PoolSize)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestRulePoolSizeRoundTrip -v`
Expected: FAIL — compile error (`Rule.PoolSize` and `SetRulePoolSize` undefined).

- [ ] **Step 3: Create the migration**

Create `internal/db/migrations/0007_rule_pool_size.sql`:

```sql
-- Per-rule userspace connection-pool prewarm size.
-- NULL = use the node default (NFT_FORWARD_POOL_SIZE); 0 = disable prewarm; N = prewarm N.
ALTER TABLE rules ADD COLUMN pool_size INTEGER;
```

- [ ] **Step 4: Add the struct field**

In `internal/db/queries.go`, in `type Rule struct`, add the field after `Comment`:

```go
	Comment         string        `json:"comment"`
	// PoolSize is the userspace connection-pool prewarm size: nil = use the
	// node default, 0 = disable prewarm, N = prewarm N. Only userspace hops
	// read it.
	PoolSize        *int64        `json:"pool_size,omitempty"`
	Disabled        bool          `json:"disabled"`
```

- [ ] **Step 5: Add pool_size to ruleCols and scanRule**

In `internal/db/queries.go`, change `ruleCols` (line 366) to append `pool_size` at the end:

```go
const ruleCols = `id,node_id,owner_id,name,proto,exit_host,exit_port,entry_listen_port,comment,disabled,created_at,pool_size`
```

Change `scanRule` to scan it (NULL-safe) as the last column:

```go
func scanRule(r rowScanner) (*Rule, error) {
	rl := &Rule{}
	var disabled int
	var poolSize sql.NullInt64
	if err := r.Scan(&rl.ID, &rl.NodeID, &rl.OwnerID, &rl.Name, &rl.Proto, &rl.ExitHost, &rl.ExitPort, &rl.EntryListenPort, &rl.Comment, &disabled, &rl.CreatedAt, &poolSize); err != nil {
		return nil, err
	}
	rl.Disabled = disabled == 1
	if poolSize.Valid {
		v := poolSize.Int64
		rl.PoolSize = &v
	}
	return rl, nil
}
```

(`database/sql` is already imported in queries.go via the package; if not, add `"database/sql"`.)

- [ ] **Step 6: Update CreateRule and add SetRulePoolSize + helper**

In `internal/db/rules.go`, change `CreateRule` to persist `pool_size`, and add the helper + setter below it:

```go
// nptrToNull converts an optional int64 into a NULL-able SQL value.
func nptrToNull(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// CreateRule inserts the rule header; hops are written by RegenerateRule.
// entry_listen_port starts at 0 until the first regeneration.
func CreateRule(d DBTX, r *Rule) (int64, error) {
	res, err := d.Exec(`INSERT INTO rules(node_id,owner_id,name,proto,exit_host,exit_port,comment,pool_size,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.NodeID, r.OwnerID, r.Name, r.Proto, r.ExitHost, r.ExitPort, r.Comment, nptrToNull(r.PoolSize), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetRulePoolSize sets (size != nil) or clears (size == nil) the rule's
// userspace connection-pool prewarm size. Clearing stores NULL so the node
// falls back to its NFT_FORWARD_POOL_SIZE default.
func SetRulePoolSize(d DBTX, id int64, size *int64) error {
	_, err := d.Exec(`UPDATE rules SET pool_size=? WHERE id=?`, nptrToNull(size), id)
	return err
}
```

Ensure `internal/db/rules.go` imports `"database/sql"` (add to the import block if missing).

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestRulePoolSizeRoundTrip -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/db/migrations/0007_rule_pool_size.sql internal/db/queries.go internal/db/rules.go internal/db/rules_pool_test.go
git commit -m "feat(db): store per-rule connection-pool prewarm size"
```

---

## Task 2: Carry pool size on nft.Rule and into buildRules

**Files:**
- Modify: `internal/nft/nft.go` (`Rule` struct ~line 25)
- Modify: `internal/server/server.go` (`buildRules` ~line 177)
- Test: `internal/server/poolsize_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/server/poolsize_test.go`:

```go
package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func TestBuildRulesSetsPoolSize(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "n1", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")

	four := int64(4)
	id, _ := db.CreateRule(d, &db.Rule{NodeID: g.ID, Name: "r", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, PoolSize: &four})
	rl, err := db.GetRule(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RegenerateRule(d, rl, []db.HopInput{{NodeID: g.ID, Mode: nft.ModeUserspace}}, nil); err != nil {
		t.Fatal(err)
	}

	hops, _ := db.ActiveRuleHopsForPush(d, g.ID)
	rules := buildRules(d, hops)
	if len(rules) == 0 {
		t.Fatal("no rules built")
	}
	for _, r := range rules {
		if r.PoolSize == nil || *r.PoolSize != 4 {
			t.Fatalf("hop on port %d: want PoolSize 4, got %v", r.SrcPort, r.PoolSize)
		}
	}
}

func TestComputeRevTracksPoolSize(t *testing.T) {
	four := int64(4)
	base := []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "9.9.9.9", DestPort: 443}}
	withPool := []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "9.9.9.9", DestPort: 443, PoolSize: &four}}
	if computeRev(base) == computeRev(withPool) {
		t.Fatal("rev must change when pool size changes")
	}
}
```

NOTE: `computeRev` in `TestComputeRevTracksPoolSize` takes a `*int` via `nft.Rule.PoolSize`, so `four` there must be `int`, not `int64`. Use `four := 4` (an `int`) in that test, and keep `four := int64(4)` only in `TestBuildRulesSetsPoolSize` where it feeds `db.Rule.PoolSize` (`*int64`). Corrected `TestComputeRevTracksPoolSize`:

```go
func TestComputeRevTracksPoolSize(t *testing.T) {
	four := 4
	base := []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "9.9.9.9", DestPort: 443}}
	withPool := []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "9.9.9.9", DestPort: 443, PoolSize: &four}}
	if computeRev(base) == computeRev(withPool) {
		t.Fatal("rev must change when pool size changes")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'TestBuildRulesSetsPoolSize|TestComputeRevTracksPoolSize' -v`
Expected: FAIL — `nft.Rule` has no field `PoolSize`.

- [ ] **Step 3: Add the nft.Rule field**

In `internal/nft/nft.go`, in `type Rule struct`, add after `BandwidthMbps`:

```go
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
	// PoolSize overrides the userspace connection-pool prewarm size for this
	// forward: nil = node default, 0 = disable prewarm, N = prewarm N. Only the
	// userspace data plane reads it. Included in computeRev so a change
	// re-pushes; old daemons ignore the unknown field.
	PoolSize      *int   `json:"pool_size,omitempty"`
```

- [ ] **Step 4: Set PoolSize in buildRules**

In `internal/server/server.go`, inside `buildRules`, the block that fills rule metadata from `ruleMap[rh.RuleID]`:

```go
		if r := ruleMap[rh.RuleID]; r != nil {
			rule.RuleID = r.ID
			rule.RuleName = r.Name
			if r.PoolSize != nil {
				v := int(*r.PoolSize)
				rule.PoolSize = &v
			}
			if r.OwnerID.Valid {
				if u := users[r.OwnerID.Int64]; u != nil {
					rule.OwnerName = u.Username
				}
			}
		}
```

(`computeRev` already marshals `nft.Rule` minus RuleID/RuleName/OwnerName, so `PoolSize` is included automatically — no change needed there.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/server/ -run 'TestBuildRulesSetsPoolSize|TestComputeRevTracksPoolSize' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/nft/nft.go internal/server/server.go internal/server/poolsize_test.go
git commit -m "feat: carry per-rule pool size on nft.Rule into buildRules and rev"
```

---

## Task 3: Apply per-rule pool size in the userspace data plane

**Files:**
- Modify: `internal/forward/relay.go` (add `effectivePoolSize`)
- Modify: `internal/forward/userspace.go` (`Reconcile` ~lines 197 and 209-221)
- Test: `internal/forward/userspace_poolsize_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/forward/userspace_poolsize_test.go`:

```go
package forward

import (
	"testing"

	"nft-forward/internal/nft"
)

func intp(v int) *int { return &v }

func TestEffectivePoolSize(t *testing.T) {
	if got := effectivePoolSize(nft.Rule{}, 4); got != 4 {
		t.Errorf("nil override: want node default 4, got %d", got)
	}
	if got := effectivePoolSize(nft.Rule{PoolSize: intp(0)}, 4); got != 0 {
		t.Errorf("override 0: want 0 (disabled), got %d", got)
	}
	if got := effectivePoolSize(nft.Rule{PoolSize: intp(8)}, 4); got != 8 {
		t.Errorf("override 8: want 8, got %d", got)
	}
}

func TestReconcilePoolSizeControlsPool(t *testing.T) {
	b := &userspaceBackend{listeners: map[int]*listener{}, poolSize: 4}
	defer b.Close()

	// pool_size 0 -> listener has no pool.
	r := nft.Rule{Proto: "tcp", SrcPort: 0, DestIP: "127.0.0.1", DestPort: 9, PoolSize: intp(0)}
	if err := b.Reconcile([]nft.Rule{r}); err != nil {
		t.Fatal(err)
	}
	var port int
	for p := range b.listeners {
		port = p
	}
	if b.listeners[port].pool.Load() != nil {
		t.Fatal("pool_size 0: expected no pool")
	}

	// Raise to 2 on the same port -> pool gets created.
	r.SrcPort = port
	r.PoolSize = intp(2)
	if err := b.Reconcile([]nft.Rule{r}); err != nil {
		t.Fatal(err)
	}
	if b.listeners[port].pool.Load() == nil {
		t.Fatal("pool_size 2: expected a pool")
	}
}
```

NOTE: `SrcPort: 0` makes `net.Listen` pick a free port; we read it back from the listeners map for the second Reconcile. This keeps the test hermetic (no fixed port).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/forward/ -run 'TestEffectivePoolSize|TestReconcilePoolSizeControlsPool' -v`
Expected: FAIL — `effectivePoolSize` undefined.

- [ ] **Step 3: Add effectivePoolSize**

In `internal/forward/relay.go`, add near `targetAddr`:

```go
// effectivePoolSize resolves a rule's userspace connection-pool prewarm size:
// the per-rule override when set, otherwise the node default. 0 disables
// prewarming (every client dials a fresh upstream).
func effectivePoolSize(r nft.Rule, nodeDefault int) int {
	if r.PoolSize != nil {
		return *r.PoolSize
	}
	return nodeDefault
}
```

- [ ] **Step 4: Use it when opening and hot-updating listeners**

In `internal/forward/userspace.go`, in `Reconcile`, change the open-new-listeners loop to pass the effective size:

```go
	var opened []*listener
	for port, r := range desired {
		if _, ok := b.listeners[port]; ok {
			continue
		}
		l, err := openListener(r, effectivePoolSize(r, b.poolSize))
		if err != nil {
			for _, ol := range opened {
				ol.close()
				delete(b.listeners, ol.port)
			}
			return fmt.Errorf("listen tcp/%d: %w", port, err)
		}
		b.listeners[port] = l
		opened = append(opened, l)
	}
```

Then replace the hot-update loop (currently lines ~209-221) with one that recreates the pool when the target address OR the effective pool size changes:

```go
	for port, r := range desired {
		l := b.listeners[port]
		newAddr := targetAddr(r)
		newSize := effectivePoolSize(r, b.poolSize)
		oldTgt := l.tgt.Load()
		l.tgt.Store(&target{addr: newAddr})
		l.lim.Store(makeLimiter(r.BandwidthMbps))

		addrChanged := oldTgt != nil && oldTgt.addr != newAddr
		sizeChanged := l.poolSize != newSize
		if addrChanged || sizeChanged {
			if p := l.pool.Load(); p != nil {
				p.Close()
				l.pool.Store(nil)
			}
			if newSize > 0 {
				l.pool.Store(newConnPool(newAddr, newSize))
			}
			l.poolSize = newSize
		}
	}
```

(`listener.poolSize` already exists and is set in `openListener`; this is the first place it is read for change detection. `l.pool` is an `atomic.Pointer[connPool]`; storing nil makes `handle()` fall back to `dialUpstream`.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/forward/ -run 'TestEffectivePoolSize|TestReconcilePoolSizeControlsPool' -v`
Expected: PASS

- [ ] **Step 6: Run the full forward package to catch regressions**

Run: `go test ./internal/forward/ -v`
Expected: PASS (existing pool/reconcile tests still green)

- [ ] **Step 7: Commit**

```bash
git add internal/forward/relay.go internal/forward/userspace.go internal/forward/userspace_poolsize_test.go
git commit -m "feat(forward): apply per-rule connection-pool prewarm size with hot-update"
```

---

## Task 4: Admin API — accept pool_size on create + dedicated set endpoint

**Files:**
- Modify: `internal/server/api.go` (`apiCreateRule` body struct ~line 686 and rule construction ~line 87; add `apiSetRulePoolSize`)
- Modify: `internal/server/server.go` (admin routes ~line 274)
- Test: `internal/server/poolsize_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/server/poolsize_test.go`:

```go
func TestSetRulePoolSizeEndpoint(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	g, _ := db.CreateNode(d, "n1", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	id, _ := db.CreateRule(d, &db.Rule{NodeID: g.ID, Name: "r", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443})
	rl, _ := db.GetRule(d, id)
	_, _, _ = db.RegenerateRule(d, rl, []db.HopInput{{NodeID: g.ID, Mode: nft.ModeUserspace}}, nil)

	// Set to 8.
	if code := postJSON(t, s, admin, "/api/rules/"+itoa(id)+"/pool-size", map[string]any{"pool_size": 8}); code != 200 {
		t.Fatalf("set 8: status %d", code)
	}
	rl, _ = db.GetRule(d, id)
	if rl.PoolSize == nil || *rl.PoolSize != 8 {
		t.Fatalf("want 8, got %v", rl.PoolSize)
	}

	// Out of range -> 400.
	if code := postJSON(t, s, admin, "/api/rules/"+itoa(id)+"/pool-size", map[string]any{"pool_size": 999}); code != 400 {
		t.Fatalf("out of range: want 400, got %d", code)
	}

	// Clear (null) -> NULL.
	if code := postJSON(t, s, admin, "/api/rules/"+itoa(id)+"/pool-size", map[string]any{"pool_size": nil}); code != 200 {
		t.Fatalf("clear: status %d", code)
	}
	rl, _ = db.GetRule(d, id)
	if rl.PoolSize != nil {
		t.Fatalf("want nil after clear, got %v", *rl.PoolSize)
	}
}

func TestCreateRulePersistsPoolSize(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	g, _ := db.CreateNode(d, "n1", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")

	if code := postJSON(t, s, admin, "/api/rules", map[string]any{
		"node_id": g.ID, "name": "r", "proto": "tcp", "exit": "9.9.9.9:443", "pool_size": 0,
	}); code != 200 {
		t.Fatalf("create: status %d", code)
	}
	rules, _ := db.ListAllRules(d)
	if len(rules) != 1 || rules[0].PoolSize == nil || *rules[0].PoolSize != 0 {
		t.Fatalf("want one rule with pool_size 0, got %+v", rules)
	}
}
```

Add these small test helpers at the top of `internal/server/poolsize_test.go` (after the imports — add `"bytes"`, `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"strconv"` to the import block):

```go
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func postJSON(t *testing.T, s *Server, cookie *http.Cookie, path string, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'TestSetRulePoolSizeEndpoint|TestCreateRulePersistsPoolSize' -v`
Expected: FAIL — route `/api/rules/{id}/pool-size` returns 404/405 and create ignores `pool_size`.

- [ ] **Step 3: Accept pool_size in apiCreateRule**

In `internal/server/api.go`, in `apiCreateRule`'s body struct, add the field:

```go
	var body struct {
		NodeID    int64  `json:"node_id"`
		Name      string `json:"name"`
		Proto     string `json:"proto"`
		Exit      string `json:"exit"`
		EntryPort int    `json:"entry_port"`
		Comment   string `json:"comment"`
		PoolSize  *int   `json:"pool_size"`
		Hops      []struct {
			NodeID int64  `json:"node_id"`
			Mode   string `json:"mode"`
		} `json:"hops"`
	}
```

After `exitHost, exitPort, err := parseExit(...)` validation, add a range check:

```go
	if err := validatePoolSize(body.PoolSize); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
```

In the `rl := &db.Rule{...}` construction, set the field:

```go
	rl := &db.Rule{
		NodeID:   ruleNodeID,
		Name:     name,
		Proto:    proto,
		ExitHost: exitHost,
		ExitPort: exitPort,
		Comment:  strings.TrimSpace(body.Comment),
		PoolSize: poolSizePtr(body.PoolSize),
	}
```

- [ ] **Step 4: Add the validation helper, conversion helper, and the endpoint**

In `internal/server/api.go`, add near the other rule handlers:

```go
// validatePoolSize bounds a per-rule connection-pool prewarm size. nil (unset)
// is allowed and means "use the node default".
func validatePoolSize(p *int) error {
	if p == nil {
		return nil
	}
	if *p < 0 || *p > 64 {
		return fmt.Errorf("预热连接数须在 0–64 之间")
	}
	return nil
}

// poolSizePtr converts the request's *int into the db layer's *int64.
func poolSizePtr(p *int) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

// apiSetRulePoolSize sets or clears a rule's userspace connection-pool prewarm
// size and re-pushes the rule's nodes so the change takes effect immediately.
func (s *Server) apiSetRulePoolSize(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetRule(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	var body struct {
		PoolSize *int `json:"pool_size"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := validatePoolSize(body.PoolSize); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := db.SetRulePoolSize(s.DB, id, poolSizePtr(body.PoolSize)); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hops, _ := db.ListRuleHops(s.DB, id)
	nodeIDs := make([]int64, 0, len(hops))
	for _, h := range hops {
		nodeIDs = append(nodeIDs, h.NodeID)
	}
	db.WriteAudit(s.DB, u.ID, "rule.pool_size", strconv.FormatInt(id, 10), "")
	s.apiDispatchFanout(nodeIDs)
	jsonOK(w, map[string]any{"ok": true})
}
```

(`fmt` is already imported in api.go.)

- [ ] **Step 5: Register the route**

In `internal/server/server.go`, in the admin group, after the reallocate route (~line 274):

```go
			r.Post("/rules/{id}/hops/{pos}/reallocate", s.apiReallocateRuleHop)
			r.Post("/rules/{id}/pool-size", s.apiSetRulePoolSize)
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestSetRulePoolSizeEndpoint|TestCreateRulePersistsPoolSize' -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server/api.go internal/server/server.go internal/server/poolsize_test.go
git commit -m "feat(api): accept pool_size on create and add set-pool-size endpoint"
```

---

## Task 5: Web UI — create-modal field + detail editor

**Files:**
- Modify: `web/src/pages/rules/List.jsx` (`CreateRuleModal` ~lines 108-156)
- Modify: `web/src/pages/rules/Detail.jsx` (add a pool-size card + form)

- [ ] **Step 1: Add the pool-size field to the create modal**

In `web/src/pages/rules/List.jsx`, change `CreateRuleModal`'s initial state to include `pool` and submit it:

```jsx
function CreateRuleModal({ open, onClose, nodes, onDone }) {
  const [form, setForm] = useState({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '', pool: '' })
  const [loading, setLoading] = useState(false)
  const toast = useToast()

  const set = (k, v) => setForm(f => ({ ...f, [k]: v }))

  const submit = async (e) => {
    e.preventDefault()
    setLoading(true)
    try {
      await api.post('/rules', {
        node_id: Number(form.node_id),
        name: form.name,
        proto: form.proto,
        exit: form.exit,
        comment: form.comment || undefined,
        pool_size: form.pool === '' ? undefined : Number(form.pool),
      })
      toast('规则已创建')
      setForm({ node_id: '', name: '', proto: 'tcp', exit: '', comment: '', pool: '' })
      onDone()
    } catch (err) { toast(err.message) } finally { setLoading(false) }
  }
```

And add the input row inside the grid, after the 备注 row:

```jsx
          <label className="fl">备注 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field" value={form.comment} onChange={e => set('comment', e.target.value)} placeholder="备注" />
          <label className="fl">预热连接数 <span className="text-ink-mut font-normal text-xs">(可选)</span></label>
          <input className="input-field font-mono" type="number" min="0" max="64" value={form.pool}
            onChange={e => set('pool', e.target.value)} placeholder="默认 4 · 0=关闭 · 仅 userspace 模式生效" />
```

- [ ] **Step 2: Add the pool-size editor card to the detail page**

In `web/src/pages/rules/Detail.jsx`, render a `<PoolSizeCard>` after `<EditRuleCard>` in the returned JSX:

```jsx
      {/* Edit rule */}
      <EditRuleCard rule={rule} onDone={load} />

      {/* Connection-pool prewarm */}
      <PoolSizeCard rule={rule} onDone={load} />
```

Then add the component at the bottom of the file:

```jsx
function PoolSizeCard({ rule, onDone }) {
  const [pool, setPool] = useState(rule.pool_size == null ? '' : String(rule.pool_size))
  const [saving, setSaving] = useState(false)
  const toast = useToast()

  const submit = async (e) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.post(`/rules/${rule.id}/pool-size`, { pool_size: pool === '' ? null : Number(pool) })
      toast('已保存并重下发')
      onDone()
    } catch (err) { toast(err.message) } finally { setSaving(false) }
  }

  return (
    <div className="card mb-5">
      <div className="card-header"><h3 className="text-sm font-bold">连接池预热</h3>
        <span className="text-xs text-ink-mut">仅对 userspace 模式的跳生效</span></div>
      <div className="p-5">
        <form onSubmit={submit} className="flex items-center gap-3 flex-wrap">
          <input className="input-field font-mono" type="number" min="0" max="64" value={pool}
            onChange={e => setPool(e.target.value)} placeholder="默认 4 · 0=关闭" style={{ maxWidth: 220 }} />
          <button type="submit" disabled={saving} className="btn-primary">保存并重下发</button>
          <span className="text-xs text-ink-mut">留空=用节点默认（{`NFT_FORWARD_POOL_SIZE`}，默认 4）；0=关闭预热</span>
        </form>
      </div>
    </div>
  )
}
```

- [ ] **Step 3: Build the frontend to verify it compiles**

Run: `cd web && npm run build`
Expected: build succeeds (no missing import / syntax errors).

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/rules/List.jsx web/src/pages/rules/Detail.jsx web/dist
git commit -m "feat(web): per-rule connection-pool prewarm field in create modal and detail"
```

---

## Final verification

- [ ] Run `go vet ./...` — expect clean.
- [ ] Run `go test ./...` — expect all packages PASS.
- [ ] Run `cd web && npm run build` — expect success.
- [ ] Manual smoke (optional, on a node with a userspace rule): set pool size to 0 in the detail page, confirm the node re-applies (rev changes) and the listener drops its pool.

---

## Self-Review

**Spec coverage:**
- Data model NULL/0/N — Task 1 (column, scan, CRUD) + Task 4 (validation 0–64). ✓
- Carry on nft.Rule + buildRules per-hop + computeRev — Task 2. ✓
- Daemon per-listener size + hot-update recreate — Task 3. ✓
- Admin UI create + edit (dedicated endpoint, see deviation) — Task 4 (endpoint) + Task 5 (UI). ✓
- Admin-only, user page untouched — endpoint is in the admin route group; no `/my` change. ✓
- Backward compat (NULL → default; old daemon ignores field) — `*int`/`*int64` + omitempty. ✓

**Placeholder scan:** none — every step has concrete code/commands. (Task 2 Step 1 intentionally shows a discarded helper then the corrected final form; the engineer writes the final `TestBuildRulesSetsPoolSize` shown.)

**Type consistency:** `db.Rule.PoolSize *int64`; `nft.Rule.PoolSize *int`; conversion via `poolSizePtr` (req `*int`→db `*int64`) and `int(*r.PoolSize)` (db→nft) in buildRules; helpers `validatePoolSize(*int)`, `nptrToNull(*int64)`, `effectivePoolSize(nft.Rule,int) int`. Consistent across tasks.
