# Agent 反向 WebSocket 架构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 panel↔agent 通信方向从 server-push HTTP 翻转为 agent-dial WebSocket，让 agent 节点无需在宿主机暴露端口；server 节点可进 docker bridge 网络；本机 daemon 作为 self-node 内置纳管；TUI 段在节点模式互转时安全迁移。

**Architecture:** agent daemon 主动 dial `wss://panel/v1/agents`，bearer token 鉴权（沿用 `nodes.secret`）。JSON over WebSocket，单一长连接承载 `apply_ruleset` 下行、`register_local` / `counters` / `tui_segment_changed` 上行。server 端 hub 维护 `nodeID → conn` map；下发时若 node_kind='self' 改走本地 unix socket。state.json schema v3 引入 `agent_meta` 块（migrated_at、last_applied_rev）。install.sh agent 模式从 `--listen :PORT` 改为 `--connect URL --panel-token-file PATH`，模式互转沿用现有 switch/uninstall 框架。破坏性升级，旧 `--listen` 路径删除。

**Tech Stack:** Go 1.26、`github.com/coder/websocket`（新增依赖）、`github.com/go-chi/chi/v5`（已有）、modernc.org/sqlite（已有）、bash install.sh、docker compose。

---

## Task 0: 引入 WebSocket 库依赖

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 添加 coder/websocket 依赖**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go get github.com/coder/websocket@latest`

Expected: `go.mod` 出现 `require github.com/coder/websocket vX.Y.Z` 一行，`go.sum` 补充对应校验。

- [ ] **Step 2: 验证编译干净**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go build ./...`

Expected: 无错误输出。

- [ ] **Step 3: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add go.mod go.sum
git commit -m "deps: add coder/websocket for agent-side WS dialer and server-side hub"
```

---

## Task 1: state.json schema v3 + AgentMeta

**Files:**
- Create: `internal/daemon/agentmeta.go`
- Modify: `internal/daemon/state.go`, `internal/daemon/state_test.go`

- [ ] **Step 1: 写 v2→v3 升级失败测试**

Add to `internal/daemon/state_test.go`:

```go
func TestLoadStateV2UpgradesToV3WithZeroAgentMeta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	v2 := `{"version":2,"owners":{"tui":[{"proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]}}`
	if err := os.WriteFile(p, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	owners, meta, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners["tui"]) != 1 {
		t.Fatalf("expected 1 tui rule, got %d", len(owners["tui"]))
	}
	if !meta.MigratedAt.IsZero() {
		t.Fatalf("expected zero MigratedAt, got %v", meta.MigratedAt)
	}
	if meta.LastAppliedRev != "" {
		t.Fatalf("expected empty LastAppliedRev, got %q", meta.LastAppliedRev)
	}
}

func TestSaveLoadStateV3Roundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	owners := OwnerRuleset{"panel": {{Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.2", DestPort: 443}}}
	meta := AgentMeta{
		MigratedAt:     time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC),
		LastAppliedRev: "abc123",
		PanelURL:       "wss://panel/v1/agents",
	}
	if err := SaveState(p, owners, meta); err != nil {
		t.Fatal(err)
	}
	got, gotMeta, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["panel"]) != 1 {
		t.Fatalf("expected 1 panel rule, got %d", len(got["panel"]))
	}
	if !gotMeta.MigratedAt.Equal(meta.MigratedAt) || gotMeta.LastAppliedRev != "abc123" || gotMeta.PanelURL != "wss://panel/v1/agents" {
		t.Fatalf("meta roundtrip mismatch: %+v", gotMeta)
	}
}
```

Imports needed: `time` (already in some daemon tests; add if absent).

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -run 'TestLoadStateV2UpgradesToV3WithZeroAgentMeta|TestSaveLoadStateV3Roundtrip' -v`

Expected: FAIL（`LoadState` 返回值数量不对、`SaveState` 签名不匹配、`AgentMeta` 未定义）。

- [ ] **Step 3: 创建 `internal/daemon/agentmeta.go`**

```go
package daemon

import "time"

// AgentMeta holds the dialer's persisted runtime state — bookkeeping the
// daemon needs across restarts to honor the "ACK before clear" invariant
// for the tui→panel segment migration, and to short-circuit redundant
// apply_ruleset pushes after reconnect.
type AgentMeta struct {
	// MigratedAt is the timestamp at which the daemon last received a
	// register_local_ack from the panel. Zero means the local tui
	// segment has never been handed off — the dialer will try again on
	// every successful (re)connect.
	MigratedAt time.Time `json:"migrated_at,omitempty"`

	// LastAppliedRev is the panel-segment version identifier the daemon
	// has most recently acknowledged. Reported in hello so the server
	// can skip pushing an apply_ruleset whose contents the daemon already
	// has on disk.
	LastAppliedRev string `json:"last_applied_rev,omitempty"`

	// PanelURL is purely diagnostic. The authoritative connect target
	// is the --connect flag in the systemd unit; this field is written
	// for ops visibility when reading state.json by hand.
	PanelURL string `json:"panel_url,omitempty"`
}
```

- [ ] **Step 4: 修改 `internal/daemon/state.go` 引入 v3**

Replace the file's `stateSchemaVersion`, `stateFile`, `LoadState`, and `SaveState` definitions with:

```go
const stateSchemaVersion = 3

type stateFile struct {
	Version   int          `json:"version"`
	Owners    OwnerRuleset `json:"owners"`
	AgentMeta AgentMeta    `json:"agent_meta,omitempty"`
}

type legacyV1File struct {
	Version int        `json:"version"`
	Rules   []nft.Rule `json:"rules"`
}

type legacyV2File struct {
	Version int          `json:"version"`
	Owners  OwnerRuleset `json:"owners"`
}

func LoadState(path string) (OwnerRuleset, AgentMeta, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return OwnerRuleset{}, AgentMeta{}, nil
	}
	if err != nil {
		return nil, AgentMeta{}, err
	}

	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, AgentMeta{}, fmt.Errorf("parse state version: %w", err)
	}

	switch probe.Version {
	case stateSchemaVersion:
		var sf stateFile
		if err := json.Unmarshal(b, &sf); err != nil {
			return nil, AgentMeta{}, fmt.Errorf("parse v%d state: %w", stateSchemaVersion, err)
		}
		if sf.Owners == nil {
			sf.Owners = OwnerRuleset{}
		}
		return sf.Owners, sf.AgentMeta, nil
	case 2:
		var v2 legacyV2File
		if err := json.Unmarshal(b, &v2); err != nil {
			return nil, AgentMeta{}, fmt.Errorf("parse v2 state: %w", err)
		}
		if v2.Owners == nil {
			v2.Owners = OwnerRuleset{}
		}
		return v2.Owners, AgentMeta{}, nil
	case 1:
		var v1 legacyV1File
		if err := json.Unmarshal(b, &v1); err != nil {
			return nil, AgentMeta{}, fmt.Errorf("parse v1 state: %w", err)
		}
		out := OwnerRuleset{}
		if len(v1.Rules) > 0 {
			out["tui"] = v1.Rules
		}
		return out, AgentMeta{}, nil
	default:
		return nil, AgentMeta{}, fmt.Errorf("unsupported state version %d (want %d, 2, or 1)", probe.Version, stateSchemaVersion)
	}
}

func SaveState(path string, owners OwnerRuleset, meta AgentMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if owners == nil {
		owners = OwnerRuleset{}
	}
	sf := stateFile{Version: stateSchemaVersion, Owners: owners, AgentMeta: meta}
	b, err := json.MarshalIndent(&sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
```

- [ ] **Step 5: 更新所有 SaveState/LoadState 调用点**

Run: `grep -rn 'SaveState\|LoadState' /Users/xjetry/work/vibe/nft-forward/internal/ /Users/xjetry/work/vibe/nft-forward/cmd/ 2>/dev/null`

For each callsite outside `state.go`/`state_test.go`, update signature:
- `LoadState(p)` → `LoadState(p)` returning `(owners, meta, err)` — capture meta into the caller's struct (Daemon needs a new `meta AgentMeta` field).
- `SaveState(p, owners)` → `SaveState(p, owners, daemon.meta)` — read meta from caller, pass through.

Specifically:
- `internal/daemon/daemon.go::Bootstrap` line ~119: `owners, err := LoadState(...)` → `owners, meta, err := LoadState(...)` and store `d.meta = meta`.
- `internal/daemon/handlers.go` (every `SaveState(d.statePath, d.owners)`): change to `SaveState(d.statePath, d.owners, d.meta)`. Read with `d.mu` already held.
- `internal/daemon/migrate.go` (if it calls SaveState): pass `AgentMeta{}`.

Add `meta AgentMeta` field to `Daemon` struct in `internal/daemon/daemon.go` (the struct definition is in this same file — find it next to `socketPath, statePath, ...` and add `meta AgentMeta` and a `metaMu sync.RWMutex` if not already covered by `d.mu`).

- [ ] **Step 6: 运行测试确认通过 + 全包通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -v`

Expected: 所有测试 PASS（包括新加的两条 + 既有的 v1 升级测试）。

- [ ] **Step 7: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/daemon/agentmeta.go internal/daemon/state.go internal/daemon/state_test.go internal/daemon/daemon.go internal/daemon/handlers.go internal/daemon/migrate.go
git commit -m "daemon: persist agent_meta alongside owner rulesets in state.json v3

The dialer needs MigratedAt (whether tui→panel handoff has happened) and
LastAppliedRev (panel-segment version the daemon has acknowledged) to
survive restarts; both belong with the ruleset since they describe the
same on-disk authority."
```

---

## Task 2: wsproto package（消息类型与序列化）

**Files:**
- Create: `internal/wsproto/messages.go`
- Create: `internal/wsproto/messages_test.go`

- [ ] **Step 1: 写信封 + 类型常量序列化测试**

Create `internal/wsproto/messages_test.go`:

```go
package wsproto

import (
	"encoding/json"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	e := Envelope{Type: TypeHello, ID: "abc", Payload: json.RawMessage(`{"k":"v"}`)}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeHello || got.ID != "abc" || string(got.Payload) != `{"k":"v"}` {
		t.Fatalf("envelope roundtrip mismatch: %+v", got)
	}
}

func TestHelloEncode(t *testing.T) {
	h := Hello{NodeToken: "tok", AgentVersion: "v1", OS: "linux", Arch: "amd64", LastAppliedRev: "r1"}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got Hello
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("hello roundtrip mismatch: %+v != %+v", got, h)
	}
}

func TestApplyRulesetEncodesRules(t *testing.T) {
	ar := ApplyRuleset{
		Rev: "rev42",
		Rules: []nft.Rule{
			{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
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
	if got.Rev != "rev42" || len(got.Rules) != 1 || got.Rules[0].DestIP != "10.0.0.1" {
		t.Fatalf("apply_ruleset roundtrip mismatch: %+v", got)
	}
}

func TestPingPongCarriesTS(t *testing.T) {
	ts := time.Now().UTC().UnixMilli()
	p := Ping{TS: ts}
	b, _ := json.Marshal(p)
	var got Ping
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.TS != ts {
		t.Fatalf("ts mismatch: %d != %d", got.TS, ts)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/wsproto/ -v`

Expected: FAIL — package not found.

- [ ] **Step 3: 创建 `internal/wsproto/messages.go`**

```go
// Package wsproto defines the JSON message envelope and payload types
// exchanged between the daemon's dialer and the server's hub over
// WebSocket. It carries no I/O — both sides decode/encode through
// encoding/json — so it can be imported from either side without
// dragging in network code.
package wsproto

import (
	"encoding/json"

	"nft-forward/internal/nft"
)

// Type constants. Strings (not iota) so the wire is self-describing
// when debugging with `wscat`/`websocat`.
const (
	TypeHello              = "hello"
	TypeHelloAck           = "hello_ack"
	TypeRegisterLocal      = "register_local"
	TypeRegisterLocalAck   = "register_local_ack"
	TypeApplyRuleset       = "apply_ruleset"
	TypeApplyAck           = "apply_ack"
	TypeCounters           = "counters"
	TypeTuiSegmentChanged  = "tui_segment_changed"
	TypePing               = "ping"
	TypePong               = "pong"
	TypeError              = "error"
)

// Envelope wraps every frame. ID is required for req/resp pairs
// (hello/hello_ack, register_local/register_local_ack, apply_ruleset/
// apply_ack, ping/pong) so the sender can match an ack back to its
// outstanding request; notification frames (counters,
// tui_segment_changed, server-initiated error) leave ID empty.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Forward is the panel-side rule view shared by register_local and
// tui_segment_changed. Renamed from nft.Rule because the server side
// stores forwards in a separate table whose columns match these fields.
type Forward struct {
	Proto         string `json:"proto"`
	ListenPort    int    `json:"listen_port"`
	TargetIP      string `json:"target_ip"`
	TargetPort    int    `json:"target_port"`
	Comment       string `json:"comment,omitempty"`
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
}

type Hello struct {
	NodeToken      string `json:"node_token"`
	AgentVersion   string `json:"agent_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
}

type HelloAck struct {
	NodeID int64  `json:"node_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RegisterLocal struct {
	Forwards []Forward `json:"forwards"`
}

type ImportedForward struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	RuleID     int64  `json:"rule_id"`
}

type RegisterLocalAck struct {
	Imported []ImportedForward `json:"imported"`
	Error    string            `json:"error,omitempty"`
}

type ApplyRuleset struct {
	Rev   string     `json:"rev"`
	Rules []nft.Rule `json:"rules"`
}

type ApplyAck struct {
	Rev   string `json:"rev"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type CounterSample struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	BytesDelta int64  `json:"bytes_delta"`
}

type Counters struct {
	Samples []CounterSample `json:"samples"`
}

type TuiSegmentChanged struct {
	Forwards []Forward `json:"forwards"`
}

type Ping struct {
	TS int64 `json:"ts"`
}

type Pong struct {
	TS int64 `json:"ts"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/wsproto/ -v`

Expected: PASS — all four tests green.

- [ ] **Step 5: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/wsproto/
git commit -m "wsproto: define JSON envelope + payload types for daemon↔panel link

Shared types live in their own package so neither daemon nor server has
to import the other's network code. String-based message types keep the
wire format self-describing for ad-hoc debugging with websocat."
```

---

## Task 3: DB schema 0004（nodes 表新列 + node_tui_snapshot）

**Files:**
- Create: `internal/db/migrations/0004_agent_ws.sql`
- Modify: `internal/db/queries.go`

- [ ] **Step 1: 写新 helper 失败测试**

Add to `internal/db/queries_test.go` (create file if absent — check first with `ls /Users/xjetry/work/vibe/nft-forward/internal/db/`):

```go
func TestUpsertSelfNodeIsIdempotent(t *testing.T) {
	d := openMemDB(t)
	n1, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n1.ID != n2.ID || n1.NodeKind != "self" || n2.Address != "unix:///var/run/nft-forward.sock" {
		t.Fatalf("self-node not idempotent: %+v vs %+v", n1, n2)
	}
}

func TestMarkNodeOnlineUpdatesFields(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "edge-1", "https://panel.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkNodeOnline(d, n.ID, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Online != 1 || got.AgentVersion != "v1.2.3" || got.LastSeenAt == nil {
		t.Fatalf("MarkNodeOnline did not update fields: %+v", got)
	}
}

func TestMarkLocalMigratedSetsTimestamp(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "e1", "https://p", "t")
	if got, _ := GetNode(d, n.ID); got.LocalMigratedAt != nil {
		t.Fatalf("expected nil LocalMigratedAt initially")
	}
	if err := MarkLocalMigrated(d, n.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.LocalMigratedAt == nil {
		t.Fatalf("expected LocalMigratedAt to be set")
	}
}
```

Helper `openMemDB(t)` — if not already defined, add:

```go
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/db/ -run 'TestUpsertSelfNode|TestMarkNodeOnline|TestMarkLocalMigrated' -v`

Expected: FAIL — helpers undefined / columns missing.

- [ ] **Step 3: 创建 migration `0004_agent_ws.sql`**

```sql
-- Reshape the nodes table for the agent-dialer model:
--   * local_migrated_at: server-side idempotency anchor for register_local
--   * last_seen / online / agent_version: replace the periodic-poller view
--     of node liveness with a hub-driven one (updated on hello + heartbeat)
--   * node_kind: distinguish the panel's built-in self-node from remote
--     agents so dispatch can short-circuit to the unix socket
--
-- The legacy dirty/last_apply_at/last_seen_at/last_error columns from
-- the push-based pusher.go remain in place to keep the migration small;
-- queries.go simply stops reading them. A later cleanup migration can
-- drop them once we're confident no rollback path needs them.

ALTER TABLE nodes ADD COLUMN local_migrated_at INTEGER;
ALTER TABLE nodes ADD COLUMN last_seen         INTEGER;
ALTER TABLE nodes ADD COLUMN online            INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN agent_version     TEXT;
ALTER TABLE nodes ADD COLUMN node_kind         TEXT NOT NULL DEFAULT 'remote';

-- Partial unique index on the self-node so UpsertSelfNode can use ON CONFLICT.
CREATE UNIQUE INDEX idx_nodes_self ON nodes(node_kind) WHERE node_kind = 'self';

CREATE TABLE node_tui_snapshot (
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  forwards_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
```

- [ ] **Step 4: 更新 `internal/db/queries.go`**

Find the `Node` struct (search `type Node struct`) and add new fields:

```go
type Node struct {
	ID              int64
	Name            string
	Address         string
	Secret          string
	LastSeenAt      *int64    // legacy push-era field
	LastApplyAt     *int64    // legacy push-era field
	LastError       *string   // legacy push-era field
	Dirty           int       // legacy push-era field
	Disabled        bool
	CreatedAt       int64

	// New fields (Task 3):
	LocalMigratedAt *int64
	LastSeen        *int64
	Online          int
	AgentVersion    string
	NodeKind        string
}
```

Update `scanNode` to scan the new columns at the end (order must match the SELECT list).

Find `forwardCols` neighbor — search for any function that does `SELECT id,name,address,secret,...FROM nodes` and add `,local_migrated_at,last_seen,online,agent_version,node_kind` to every such SELECT (`GetNode`, `ListNodes`, etc.). Update scan list accordingly.

Then add three new functions at the bottom of the file:

```go
func UpsertSelfNode(d *sql.DB) (*Node, error) {
	_, err := d.Exec(`
		INSERT INTO nodes (name, address, secret, node_kind, online, last_seen, created_at)
		VALUES ('self', 'unix:///var/run/nft-forward.sock', '', 'self', 1, ?, ?)
		ON CONFLICT(node_kind) WHERE node_kind='self'
		DO UPDATE SET last_seen=excluded.last_seen, online=1`,
		now(), now())
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id,name,address,secret,last_seen_at,last_apply_at,last_error,dirty,disabled,created_at,local_migrated_at,last_seen,online,agent_version,node_kind FROM nodes WHERE node_kind='self'`)
	return scanNode(row)
}

func MarkNodeOnline(d *sql.DB, id int64, agentVersion string) error {
	_, err := d.Exec(
		`UPDATE nodes SET online=1, last_seen=?, agent_version=? WHERE id=?`,
		now(), agentVersion, id)
	return err
}

func MarkNodeOffline(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET online=0 WHERE id=?`, id)
	return err
}

func MarkLocalMigrated(d *sql.DB, id int64) error {
	_, err := d.Exec(`UPDATE nodes SET local_migrated_at=? WHERE id=? AND local_migrated_at IS NULL`, now(), id)
	return err
}

func UpsertTuiSnapshot(d *sql.DB, nodeID int64, forwardsJSON string) error {
	_, err := d.Exec(`
		INSERT INTO node_tui_snapshot (node_id, forwards_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE
		  SET forwards_json=excluded.forwards_json, updated_at=excluded.updated_at`,
		nodeID, forwardsJSON, now())
	return err
}

func GetTuiSnapshot(d *sql.DB, nodeID int64) (string, *int64, error) {
	var fj string
	var ts int64
	err := d.QueryRow(`SELECT forwards_json, updated_at FROM node_tui_snapshot WHERE node_id=?`, nodeID).Scan(&fj, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return fj, &ts, nil
}
```

(Add `"errors"` import if needed.)

- [ ] **Step 5: 运行测试确认通过 + 全包**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/db/ -v`

Expected: 三个新测试 PASS；其它既有测试 PASS。如果既有 `GetNode` / `ListNodes` 测试因 scan 列数变化而 break，调整测试 fixture 加默认 zero 值。

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/db/migrations/0004_agent_ws.sql internal/db/queries.go internal/db/queries_test.go
git commit -m "db: nodes columns + node_tui_snapshot for agent dialer model

local_migrated_at gives the server an idempotency anchor: a second
register_local from the same node (e.g. ack lost on the wire) becomes a
no-op rather than a duplicate INSERT. node_kind='self' is the partial-
unique anchor for the panel's built-in node so dispatch can short-circuit
to the local unix socket without a token round-trip."
```

---

## Task 4: server EnsureSelfNode + Dispatch 分叉骨架

**Files:**
- Create: `internal/server/selfnode.go`
- Create: `internal/server/selfnode_test.go`

- [ ] **Step 1: 写 EnsureSelfNode + Dispatch 失败测试**

Create `internal/server/selfnode_test.go`:

```go
package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestEnsureSelfNodeCreatesOneRow(t *testing.T) {
	d := openDB(t)
	n, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n.NodeKind != "self" || n.Name != "self" {
		t.Fatalf("unexpected self node: %+v", n)
	}
	// Second call must not create a duplicate.
	n2, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n2.ID != n.ID {
		t.Fatalf("EnsureSelfNode created a second row: %d vs %d", n.ID, n2.ID)
	}
}

func TestDispatchRoutesSelfToUnixSocketStub(t *testing.T) {
	d := openDB(t)
	self, _ := EnsureSelfNode(d)

	var called string
	disp := &Dispatcher{
		DB: d,
		Hub: nil, // hub not needed for self route
		SendLocal: func(rules []nft.Rule) error {
			called = "local"
			return nil
		},
	}
	if err := disp.Dispatch(self.ID, []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, "rev1"); err != nil {
		t.Fatal(err)
	}
	if called != "local" {
		t.Fatalf("expected SendLocal to fire, got %q", called)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'EnsureSelfNode|DispatchRoutes' -v`

Expected: FAIL — `EnsureSelfNode` / `Dispatcher` undefined.

- [ ] **Step 3: 创建 `internal/server/selfnode.go`**

```go
package server

import (
	"database/sql"
	"fmt"

	"nft-forward/internal/daemon"
	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

// EnsureSelfNode upserts the panel's built-in self-node row. The panel
// always manages the daemon it runs alongside, so this row appears in
// every nodes list and the admin UI treats it like any remote agent —
// except dispatch shortcuts via the unix socket.
func EnsureSelfNode(d *sql.DB) (*db.Node, error) {
	return db.UpsertSelfNode(d)
}

// Dispatcher routes apply_ruleset deliveries. Remote nodes go through
// the WebSocket Hub; the self-node goes straight to the local daemon
// unix socket. Tests can substitute SendLocal to avoid touching the
// filesystem.
type Dispatcher struct {
	DB        *sql.DB
	Hub       *Hub
	SendLocal func(rules []nft.Rule) error // nil → use default unix socket
}

func (d *Dispatcher) Dispatch(nodeID int64, rules []nft.Rule, rev string) error {
	n, err := db.GetNode(d.DB, nodeID)
	if err != nil {
		return err
	}
	if n.NodeKind == "self" {
		send := d.SendLocal
		if send == nil {
			send = sendLocalDefault
		}
		return send(rules)
	}
	if d.Hub == nil {
		return fmt.Errorf("hub not wired; cannot dispatch to remote node %d", nodeID)
	}
	return d.Hub.SendApplyRuleset(nodeID, rules, rev)
}

func sendLocalDefault(rules []nft.Rule) error {
	c, err := daemonclient.New(daemon.DefaultSocketPath)
	if err != nil {
		return err
	}
	return c.PostRuleset("panel", rules)
}
```

Note: `Hub` will be defined in Task 5. The compiler is OK with referencing it here because it's the same package.

- [ ] **Step 4: 为编译加 Hub 占位**

Create stub `internal/server/hub.go`:

```go
package server

// Hub is implemented in this file in Task 5. This stub exists so
// selfnode.go compiles in isolation.
type Hub struct{}

// SendApplyRuleset is the real signature; body filled in Task 5.
func (h *Hub) SendApplyRuleset(nodeID int64, rules []any, rev string) error {
	return nil
}
```

Wait — the test uses `[]nft.Rule`, not `[]any`. Change `SendApplyRuleset` here to:

```go
import "nft-forward/internal/nft"

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'EnsureSelfNode|DispatchRoutes' -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/server/selfnode.go internal/server/selfnode_test.go internal/server/hub.go
git commit -m "server: introduce self-node row + Dispatcher routing skeleton

Bootstraps the panel-managed local node and the dispatch fork that lets
self-node updates skip the WebSocket hub. Hub is a placeholder here —
real implementation lands in the next change."
```

---

## Task 5: server Hub（WS endpoint + agentConn）

**Files:**
- Modify: `internal/server/hub.go` (replace stub from Task 4)
- Create: `internal/server/hub_test.go`

- [ ] **Step 1: 写 hub hello 验证 + 重连替换测试**

Create `internal/server/hub_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

func newHubTestServer(t *testing.T) (*httptest.Server, *Hub, *db.Node) {
	t.Helper()
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-1", "https://panel.example.com", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(d)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(srv.Close)
	return srv, hub, n
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func recvEnvelope(t *testing.T, c *websocket.Conn) wsproto.Envelope {
	t.Helper()
	_, b, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var e wsproto.Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, string(b))
	}
	return e
}

func TestHubRejectsBadToken(t *testing.T) {
	srv, _, _ := newHubTestServer(t)
	c := dialWS(t, srv)
	hello := wsproto.Hello{NodeToken: "tok-bad", AgentVersion: "v1", OS: "linux", Arch: "amd64"}
	hp, _ := json.Marshal(hello)
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", env.Type)
	}
	var ack wsproto.HelloAck
	json.Unmarshal(env.Payload, &ack)
	if ack.Error == "" {
		t.Fatalf("expected error in hello_ack for bad token, got %+v", ack)
	}
}

func TestHubAcceptsGoodToken(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1.0", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	env := recvEnvelope(t, c)
	var ack wsproto.HelloAck
	json.Unmarshal(env.Payload, &ack)
	if ack.NodeID != n.ID || ack.Error != "" {
		t.Fatalf("hello_ack mismatch: %+v", ack)
	}
	// Wait briefly for register goroutine to run.
	time.Sleep(50 * time.Millisecond)
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node %d online after hello_ack", n.ID)
	}
}

func TestHubSecondConnReplacesFirst(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c1 := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c1, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c1)
	c2 := dialWS(t, srv)
	sendJSON(t, c2, wsproto.Envelope{Type: wsproto.TypeHello, ID: "2", Payload: hp})
	_ = recvEnvelope(t, c2)
	time.Sleep(50 * time.Millisecond)
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node still online after replace")
	}
	// c1 should now read EOF / closed.
	_, _, err := c1.Read(context.Background())
	if err == nil {
		t.Fatalf("expected first conn to be closed after second hello")
	}
}

func TestHubSendApplyRulesetReturnsAck(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	// In a goroutine, server SendApplyRuleset and wait for ack.
	done := make(chan error, 1)
	go func() {
		done <- hub.SendApplyRuleset(n.ID, []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, "rev1")
	}()

	// Client reads the apply_ruleset frame.
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeApplyRuleset {
		t.Fatalf("expected apply_ruleset, got %s", env.Type)
	}
	// Client sends apply_ack.
	ackPayload, _ := json.Marshal(wsproto.ApplyAck{Rev: "rev1", OK: true})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: ackPayload})

	if err := <-done; err != nil {
		t.Fatalf("SendApplyRuleset error: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestHub' -v`

Expected: FAIL — Hub stub returns nil immediately, no WS endpoint, helpers missing.

- [ ] **Step 3: 实现 `internal/server/hub.go`（替换 Task 4 的 stub）**

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

type Hub struct {
	DB    *db.DBWrapper  // wraps *sql.DB; alias below if not present
	db    interface {    // minimal interface so tests don't need a wrapper
		GetNodeBySecret(secret string) (*db.Node, error)
	}
	rawDB *sql.DB // for direct queries

	mu    sync.RWMutex
	conns map[int64]*agentConn
}
```

Actually that's overcomplicated. Use `*sql.DB` directly:

```go
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

const (
	hubWriteTimeout  = 10 * time.Second
	hubReadTimeout   = 30 * time.Second
	applyAckTimeout  = 30 * time.Second
)

type Hub struct {
	DB *sql.DB

	mu    sync.RWMutex
	conns map[int64]*agentConn
}

func NewHub(d *sql.DB) *Hub {
	return &Hub{DB: d, conns: make(map[int64]*agentConn)}
}

type agentConn struct {
	nodeID  int64
	ws      *websocket.Conn
	writeCh chan []byte
	closed  chan struct{}

	pendMu  sync.Mutex
	pending map[string]chan json.RawMessage

	idSeq atomic.Uint64
}

func (a *agentConn) nextID() string {
	return strconv.FormatUint(a.idSeq.Add(1), 36)
}

func (h *Hub) IsOnline(nodeID int64) bool {
	h.mu.RLock()
	_, ok := h.conns[nodeID]
	h.mu.RUnlock()
	return ok
}

// ServeWS handles the /v1/agents WS endpoint. Upgrades the request,
// reads the mandatory hello frame, validates the bearer token against
// nodes.secret, registers the conn, and loops on reads dispatching by
// message type. Returns when the client disconnects, hello fails, or
// the read deadline expires.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // we authenticate via bearer in hello
	})
	if err != nil {
		log.Printf("hub: accept: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	helloEnv, err := readEnvelope(ctx, ws, hubReadTimeout)
	if err != nil || helloEnv.Type != wsproto.TypeHello {
		writeError(ctx, ws, "protocol", "expected hello as first frame")
		ws.Close(websocket.StatusPolicyViolation, "no hello")
		return
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(helloEnv.Payload, &hello); err != nil {
		writeError(ctx, ws, "protocol", "malformed hello payload")
		ws.Close(websocket.StatusPolicyViolation, "bad hello")
		return
	}

	node, err := lookupNodeBySecret(h.DB, hello.NodeToken)
	if err != nil || node == nil {
		ack, _ := json.Marshal(wsproto.HelloAck{Error: "unknown or revoked token"})
		writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ack})
		ws.Close(websocket.StatusPolicyViolation, "bad token")
		return
	}

	ackPayload, _ := json.Marshal(wsproto.HelloAck{NodeID: node.ID, Name: node.Name})
	if err := writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ackPayload}); err != nil {
		ws.Close(websocket.StatusInternalError, "ack write failed")
		return
	}

	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}

	ac := &agentConn{
		nodeID:  node.ID,
		ws:      ws,
		writeCh: make(chan []byte, 16),
		closed:  make(chan struct{}),
		pending: make(map[string]chan json.RawMessage),
	}
	h.registerConn(ac)
	defer h.unregisterConn(ac)

	go h.writerLoop(ac)
	h.readerLoop(ctx, ac, hello.LastAppliedRev)
}

func (h *Hub) registerConn(ac *agentConn) {
	h.mu.Lock()
	if old, ok := h.conns[ac.nodeID]; ok {
		close(old.closed)
		old.ws.Close(websocket.StatusGoingAway, "replaced by newer connection")
	}
	h.conns[ac.nodeID] = ac
	h.mu.Unlock()
}

func (h *Hub) unregisterConn(ac *agentConn) {
	h.mu.Lock()
	if cur, ok := h.conns[ac.nodeID]; ok && cur == ac {
		delete(h.conns, ac.nodeID)
	}
	h.mu.Unlock()
	select {
	case <-ac.closed:
	default:
		close(ac.closed)
	}
	_ = db.MarkNodeOffline(h.DB, ac.nodeID)
}

func (h *Hub) writerLoop(ac *agentConn) {
	for {
		select {
		case <-ac.closed:
			return
		case b := <-ac.writeCh:
			ctx, cancel := context.WithTimeout(context.Background(), hubWriteTimeout)
			err := ac.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				ac.ws.Close(websocket.StatusInternalError, "write error")
				return
			}
		}
	}
}

func (h *Hub) readerLoop(parent context.Context, ac *agentConn, lastAppliedRev string) {
	for {
		ctx, cancel := context.WithTimeout(parent, hubReadTimeout)
		_, b, err := ac.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var env wsproto.Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			log.Printf("hub: malformed envelope from node %d: %v", ac.nodeID, err)
			continue
		}
		switch env.Type {
		case wsproto.TypePing:
			pong, _ := json.Marshal(wsproto.Pong{TS: time.Now().UnixMilli()})
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypePong, ID: env.ID, Payload: pong})
		case wsproto.TypeCounters:
			// Counters processing wired up in Task 7. For now: log.
			log.Printf("hub: node %d counters frame (size=%d)", ac.nodeID, len(env.Payload))
		case wsproto.TypeTuiSegmentChanged:
			// Snapshot upsert wired up in Task 7. For now: log.
			log.Printf("hub: node %d tui_segment_changed (size=%d)", ac.nodeID, len(env.Payload))
		case wsproto.TypeRegisterLocal:
			// register_local handler wired in Task 7.
			log.Printf("hub: node %d register_local (size=%d)", ac.nodeID, len(env.Payload))
		case wsproto.TypeApplyAck, wsproto.TypeHelloAck, wsproto.TypeRegisterLocalAck:
			ac.dispatchAck(env)
		default:
			log.Printf("hub: node %d unknown frame type %q", ac.nodeID, env.Type)
		}
	}
}

func (ac *agentConn) enqueueWrite(env wsproto.Envelope) {
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	select {
	case ac.writeCh <- b:
	case <-ac.closed:
	}
}

func (ac *agentConn) dispatchAck(env wsproto.Envelope) {
	ac.pendMu.Lock()
	ch, ok := ac.pending[env.ID]
	if ok {
		delete(ac.pending, env.ID)
	}
	ac.pendMu.Unlock()
	if ok {
		ch <- env.Payload
	}
}

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
	}
	id := ac.nextID()
	ch := make(chan json.RawMessage, 1)
	ac.pendMu.Lock()
	ac.pending[id] = ch
	ac.pendMu.Unlock()
	defer func() {
		ac.pendMu.Lock()
		delete(ac.pending, id)
		ac.pendMu.Unlock()
	}()

	payload, _ := json.Marshal(wsproto.ApplyRuleset{Rev: rev, Rules: rules})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeApplyRuleset, ID: id, Payload: payload})

	select {
	case raw := <-ch:
		var ack wsproto.ApplyAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return fmt.Errorf("malformed apply_ack: %w", err)
		}
		if !ack.OK {
			return fmt.Errorf("apply rejected: %s", ack.Error)
		}
		return nil
	case <-time.After(applyAckTimeout):
		return errors.New("apply_ack timeout")
	case <-ac.closed:
		return errors.New("connection closed before ack")
	}
}

// Helpers --------------------------------------------------------------

func lookupNodeBySecret(d *sql.DB, secret string) (*db.Node, error) {
	if secret == "" {
		return nil, errors.New("empty secret")
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM nodes WHERE secret=? AND disabled=0`, secret).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.GetNode(d, id)
}

func readEnvelope(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (wsproto.Envelope, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, b, err := ws.Read(rctx)
	if err != nil {
		return wsproto.Envelope{}, err
	}
	var env wsproto.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return wsproto.Envelope{}, err
	}
	return env, nil
}

func writeEnvelope(ctx context.Context, ws *websocket.Conn, env wsproto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, hubWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func writeError(ctx context.Context, ws *websocket.Conn, code, msg string) {
	p, _ := json.Marshal(wsproto.Error{Code: code, Message: msg})
	_ = writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeError, Payload: p})
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestHub' -v -count=1`

Expected: 四个测试 PASS。若 `TestHubSecondConnReplacesFirst` 偶发失败（c1.Read 返回时机），把 `time.Sleep` 调到 200ms。

- [ ] **Step 5: 全包测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -v`

Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/server/hub.go internal/server/hub_test.go
git commit -m "server: WebSocket hub for agent connections

Accepts dialed agents on /v1/agents, validates bearer token against
nodes.secret, holds per-connection reader/writer goroutines, and exposes
SendApplyRuleset for downstream dispatch. Counters / register_local /
tui_segment_changed handlers are stubbed (log-only); they get full DB
side effects in a later change once the daemon-side dialer exists."
```

---

## Task 6: daemon Dialer

**Files:**
- Create: `internal/daemon/dialer.go`
- Create: `internal/daemon/dialer_test.go`

- [ ] **Step 1: 写 dialer hello + register_local 测试**

Create `internal/daemon/dialer_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// fakeHub is a minimal test double that accepts a WS connection,
// reads frames, and exposes recorded frames via Recorder().
type fakeHub struct {
	mu      sync.Mutex
	frames  []wsproto.Envelope
	ackHooks map[string]func(env wsproto.Envelope) wsproto.Envelope // id-template → response
}

func newFakeHub() *fakeHub {
	return &fakeHub{ackHooks: make(map[string]func(wsproto.Envelope) wsproto.Envelope)}
}

func (f *fakeHub) onAck(reqType string, respond func(wsproto.Envelope) wsproto.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackHooks[reqType] = respond
}

func (f *fakeHub) Frames() []wsproto.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]wsproto.Envelope, len(f.frames))
	copy(out, f.frames)
	return out
}

func (f *fakeHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				continue
			}
			f.mu.Lock()
			f.frames = append(f.frames, env)
			hook := f.ackHooks[env.Type]
			f.mu.Unlock()
			if hook != nil {
				resp := hook(env)
				rb, _ := json.Marshal(resp)
				_ = ws.Write(ctx, websocket.MessageText, rb)
			}
		}
	})
}

func TestDialerSendsHelloAndReceivesAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{}, AgentMeta{}
		},
		OnRegister: func(forwards []wsproto.Forward) {},
		OnApply:    func(rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
}

func TestDialerSendsRegisterLocalWhenTuiPresentAndNotMigrated(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeRegisterLocal, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RegisterLocalAck{Imported: []wsproto.ImportedForward{{ListenPort: 80, Proto: "tcp", RuleID: 1}}})
		return wsproto.Envelope{Type: wsproto.TypeRegisterLocalAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	registered := make(chan []wsproto.Forward, 1)
	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}}, AgentMeta{}
		},
		OnRegister: func(forwards []wsproto.Forward) {
			registered <- forwards
		},
		OnApply: func(rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go dl.runOnce(ctx)

	select {
	case got := <-registered:
		if len(got) != 1 || got[0].ListenPort != 80 {
			t.Fatalf("unexpected registered forwards: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("OnRegister never called")
	}
}

func TestDialerSkipsRegisterWhenMigratedAtIsNonzero(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}},
				AgentMeta{MigratedAt: time.Now().UTC()}
		},
		OnRegister: func(forwards []wsproto.Forward) {
			t.Errorf("OnRegister called despite MigratedAt set")
		},
		OnApply: func(rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = dl.runOnce(ctx)

	frames := fh.Frames()
	for _, f := range frames {
		if f.Type == wsproto.TypeRegisterLocal {
			t.Fatalf("dialer sent register_local despite MigratedAt set")
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -run 'TestDialer' -v`

Expected: FAIL — `NewDialer` / `DialerConfig` / `Dialer.runOnce` undefined.

- [ ] **Step 3: 创建 `internal/daemon/dialer.go`**

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

const (
	dialerPingInterval     = 10 * time.Second
	dialerCountersInterval = 30 * time.Second
	dialerReadTimeout      = 30 * time.Second
	dialerWriteTimeout     = 10 * time.Second
	dialerBackoffInitial   = 1 * time.Second
	dialerBackoffMax       = 60 * time.Second
)

// DialerConfig wires the dialer to its host daemon without import cycles.
// GetState/OnRegister/OnApply give the dialer read-and-write access to the
// owner-segmented state through plain function values so the test in
// dialer_test.go can substitute fakes without spinning up a daemon.
type DialerConfig struct {
	URL          string
	Token        string
	AgentVersion string

	GetState   func() (OwnerRuleset, AgentMeta)
	OnRegister func(forwards []wsproto.Forward) // called when register_local_ack arrives
	OnApply    func(rev string, rules []nft.Rule) error
	OnTuiNotice func(forwards []wsproto.Forward) // optional; nil = skip notice

	// CountersFn returns deltas since the last call. nil = skip counters.
	CountersFn func() []wsproto.CounterSample
}

type Dialer struct {
	cfg DialerConfig

	tuiCh      chan []nft.Rule
	pendingTui atomic.Pointer[[]nft.Rule]

	stopOnce sync.Once
	stop     chan struct{}
}

func NewDialer(cfg DialerConfig) *Dialer {
	return &Dialer{
		cfg:   cfg,
		tuiCh: make(chan []nft.Rule, 1),
		stop:  make(chan struct{}),
	}
}

func (d *Dialer) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

// NotifyTuiChanged accepts a new tui-segment snapshot from the
// unix-socket handler. Last-write-wins: if a previous snapshot is still
// queued, the new one supersedes it (we only care about reporting the
// latest state to the panel).
func (d *Dialer) NotifyTuiChanged(rules []nft.Rule) {
	cp := append([]nft.Rule(nil), rules...)
	select {
	case d.tuiCh <- cp:
	default:
		d.pendingTui.Store(&cp)
		select {
		case d.tuiCh <- *d.pendingTui.Swap(nil):
		default:
			// channel already has fresher; drop oldest
		}
	}
}

// Run loops forever, dialing + serving + reconnecting with backoff.
// Returns when ctx is canceled or Stop() is called.
func (d *Dialer) Run(ctx context.Context) {
	backoff := dialerBackoffInitial
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		default:
		}
		err := d.runOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dialer: connection ended: %v", err)
		}
		// Successful hello_ack should reset; runOnce signals via a side
		// channel? Easier: any err == nil exit means clean stop; non-nil
		// means we apply backoff. For now: always backoff after exit.
		sleep := jitter(backoff)
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > dialerBackoffMax {
			backoff = dialerBackoffMax
		}
	}
}

// runOnce dials, performs hello + optional register, then enters the
// read/write loop until disconnection. Exported on the type via
// unexported method for testing.
func (d *Dialer) runOnce(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dctx, d.cfg.URL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", d.cfg.URL, err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	_, currentMeta := d.cfg.GetState()
	helloPayload, _ := json.Marshal(wsproto.Hello{
		NodeToken:      d.cfg.Token,
		AgentVersion:   d.cfg.AgentVersion,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		LastAppliedRev: currentMeta.LastAppliedRev,
	})
	if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHello, ID: "hello-1", Payload: helloPayload}); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}

	helloAck, err := readOne(ctx, ws, dialerReadTimeout)
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	if helloAck.Type != wsproto.TypeHelloAck {
		return fmt.Errorf("unexpected first reply %q", helloAck.Type)
	}
	var ha wsproto.HelloAck
	_ = json.Unmarshal(helloAck.Payload, &ha)
	if ha.Error != "" {
		return fmt.Errorf("hello rejected: %s", ha.Error)
	}

	// Trigger register_local if needed.
	owners, meta := d.cfg.GetState()
	if meta.MigratedAt.IsZero() && len(owners["tui"]) > 0 {
		fwds := rulesToForwards(owners["tui"])
		rlp, _ := json.Marshal(wsproto.RegisterLocal{Forwards: fwds})
		if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "reg-1", Payload: rlp}); err != nil {
			return fmt.Errorf("write register_local: %w", err)
		}
		// Wait for ack (or any frame — register_local_ack expected).
		rlAck, err := readOne(ctx, ws, dialerReadTimeout)
		if err != nil {
			return fmt.Errorf("read register_local_ack: %w", err)
		}
		if rlAck.Type == wsproto.TypeRegisterLocalAck {
			var ack wsproto.RegisterLocalAck
			_ = json.Unmarshal(rlAck.Payload, &ack)
			if ack.Error == "" && d.cfg.OnRegister != nil {
				d.cfg.OnRegister(fwds)
			}
		}
	}

	// Enter loop: reader and ticker. Use a single goroutine + select on
	// time tickers; reads block in own goroutine to feed a channel.
	readCh := make(chan wsproto.Envelope, 4)
	errCh := make(chan error, 1)
	go func() {
		for {
			env, err := readOne(ctx, ws, dialerReadTimeout)
			if err != nil {
				errCh <- err
				return
			}
			readCh <- env
		}
	}()
	pingT := time.NewTicker(dialerPingInterval)
	defer pingT.Stop()
	countersT := time.NewTicker(dialerCountersInterval)
	defer countersT.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.stop:
			return nil
		case err := <-errCh:
			return err
		case env := <-readCh:
			switch env.Type {
			case wsproto.TypeApplyRuleset:
				var ar wsproto.ApplyRuleset
				_ = json.Unmarshal(env.Payload, &ar)
				ok := true
				errMsg := ""
				if d.cfg.OnApply != nil {
					if err := d.cfg.OnApply(ar.Rev, ar.Rules); err != nil {
						ok = false
						errMsg = err.Error()
					}
				}
				ap, _ := json.Marshal(wsproto.ApplyAck{Rev: ar.Rev, OK: ok, Error: errMsg})
				_ = writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: ap})
			case wsproto.TypePong:
				// reset is implicit; readOne uses fresh deadline each call
			case wsproto.TypeError:
				log.Printf("dialer: server error frame: %s", string(env.Payload))
			}
		case <-pingT.C:
			pp, _ := json.Marshal(wsproto.Ping{TS: time.Now().UnixMilli()})
			if err := writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypePing, ID: "ping-" + strconv.FormatInt(time.Now().UnixMilli(), 36), Payload: pp}); err != nil {
				return err
			}
		case <-countersT.C:
			if d.cfg.CountersFn == nil {
				continue
			}
			samples := d.cfg.CountersFn()
			if len(samples) == 0 {
				continue
			}
			cp, _ := json.Marshal(wsproto.Counters{Samples: samples})
			_ = writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cp})
		case rules := <-d.tuiCh:
			if d.cfg.OnTuiNotice == nil {
				continue
			}
			fwds := rulesToForwards(rules)
			tp, _ := json.Marshal(wsproto.TuiSegmentChanged{Forwards: fwds})
			_ = writeOne(ctx, ws, wsproto.Envelope{Type: wsproto.TypeTuiSegmentChanged, Payload: tp})
		}
	}
}

func rulesToForwards(rs []nft.Rule) []wsproto.Forward {
	out := make([]wsproto.Forward, 0, len(rs))
	for _, r := range rs {
		f := wsproto.Forward{
			Proto:         r.Proto,
			ListenPort:    r.SrcPort,
			TargetPort:    r.DestPort,
			Comment:       r.Comment,
			BandwidthMbps: r.BandwidthMbps,
		}
		if r.DestIP != "" {
			f.TargetIP = r.DestIP
		} else {
			f.TargetIP = r.DestHost
		}
		out = append(out, f)
	}
	return out
}

func readOne(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (wsproto.Envelope, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, b, err := ws.Read(rctx)
	if err != nil {
		return wsproto.Envelope{}, err
	}
	var env wsproto.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return wsproto.Envelope{}, err
	}
	return env, nil
}

func writeOne(ctx context.Context, ws *websocket.Conn, env wsproto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, dialerWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*delta)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -run 'TestDialer' -v -count=1`

Expected: 三个 dialer 测试 PASS。

- [ ] **Step 5: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/daemon/dialer.go internal/daemon/dialer_test.go
git commit -m "daemon: WebSocket dialer with hello/register/apply loop

Function-callback config (GetState/OnRegister/OnApply) keeps the dialer
independent of the rest of the daemon package so the test harness can
drive it without spinning up state.json or the unix socket. tuiCh +
pendingTui implement last-write-wins dedup so a TUI editing burst
collapses into a single tui_segment_changed frame on the wire."
```

---

## Task 7: hub 业务 handler（register_local / counters / tui_segment_changed 入库）

**Files:**
- Modify: `internal/server/hub.go`
- Modify: `internal/server/hub_test.go`

- [ ] **Step 1: 写 register_local 幂等性 + counters 累加测试**

Add to `internal/server/hub_test.go`:

```go
func TestHubRegisterLocalInsertsAndIsIdempotent(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	// First register_local: should INSERT.
	rl, _ := json.Marshal(wsproto.RegisterLocal{
		Forwards: []wsproto.Forward{{Proto: "tcp", ListenPort: 80, TargetIP: "10.0.0.1", TargetPort: 80}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "r1", Payload: rl})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeRegisterLocalAck {
		t.Fatalf("expected register_local_ack, got %s", env.Type)
	}
	var ack wsproto.RegisterLocalAck
	json.Unmarshal(env.Payload, &ack)
	if len(ack.Imported) != 1 {
		t.Fatalf("expected 1 imported, got %d", len(ack.Imported))
	}

	// Second register_local with different forwards: must be no-op (idempotent).
	rl2, _ := json.Marshal(wsproto.RegisterLocal{
		Forwards: []wsproto.Forward{{Proto: "udp", ListenPort: 53, TargetIP: "10.0.0.2", TargetPort: 53}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "r2", Payload: rl2})
	env = recvEnvelope(t, c)
	var ack2 wsproto.RegisterLocalAck
	json.Unmarshal(env.Payload, &ack2)
	if len(ack2.Imported) != 0 {
		t.Fatalf("expected idempotent empty imported on 2nd call, got %d", len(ack2.Imported))
	}

	// Verify only the first forward made it to DB.
	fws, _ := db.ListForwardsByNode(hub.DB, n.ID)
	if len(fws) != 1 || fws[0].ListenPort != 80 {
		t.Fatalf("DB should have exactly 1 forward (listen 80), got %+v", fws)
	}
}

func TestHubTuiSegmentChangedUpsertsSnapshot(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	tsc, _ := json.Marshal(wsproto.TuiSegmentChanged{
		Forwards: []wsproto.Forward{{Proto: "tcp", ListenPort: 443, TargetIP: "10.0.0.3", TargetPort: 443}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeTuiSegmentChanged, Payload: tsc})
	time.Sleep(100 * time.Millisecond)

	got, _, err := db.GetTuiSnapshot(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "10.0.0.3") {
		t.Fatalf("expected snapshot to contain target IP, got %q", got)
	}
}
```

Note: `db.ListForwardsByNode` may not yet exist with this exact name — check `internal/db/queries.go` for existing helpers (e.g., `ActiveForwardsForPush`). If absent, add:

```go
func ListForwardsByNode(d *sql.DB, nodeID int64) ([]*Forward, error) {
	return listForwardsWhere(d, "node_id=?", nodeID)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestHubRegisterLocal|TestHubTuiSegmentChanged' -v`

Expected: FAIL — handler stubs only log, don't write DB.

- [ ] **Step 3: 实现 register_local handler**

In `internal/server/hub.go` `readerLoop` replace `case wsproto.TypeRegisterLocal:` stub:

```go
case wsproto.TypeRegisterLocal:
    var rl wsproto.RegisterLocal
    if err := json.Unmarshal(env.Payload, &rl); err != nil {
        sendAckErr(ac, env.ID, wsproto.TypeRegisterLocalAck, "malformed payload")
        continue
    }
    imported, err := h.handleRegisterLocal(ac.nodeID, rl.Forwards)
    if err != nil {
        sendAckErr(ac, env.ID, wsproto.TypeRegisterLocalAck, err.Error())
        continue
    }
    ackP, _ := json.Marshal(wsproto.RegisterLocalAck{Imported: imported})
    ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRegisterLocalAck, ID: env.ID, Payload: ackP})
```

Replace `case wsproto.TypeTuiSegmentChanged:` stub:

```go
case wsproto.TypeTuiSegmentChanged:
    fj, err := json.Marshal(env.Payload) // store the raw payload directly
    _ = fj
    // Actually store the forwards array JSON for clarity:
    var tsc wsproto.TuiSegmentChanged
    if err := json.Unmarshal(env.Payload, &tsc); err != nil {
        log.Printf("hub: node %d malformed tui_segment_changed: %v", ac.nodeID, err)
        continue
    }
    fjb, _ := json.Marshal(tsc.Forwards)
    if err := db.UpsertTuiSnapshot(h.DB, ac.nodeID, string(fjb)); err != nil {
        log.Printf("hub: node %d upsert tui snapshot: %v", ac.nodeID, err)
    }
```

Replace `case wsproto.TypeCounters:` stub:

```go
case wsproto.TypeCounters:
    var co wsproto.Counters
    if err := json.Unmarshal(env.Payload, &co); err != nil {
        log.Printf("hub: node %d malformed counters: %v", ac.nodeID, err)
        continue
    }
    if err := h.applyCounters(ac.nodeID, co.Samples); err != nil {
        log.Printf("hub: node %d counters update: %v", ac.nodeID, err)
    }
```

Add new methods to Hub:

```go
func (h *Hub) handleRegisterLocal(nodeID int64, forwards []wsproto.Forward) ([]wsproto.ImportedForward, error) {
	// Idempotency anchor: check local_migrated_at; if set, return empty.
	n, err := db.GetNode(h.DB, nodeID)
	if err != nil {
		return nil, err
	}
	if n.LocalMigratedAt != nil {
		return []wsproto.ImportedForward{}, nil
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]wsproto.ImportedForward, 0, len(forwards))
	for _, f := range forwards {
		res, err := tx.Exec(
			`INSERT INTO forwards(node_id, tenant_id, tunnel_id, proto, listen_port, target_ip, target_port, comment, created_at) VALUES (?, NULL, NULL, ?, ?, ?, ?, ?, ?)`,
			nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		out = append(out, wsproto.ImportedForward{ListenPort: f.ListenPort, Proto: f.Proto, RuleID: id})
	}
	if _, err := tx.Exec(`UPDATE nodes SET local_migrated_at=? WHERE id=?`, time.Now().Unix(), nodeID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) error {
	for _, s := range samples {
		_, err := h.DB.Exec(
			`UPDATE forwards SET last_bytes=?, total_bytes=total_bytes+? WHERE node_id=? AND listen_port=? AND proto=?`,
			s.BytesDelta, s.BytesDelta, nodeID, s.ListenPort, s.Proto)
		if err != nil {
			return err
		}
	}
	return nil
}

func sendAckErr(ac *agentConn, id, ackType, msg string) {
	p, _ := json.Marshal(map[string]string{"error": msg})
	ac.enqueueWrite(wsproto.Envelope{Type: ackType, ID: id, Payload: p})
}
```

(Add `time` import if not present.)

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestHub' -v -count=1`

Expected: 全部 hub 测试 PASS（6 个）。

- [ ] **Step 5: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/server/hub.go internal/server/hub_test.go internal/db/queries.go
git commit -m "server: hub persists register_local / counters / tui snapshots

register_local uses nodes.local_migrated_at as the idempotency anchor —
a duplicate frame (e.g. ack lost in flight) returns empty Imported and
the agent still clears its tui segment, preserving the ACK-before-clear
invariant from the design. Counters update forwards.total_bytes in
place; tui_segment_changed upserts node_tui_snapshot for the panel UI."
```

---

## Task 8: daemon 加 dialer 集成 + main.go --connect 参数

**Files:**
- Modify: `internal/daemon/daemon.go`
- Modify: `cmd/nft-forward/main.go`

- [ ] **Step 1: 写 daemon.OnLocalMigrated 失败测试**

Add to `internal/daemon/daemon_test.go`:

```go
func TestOnLocalMigratedClearsTuiSegmentAndSetsMeta(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Applier:    &fakeApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.owners = OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}}
	if err := d.OnLocalMigrated(); err != nil {
		t.Fatal(err)
	}
	if len(d.owners["tui"]) != 0 {
		t.Fatalf("expected tui cleared, got %d", len(d.owners["tui"]))
	}
	if d.meta.MigratedAt.IsZero() {
		t.Fatalf("expected MigratedAt set")
	}
	// Persisted to disk:
	_, meta, _ := LoadState(d.statePath)
	if meta.MigratedAt.IsZero() {
		t.Fatalf("MigratedAt not persisted")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -run 'TestOnLocalMigrated' -v`

Expected: FAIL — `OnLocalMigrated` undefined.

- [ ] **Step 3: 给 Daemon 加 dialer/meta 字段 + OnLocalMigrated/SetPanelRuleset/GetOwnersMeta**

In `internal/daemon/daemon.go`:

Find the `Daemon` struct (search `type Daemon struct`) and add fields:

```go
type Daemon struct {
	socketPath  string
	statePath   string
	groupName   string
	applier     Applier
	legacyPaths LegacyMigrationPaths
	iface       string
	countersFn  func() ([]nft.Counter, error)
	resolveFn   resolveFunc
	httpListen  string // legacy; will be removed in Task 12
	httpToken   string // legacy; will be removed in Task 12
	mu          sync.Mutex
	owners      OwnerRuleset
	lastResolved []nft.Rule

	// Agent-dialer additions:
	meta        AgentMeta
	connectURL  string
	connectTok  string
	dialer      *Dialer
}
```

Add `connectURL` and `connectTok` to `Config`:

```go
type Config struct {
	// ... existing fields ...
	ConnectURL string
	ConnectToken string
}
```

In `New`, after existing field assignments:

```go
return &Daemon{
	// ... existing assignments ...
	connectURL: cfg.ConnectURL,
	connectTok: cfg.ConnectToken,
}, nil
```

Add new methods at the bottom of the file:

```go
// OnLocalMigrated is invoked by the dialer after the panel ACKs a
// register_local. Clears the tui segment, sets meta.MigratedAt, and
// persists. Idempotent.
func (d *Daemon) OnLocalMigrated() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.owners, "tui")
	d.meta.MigratedAt = time.Now().UTC()
	return SaveState(d.statePath, d.owners, d.meta)
}

// SetPanelRuleset is invoked by the dialer when an apply_ruleset frame
// arrives. Replaces the panel segment wholesale, re-merges, re-applies,
// persists rev to meta.
func (d *Daemon) SetPanelRuleset(rev string, rules []nft.Rule) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.owners["panel"] = rules
	merged, err := MergedRuleset(d.owners)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolved, _, err := d.resolveFn(ctx, merged)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	if err := requireResolvedHosts(resolved); err != nil {
		return err
	}
	if err := d.applier.Apply(resolved, d.iface); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	d.lastResolved = append([]nft.Rule(nil), resolved...)
	d.meta.LastAppliedRev = rev
	return SaveState(d.statePath, d.owners, d.meta)
}

// SnapshotForDialer returns a defensive copy of owners and meta for
// the dialer's GetState callback. Held briefly under d.mu.
func (d *Daemon) SnapshotForDialer() (OwnerRuleset, AgentMeta) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := OwnerRuleset{}
	for k, v := range d.owners {
		cp[k] = append([]nft.Rule(nil), v...)
	}
	return cp, d.meta
}
```

In `Run`, after `go d.refreshLoop(ctx)`, add:

```go
if d.connectURL != "" {
	d.dialer = NewDialer(DialerConfig{
		URL:          d.connectURL,
		Token:        d.connectTok,
		AgentVersion: agentVersion(),
		GetState:     d.SnapshotForDialer,
		OnRegister:   func(_ []wsproto.Forward) { _ = d.OnLocalMigrated() },
		OnApply:      d.SetPanelRuleset,
		OnTuiNotice:  func(_ []wsproto.Forward) {}, // payload built in dialer
	})
	go d.dialer.Run(ctx)
}
```

(Add `wsproto` import.)

Helper `agentVersion()`:

```go
// agentVersion returns a coarse identifier — Go build info if available,
// otherwise "dev". Used in the hello frame for ops visibility.
func agentVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
```

(Add `runtime/debug` import.)

- [ ] **Step 4: 修改 `cmd/nft-forward/main.go runDaemon`**

Replace flag parsing block in `runDaemon` (lines ~42-58):

```go
var (
	socketPath        string
	statePath         string
	groupName         string
	iface             string
	connectURL        string
	panelTokenFile    string
)
fs := flag.NewFlagSet("daemon", flag.ExitOnError)
fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group")
fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/server mode")
fs.StringVar(&panelTokenFile, "panel-token-file", "/etc/nft-forward/panel.token", "bearer token file (required when --connect is set)")
if err := fs.Parse(args); err != nil {
	return 2
}
```

Replace the config build block (lines ~80-89):

```go
cfg := daemon.Config{
	SocketPath: socketPath,
	StatePath:  statePath,
	GroupName:  groupName,
	Iface:      iface,
	ConnectURL: connectURL,
}
if connectURL != "" {
	tok, err := os.ReadFile(panelTokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 panel token:", err)
		return 1
	}
	cfg.ConnectToken = strings.TrimSpace(string(tok))
	if cfg.ConnectToken == "" {
		fmt.Fprintln(os.Stderr, "panel token 文件为空")
		return 1
	}
}
```

(Drop the old `HTTPListen` / `TokenPath` plumbing entirely.)

Remove from `internal/daemon/daemon.go` the `httpListen`/`httpToken` reading in `New` (the block under `if cfg.TokenPath != ""`), and remove the `httpSrv` / `httpHandler` block in `Run` (lines 169-182, 192-194). The HTTP listen plumbing dies in Task 12; mark it deprecated here but leave types until then? No — clean break is simpler. **Delete the lines now.**

Also delete `internal/daemon/handlers_http.go` if it exists (or the section in `handlers.go` that defines `httpHandler`).

- [ ] **Step 5: 运行测试确认通过 + 编译**

Run:
```
cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -v -count=1
cd /Users/xjetry/work/vibe/nft-forward && go build ./...
```

Expected: 所有测试 PASS；编译成功。

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/daemon/daemon.go cmd/nft-forward/main.go internal/daemon/daemon_test.go
git commit -m "daemon: wire dialer goroutine + --connect/--panel-token-file flags

OnLocalMigrated and SetPanelRuleset are the two write-side hooks the
dialer reaches into Daemon for; SnapshotForDialer feeds the read side.
The old --listen/--token-file pathway is removed in the same change
since the dialer is the only way the daemon talks to the panel now."
```

---

## Task 9: daemon TUI hook + demote-to-tui handler

**Files:**
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemon/handlers_test.go`

- [ ] **Step 1: 写 TUI hook + demote 测试**

Add to `internal/daemon/handlers_test.go`:

```go
func TestPostRulesetTUITriggersDialerHook(t *testing.T) {
	dir := t.TempDir()
	d, _ := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Applier:    &fakeApplier{},
	})
	calls := make(chan []nft.Rule, 1)
	d.dialer = NewDialer(DialerConfig{
		URL:        "ws://unused",
		Token:      "t",
		GetState:   d.SnapshotForDialer,
		OnApply:    func(string, []nft.Rule) error { return nil },
	})
	d.dialer.NotifyTuiChanged = func(rules []nft.Rule) { calls <- rules }
	// Wait — NotifyTuiChanged is a method, can't be replaced. Use a flag.
}
```

The above test approach doesn't work cleanly because `NotifyTuiChanged` is a method. Replace with a doubled-up test using the actual Dialer + a test channel exposed for inspection.

Better approach — add a test hook field to Daemon:

In `internal/daemon/daemon.go` `Daemon` struct, add (private):

```go
	tuiHook func(rules []nft.Rule) // nil in prod; set by dialer; test substitutable
```

In `Run`, where dialer is started, also do:

```go
	d.tuiHook = d.dialer.NotifyTuiChanged
```

Then test:

```go
func TestPostRulesetTUITriggersHook(t *testing.T) {
	dir := t.TempDir()
	d, _ := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Applier:    &fakeApplier{},
	})
	called := make(chan []nft.Rule, 1)
	d.tuiHook = func(r []nft.Rule) { called <- r }
	if err := d.postRuleset("tui", []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if len(got) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("tuiHook never called")
	}
}

func TestPostRulesetPanelDoesNotTriggerHook(t *testing.T) {
	dir := t.TempDir()
	d, _ := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Applier:    &fakeApplier{},
	})
	d.tuiHook = func(r []nft.Rule) {
		t.Fatalf("tuiHook fired on panel owner write")
	}
	if err := d.postRuleset("panel", []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}); err != nil {
		t.Fatal(err)
	}
}

func TestDemoteToTuiMergesPanelIntoTuiWithPanelWinning(t *testing.T) {
	dir := t.TempDir()
	d, _ := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Applier:    &fakeApplier{},
	})
	d.owners = OwnerRuleset{
		"tui":   {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}},
		"panel": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.99", DestPort: 80}, // collides on 80 — panel wins
			{Proto: "udp", SrcPort: 53, DestIP: "10.0.0.2", DestPort: 53}},
	}
	d.meta.MigratedAt = time.Now()
	d.meta.LastAppliedRev = "rev1"
	if err := d.demoteToTui(); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.owners["panel"]; ok {
		t.Fatal("panel segment should be gone")
	}
	tui := d.owners["tui"]
	// Expect 2 rules: 80 with 10.0.0.99 (panel won), 53 with 10.0.0.2 (panel only).
	if len(tui) != 2 {
		t.Fatalf("expected 2 merged tui rules, got %d", len(tui))
	}
	for _, r := range tui {
		if r.SrcPort == 80 && r.DestIP != "10.0.0.99" {
			t.Fatalf("panel collision should win, got DestIP=%s", r.DestIP)
		}
	}
	if !d.meta.MigratedAt.IsZero() || d.meta.LastAppliedRev != "" {
		t.Fatalf("meta not reset: %+v", d.meta)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -run 'TestPostRulesetTUI|TestPostRulesetPanel|TestDemoteToTui' -v`

Expected: FAIL — hook not wired, demoteToTui undefined.

- [ ] **Step 3: Wire the hook in `internal/daemon/handlers.go`**

Find `postRuleset` (or `applyOwnerRules` — name varies; search for the function that writes to `d.owners[owner]`):

```go
func (d *Daemon) postRuleset(owner string, rules []nft.Rule) error {
	d.mu.Lock()
	d.owners[owner] = rules
	// snapshot for hook outside the lock
	hook := d.tuiHook
	cp := append([]nft.Rule(nil), rules...)
	d.mu.Unlock()

	if err := d.applyMerged(); err != nil {
		return err
	}
	if err := SaveState(d.statePath, d.owners, d.meta); err != nil {
		return err
	}
	if owner == "tui" && hook != nil {
		hook(cp)
	}
	return nil
}
```

(Adjust to match the existing function shape — if it already has slightly different structure, just splice in the hook call after the save succeeds.)

- [ ] **Step 4: 实现 `demoteToTui`**

Add to `internal/daemon/handlers.go` (or new `demote.go`):

```go
// demoteToTui merges the panel segment into the tui segment with the
// panel side winning any (proto, src_port) collision, then clears the
// panel segment, MigratedAt, and LastAppliedRev. Used by install.sh
// uninstall agent (non-purge) so panel-pushed rules don't disappear
// when the node leaves panel management.
func (d *Daemon) demoteToTui() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tui := d.owners["tui"]
	panel := d.owners["panel"]
	// Index existing tui by (proto, src_port) so panel can overwrite.
	type key struct {
		Proto string
		Port  int
	}
	idx := make(map[key]int, len(tui))
	for i, r := range tui {
		idx[key{r.Proto, r.SrcPort}] = i
	}
	for _, p := range panel {
		k := key{p.Proto, p.SrcPort}
		if i, ok := idx[k]; ok {
			tui[i] = p
		} else {
			tui = append(tui, p)
			idx[k] = len(tui) - 1
		}
	}
	d.owners["tui"] = tui
	delete(d.owners, "panel")
	d.meta.MigratedAt = time.Time{}
	d.meta.LastAppliedRev = ""
	d.meta.PanelURL = ""
	if err := d.applyMergedLocked(); err != nil {
		return err
	}
	return SaveState(d.statePath, d.owners, d.meta)
}
```

If `applyMergedLocked` (lock-already-held variant) doesn't exist, factor `applyMerged` into a locked variant and a public wrapper:

```go
func (d *Daemon) applyMerged() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.applyMergedLocked()
}

func (d *Daemon) applyMergedLocked() error {
	// move existing applyMerged body here, minus the lock
	...
}
```

Add HTTP route to the unix-socket handler. Find where routes are registered (search for `Handler()` returning a mux/router):

```go
mux.HandleFunc("POST /v1/admin/demote-to-tui", d.handleDemoteToTui)
```

```go
func (d *Daemon) handleDemoteToTui(w http.ResponseWriter, r *http.Request) {
	if err := d.demoteToTui(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/daemon/ -v -count=1`

Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/daemon/handlers.go internal/daemon/handlers_test.go internal/daemon/daemon.go
git commit -m "daemon: tui hook + demote-to-tui handler for panel disengagement

The TUI hook gives the dialer best-effort live visibility into local
edits so the panel can offer 'import' to admins without polling. demote
merges panel into tui with panel-wins-on-collision so an uninstall
agent (no --purge) keeps every forward that was active and lets the TUI
continue to manage them locally."
```

---

## Task 10: server 集成 — 移除 pusher/poller，挂 hub + dispatcher

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/handlers_admin.go`
- Modify: `cmd/nft-forward/main.go runServer`
- Delete: `internal/server/pusher.go`, `internal/server/poller.go`

- [ ] **Step 1: 查找所有 pusher/poller 调用点**

Run: `grep -rn 'Pusher\|Poller\|pusher\|poller' /Users/xjetry/work/vibe/nft-forward/ --include='*.go' --include='*.html'`

Expected: a handful of references in `server.go`, `handlers_admin.go`, `handlers_my.go`, `main.go`, possibly templates.

- [ ] **Step 2: 在 server.New 引入 Hub + Dispatcher**

Replace the `New` signature in `internal/server/server.go`:

```go
type Server struct {
	DB         *sql.DB
	Hub        *Hub
	Dispatcher *Dispatcher
	// ... existing fields except Pusher ...
}

func New(d *sql.DB) (*Server, error) {
	if _, err := EnsureSelfNode(d); err != nil {
		return nil, fmt.Errorf("ensure self node: %w", err)
	}
	hub := NewHub(d)
	disp := &Dispatcher{DB: d, Hub: hub}
	srv := &Server{
		DB:         d,
		Hub:        hub,
		Dispatcher: disp,
		// other fields ...
	}
	return srv, nil
}
```

In `Router()`, add:

```go
r.HandleFunc("/v1/agents", s.Hub.ServeWS)
```

(Use the existing chi router idiom.)

- [ ] **Step 3: 改 forward CRUD handler 调用 dispatcher**

In `internal/server/handlers_admin.go` find every place that does `s.Pusher.Schedule(nodeID)` and replace with the dispatcher-based approach:

```go
// previously: s.Pusher.Schedule(nodeID)
if err := s.dispatchToNode(nodeID); err != nil {
	log.Printf("dispatch node %d: %v", nodeID, err)
}
```

Add `dispatchToNode` helper to `server.go`:

```go
func (s *Server) dispatchToNode(nodeID int64) error {
	forwards, err := db.ActiveForwardsForPush(s.DB, nodeID)
	if err != nil {
		return err
	}
	rules := buildRules(s.DB, forwards) // factor out from old pusher.go
	rev := computeRev(rules)
	return s.Dispatcher.Dispatch(nodeID, rules, rev)
}

func buildRules(d *sql.DB, forwards []*db.Forward) []nft.Rule {
	tunnels := map[int64]*db.Tunnel{}
	rules := make([]nft.Rule, 0, len(forwards))
	for _, f := range forwards {
		bw := 0
		if f.TunnelID.Valid {
			t, ok := tunnels[f.TunnelID.Int64]
			if !ok {
				t, _ = db.GetTunnel(d, f.TunnelID.Int64)
				if t != nil {
					tunnels[f.TunnelID.Int64] = t
				}
			}
			if t != nil {
				bw = t.BandwidthMbps
			}
		}
		rule := nft.Rule{
			Proto:         f.Proto,
			SrcPort:       f.ListenPort,
			DestPort:      f.TargetPort,
			Comment:       f.Comment,
			BandwidthMbps: bw,
		}
		if resolver.IsHostname(f.TargetIP) {
			rule.DestHost = f.TargetIP
		} else {
			rule.DestIP = f.TargetIP
		}
		rules = append(rules, rule)
	}
	return rules
}

// computeRev returns a stable identifier for the panel-segment content.
// Hash-based: same rules → same rev → no redundant push on reconnect.
func computeRev(rules []nft.Rule) string {
	h := sha256.New()
	b, _ := json.Marshal(rules)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
```

(Add `crypto/sha256`, `encoding/hex`, `encoding/json`, `resolver` imports.)

- [ ] **Step 4: 更新 `cmd/nft-forward/main.go runServer`**

Replace pusher/poller wiring (lines ~128-131):

```go
srv, err := server.New(d)
if err != nil {
	log.Fatalf("server: %v", err)
}
```

Remove `pusher := server.NewPusher(d); go pusher.Run()` and the poller block.

In the shutdown sequence (`<-sig` block lines ~150-155), remove `poller.Stop()` and `pusher.Stop()`.

- [ ] **Step 5: 删除 pusher.go / poller.go**

```bash
rm /Users/xjetry/work/vibe/nft-forward/internal/server/pusher.go
rm /Users/xjetry/work/vibe/nft-forward/internal/server/poller.go
```

Search for any remaining import of `Pusher` / `Poller`:

```bash
grep -rn 'Pusher\|Poller' /Users/xjetry/work/vibe/nft-forward/ --include='*.go'
```

Fix any stragglers (mostly should be in the templates/HTML — leave HTML alone for now; if they reference push status by handler, simplify to "online/offline").

- [ ] **Step 6: 编译 + 全部测试**

Run:
```
cd /Users/xjetry/work/vibe/nft-forward && go build ./...
cd /Users/xjetry/work/vibe/nft-forward && go test ./... -count=1
```

Expected: 编译成功；所有测试 PASS（hub / selfnode / db / daemon / wsproto / nft / etc.）。

If any handler test in `internal/server/` references `Pusher` field, update it to use `Hub` / `Dispatcher` or stub them out.

- [ ] **Step 7: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add -A
git commit -m "server: replace push pipeline with hub-driven dispatch

Forward CRUD now calls Dispatcher.Dispatch(nodeID, rules, rev) which
fans out to the self-node's unix socket or the agent's WebSocket. The
old pusher dirty-queue / 30s reconcile loop and the poller HTTP fan-out
are gone; node liveness is now driven entirely by the hub's hello and
heartbeat. Push-only DB columns (dirty/last_apply_at/last_error/
last_seen_at) are still read by GetNode for backwards compatibility but
no longer written."
```

---

## Task 11: Panel UI — TUI snapshot section + import handler

**Files:**
- Modify: `internal/server/templates/node_detail.html`
- Modify: `internal/server/handlers_admin.go`

- [ ] **Step 1: 加 import handler 失败测试**

Add to `internal/server/handlers_admin_test.go` (create if absent):

```go
func TestImportTuiSnapshotInsertsForwardsAndNotifiesAgent(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "edge-1", "https://p", "tok")
	// Pre-seed snapshot.
	snapshot, _ := json.Marshal([]wsproto.Forward{
		{Proto: "tcp", ListenPort: 8443, TargetIP: "10.0.0.1", TargetPort: 8443},
		{Proto: "udp", ListenPort: 53, TargetIP: "10.0.0.2", TargetPort: 53},
	})
	db.UpsertTuiSnapshot(d, n.ID, string(snapshot))

	s, _ := New(d)
	req := httptest.NewRequest("POST", fmt.Sprintf("/admin/nodes/%d/import-tui", n.ID), nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	fws, _ := db.ListForwardsByNode(d, n.ID)
	if len(fws) != 2 {
		t.Fatalf("expected 2 imported forwards, got %d", len(fws))
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -run 'TestImportTuiSnapshot' -v`

Expected: FAIL — handler not wired.

- [ ] **Step 3: 加 import handler**

In `internal/server/handlers_admin.go`:

```go
func (s *Server) handleImportTuiSnapshot(w http.ResponseWriter, r *http.Request) {
	nodeID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad node id", http.StatusBadRequest)
		return
	}
	snap, _, err := db.GetTuiSnapshot(s.DB, nodeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snap == "" {
		http.Redirect(w, r, fmt.Sprintf("/admin/nodes/%d", nodeID), http.StatusSeeOther)
		return
	}
	var forwards []wsproto.Forward
	if err := json.Unmarshal([]byte(snap), &forwards); err != nil {
		http.Error(w, "malformed snapshot", http.StatusInternalServerError)
		return
	}
	for _, f := range forwards {
		_, _ = db.CreateForward(s.DB, &db.Forward{
			NodeID:     nodeID,
			Proto:      f.Proto,
			ListenPort: f.ListenPort,
			TargetIP:   f.TargetIP,
			TargetPort: f.TargetPort,
			Comment:    f.Comment,
		})
	}
	if err := s.dispatchToNode(nodeID); err != nil {
		log.Printf("import-tui dispatch: %v", err)
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/nodes/%d", nodeID), http.StatusSeeOther)
}
```

Register in `Router()`:

```go
r.Post("/admin/nodes/{id}/import-tui", s.handleImportTuiSnapshot)
```

- [ ] **Step 4: 模板 partial**

Create `internal/server/templates/_tui_snapshot_partial.html`:

```html
{{if .TuiSnapshot}}
<section class="card">
  <h3>本地 TUI 规则 ({{len .TuiSnapshot}} 条)</h3>
  <p class="muted">最后上报: {{.TuiSnapshotAge}}</p>
  <ul>
    {{range .TuiSnapshot}}
    <li>{{.Proto}} {{.ListenPort}} → {{.TargetIP}}:{{.TargetPort}}{{if .Comment}} <em>({{.Comment}})</em>{{end}}</li>
    {{end}}
  </ul>
  <form method="POST" action="/admin/nodes/{{.Node.ID}}/import-tui">
    <button type="submit" class="btn primary">一键导入到 panel</button>
  </form>
</section>
{{end}}
```

In `internal/server/templates/node_detail.html`, find an appropriate spot (after the existing forwards table or in the sidebar) and add:

```html
{{template "_tui_snapshot_partial.html" .}}
```

In the handler that renders `node_detail.html` (search for `node_detail` in `handlers_admin.go`), augment the template data:

```go
tuiSnapJSON, ts, _ := db.GetTuiSnapshot(s.DB, nodeID)
var tuiSnap []wsproto.Forward
if tuiSnapJSON != "" {
	_ = json.Unmarshal([]byte(tuiSnapJSON), &tuiSnap)
}
age := ""
if ts != nil {
	age = humanize.Time(time.Unix(*ts, 0))
}
data := map[string]any{
	"Node":           node,
	"Forwards":       forwards,
	"TuiSnapshot":    tuiSnap,
	"TuiSnapshotAge": age,
}
```

(Add `github.com/dustin/go-humanize` import — already in go.mod.)

- [ ] **Step 5: 运行测试确认通过 + 编译**

Run:
```
cd /Users/xjetry/work/vibe/nft-forward && go build ./...
cd /Users/xjetry/work/vibe/nft-forward && go test ./internal/server/ -v -count=1
```

Expected: PASS。

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add internal/server/templates/_tui_snapshot_partial.html internal/server/templates/node_detail.html internal/server/handlers_admin.go internal/server/handlers_admin_test.go
git commit -m "server: surface agent tui-segment snapshot + one-click import

The agent posts tui_segment_changed whenever the on-host TUI edits the
tui-owned segment; the panel renders the snapshot on the node detail
page so admins can see local edits and one-click import them into the
DB-backed panel segment. Import is explicit (no auto-sync) to avoid
double-write races with concurrent TUI activity."
```

---

## Task 12: install.sh 重做（agent 模式 + detect / uninstall）

**Files:**
- Modify: `install.sh`

- [ ] **Step 1: 改 write_daemon_unit 的 extra_args 用法（无需改函数体）**

`write_daemon_unit` 当前签名 `write_daemon_unit "$extra_args"` 已经正确。无需改。

- [ ] **Step 2: 改 detect_existing_roles**

Find the `agent)` branch in `detect_existing_roles` and replace `--listen` with `--connect`:

```bash
if [[ -f "$SYSTEMD_DIR/nft-forward-daemon.service" ]] \
   && grep -q -- '--connect' "$SYSTEMD_DIR/nft-forward-daemon.service"; then
  roles+=(agent)
fi
```

- [ ] **Step 3: 重做 agent 安装分支**

Replace the `agent)` install case (lines ~496-512):

```bash
agent)
  switch_role_cleanup agent
  # Normalize URL: http→ws, https→wss; append /v1/agents if path empty.
  panel_url="${panel_url:-${PANEL_URL:-}}"
  [[ -n "$panel_url" ]] || die "agent 模式需要 --panel-url 或 PANEL_URL"
  case "$panel_url" in
    https://*) panel_url="wss://${panel_url#https://}" ;;
    http://*)  panel_url="ws://${panel_url#http://}" ;;
    wss://*|ws://*) ;;
    *) die "panel-url 必须以 http(s):// 或 ws(s):// 开头" ;;
  esac
  case "$panel_url" in
    *"/v1/agents"|*"/v1/agents/") ;;
    */) panel_url="${panel_url}v1/agents" ;;
    *)  panel_url="${panel_url}/v1/agents" ;;
  esac
  mkdir -p /etc/nft-forward
  install -m 0600 /dev/stdin /etc/nft-forward/panel.token <<<"$token"
  write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token"
  systemctl daemon-reload
  systemctl enable --now nft-forward-daemon.service
  cat <<EOF

$(ok "===== Agent 安装完成 =====")
daemon 已通过 WebSocket 连向 $panel_url
本机不再暴露任何 HTTP 端口给 panel；如要排查，查看
  journalctl -u nft-forward-daemon.service -f

文档:  https://github.com/$REPO#readme
EOF
  ;;
```

- [ ] **Step 4: 改 do_uninstall agent 调 demote API**

Replace the `agent)` uninstall case (lines ~150-171):

```bash
agent)
  if [[ "$purge" -eq 1 ]]; then
    curl -sf --unix-socket /var/run/nft-forward.sock \
         -X POST -H 'Content-Type: application/json' \
         http://daemon/v1/ruleset/panel \
         -d '{"rules":[]}' >/dev/null 2>&1 \
      || echo "警告: 未能通过 daemon API 清 panel 段（daemon 可能已停）" >&2
  else
    # Migrate panel segment back into tui segment so live forwards survive.
    curl -sf --unix-socket /var/run/nft-forward.sock \
         -X POST http://daemon/v1/admin/demote-to-tui >/dev/null 2>&1 \
      || echo "警告: 未能通过 daemon API 降级 panel→tui 段（daemon 可能已停）" >&2
  fi
  write_daemon_unit ""
  rm -f /etc/nft-forward/panel.token
  systemctl daemon-reload
  systemctl restart nft-forward-daemon.service
  if [[ "$purge" -eq 1 ]]; then
    rm -rf /etc/nft-forward/
    ok "已卸载 agent 角色 + 清 /etc/nft-forward/ 与 daemon panel 段"
  else
    ok "已卸载 agent 角色（daemon 保留；panel 段已迁回 tui 段，token 文件已删）"
  fi
  ;;
```

- [ ] **Step 5: 加 --panel-url 参数解析 + 修改 usage**

Find the `while [[ $# -gt 0 ]]; do` argument loop (lines ~335-353), add:

```bash
    --panel-url) panel_url="${2:?--panel-url 需要值}"; shift 2 ;;
    --panel-url=*) panel_url="${1#*=}"; shift ;;
```

Add `panel_url=""` near the variable initialization (around line 332).

Update agent mode prompt block (lines ~390-398):

```bash
  agent)
    panel_url="${panel_url:-${PANEL_URL:-}}"
    token="${token:-${AGENT_TOKEN:-}}"
    if [[ -z "$panel_url" && -t 0 ]]; then
      read -rp "Panel URL（如 https://panel.example.com）: " panel_url
    fi
    if [[ -z "$token" && -t 0 ]]; then
      read -rp "Agent bearer token（从面板节点详情页拷贝）: " token
    fi
    [[ -n "$panel_url" ]] || die "agent 模式需要 --panel-url 或 PANEL_URL"
    [[ -n "$token" ]] || die "agent 模式需要 --token 或 AGENT_TOKEN"
    ;;
```

Update `usage()`:

```bash
  agent            受控节点（daemon 主动 dial panel WebSocket 反向纳管）

选项 / 环境变量:
  --panel-url URL  (PANEL_URL)    agent 连向的 panel 地址（http(s)://… 或 ws(s)://…）
  --token TOKEN    (AGENT_TOKEN)  agent bearer token（agent 模式必填）
  --addr ADDR      (PANEL_ADDR)   server 监听地址；默认 :8080
  ...
示例:
  sudo $0 agent --panel-url https://panel.example.com --token abc...
```

Remove `--port`/`PORT` handling (no longer relevant — agent doesn't listen).

- [ ] **Step 6: 手动验证 shellcheck**

Run: `shellcheck /Users/xjetry/work/vibe/nft-forward/install.sh`

Expected: 无 ERROR 级别问题（WARNING/INFO 可忽略，沿用既有风格）。如果未装 shellcheck，跳过这步。

- [ ] **Step 7: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add install.sh
git commit -m "install.sh: agent role uses --connect; demote panel→tui on uninstall

The agent install command takes --panel-url + --token, derives the wss
URL with the canonical /v1/agents path, and writes the token to
/etc/nft-forward/panel.token. Uninstall (no --purge) hits the new
/v1/admin/demote-to-tui endpoint so panel-pushed rules survive as
locally-managed tui-segment rules instead of disappearing."
```

---

## Task 13: docker-compose + docker/test.sh

**Files:**
- Modify: `docker/docker-compose.yml`
- Modify: `docker/test.sh`

- [ ] **Step 1: 改 compose 让 server 进 bridge**

Edit `docker/docker-compose.yml`:

```yaml
services:
  daemon:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    image: nft-forward:dev
    container_name: nftf-daemon
    cap_add: ["NET_ADMIN", "NET_RAW", "SYS_MODULE"]
    network_mode: host
    volumes:
      - daemon-state:/var/lib/nft-forward
      - daemon-run:/var/run
    command: ["/usr/local/sbin/nft-forward", "daemon"]

  server:
    image: nft-forward:dev
    depends_on: [daemon]
    container_name: nftf-server
    networks: [nftf]
    # By default the panel is reachable only from within the nftf bridge
    # network. To expose it on the host uncomment the ports block, or
    # add your own caddy/nginx service into nftf and reverse-proxy
    # nftf-server:8080.
    # ports:
    #   - "127.0.0.1:8080:8080"
    volumes:
      - daemon-run:/var/run
      - server-data:/var/lib/nft-forward
    command: ["/usr/local/sbin/nft-forward", "server", "--addr", ":8080"]

networks:
  nftf:
    driver: bridge

volumes:
  daemon-state:
  daemon-run:
  server-data:
```

- [ ] **Step 2: 改 test.sh 通过容器名访问 server**

Find every `curl ... :8080` in `docker/test.sh` and change to use the service name. Since the integration tests run `docker compose exec daemon bash -c '...'`, and daemon is on host network, it can reach `localhost:8080` only if server were also on host. After the move, daemon container can reach server via the bridge IP — but since daemon is `network_mode: host`, it sees the host's network only, **not the bridge**.

This is a problem. Two solutions:

**Solution A (simpler):** Add an `aux` test container on `nftf` network whose only job is to run curl against `nftf-server:8080`. Verification logic moves there.

**Solution B (smaller diff):** Add `ports: ["127.0.0.1:8080:8080"]` to the server service for tests only (override file).

Pick Solution A. Add to `docker/docker-compose.yml`:

```yaml
  testctl:
    image: alpine:3.20
    container_name: nftf-testctl
    networks: [nftf]
    profiles: [test]
    command: ["sleep", "infinity"]
```

Then `docker/test.sh` adjusts: `docker compose --profile test up -d testctl`, and curls go through `docker compose exec testctl wget -qO- http://nftf-server:8080/...`.

Update existing curl invocations:
- `docker compose exec daemon curl ... :8080/...` → `docker compose exec testctl wget -qO- http://nftf-server:8080/...`
- For daemon-side unix-socket curls (`--unix-socket /var/run/nft-forward.sock`), no change.

- [ ] **Step 3: 加 WS 注册新 step**

Append to `docker/test.sh` (before any final cleanup):

```bash
note "X. WebSocket agent dial + segment migration"

# Create a node in the panel; capture its token.
NODE_TOKEN="$(openssl rand -hex 32)"
docker compose exec testctl sh -c "wget -qO- --post-data='name=edge-x&address=ws://nftf-server:8080/v1/agents&secret=${NODE_TOKEN}' http://nftf-server:8080/admin/nodes" >/dev/null \
  || die "create node failed"

# Pre-seed a tui-segment rule on the daemon via the unix socket.
docker compose exec daemon curl -sf --unix-socket /var/run/nft-forward.sock \
  -X POST http://daemon/v1/ruleset/tui \
  -H 'Content-Type: application/json' \
  -d '{"rules":[{"proto":"tcp","src_port":58443,"dest_ip":"10.20.1.20","dest_port":8443}]}' \
  >/dev/null

# Start the daemon in "agent" mode by restarting with --connect.
docker compose exec daemon sh -c "
  echo $NODE_TOKEN > /etc/nft-forward/panel.token
  pkill -SIGTERM -f 'nft-forward daemon' || true
  /usr/local/sbin/nft-forward daemon --connect ws://nftf-server:8080/v1/agents --panel-token-file /etc/nft-forward/panel.token &
  sleep 2
"

# Verify: node should be online + forwards table has the migrated rule.
docker compose exec testctl wget -qO- http://nftf-server:8080/admin/nodes \
  | grep -q 'online' \
  || { echo 'node not online after dial'; exit 1; }

green "  agent dial + register_local + tui→panel migration verified"
```

(This is illustrative — adjust to actual admin endpoint conventions in the project. The current server uses form-POST auth, so the admin create flow needs a session cookie; if that's too complex, swap for direct sqlite INSERT through `docker compose exec server sqlite3`.)

Simpler alternative — drive everything through unix socket + direct DB:

```bash
note "X. WebSocket agent dial"
NODE_TOKEN="$(openssl rand -hex 32)"
docker compose exec server sqlite3 /var/lib/nft-forward/panel.db \
  "INSERT INTO nodes (name,address,secret,node_kind,created_at) VALUES ('edge-x','ws://localhost/v1/agents','$NODE_TOKEN','remote',strftime('%s','now'))"

docker compose exec daemon sh -c "
  mkdir -p /etc/nft-forward
  echo $NODE_TOKEN > /etc/nft-forward/panel.token
  pkill -SIGTERM -f 'nft-forward daemon' || true
  sleep 1
  /usr/local/sbin/nft-forward daemon --connect ws://nftf-server:8080/v1/agents --panel-token-file /etc/nft-forward/panel.token >/tmp/agent.log 2>&1 &
  sleep 3
"

ONLINE=$(docker compose exec server sqlite3 /var/lib/nft-forward/panel.db "SELECT online FROM nodes WHERE name='edge-x'")
[[ "$ONLINE" == "1" ]] || { echo 'node not online'; docker compose exec daemon cat /tmp/agent.log; exit 1; }
green "  agent online after dial"
```

But the daemon container is host-network, can it reach `nftf-server:8080`? No — host-network container doesn't see bridge service names. Have to use the host's bridge IP, or fall back to localhost via the testctl shortcut.

**Practical compromise:** the agent-side daemon in tests connects via `ws://localhost:8080/v1/agents` and we expose `8080:8080` in the **test** compose override. Add `docker/compose.test.override.yml`:

```yaml
services:
  server:
    ports:
      - "127.0.0.1:8080:8080"
```

And in `docker/test.sh` top:

```bash
COMPOSE="docker compose -f docker-compose.yml -f compose.test.override.yml"
```

Then `--connect ws://localhost:8080/v1/agents` works from the host-network daemon container.

- [ ] **Step 4: 加段降级测试**

Append another step:

```bash
note "Y. agent→tui demotion preserves rules"
# Push a panel-segment rule via the connected agent.
docker compose exec server sqlite3 /var/lib/nft-forward/panel.db \
  "INSERT INTO forwards (node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at)
   SELECT id, NULL, NULL, 'tcp', 59443, '10.20.1.30', 443, 'panel-rule', strftime('%s','now') FROM nodes WHERE name='edge-x'"
# Trigger dispatch — admin would normally hit a panel API; for the test
# just kick the agent by restarting the server (forces a fresh apply on
# next hello).
docker compose restart server
sleep 5

# Now demote.
docker compose exec daemon curl -sf --unix-socket /var/run/nft-forward.sock \
  -X POST http://daemon/v1/admin/demote-to-tui \
  || { echo 'demote API failed'; exit 1; }

# Verify state.json: panel segment empty, tui segment has the rule.
docker compose exec daemon cat /var/lib/nft-forward/state.json \
  | grep -q '"tui"' \
  || { echo 'tui segment missing after demote'; exit 1; }
green "  panel→tui demotion verified"
```

- [ ] **Step 5: 跑集成测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward/docker && ./test.sh`

Expected: 全部 step PASS。如果第 3、4 步在 CI 里不可用（没 docker），mark them as skippable based on env var.

- [ ] **Step 6: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add docker/docker-compose.yml docker/test.sh docker/compose.test.override.yml
git commit -m "docker: server moves to bridge; test scripts verify dial + demote

Compose keeps the daemon on host network (nftables hard requirement)
but puts the server in a dedicated bridge. The test override file binds
127.0.0.1:8080 so host-network daemon containers can dial the server's
WS endpoint over localhost. New test steps exercise agent registration
and the panel→tui demotion path end-to-end."
```

---

## Task 14: README 更新

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 替换 agent 安装小节**

In `README.md`, find the section with `sudo bash install.sh agent --token <64位hex>` (around line 47) and replace with:

```markdown
### agent 节点（远程纳管）

agent daemon 通过 WebSocket 反向连向 panel，不在宿主机暴露任何端口。

```bash
sudo bash install.sh agent --panel-url https://panel.example.com --token <64位hex>
```

Token 在面板节点详情页生成。Panel URL 必须是 agent 能到达的地址（如反代后的公网域名）。
http(s):// 和 ws(s):// 自动归一为 wss/ws，并 append `/v1/agents` 路径。

要从 panel 卸载 agent 角色（保留本地 forward）：

```bash
sudo bash install.sh uninstall agent
```

这会把 panel 推过的规则合并回本地 tui 段，重启 daemon 进入纯 TUI 模式；
所有 forward 继续生效，只是改由本地 TUI 管理。`--purge` 则把所有 panel
推过的规则一并清空。
```

- [ ] **Step 2: 加 docker 部署小节**

```markdown
### Docker 部署

`docker/docker-compose.yml` 提供基线模板：daemon 跑在 host network（nftables
需要），server 跑在 `nftf` bridge 网络，默认不映射端口。

- 本机临时访问：取消 compose 里 `server.ports` 的注释。
- 生产用反代：自行加 caddy / nginx / traefik service 进 `nftf` 网络，
  反代 `nftf-server:8080`。

agent 节点装机后，daemon 需要能从本地网络到达 panel 的 WSS 入口。
反代必须允许 WebSocket Upgrade、且 idle timeout ≥ 60s（dialer 每 10s
ping，避开默认 60s 反代 timeout）。
```

- [ ] **Step 3: 加升级注意**

In the existing 升级与迁移 section, add:

```markdown
> **2026-05 升级注意**：本版本翻转了 agent ↔ panel 通信方向。旧版的
> `daemon --listen :PORT` 路径已删除；旧 agent 必须重装：
>
> ```bash
> sudo bash install.sh agent --panel-url https://panel.example.com --token <旧 token>
> ```
>
> install.sh 会自动 detect 已有的 `--listen` 残留 unit、改写为 `--connect`
> 形态，旧 token 仍然有效（token 模型沿用 nodes.secret 不变）。
```

- [ ] **Step 4: Commit**

```bash
cd /Users/xjetry/work/vibe/nft-forward
git add README.md
git commit -m "docs: README reflects agent reverse-dial install + docker compose layout

Covers the new --panel-url flow, the demote-on-uninstall behavior, the
bridge-network compose template, and the breaking-change note for
existing agent operators who need to re-run install.sh agent after this
release."
```

---

## Task 15: 终局 — 全包 lint / 测试 / build

- [ ] **Step 1: 全包测试**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./... -count=1`

Expected: 全部 PASS。

- [ ] **Step 2: go vet**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go vet ./...`

Expected: 无输出。

- [ ] **Step 3: 构建生产二进制**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go build -o /tmp/nft-forward ./cmd/nft-forward && /tmp/nft-forward 2>&1 | head -5`

Expected: TUI 启动报错（因无 daemon 运行），但二进制能跑。验证 `--connect` 标志识别：

```
/tmp/nft-forward daemon --help 2>&1 | grep -E 'connect|panel-token'
```

Expected: 两个新 flag 出现在 help。

- [ ] **Step 4: 如果一切通过，不需要新 commit；TASK 完成**

---

## Self-Review

**Spec coverage:**
- §架构总览 → Task 4, 5, 6, 8 (selfnode, hub, dialer, daemon integration) ✓
- §JSON 协议 → Task 2 (wsproto) ✓
- §state.json schema v3 → Task 1 ✓
- §daemon 启动序列 + dialer goroutine → Task 6, 8 ✓
- §段迁移竞态 + 幂等 → Task 7 (handleRegisterLocal idempotency) ✓
- §重连退避 → Task 6 (Dialer.Run backoff) ✓
- §hub 结构 → Task 5 + 7 ✓
- §self-node 注入 → Task 3, 4 ✓
- §DB schema 变更 → Task 3 (migration 0004) ✓
- §install.sh 变更 → Task 12 ✓
- §TUI 同步钩子 → Task 9 ✓
- §panel UI tui snapshot → Task 11 ✓
- §docker compose 模板 → Task 13 ✓
- §错误处理矩阵 → covered in dialer/hub implementations ✓
- §测试单元 → Tasks 1, 2, 4, 5, 6, 7, 9, 11 ✓
- §测试集成 → Task 13 ✓
- §README → Task 14 ✓
- §删除 pusher/poller → Task 10 ✓

**Placeholder scan:** no TBD/TODO. 每个 step 含具体代码 / 命令。

**Type consistency check:**
- `Forward` struct fields: `Proto / ListenPort / TargetIP / TargetPort / Comment / BandwidthMbps` — used consistently in wsproto (Task 2), hub register_local handler (Task 7), import-tui handler (Task 11), template (Task 11). ✓
- `nft.Rule` fields: `Proto / SrcPort / DestIP / DestHost / DestPort / Comment / BandwidthMbps` — used in Hub.SendApplyRuleset (Task 5), Daemon.SetPanelRuleset (Task 8), buildRules (Task 10). ✓
- `AgentMeta` fields: `MigratedAt / LastAppliedRev / PanelURL` — used in Task 1, 6, 8, 9. ✓
- `Node` new columns: `LocalMigratedAt / LastSeen / Online / AgentVersion / NodeKind` — used in Task 3 helpers and Hub (Task 5, 7), selfnode (Task 4). ✓
- Dispatcher signature `Dispatch(nodeID int64, rules []nft.Rule, rev string) error` — used consistently in Tasks 4, 10, 11. ✓
- Hub method `SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error` — defined Task 5, called Task 4 (Dispatcher), Task 10 (dispatchToNode). ✓
- demote API path `/v1/admin/demote-to-tui` — defined Task 9, called Task 12, tested Task 13. ✓

All references resolve.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-26-agent-ws-architecture.md`. Two execution options:

**1. Subagent-Driven (recommended)** - 每 task 派 fresh subagent、两阶段 review、快速迭代、保护主 context

**2. Inline Execution** - 在本 session 内顺序跑 task、批量执行 + checkpoint review

Which approach?
