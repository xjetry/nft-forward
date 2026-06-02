# 中继链路编排（Relay Chain Orchestration）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **代码卫生（贯穿所有任务）**：注释只解释 WHY / invariant，**禁止**出现任务号、阶段号、方案代号、审阅轮次等过程信息；commit message 同理（用语义化前缀，不写 "Task N"）。派发 subagent 时务必转达本规则。

**Goal:** 在 webui 里把「跨多节点的中继转发链」做成一等持久对象：选有序的跳 + 填出口，系统自动分配/对齐各跳端口、生成可复制入口，管理员串节点、租户串自己的通道。

**Architecture:** 新增 `chains` / `chain_hops` 表与 `forwards.chain_id` 标记，复用现有 forward 下发/计量管线；核心 `db.RegenerateChain` 在事务里完成端口分配（按 node 稳定复用）+ 目标对齐 + 落 forward；端口占用以 `db.OccupiedPortsOnNode`（面板 forwards ∪ 节点 tui 快照）为准，并回填修复手动建转发的占用预检。

**Tech Stack:** Go 1.26（无 CGO）、modernc.org/sqlite、chi v5 路由、html/template 服务端渲染；测试用 `Open(":memory:")`。

**Spec:** `docs/superpowers/specs/2026-06-02-relay-chain-orchestration-design.md`

---

## File Structure

**新建：**
- `internal/db/migrations/0006_relay_chains.sql` — schema：`nodes.relay_host`、`chains`、`chain_hops`、`forwards.chain_id`。
- `internal/db/chains.go` — `DBTX` 接口、`Chain`/`ChainHop` 类型、链路 CRUD、`OccupiedPortsOnNode`、`RegenerateChain` 核心。
- `internal/db/chains_test.go` — 占用检查 + 生成/分配核心的单元测试。
- `internal/server/chains.go` — 管理员链路 handler + 节点中继地址 handler + 链路视图模型。
- `internal/server/chains_test.go` — 管理员 handler 的 httptest。
- `internal/server/my_chains.go` — 租户链路 handler（积木 = 已授权通道）。
- `internal/server/my_chains_test.go` — 租户 handler 的 httptest。
- `internal/server/templates/chains.html`、`chain_form.html`、`chain_detail.html` — 管理员 UI。
- `internal/server/templates/my_chains.html`、`my_chain_form.html` — 租户 UI。

**修改：**
- `internal/db/queries.go` — `Node` 加 `RelayHost` + `nodeCols`/`scanNode`；`UpdateNodeRelayHost`；`Forward` 加 `ChainID` + `forwardCols`/`scanForward`/`CreateForward`。
- `internal/server/server.go` — 注册管理员/租户链路路由；`createForward` 加完整占用预检。
- `internal/server/handlers_my.go` — `tenantCreateForward` 端口占用由 `UsedPortsOnNode` 换成完整占用检查。
- `internal/server/templates/_layout.html` — 导航加「链路」(admin) / 「我的链路」(tenant)。
- `internal/server/templates/node_detail.html` — 加「中继地址」展示 + 编辑表单。
- `internal/server/templates/my_dashboard.html`（可选）— 指向「我的链路」。

---

## Task 1: Schema 与 Node/Forward 列扩展

**Files:**
- Create: `internal/db/migrations/0006_relay_chains.sql`
- Modify: `internal/db/queries.go`（`Node` 结构 / `nodeCols` / `scanNode` / `UpdateNodeRelayHost` / `Forward` 结构 / `forwardCols` / `scanForward` / `CreateForward`）
- Test: `internal/db/chains_test.go`

- [ ] **Step 1: 写迁移文件**

Create `internal/db/migrations/0006_relay_chains.sql`：

```sql
-- 节点数据面可达地址：当该节点处于中继链路里时，上一跳 DNAT/relay 打过去的目标。
-- 空 = 从未进过链路；进链路前由 handler 校验必填。与 nodes.address（控制面，agent
-- 反向拨入，无可靠数据面 host）区分。
ALTER TABLE nodes ADD COLUMN relay_host TEXT NOT NULL DEFAULT '';

-- 一条中继链路 = 从自动分配的入口端点、经 N 个受管节点、到自由填写的出口的有序转发链。
-- tenant_id NULL => 管理员链路（不计量、端口在高位段自由分配）；非 NULL => 租户链路
-- （每跳落在租户已授权 tunnel 内，复用 tunnel 的端口段/CIDR/带宽/配额）。
CREATE TABLE chains (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id         INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  proto             TEXT NOT NULL CHECK(proto IN ('tcp','udp')),
  exit_host         TEXT NOT NULL,
  exit_port         INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_node_id     INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_chains_tenant ON chains(tenant_id);

-- 每跳一行，按 position 升序（0 = 入口跳）。tunnel_id：admin 链路 NULL，租户链路为该跳
-- 取端口/约束所依据的 granted tunnel。listen_port 为在 node_id 上分配的端口；mode 为该跳数据面。
CREATE TABLE chain_hops (
  chain_id    INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  tunnel_id   INTEGER REFERENCES tunnels(id) ON DELETE CASCADE,
  listen_port INTEGER NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  PRIMARY KEY (chain_id, position)
);
CREATE INDEX idx_chain_hops_node ON chain_hops(node_id);

-- 给链路自动生成的 forward 打标记，使链路能按 chain_id 整条重算/删除。一跳拥有恰好一条 forward。
ALTER TABLE forwards ADD COLUMN chain_id INTEGER REFERENCES chains(id) ON DELETE CASCADE;
```

- [ ] **Step 2: 扩展 `Node` 结构与列**

在 `internal/db/queries.go`，`Node` 结构体末尾（`NodeKind string` 之后、闭合 `}` 之前）加字段：

```go
	// RelayHost 是节点数据面可达地址（公网 IPv4 或域名），中继链路用它作为上一跳目标。
	// 与 Address（控制面）区分；空表示该节点尚不能进链路。
	RelayHost string
```

把 `nodeCols` 常量改为（在结尾追加 `,relay_host`）：

```go
const nodeCols = `id,name,address,secret,last_seen_at,last_apply_at,last_error,disabled,created_at,local_migrated_at,last_seen,online,agent_version,node_kind,relay_host`
```

在 `scanNode` 的 `r.Scan(...)` 调用中，于最后一个参数 `&n.NodeKind,` 之后加 `&n.RelayHost,`：

```go
	if err := r.Scan(
		&n.ID, &n.Name, &n.Address, &n.Secret,
		&n.LastSeenAt, &n.LastApplyAt, &n.LastError,
		&disabled, &n.CreatedAt,
		&localMigratedAt, &lastSeen, &n.Online, &agentVersion, &n.NodeKind,
		&n.RelayHost,
	); err != nil {
		return nil, err
	}
```

- [ ] **Step 3: 加 `UpdateNodeRelayHost`**

在 `internal/db/queries.go` 的 `DeleteNode` 之后加：

```go
// UpdateNodeRelayHost sets a node's data-plane reachable address (empty clears
// it). Validation of the value (IPv4 / hostname) is the caller's job.
func UpdateNodeRelayHost(d *sql.DB, id int64, relayHost string) error {
	_, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, relayHost, id)
	return err
}
```

- [ ] **Step 4: 扩展 `Forward` 结构与列**

在 `internal/db/queries.go`，`Forward` 结构体末尾（`CreatedAt int64` 之后）加：

```go
	// ChainID tags forwards generated by a relay chain so the chain can
	// regenerate/delete its hops as a unit; NULL for standalone forwards.
	ChainID sql.NullInt64
```

把 `forwardCols` 结尾追加 `,chain_id`：

```go
const forwardCols = `id,node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,disabled,last_bytes,total_bytes,created_at,mode,chain_id`
```

`scanForward` 的 `r.Scan(...)` 末尾加 `&f.ChainID`：

```go
	if err := r.Scan(&f.ID, &f.NodeID, &f.TenantID, &f.TunnelID, &f.Proto, &f.ListenPort, &f.TargetIP, &f.TargetPort, &f.Comment, &disabled, &f.LastBytes, &f.TotalBytes, &f.CreatedAt, &f.Mode, &f.ChainID); err != nil {
		return nil, err
	}
```

`CreateForward` 的 INSERT 加 `chain_id` 列与值（现有调用方不设 `ChainID`，零值 = NULL）：

```go
func CreateForward(d *sql.DB, f *Forward) (int64, error) {
	res, err := d.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode,chain_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		f.NodeID, f.TenantID, f.TunnelID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, now(), NormalizeForwardMode(f.Mode), f.ChainID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
```

- [ ] **Step 5: 写测试验证迁移与列读写**

Create `internal/db/chains_test.go`（本任务先放这一个测试，后续任务往本文件追加）：

```go
package db

import "testing"

func TestRelayHostRoundTrip(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "gomami", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHost != "" {
		t.Fatalf("new node relay_host should default empty, got %q", n.RelayHost)
	}
	if err := UpdateNodeRelayHost(d, n.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "1.2.3.4" {
		t.Fatalf("relay_host = %q, want 1.2.3.4", got.RelayHost)
	}
}

func TestCreateForwardCarriesChainID(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "5.6.7.8", TargetPort: 20001, Mode: "userspace"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.ChainID.Valid {
		t.Fatalf("standalone forward should have NULL chain_id, got %+v", f.ChainID)
	}
	if f.Mode != "userspace" {
		t.Fatalf("mode = %q, want userspace", f.Mode)
	}
}
```

- [ ] **Step 6: 跑测试**

Run: `go test ./internal/db/ -run 'TestRelayHostRoundTrip|TestCreateForwardCarriesChainID' -v`
Expected: PASS（迁移自动应用，列读写正常）。

- [ ] **Step 7: 全量回归（确认未破坏既有列扫描）**

Run: `go test ./internal/db/... ./internal/server/... -count=1`
Expected: PASS（`nodeCols`/`forwardCols` 改动后所有既有查询仍能扫描）。

- [ ] **Step 8: Commit**

```bash
git add internal/db/migrations/0006_relay_chains.sql internal/db/queries.go internal/db/chains_test.go
git commit -m "feat(db): add relay_host, chains/chain_hops tables, forwards.chain_id"
```

---

## Task 2: 完整端口占用检查 + 回填手动建转发预检

**Files:**
- Create: `internal/db/chains.go`（本任务建文件，放 `DBTX` 接口与 `OccupiedPortsOnNode`；后续任务继续往里加）
- Modify: `internal/server/handlers_my.go`（`tenantCreateForward`）、`internal/server/server.go`（`createForward`）
- Test: `internal/db/chains_test.go`

- [ ] **Step 1: 写占用检查的失败测试**

往 `internal/db/chains_test.go` 追加：

```go
func TestOccupiedPortsUnionsForwardsAndTuiSnapshot(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	// panel 段：一条 tcp forward 占 20000
	if _, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}); err != nil {
		t.Fatal(err)
	}
	// 节点本地 tui 段快照：tcp 占 20001、udp 占 53
	if err := UpsertTuiSnapshot(d, n.ID, `[{"proto":"tcp","listen_port":20001,"target_ip":"2.2.2.2","target_port":2},{"proto":"udp","listen_port":53,"target_ip":"3.3.3.3","target_port":3}]`); err != nil {
		t.Fatal(err)
	}
	occ, err := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !occ[20000] || !occ[20001] {
		t.Fatalf("tcp occupancy should include panel(20000) ∪ tui(20001): %v", occ)
	}
	if occ[53] {
		t.Fatalf("udp port 53 must not appear in tcp occupancy: %v", occ)
	}
}

func TestOccupiedPortsExcludesGivenChain(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	cid, err := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	f := &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}
	f.ChainID = sql.NullInt64{Int64: cid, Valid: true}
	if _, err := CreateForward(d, f); err != nil {
		t.Fatal(err)
	}
	// 排除本链路 -> 看不到自己的端口
	occ, _ := OccupiedPortsOnNode(d, n.ID, "tcp", cid)
	if occ[20000] {
		t.Fatalf("excludeChainID should drop the chain's own port: %v", occ)
	}
	// 不排除 -> 看得到
	occ2, _ := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if !occ2[20000] {
		t.Fatalf("without exclude the port must be occupied: %v", occ2)
	}
}
```

> 注：`TestOccupiedPortsExcludesGivenChain` 依赖 Task 3 的 `CreateChain`。若按顺序执行，可先实现 `CreateChain`（Task 3 Step 1）再跑本测试；或本步只跑 `TestOccupiedPortsUnionsForwardsAndTuiSnapshot`，待 Task 3 后再回归。

- [ ] **Step 2: 实现 `DBTX` 与 `OccupiedPortsOnNode`**

Create `internal/db/chains.go`：

```go
package db

import (
	"database/sql"
	"encoding/json"
	"math/rand"
	"net"
	"strconv"
)

// DBTX is satisfied by both *sql.DB and *sql.Tx so chain helpers can run either
// standalone or inside a regeneration transaction.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Admin chains allocate listen ports from this high range, skipping anything
// already occupied on the node. Tenant chains use their tunnel's port range.
const (
	ChainPortMin = 20000
	ChainPortMax = 60000
)

// OccupiedPortsOnNode returns every listen port held on (node, proto), unioning
// the panel forwards table with the node's last-reported tui-segment snapshot.
// The daemon rejects cross-segment port conflicts at apply time, so the tui
// snapshot must be consulted or auto-allocation would pick ports the daemon
// then refuses. excludeChainID>0 drops that chain's own forwards so a chain
// regenerating in place doesn't see itself as occupying its ports.
func OccupiedPortsOnNode(d DBTX, nodeID int64, proto string, excludeChainID int64) (map[int]bool, error) {
	out := map[int]bool{}
	rows, err := d.Query(
		`SELECT listen_port FROM forwards WHERE node_id=? AND proto=? AND (chain_id IS NULL OR chain_id<>?)`,
		nodeID, proto, excludeChainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err == nil {
			out[p] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// tui snapshot is best-effort (may be stale/absent); the daemon's 409 is the
	// ultimate authority, this only avoids the common collisions up front.
	var fj string
	switch err := d.QueryRow(`SELECT forwards_json FROM node_tui_snapshot WHERE node_id=?`, nodeID).Scan(&fj); err {
	case nil:
		var snap []struct {
			Proto      string `json:"proto"`
			ListenPort int    `json:"listen_port"`
		}
		if json.Unmarshal([]byte(fj), &snap) == nil {
			for _, f := range snap {
				if f.Proto == proto {
					out[f.ListenPort] = true
				}
			}
		}
	case sql.ErrNoRows:
		// node never reported a tui segment; nothing to union
	default:
		return nil, err
	}
	return out, nil
}

// hostPort joins a relay host / exit host with a port for display + targets.
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// PickFreePort returns a port in [start,end] not present in used, or 0 when the
// range is exhausted. A random offset keeps assignment unpredictable so two
// near-simultaneous allocations don't keep colliding on the same port.
func PickFreePort(start, end int, used map[int]bool) int {
	span := end - start + 1
	if span <= 0 {
		return 0
	}
	offset := rand.Intn(span)
	for i := 0; i < span; i++ {
		p := start + ((offset + i) % span)
		if !used[p] {
			return p
		}
	}
	return 0
}
```

> `PickFreePort` 从 `internal/server/handlers_my.go` 的非导出 `pickFreePort` 上移到此（端口分配属于 DB 层，`RegenerateChain` 与租户建转发共用一处）。Step 4 会把 server 侧调用改成 `db.PickFreePort` 并删除 server 的旧副本。

- [ ] **Step 3: 跑占用检查测试**

Run: `go test ./internal/db/ -run TestOccupiedPortsUnionsForwardsAndTuiSnapshot -v`
Expected: PASS。

- [ ] **Step 4: 回填 `tenantCreateForward` 占用预检**

在 `internal/server/handlers_my.go` 的自动分配分支里，把 `db.UsedPortsOnNode(...)` 调用替换为完整占用检查，并在显式填端口分支加占用拦截。改 `tenantCreateForward` 中端口处理块（现 `var listenPort int { ... }`）为：

```go
	occupied, err := db.OccupiedPortsOnNode(s.DB, tunnel.NodeID, proto, 0)
	if err != nil {
		setFlash(w, "端口检查失败: "+err.Error())
		http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
		return
	}
	var listenPort int
	if listenPortStr == "" {
		listenPort = db.PickFreePort(tunnel.PortStart, tunnel.PortEnd, occupied)
		if listenPort == 0 {
			setFlash(w, fmt.Sprintf("通道 %d-%d 内已无可用端口", tunnel.PortStart, tunnel.PortEnd))
			http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
			return
		}
	} else {
		listenPort, _ = strconv.Atoi(listenPortStr)
		if occupied[listenPort] {
			setFlash(w, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort))
			http.Redirect(w, r, "/my/forwards", http.StatusSeeOther)
			return
		}
	}
```

> 这样自动分配与显式填端口都过同一张完整占用表，撞 tui 段时当场拒绝而非落库后下发失败。配套清理：
> - 删除 `handlers_my.go` 里的非导出 `pickFreePort`（已上移到 `db.PickFreePort`）及其 `"math/rand"` import（若该文件不再有其它用处）。
> - 删除 `db.UsedPortsOnNode`（连同其在 `queries.go` 的定义与注释），它已被 `OccupiedPortsOnNode` 取代。
> - 若有测试直接引用 server 的 `pickFreePort`，改成 `db.PickFreePort`。
>
> 删除后 `go build ./...` 必须通过。

- [ ] **Step 5: 给管理员 `createForward` 加占用预检**

在 `internal/server/server.go` 的 `createForward` 里，`nft.Validate(testRule)` 通过之后、`db.CreateForward` 之前，插入：

```go
	occupied, err := db.OccupiedPortsOnNode(s.DB, nodeID, proto, 0)
	if err != nil {
		setFlash(w, "端口检查失败: "+err.Error())
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
	if occupied[listenPort] {
		setFlash(w, fmt.Sprintf("端口 %d 已被占用（本地 TUI / 其他转发）", listenPort))
		http.Redirect(w, r, "/forwards", http.StatusSeeOther)
		return
	}
```

（`createForward` 已 import `fmt`/`db`，无需新增 import；注意原 `id, err := db.CreateForward(...)` 现在 `err` 已声明，改成 `id, err = db.CreateForward(...)`。）

- [ ] **Step 6: 跑回归**

Run: `go build ./... && go test ./internal/server/... -count=1`
Expected: PASS（既有 `tenantCreateForward` 测试仍过；`UsedPortsOnNode` 删除后无悬挂引用）。

- [ ] **Step 7: Commit**

```bash
git add internal/db/chains.go internal/db/chains_test.go internal/db/queries.go internal/server/handlers_my.go internal/server/server.go
git commit -m "feat(db): full node port occupancy (panel forwards ∪ tui snapshot); use in manual create"
```

---

## Task 3: 链路 DB 层（CRUD + 视图查询）

**Files:**
- Modify: `internal/db/chains.go`
- Test: `internal/db/chains_test.go`

- [ ] **Step 1: 实现链路类型与 CRUD**

在 `internal/db/chains.go` 追加（`hostPort` 之后）：

```go
type Chain struct {
	ID              int64
	TenantID        sql.NullInt64
	Name            string
	Proto           string
	ExitHost        string
	ExitPort        int
	EntryNodeID     sql.NullInt64
	EntryListenPort int
	CreatedAt       int64
}

type ChainHop struct {
	ChainID    int64
	Position   int
	NodeID     int64
	TunnelID   sql.NullInt64
	ListenPort int
	Mode       string
}

const chainCols = `id,tenant_id,name,proto,exit_host,exit_port,entry_node_id,entry_listen_port,created_at`

func scanChain(r rowScanner) (*Chain, error) {
	c := &Chain{}
	if err := r.Scan(&c.ID, &c.TenantID, &c.Name, &c.Proto, &c.ExitHost, &c.ExitPort,
		&c.EntryNodeID, &c.EntryListenPort, &c.CreatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// CreateChain inserts the chain header; hops + forwards are written by
// RegenerateChain. entry_* start at 0/NULL until the first regeneration.
func CreateChain(d DBTX, c *Chain) (int64, error) {
	res, err := d.Exec(`INSERT INTO chains(tenant_id,name,proto,exit_host,exit_port,created_at) VALUES (?,?,?,?,?,?)`,
		c.TenantID, c.Name, c.Proto, c.ExitHost, c.ExitPort, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetChain(d DBTX, id int64) (*Chain, error) {
	return scanChain(d.QueryRow(`SELECT ` + chainCols + ` FROM chains WHERE id=?`, id))
}

// UpdateChainHeader persists editable header fields (name/proto/exit). entry_*
// is owned by RegenerateChain and not touched here.
func UpdateChainHeader(d DBTX, c *Chain) error {
	_, err := d.Exec(`UPDATE chains SET name=?,proto=?,exit_host=?,exit_port=? WHERE id=?`,
		c.Name, c.Proto, c.ExitHost, c.ExitPort, c.ID)
	return err
}

func listChainsWhere(d *sql.DB, where string, args ...any) ([]*Chain, error) {
	q := `SELECT ` + chainCols + ` FROM chains`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chain
	for rows.Next() {
		c, err := scanChain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListAdminChains returns chains with no owning tenant (admin-built, unmetered).
func ListAdminChains(d *sql.DB) ([]*Chain, error) {
	return listChainsWhere(d, "tenant_id IS NULL")
}

func ListChainsByTenant(d *sql.DB, tenantID int64) ([]*Chain, error) {
	return listChainsWhere(d, "tenant_id=?", tenantID)
}

func ListChainHops(d DBTX, chainID int64) ([]*ChainHop, error) {
	rows, err := d.Query(`SELECT chain_id,position,node_id,tunnel_id,listen_port,mode FROM chain_hops WHERE chain_id=? ORDER BY position`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChainHop
	for rows.Next() {
		h := &ChainHop{}
		if err := rows.Scan(&h.ChainID, &h.Position, &h.NodeID, &h.TunnelID, &h.ListenPort, &h.Mode); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func ListForwardsByChain(d DBTX, chainID int64) ([]*Forward, error) {
	rows, err := d.Query(`SELECT `+forwardCols+` FROM forwards WHERE chain_id=? ORDER BY node_id, listen_port`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Forward
	for rows.Next() {
		f, err := scanForward(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteChain removes a chain and returns the nodes whose kernel state must be
// re-dispatched (i.e. the nodes its forwards lived on). The ON DELETE CASCADE on
// chain_hops + forwards.chain_id clears the rows; we collect nodes first so the
// caller can re-push them after the rules are gone.
func DeleteChain(d *sql.DB, id int64) ([]int64, error) {
	rows, err := d.Query(`SELECT DISTINCT node_id FROM forwards WHERE chain_id=?`, id)
	if err != nil {
		return nil, err
	}
	var nodes []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM chains WHERE id=?`, id); err != nil {
		return nil, err
	}
	return nodes, nil
}
```

- [ ] **Step 2: 写 CRUD 测试**

往 `internal/db/chains_test.go` 追加：

```go
func TestChainCRUD(t *testing.T) {
	d := openMemDB(t)
	id, err := CreateChain(d, &Chain{Name: "vless", Proto: "tcp", ExitHost: "seednet", ExitPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	c, err := GetChain(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "vless" || c.Proto != "tcp" || c.ExitHost != "seednet" || c.ExitPort != 8443 {
		t.Fatalf("round-trip mismatch: %+v", c)
	}
	if c.TenantID.Valid || c.EntryListenPort != 0 {
		t.Fatalf("fresh admin chain should have NULL tenant + entry 0: %+v", c)
	}
	admin, _ := ListAdminChains(d)
	if len(admin) != 1 {
		t.Fatalf("ListAdminChains = %d, want 1", len(admin))
	}
	nodes, err := DeleteChain(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("no forwards yet, affected nodes should be empty: %v", nodes)
	}
	if _, err := GetChain(d, id); err == nil {
		t.Fatalf("chain should be gone after delete")
	}
}
```

- [ ] **Step 3: 跑测试**

Run: `go test ./internal/db/ -run 'TestChainCRUD|TestOccupiedPortsExcludesGivenChain' -v`
Expected: PASS（此时 `CreateChain` 已就绪，Task 2 Step 1 里依赖它的用例也应转绿）。

- [ ] **Step 4: Commit**

```bash
git add internal/db/chains.go internal/db/chains_test.go
git commit -m "feat(db): relay chain CRUD and view queries"
```

---

## Task 4: `RegenerateChain` 核心（分配 + 对齐 + 结构校验）

**Files:**
- Modify: `internal/db/chains.go`
- Test: `internal/db/chains_test.go`

- [ ] **Step 1: 写核心行为的失败测试**

往 `internal/db/chains_test.go` 追加（覆盖：三跳对齐 + 入口、缺 relay_host、同节点重复、改顺序保留端口、udp 强制 kernel）：

```go
// chainTestNode creates a node with a relay_host set so it can join a chain.
func chainTestNode(t *testing.T, d *sql.DB, name, relay string) *Node {
	t.Helper()
	n, err := CreateNode(d, name, "https://p", name+"-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeRelayHost(d, n.ID, relay); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	return got
}

func regen(t *testing.T, d *sql.DB, c *Chain, hops []HopInput) (string, []int64) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	entry, affected, err := RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		t.Fatalf("RegenerateChain: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return entry, affected
}

func TestRegenerateThreeHopWiring(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	w := chainTestNode(t, d, "nnc-tw", "3.3.3.3")
	cid, _ := CreateChain(d, &Chain{Name: "vless", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)

	entry, affected := regen(t, d, c, []HopInput{
		{NodeID: g.ID, Mode: "userspace"},
		{NodeID: h.ID, Mode: "userspace"},
		{NodeID: w.ID, Mode: "kernel"},
	})

	fws, _ := ListForwardsByChain(d, cid)
	if len(fws) != 3 {
		t.Fatalf("want 3 hop forwards, got %d", len(fws))
	}
	byNode := map[int64]*Forward{}
	for _, f := range fws {
		byNode[f.NodeID] = f
	}
	// 末跳打到出口
	if byNode[w.ID].TargetIP != "9.9.9.9" || byNode[w.ID].TargetPort != 8443 {
		t.Fatalf("last hop must target exit, got %s:%d", byNode[w.ID].TargetIP, byNode[w.ID].TargetPort)
	}
	// 中间跳打到下一跳的 relay_host:下一跳监听端口
	if byNode[g.ID].TargetIP != "2.2.2.2" || byNode[g.ID].TargetPort != byNode[h.ID].ListenPort {
		t.Fatalf("hop1 must target hop2 relay:port, got %s:%d (hop2 listen %d)", byNode[g.ID].TargetIP, byNode[g.ID].TargetPort, byNode[h.ID].ListenPort)
	}
	if byNode[h.ID].TargetIP != "3.3.3.3" || byNode[h.ID].TargetPort != byNode[w.ID].ListenPort {
		t.Fatalf("hop2 must target hop3 relay:port")
	}
	// 入口 = 第一跳 relay_host:监听端口
	wantEntry := hostPort("1.1.1.1", byNode[g.ID].ListenPort)
	if entry != wantEntry {
		t.Fatalf("entry = %q, want %q", entry, wantEntry)
	}
	if len(affected) != 3 {
		t.Fatalf("affected nodes = %d, want 3", len(affected))
	}
	// 模式逐跳：g/h userspace、w kernel
	if byNode[g.ID].Mode != "userspace" || byNode[w.ID].Mode != "kernel" {
		t.Fatalf("per-hop mode not honored: g=%s w=%s", byNode[g.ID].Mode, byNode[w.ID].Mode)
	}
}

func TestRegenerateRejectsMissingRelayHost(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	bare, _ := CreateNode(d, "bare", "https://p", "x") // 无 relay_host
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: g.ID}, {NodeID: bare.ID}}, nil)
	tx.Rollback()
	if err == nil {
		t.Fatalf("expected error for node without relay_host")
	}
}

func TestRegenerateRejectsRepeatedNode(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: g.ID}, {NodeID: g.ID}}, nil)
	tx.Rollback()
	if err == nil {
		t.Fatalf("expected error for repeated node")
	}
}

func TestRegenerateKeepsPortOnReorder(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: g.ID}, {NodeID: h.ID}})
	before, _ := ListForwardsByChain(d, cid)
	portByNode := map[int64]int{}
	for _, f := range before {
		portByNode[f.NodeID] = f.ListenPort
	}
	// 交换顺序：节点未变，各自端口应保留
	c, _ = GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: h.ID}, {NodeID: g.ID}})
	after, _ := ListForwardsByChain(d, cid)
	for _, f := range after {
		if portByNode[f.NodeID] != f.ListenPort {
			t.Fatalf("node %d port changed on reorder: %d -> %d", f.NodeID, portByNode[f.NodeID], f.ListenPort)
		}
	}
}

func TestRegenerateUDPForcesKernel(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "udp", ExitHost: "9.9.9.9", ExitPort: 53})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: g.ID, Mode: "userspace"}, {NodeID: h.ID, Mode: "userspace"}})
	fws, _ := ListForwardsByChain(d, cid)
	for _, f := range fws {
		if f.Mode != "kernel" {
			t.Fatalf("udp hop must be kernel, got %s", f.Mode)
		}
	}
}
```

- [ ] **Step 2: 实现 `HopInput` 与 `RegenerateChain`**

在 `internal/db/chains.go` 的 import 块加入 `"fmt"`（`RegenerateChain` 用到），然后在文件末尾追加：

```go
// HopInput is one ordered hop the caller wants the chain to have. TunnelID is
// set for tenant chains (the granted tunnel the hop draws its port/range from)
// and invalid for admin chains. Mode is the requested data plane; udp chains
// coerce every hop to kernel.
type HopInput struct {
	NodeID   int64
	TunnelID sql.NullInt64
	Mode     string
}

// RegenerateChain rewrites chain c's hops + generated forwards for the given
// ordered hops and returns the copyable entry endpoint plus the set of nodes
// whose kernel state must be re-dispatched (current hops ∪ previously-touched
// nodes). Ports are kept stable per node across edits; avoid[nodeID]=port forces
// that node off a given port (used by the reallocate-on-conflict flow).
//
// Structural validation only: relay_host present, no repeated node, port-range
// exhaustion, tunnel<->node match + proto_mask, udp=>kernel. Tenant policy
// (grant ownership, exit CIDR, quota) is the caller's responsibility.
func RegenerateChain(tx DBTX, c *Chain, hops []HopInput, avoid map[int64]int) (string, []int64, error) {
	if len(hops) == 0 {
		return "", nil, fmt.Errorf("链路至少需要一跳")
	}

	type resolved struct {
		nodeID    int64
		relayHost string
		tunnelID  sql.NullInt64
		mode      string
		rangeLo   int
		rangeHi   int
	}
	rs := make([]resolved, len(hops))
	seen := map[int64]bool{}
	for i, hop := range hops {
		if seen[hop.NodeID] {
			return "", nil, fmt.Errorf("同一节点不能在链路中重复")
		}
		seen[hop.NodeID] = true

		var name, relay string
		if err := tx.QueryRow(`SELECT name, relay_host FROM nodes WHERE id=?`, hop.NodeID).Scan(&name, &relay); err != nil {
			return "", nil, fmt.Errorf("节点 %d 不存在", hop.NodeID)
		}
		if relay == "" {
			return "", nil, fmt.Errorf("节点 %s 未设置中继地址", name)
		}
		mode := NormalizeForwardMode(hop.Mode)
		if c.Proto == "udp" {
			mode = "kernel" // userspace relay is TCP-only
		}
		lo, hi := ChainPortMin, ChainPortMax
		tunnelID := hop.TunnelID
		if tunnelID.Valid {
			var pm string
			var ps, pe int
			var tNode int64
			if err := tx.QueryRow(`SELECT node_id, proto_mask, port_start, port_end FROM tunnels WHERE id=?`, tunnelID.Int64).Scan(&tNode, &pm, &ps, &pe); err != nil {
				return "", nil, fmt.Errorf("通道 %d 不存在", tunnelID.Int64)
			}
			if tNode != hop.NodeID {
				return "", nil, fmt.Errorf("通道与节点不匹配")
			}
			if pm != "tcp+udp" && pm != c.Proto {
				return "", nil, fmt.Errorf("通道 %d 不允许 %s", tunnelID.Int64, c.Proto)
			}
			lo, hi = ps, pe
		}
		rs[i] = resolved{nodeID: hop.NodeID, relayHost: relay, tunnelID: tunnelID, mode: mode, rangeLo: lo, rangeHi: hi}
	}

	// Read existing ports (keyed by node) BEFORE deleting so unchanged nodes keep
	// their port — entry endpoint + installed rules don't churn on edits.
	prev, err := ListForwardsByChain(tx, c.ID)
	if err != nil {
		return "", nil, err
	}
	prevPort := map[int64]int{}
	affected := map[int64]bool{}
	for _, f := range prev {
		prevPort[f.NodeID] = f.ListenPort
		affected[f.NodeID] = true
	}

	if _, err := tx.Exec(`DELETE FROM forwards WHERE chain_id=?`, c.ID); err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(`DELETE FROM chain_hops WHERE chain_id=?`, c.ID); err != nil {
		return "", nil, err
	}

	ports := make([]int, len(rs))
	for i, h := range rs {
		occ, err := OccupiedPortsOnNode(tx, h.nodeID, c.Proto, c.ID)
		if err != nil {
			return "", nil, err
		}
		if av, ok := avoid[h.nodeID]; ok {
			occ[av] = true // force this node off its current port
		}
		p := prevPort[h.nodeID]
		if p >= h.rangeLo && p <= h.rangeHi && !occ[p] {
			// keep
		} else {
			p = PickFreePort(h.rangeLo, h.rangeHi, occ)
			if p == 0 {
				var name string
				_ = tx.QueryRow(`SELECT name FROM nodes WHERE id=?`, h.nodeID).Scan(&name)
				return "", nil, fmt.Errorf("节点 %s 端口段(%d-%d)无可用端口", name, h.rangeLo, h.rangeHi)
			}
		}
		ports[i] = p
	}

	for i, h := range rs {
		var targetIP string
		var targetPort int
		if i < len(rs)-1 {
			targetIP = rs[i+1].relayHost
			targetPort = ports[i+1]
		} else {
			targetIP = c.ExitHost
			targetPort = c.ExitPort
		}
		if _, err := tx.Exec(`INSERT INTO chain_hops(chain_id,position,node_id,tunnel_id,listen_port,mode) VALUES (?,?,?,?,?,?)`,
			c.ID, i, h.nodeID, h.tunnelID, ports[i], h.mode); err != nil {
			return "", nil, err
		}
		comment := fmt.Sprintf("链路 %s · 第%d跳", c.Name, i+1)
		if _, err := tx.Exec(`INSERT INTO forwards(node_id,tenant_id,tunnel_id,proto,listen_port,target_ip,target_port,comment,created_at,mode,chain_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			h.nodeID, c.TenantID, h.tunnelID, c.Proto, ports[i], targetIP, targetPort, comment, now(), h.mode, c.ID); err != nil {
			return "", nil, err
		}
		affected[h.nodeID] = true
	}

	entryNodeID := rs[0].nodeID
	if _, err := tx.Exec(`UPDATE chains SET entry_node_id=?, entry_listen_port=? WHERE id=?`, entryNodeID, ports[0], c.ID); err != nil {
		return "", nil, err
	}
	c.EntryNodeID = sql.NullInt64{Int64: entryNodeID, Valid: true}
	c.EntryListenPort = ports[0]

	nodes := make([]int64, 0, len(affected))
	for n := range affected {
		nodes = append(nodes, n)
	}
	return hostPort(rs[0].relayHost, ports[0]), nodes, nil
}
```

- [ ] **Step 3: 跑核心测试**

Run: `go test ./internal/db/ -run TestRegenerate -v`
Expected: PASS（5 个用例全绿）。

- [ ] **Step 4: 全量 db 回归**

Run: `go test ./internal/db/... -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/db/chains.go internal/db/chains_test.go
git commit -m "feat(db): RegenerateChain — stable per-node port alloc, hop wiring, structural validation"
```

---

## Task 5: 管理员链路 UI（handler + 模板 + 路由 + 节点中继地址）

**Files:**
- Create: `internal/server/chains.go`、`internal/server/chains_test.go`
- Create: `internal/server/templates/chains.html`、`chain_form.html`、`chain_detail.html`
- Modify: `internal/server/server.go`（路由）、`internal/server/templates/_layout.html`、`internal/server/templates/node_detail.html`

- [ ] **Step 1: 写管理员 handler**

Create `internal/server/chains.go`：

```go
package server

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
)

// chainView is the per-chain row the list/detail templates render.
type chainView struct {
	Chain *db.Chain
	Path  string // "gomami → nnc-hk → seednet:8443"
	Entry string // "1.1.1.1:20000" or "—"
}

// buildChainView assembles the display path + entry endpoint for a chain.
func (s *Server) buildChainView(c *db.Chain) chainView {
	hops, _ := db.ListChainHops(s.DB, c.ID)
	names := make([]string, 0, len(hops)+1)
	for _, h := range hops {
		n, err := db.GetNode(s.DB, h.NodeID)
		if err == nil {
			names = append(names, n.Name)
		} else {
			names = append(names, fmt.Sprintf("#%d", h.NodeID))
		}
	}
	names = append(names, fmt.Sprintf("%s:%d", c.ExitHost, c.ExitPort))
	entry := "—"
	if c.EntryNodeID.Valid && c.EntryListenPort > 0 {
		if n, err := db.GetNode(s.DB, c.EntryNodeID.Int64); err == nil && n.RelayHost != "" {
			entry = net.JoinHostPort(n.RelayHost, strconv.Itoa(c.EntryListenPort))
		}
	}
	return chainView{Chain: c, Path: strings.Join(names, " → "), Entry: entry}
}

func (s *Server) listChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	chains, _ := db.ListAdminChains(s.DB)
	views := make([]chainView, 0, len(chains))
	for _, c := range chains {
		views = append(views, s.buildChainView(c))
	}
	s.render(w, "chains.html", map[string]any{"User": u, "Chains": views, "Flash": flashFromCookie(w, r)})
}

func (s *Server) newChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	nodes, _ := db.ListNodes(s.DB)
	s.render(w, "chain_form.html", map[string]any{
		"User": u, "Nodes": nodes, "Chain": nil, "Hops": nil, "Flash": flashFromCookie(w, r),
	})
}

// parseExit splits an "host:port" exit string. host may be IPv4 or hostname.
func parseExit(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("出口需为 host:port 形式")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("出口端口非法")
	}
	if host == "" {
		return "", 0, fmt.Errorf("出口地址不能为空")
	}
	if net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		return "", 0, fmt.Errorf("出口地址格式非法")
	}
	return host, port, nil
}

// adminHopInputs reads the ordered hop_node[] + hop_mode[] arrays the builder
// posts into structural HopInputs (no tunnel for admin chains).
func adminHopInputs(r *http.Request) ([]db.HopInput, error) {
	nodeIDs := r.Form["hop_node"]
	modes := r.Form["hop_mode"]
	if len(nodeIDs) == 0 {
		return nil, fmt.Errorf("至少添加一个节点")
	}
	hops := make([]db.HopInput, 0, len(nodeIDs))
	for i, idStr := range nodeIDs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("第 %d 跳节点非法", i+1)
		}
		mode := "kernel"
		if i < len(modes) {
			mode = modes[i]
		}
		hops = append(hops, db.HopInput{NodeID: id, Mode: mode})
	}
	return hops, nil
}

func (s *Server) createChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	c := &db.Chain{Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		tx.Rollback()
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains/new", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.create", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) showChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	hops, _ := db.ListChainHops(s.DB, id)
	forwards, _ := db.ListForwardsByChain(s.DB, id)
	fwByNode := map[int64]*db.Forward{}
	for _, f := range forwards {
		fwByNode[f.NodeID] = f
	}
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "chain_detail.html", map[string]any{
		"User": u, "View": s.buildChainView(c), "Chain": c,
		"Hops": hops, "FwByNode": fwByNode, "NodeByID": nodeByID,
		"Nodes": nodes, "Flash": flashFromCookie(w, r),
	})
}

func (s *Server) saveChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	hops, err := adminHopInputs(r)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	c.Name, c.Proto, c.ExitHost, c.ExitPort = name, proto, exitHost, exitPort

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err := db.UpdateChainHeader(tx, c); err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.save", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已保存，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路保存")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

func (s *Server) deleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/chains", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterFanout(w, nodes, "链路删除")
	http.Redirect(w, r, "/chains", http.StatusSeeOther)
}

// reallocateHop forces one hop off its current port (used when the daemon
// reports a cross-segment 409 or a userspace bind failure on that node) and
// re-dispatches.
func (s *Server) reallocateHop(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	pos, _ := strconv.Atoi(chi.URLParam(r, "pos"))
	c, err := db.GetChain(s.DB, id)
	if err != nil {
		http.Error(w, "链路不存在", http.StatusNotFound)
		return
	}
	hops, _ := db.ListChainHops(s.DB, id)
	if pos < 0 || pos >= len(hops) {
		setFlash(w, "跳序号非法")
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
	}
	avoid := map[int64]int{hops[pos].NodeID: hops[pos].ListenPort}

	tx, _ := s.DB.Begin()
	_, affected, err := db.RegenerateChain(tx, c, inputs, avoid)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
		return
	}
	tx.Commit()
	db.WriteAudit(s.DB, u.ID, "chain.reallocate", strconv.FormatInt(id, 10), strconv.Itoa(pos))
	setFlash(w, "已为该跳重新分配端口")
	s.dispatchAfterFanout(w, affected, "链路端口重分配")
	http.Redirect(w, r, fmt.Sprintf("/chains/%d", id), http.StatusSeeOther)
}

// setNodeRelayHost saves a node's data-plane address from the node detail page.
func (s *Server) setNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	host := strings.TrimSpace(r.FormValue("relay_host"))
	if host != "" && net.ParseIP(host) == nil && !resolver.IsHostname(host) {
		setFlash(w, "中继地址须为 IPv4 或域名")
		http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
		return
	}
	if err := db.UpdateNodeRelayHost(s.DB, id, host); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.set_relay_host", strconv.FormatInt(id, 10), host)
	setFlash(w, "中继地址已更新")
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d", id), http.StatusSeeOther)
}
```

> 最终文件不应有未使用的标识符或 import（`go vet ./internal/server/...` 必须过）。

- [ ] **Step 2: 注册管理员路由**

在 `internal/server/server.go` 的 admin 组（`r.Use(s.requireAuth, s.requireRole("admin"))` 块内，`/forwards` 路由附近）加：

```go
		r.Post("/nodes/{id}/relay-host", s.setNodeRelayHost)

		r.Get("/chains", s.listChains)
		r.Get("/chains/new", s.newChain)
		r.Post("/chains", s.createChain)
		r.Get("/chains/{id}", s.showChain)
		r.Post("/chains/{id}", s.saveChain)
		r.Post("/chains/{id}/delete", s.deleteChain)
		r.Post("/chains/{id}/hops/{pos}/reallocate", s.reallocateHop)
```

- [ ] **Step 3: 写列表/表单/详情模板**

Create `internal/server/templates/chains.html`：

```html
{{template "header" (mkmap "Title" "链路" "User" .User "Flash" .Flash)}}
<h1>中继链路</h1>
<div class="card">
<h2>所有链路</h2>
{{if .Chains}}
<table>
<thead><tr><th>ID</th><th>名称</th><th>协议</th><th>路径</th><th>入口（可复制）</th><th>操作</th></tr></thead>
<tbody>
{{range .Chains}}
<tr>
<td>{{.Chain.ID}}</td>
<td>{{.Chain.Name}}</td>
<td>{{upper .Chain.Proto}}</td>
<td class="mono">{{.Path}}</td>
<td class="mono">{{.Entry}}</td>
<td>
<a href="/chains/{{.Chain.ID}}">详情</a>
<form method="post" action="/chains/{{.Chain.ID}}/delete" class="inline" onsubmit="return confirm('删除该链路？会删除其所有跳的转发。');"><button class="danger" type="submit">删除</button></form>
</td>
</tr>
{{end}}
</tbody>
</table>
{{else}}<div class="empty">尚无链路。</div>{{end}}
<p style="margin-top:12px"><a href="/chains/new"><button type="button">+ 新建链路</button></a></p>
</div>
{{template "footer" .}}
```

Create `internal/server/templates/chain_form.html`（含极简 vanilla JS：增/删/上移/下移行，提交 `hop_node[]`+`hop_mode[]`）：

```html
{{template "header" (mkmap "Title" "新建链路" "User" .User "Flash" .Flash)}}
<h1>新建中继链路</h1>
<div class="card">
{{if .Nodes}}
<form method="post" action="/chains">
<div class="grid">
<label>名称</label><input name="name" required placeholder="如 vless-seednet">
<label>协议</label><select name="proto" id="proto" onchange="syncModes()"><option value="tcp">TCP</option><option value="udp">UDP</option></select>
<label>出口</label><input name="exit" required placeholder="seednet.example.com:8443（host:port）">
</div>

<h2>中继节点（按顺序，从上到下）</h2>
<table id="hops"><tbody></tbody></table>
<p><button type="button" onclick="addHop()">+ 添加一跳</button></p>
<p style="color:#86868b;font-size:13px">入口 IP:端口由系统自动生成（= 第一跳节点的中继地址 : 自动分配端口），保存后显示。未设「中继地址」的节点不可选。</p>

<div style="margin-top:12px"><button type="submit">创建链路</button> <a href="/chains" style="margin-left:8px">取消</a></div>
</form>

<template id="hopRow">
<tr>
<td><button type="button" onclick="moveHop(this,-1)" class="ghost">↑</button><button type="button" onclick="moveHop(this,1)" class="ghost">↓</button></td>
<td>
<select name="hop_node" required>
<option value="">— 选择节点 —</option>
{{range .Nodes}}<option value="{{.ID}}"{{if eq .RelayHost ""}} disabled{{end}}>{{.Name}}{{if eq .RelayHost ""}}（未设中继地址）{{else}} ({{.RelayHost}}){{end}}</option>{{end}}
</select>
</td>
<td><select name="hop_mode" class="modeSel"><option value="userspace">用户态(split-TCP)</option><option value="kernel">内核态(零拷贝)</option></select></td>
<td><button type="button" onclick="this.closest('tr').remove()" class="danger">删除</button></td>
</tr>
</template>

<script>
function addHop(){
  const t=document.getElementById('hopRow');
  document.querySelector('#hops tbody').appendChild(t.content.cloneNode(true));
  syncModes();
}
function moveHop(btn,dir){
  const tr=btn.closest('tr'), tb=tr.parentNode;
  if(dir<0&&tr.previousElementSibling) tb.insertBefore(tr,tr.previousElementSibling);
  if(dir>0&&tr.nextElementSibling) tb.insertBefore(tr.nextElementSibling,tr);
}
function syncModes(){
  // UDP 链路：用户态不可用，逐跳强制内核态
  const udp=document.getElementById('proto').value==='udp';
  document.querySelectorAll('.modeSel').forEach(s=>{
    const us=s.querySelector('option[value="userspace"]');
    if(us) us.disabled=udp;
    if(udp) s.value='kernel';
  });
}
addHop();
</script>
{{else}}<div class="empty">请先<a href="/nodes">添加节点</a>并设置其中继地址。</div>{{end}}
</div>
{{template "footer" .}}
```

Create `internal/server/templates/chain_detail.html`：

```html
{{template "header" (mkmap "Title" "链路详情" "User" .User "Flash" .Flash)}}
<h1>链路 {{.Chain.Name}} <span style="opacity:0.5;font-size:14px">#{{.Chain.ID}}</span></h1>

<div class="card">
<h2>入口（复制给客户端）</h2>
<pre>{{.View.Entry}}</pre>
<table>
<tr><th style="width:120px">协议</th><td>{{upper .Chain.Proto}}</td></tr>
<tr><th>路径</th><td class="mono">{{.View.Path}}</td></tr>
<tr><th>出口</th><td class="mono">{{.Chain.ExitHost}}:{{.Chain.ExitPort}}</td></tr>
</table>
</div>

<div class="card">
<h2>各跳状态</h2>
<table>
<thead><tr><th>#</th><th>节点</th><th>模式</th><th>监听</th><th>目标</th><th>状态</th><th>操作</th></tr></thead>
<tbody>
{{$fw := .FwByNode}}{{$nb := .NodeByID}}
{{range .Hops}}
{{$n := index $nb .NodeID}}{{$f := index $fw .NodeID}}
<tr>
<td>{{add .Position 1}}</td>
<td>{{if $n}}{{$n.Name}}{{else}}#{{.NodeID}}{{end}}</td>
<td>{{.Mode}}</td>
<td>{{.ListenPort}}</td>
<td class="mono">{{if $f}}{{$f.TargetIP}}:{{$f.TargetPort}}{{end}}</td>
<td>{{if $n}}{{if nullstr $n.LastError}}<span class="pill err">{{nullstr $n.LastError}}</span>{{else if eq $n.Online 1}}<span class="pill ok">在线</span>{{else}}<span class="pill warn">离线/待同步</span>{{end}}{{end}}</td>
<td><form method="post" action="/chains/{{$.Chain.ID}}/hops/{{.Position}}/reallocate" class="inline"><button class="ghost" type="submit">换端口</button></form></td>
</tr>
{{end}}
</tbody>
</table>
</div>

<div class="card">
<h2>编辑链路</h2>
<form method="post" action="/chains/{{.Chain.ID}}">
<div class="grid">
<label>名称</label><input name="name" value="{{.Chain.Name}}" required>
<label>协议</label><select name="proto" id="proto" onchange="syncModes()"><option value="tcp"{{if eq .Chain.Proto "tcp"}} selected{{end}}>TCP</option><option value="udp"{{if eq .Chain.Proto "udp"}} selected{{end}}>UDP</option></select>
<label>出口</label><input name="exit" value="{{.Chain.ExitHost}}:{{.Chain.ExitPort}}" required>
</div>
<h2>中继节点（按顺序）</h2>
<table id="hops"><tbody></tbody></table>
<p><button type="button" onclick="addHop()">+ 添加一跳</button></p>
<div style="margin-top:12px"><button type="submit">保存并重下发</button></div>
</form>

<template id="hopRow">
<tr>
<td><button type="button" onclick="moveHop(this,-1)" class="ghost">↑</button><button type="button" onclick="moveHop(this,1)" class="ghost">↓</button></td>
<td><select name="hop_node" required><option value="">— 选择节点 —</option>{{range .Nodes}}<option value="{{.ID}}"{{if eq .RelayHost ""}} disabled{{end}}>{{.Name}}{{if eq .RelayHost ""}}（未设中继地址）{{else}} ({{.RelayHost}}){{end}}</option>{{end}}</select></td>
<td><select name="hop_mode" class="modeSel"><option value="userspace">用户态(split-TCP)</option><option value="kernel">内核态(零拷贝)</option></select></td>
<td><button type="button" onclick="this.closest('tr').remove()" class="danger">删除</button></td>
</tr>
</template>

<script>
var INIT_HOPS=[{{range .Hops}}{node:"{{.NodeID}}",mode:"{{.Mode}}"},{{end}}];
function addHop(node,mode){
  const t=document.getElementById('hopRow');
  const frag=t.content.cloneNode(true);
  if(node){frag.querySelector('select[name=hop_node]').value=node;}
  if(mode){frag.querySelector('select[name=hop_mode]').value=mode;}
  document.querySelector('#hops tbody').appendChild(frag);
  syncModes();
}
function moveHop(btn,dir){const tr=btn.closest('tr'),tb=tr.parentNode;if(dir<0&&tr.previousElementSibling)tb.insertBefore(tr,tr.previousElementSibling);if(dir>0&&tr.nextElementSibling)tb.insertBefore(tr.nextElementSibling,tr);}
function syncModes(){const udp=document.getElementById('proto').value==='udp';document.querySelectorAll('.modeSel').forEach(s=>{const us=s.querySelector('option[value="userspace"]');if(us)us.disabled=udp;if(udp)s.value='kernel';});}
INIT_HOPS.forEach(h=>addHop(h.node,h.mode));
</script>
</div>

<div><a href="/chains">← 返回链路列表</a></div>
{{template "footer" .}}
```

- [ ] **Step 4: 导航加「链路」**

在 `internal/server/templates/_layout.html` 的 admin 导航块里，`<a href="/forwards">转发</a>` 之后加一行：

```html
<a href="/chains">链路</a>
```

- [ ] **Step 5: 节点详情页加中继地址展示 + 编辑**

在 `internal/server/templates/node_detail.html` 的「基本信息」表格里，`<tr><th ...>地址</th>...</tr>` 之后加一行展示 + 卡片底部加编辑表单。把「基本信息」card 改为：

```html
<div class="card">
<h2>基本信息</h2>
<table>
<tr><th style="width:140px">地址（控制面）</th><td class="mono">{{.Node.Address}}</td></tr>
<tr><th>中继地址（数据面）</th><td class="mono">{{if .Node.RelayHost}}{{.Node.RelayHost}}{{else}}<span style="color:#86868b">未设置（设置后才能进链路）</span>{{end}}</td></tr>
<tr><th>Token</th><td class="mono">{{.Node.Secret}}</td></tr>
<tr><th>最近同步</th><td>{{unix .Node.LastApplyAt}}</td></tr>
<tr><th>最近心跳</th><td>{{unix .Node.LastSeen}} {{if eq .Node.Online 1}}<span class="pill ok">在线</span>{{else}}<span class="pill warn">离线</span>{{end}}</td></tr>
<tr><th>状态</th><td>
{{if .Node.Disabled}}<span class="pill warn">禁用</span>
{{else if nullstr .Node.LastError}}<span class="pill err">错误：{{nullstr .Node.LastError}}</span>
{{else if .Node.LastApplyAt.Valid}}<span class="pill ok">已同步</span>
{{else}}<span class="pill warn">待同步（agent 未连上或地址不通）</span>{{end}}
</td></tr>
</table>
<form method="post" action="/nodes/{{.Node.ID}}/relay-host" style="margin-top:12px;display:flex;gap:8px;align-items:center">
<input name="relay_host" value="{{.Node.RelayHost}}" placeholder="数据面公网 IPv4 或域名" style="flex:1">
<button type="submit">保存中继地址</button>
</form>
<p style="color:#86868b;font-size:13px;margin-top:6px">中继链路用它作为上一跳打向本节点的目标地址；与上面的控制面地址（agent 反向连面板）不同。</p>
</div>
```

- [ ] **Step 6: 写管理员 handler 的 httptest**

Create `internal/server/chains_test.go`：

```go
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

func TestCreateChainWiresForwardsAndShowsEntry(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("name", "vless")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_node"] = []string{fmt.Sprint(g.ID), fmt.Sprint(h.ID)}
	form["hop_mode"] = []string{"userspace", "kernel"}

	req := httptest.NewRequest("POST", "/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, chains[0].ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 hop forwards, got %d", len(fws))
	}
	// 入口端口落库
	c, _ := db.GetChain(d, chains[0].ID)
	if !c.EntryNodeID.Valid || c.EntryListenPort == 0 {
		t.Fatalf("entry not recorded: %+v", c)
	}
}

func TestCreateChainRejectsNodeWithoutRelayHost(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	bare, _ := db.CreateNode(d, "bare", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")

	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_node"] = []string{fmt.Sprint(g.ID), fmt.Sprint(bare.ID)}
	form["hop_mode"] = []string{"kernel", "kernel"}
	req := httptest.NewRequest("POST", "/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	chains, _ := db.ListAdminChains(d)
	if len(chains) != 0 {
		t.Fatalf("chain must not persist when a hop node lacks relay_host; got %d", len(chains))
	}
}
```

> 注：`createChain` 在 `RegenerateChain` 失败时 `tx.Rollback()`，已 `CreateChain` 的 header 行随事务回滚，故第二个用例断言 0 条链路成立。

- [ ] **Step 7: 跑测试 + vet**

Run: `go vet ./internal/server/... && go test ./internal/server/ -run TestCreateChain -v`
Expected: PASS。

- [ ] **Step 8: 全量回归**

Run: `go test ./... -count=1`
Expected: PASS。

- [ ] **Step 9: Commit**

```bash
git add internal/server/chains.go internal/server/chains_test.go internal/server/server.go internal/server/templates/chains.html internal/server/templates/chain_form.html internal/server/templates/chain_detail.html internal/server/templates/_layout.html internal/server/templates/node_detail.html
git commit -m "feat(server): admin relay chain UI — build, reorder, reallocate, node relay_host"
```

---

## Task 6: 租户链路 UI（积木 = 已授权通道）

**Files:**
- Create: `internal/server/my_chains.go`、`internal/server/my_chains_test.go`
- Create: `internal/server/templates/my_chains.html`、`my_chain_form.html`
- Modify: `internal/server/server.go`（租户路由）、`internal/server/templates/_layout.html`（租户导航）、`internal/server/templates/my_dashboard.html`（入口链接，可选）

- [ ] **Step 1: 写租户 handler（含通道→节点解析 + 策略校验）**

Create `internal/server/my_chains.go`：

```go
package server

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

func (s *Server) tenantListChains(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	chains, _ := db.ListChainsByTenant(s.DB, t.ID)
	views := make([]chainView, 0, len(chains))
	for _, c := range chains {
		views = append(views, s.buildChainView(c))
	}
	s.render(w, "my_chains.html", map[string]any{"User": u, "Chains": views, "Flash": flashFromCookie(w, r)})
}

func (s *Server) tenantNewChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	tunnels, _, _ := db.ListTunnelsForTenant(s.DB, t.ID)
	nodes, _ := db.ListNodes(s.DB)
	nodeByID := map[int64]*db.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	s.render(w, "my_chain_form.html", map[string]any{
		"User": u, "Tunnels": tunnels, "NodeByID": nodeByID, "Flash": flashFromCookie(w, r),
	})
}

// tenantHopInputs reads hop_tunnel[] + hop_mode[], verifies each tunnel is
// granted to the tenant, and derives the node from the tunnel. It also returns
// the last hop's tunnel so the caller can enforce the exit CIDR.
func (s *Server) tenantHopInputs(r *http.Request, tenantID int64) ([]db.HopInput, *db.Tunnel, error) {
	tunnelIDs := r.Form["hop_tunnel"]
	modes := r.Form["hop_mode"]
	if len(tunnelIDs) == 0 {
		return nil, nil, fmt.Errorf("至少添加一个通道")
	}
	hops := make([]db.HopInput, 0, len(tunnelIDs))
	var last *db.Tunnel
	for i, idStr := range tunnelIDs {
		tid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || tid == 0 {
			return nil, nil, fmt.Errorf("第 %d 跳通道非法", i+1)
		}
		if _, err := db.GetGrant(s.DB, tenantID, tid); err != nil {
			return nil, nil, fmt.Errorf("无权使用通道 %d", tid)
		}
		tun, err := db.GetTunnel(s.DB, tid)
		if err != nil {
			return nil, nil, fmt.Errorf("通道 %d 不存在", tid)
		}
		mode := "kernel"
		if i < len(modes) {
			mode = modes[i]
		}
		hops = append(hops, db.HopInput{NodeID: tun.NodeID, TunnelID: nullInt64(tid), Mode: mode})
		last = tun
	}
	return hops, last, nil
}

// nullInt64 wraps a valid int64 for the TenantID/TunnelID sql.NullInt64 fields.
func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func (s *Server) tenantCreateChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if t.Disabled {
		setFlash(w, "用户已被禁用")
		http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "表单解析失败")
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	proto := strings.ToLower(strings.TrimSpace(r.FormValue("proto")))
	if name == "" || (proto != "tcp" && proto != "udp") {
		setFlash(w, "名称必填，协议须为 tcp 或 udp")
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	exitHost, exitPort, err := parseExit(r.FormValue("exit"))
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	hops, lastTunnel, err := s.tenantHopInputs(r, t.ID)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	// 出口必须落在末跳通道的 CIDR 白名单内（中间跳目标是受信中继地址，免检）。
	if err := exitAllowedByTunnel(lastTunnel, exitHost); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	// 配额：新链路新增 len(hops) 条 forward（每个通道 1 条）。
	if err := s.checkTenantChainQuota(t, hops, 0); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}

	tx, err := s.DB.Begin()
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	c := &db.Chain{TenantID: nullInt64(t.ID), Name: name, Proto: proto, ExitHost: exitHost, ExitPort: exitPort}
	id, err := db.CreateChain(tx, c)
	if err != nil {
		tx.Rollback()
		setFlash(w, "创建失败: "+err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	c.ID = id
	entry, affected, err := db.RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains/new", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.tenant_create", strconv.FormatInt(id, 10), name)
	setFlash(w, "链路已创建，入口："+entry)
	s.dispatchAfterFanout(w, affected, "链路创建")
	http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
}

func (s *Server) tenantDeleteChain(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	t, err := s.tenantContext(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	c, err := db.GetChain(s.DB, id)
	if err != nil || !c.TenantID.Valid || c.TenantID.Int64 != t.ID {
		http.Error(w, "无权操作该链路", http.StatusForbidden)
		return
	}
	nodes, err := db.DeleteChain(s.DB, id)
	if err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
		return
	}
	db.WriteAudit(s.DB, u.ID, "chain.tenant_delete", strconv.FormatInt(id, 10), "")
	s.dispatchAfterFanout(w, nodes, "链路删除")
	http.Redirect(w, r, "/my/chains", http.StatusSeeOther)
}

// exitAllowedByTunnel enforces the tenant security model on the user-chosen exit
// (the only arbitrary destination in a tenant chain): IPv4 must fall in the
// tunnel CIDR allowlist; a hostname exit is rejected when any CIDR is set
// (can't statically prove containment) — mirrors validateAgainstTunnel.
func exitAllowedByTunnel(t *db.Tunnel, exitHost string) error {
	if t == nil {
		return fmt.Errorf("末跳通道缺失")
	}
	ip := net.ParseIP(exitHost)
	if ip == nil {
		if strings.TrimSpace(t.TargetCIDRAllow) != "" {
			return fmt.Errorf("末跳通道限制了目标 CIDR，出口仅允许 IPv4")
		}
		return nil
	}
	if ip.To4() == nil {
		return fmt.Errorf("出口必须为 IPv4")
	}
	if !targetIPInCIDR(ip, t.TargetCIDRAllow) {
		return fmt.Errorf("出口地址不在末跳通道允许的 CIDR 内（%s）", t.TargetCIDRAllow)
	}
	return nil
}

// checkTenantChainQuota verifies the tenant's total + per-tunnel max_forwards can
// absorb this chain's hops. existingChainForwards is the count this chain already
// holds (0 for a new chain; >0 on edit) so editing in place isn't double-counted.
func (s *Server) checkTenantChainQuota(t *db.Tenant, hops []db.HopInput, existingChainForwards int) error {
	total, _ := db.CountForwardsForTenant(s.DB, t.ID)
	if (total-existingChainForwards)+len(hops) > t.MaxForwards {
		return fmt.Errorf("超出用户最大转发数（%d）", t.MaxForwards)
	}
	for _, h := range hops {
		if !h.TunnelID.Valid {
			continue
		}
		grant, err := db.GetGrant(s.DB, t.ID, h.TunnelID.Int64)
		if err != nil {
			return fmt.Errorf("无权使用通道 %d", h.TunnelID.Int64)
		}
		cnt, _ := db.CountForwardsForTenantTunnel(s.DB, t.ID, h.TunnelID.Int64)
		// 同节点禁重复 => 每通道至多 1 跳，故 +1。
		if cnt+1 > grant.MaxForwards {
			return fmt.Errorf("通道 %d 已达最大转发数（%d）", h.TunnelID.Int64, grant.MaxForwards)
		}
	}
	return nil
}
```

> `nullInt64` 用 `sql.NullInt64` 包装 TenantID/TunnelID（`my_chains.go` 已 import `database/sql`）。

- [ ] **Step 2: 注册租户路由 + 导航**

在 `internal/server/server.go` 的 tenant 组（`r.Use(s.requireAuth, s.requireRole("tenant"))` 块内）加：

```go
		r.Get("/my/chains", s.tenantListChains)
		r.Get("/my/chains/new", s.tenantNewChain)
		r.Post("/my/chains", s.tenantCreateChain)
		r.Post("/my/chains/{id}/delete", s.tenantDeleteChain)
```

在 `internal/server/templates/_layout.html` 租户导航块（`{{else}}` 分支）里 `<a href="/my/forwards">我的转发</a>` 之后加：

```html
<a href="/my/chains">我的链路</a>
```

- [ ] **Step 3: 写租户模板**

Create `internal/server/templates/my_chains.html`：

```html
{{template "header" (mkmap "Title" "我的链路" "User" .User "Flash" .Flash)}}
<h1>我的中继链路</h1>
<div class="card">
<h2>链路列表</h2>
{{if .Chains}}
<table>
<thead><tr><th>ID</th><th>名称</th><th>协议</th><th>路径</th><th>入口（可复制）</th><th>操作</th></tr></thead>
<tbody>
{{range .Chains}}
<tr>
<td>{{.Chain.ID}}</td><td>{{.Chain.Name}}</td><td>{{upper .Chain.Proto}}</td>
<td class="mono">{{.Path}}</td><td class="mono">{{.Entry}}</td>
<td><form method="post" action="/my/chains/{{.Chain.ID}}/delete" class="inline" onsubmit="return confirm('删除该链路？');"><button class="danger" type="submit">删除</button></form></td>
</tr>
{{end}}
</tbody>
</table>
{{else}}<div class="empty">尚无链路。</div>{{end}}
<p style="margin-top:12px"><a href="/my/chains/new"><button type="button">+ 新建链路</button></a></p>
</div>
{{template "footer" .}}
```

Create `internal/server/templates/my_chain_form.html`：

```html
{{template "header" (mkmap "Title" "新建链路" "User" .User "Flash" .Flash)}}
<h1>新建中继链路</h1>
<div class="card">
{{if .Tunnels}}
<form method="post" action="/my/chains">
<div class="grid">
<label>名称</label><input name="name" required>
<label>协议</label><select name="proto" id="proto" onchange="syncModes()"><option value="tcp">TCP</option><option value="udp">UDP</option></select>
<label>出口</label><input name="exit" required placeholder="目标 host:port（须落在末跳通道 CIDR 内）">
</div>
<h2>中继通道（按顺序）</h2>
<table id="hops"><tbody></tbody></table>
<p><button type="button" onclick="addHop()">+ 添加一跳</button></p>
<p style="color:#86868b;font-size:13px">从你被授权的通道里挑选并排序；入口 IP:端口由系统自动生成并在保存后显示。每一跳消耗对应通道一条转发配额。</p>
<div style="margin-top:12px"><button type="submit">创建链路</button> <a href="/my/chains" style="margin-left:8px">取消</a></div>
</form>

<template id="hopRow">
<tr>
<td><button type="button" onclick="moveHop(this,-1)" class="ghost">↑</button><button type="button" onclick="moveHop(this,1)" class="ghost">↓</button></td>
<td><select name="hop_tunnel" required>
<option value="">— 选择通道 —</option>
{{$nb := .NodeByID}}
{{range .Tunnels}}{{$n := index $nb .NodeID}}<option value="{{.ID}}">{{.Name}} @ {{if $n}}{{$n.Name}}{{if eq $n.RelayHost ""}}（节点未设中继地址）{{end}}{{else}}#{{.NodeID}}{{end}}（{{.PortStart}}-{{.PortEnd}}）</option>{{end}}
</select></td>
<td><select name="hop_mode" class="modeSel"><option value="userspace">用户态(split-TCP)</option><option value="kernel">内核态(零拷贝)</option></select></td>
<td><button type="button" onclick="this.closest('tr').remove()" class="danger">删除</button></td>
</tr>
</template>

<script>
function addHop(){document.querySelector('#hops tbody').appendChild(document.getElementById('hopRow').content.cloneNode(true));syncModes();}
function moveHop(btn,dir){const tr=btn.closest('tr'),tb=tr.parentNode;if(dir<0&&tr.previousElementSibling)tb.insertBefore(tr,tr.previousElementSibling);if(dir>0&&tr.nextElementSibling)tb.insertBefore(tr.nextElementSibling,tr);}
function syncModes(){const udp=document.getElementById('proto').value==='udp';document.querySelectorAll('.modeSel').forEach(s=>{const us=s.querySelector('option[value="userspace"]');if(us)us.disabled=udp;if(udp)s.value='kernel';});}
addHop();
</script>
{{else}}<div class="empty">管理员尚未为你授权任何通道，无法建链路。请联系管理员。</div>{{end}}
</div>
{{template "footer" .}}
```

（可选）在 `internal/server/templates/my_dashboard.html` 顶部加一行入口：`<p><a href="/my/chains">→ 我的链路</a></p>`。

- [ ] **Step 4: 写租户 handler 测试**

Create `internal/server/my_chains_test.go`：

```go
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// loginAsTenant creates a tenant + bound user + session, returns the cookie.
func loginAsTenant(t *testing.T, d *sql.DB, tenantID int64) *http.Cookie {
	t.Helper()
	hash, _ := HashPassword("pw")
	uid, err := db.CreateTenantUser(d, tenantID, fmt.Sprintf("tenant-%d", tenantID), hash)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func TestTenantCreateChainAcrossGrantedTunnels(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	h, _ := db.CreateNode(d, "nnc-hk", "https://p", "t2")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, h.ID, "2.2.2.2")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	tunA, _ := db.CreateTunnel(d, &db.Tunnel{Name: "a", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	tunB, _ := db.CreateTunnel(d, &db.Tunnel{Name: "b", NodeID: h.ID, ProtoMask: "tcp+udp", PortStart: 31000, PortEnd: 31100, TargetCIDRAllow: "0.0.0.0/0"})
	_ = db.GrantTunnel(d, tid, tunA, 5)
	_ = db.GrantTunnel(d, tid, tunB, 5)

	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "vless")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(tunA), fmt.Sprint(tunB)}
	form["hop_mode"] = []string{"userspace", "userspace"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 1 {
		t.Fatalf("want 1 tenant chain, got %d", len(chains))
	}
	fws, _ := db.ListForwardsByChain(d, chains[0].ID)
	if len(fws) != 2 {
		t.Fatalf("want 2 forwards, got %d", len(fws))
	}
	for _, f := range fws {
		if !f.TenantID.Valid || f.TenantID.Int64 != tid {
			t.Fatalf("tenant chain forward must carry tenant_id")
		}
		if !f.TunnelID.Valid {
			t.Fatalf("tenant chain forward must carry tunnel_id")
		}
		// 端口落在对应通道段内
		if f.NodeID == g.ID && (f.ListenPort < 30000 || f.ListenPort > 30100) {
			t.Fatalf("hop on gomami port %d out of tunnel range", f.ListenPort)
		}
	}
}

func TestTenantCreateChainRejectsUngrantedTunnel(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "gomami", "https://p", "t1")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	tid, _ := db.CreateTenant(d, &db.Tenant{Name: "acme", MaxForwards: 10})
	other, _ := db.CreateTunnel(d, &db.Tunnel{Name: "x", NodeID: g.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	// 不 grant
	s, _ := New(d)
	form := url.Values{}
	form.Set("name", "x")
	form.Set("proto", "tcp")
	form.Set("exit", "9.9.9.9:8443")
	form["hop_tunnel"] = []string{fmt.Sprint(other)}
	form["hop_mode"] = []string{"kernel"}
	req := httptest.NewRequest("POST", "/my/chains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAsTenant(t, d, tid))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	chains, _ := db.ListChainsByTenant(d, tid)
	if len(chains) != 0 {
		t.Fatalf("ungranted tunnel must be rejected; got %d chains", len(chains))
	}
}
```

> `loginAsTenant` 引用了 `sql.DB`，须在本测试文件 import `"database/sql"`。

- [ ] **Step 5: 跑测试 + vet**

Run: `go vet ./internal/server/... && go test ./internal/server/ -run 'TestTenant.*Chain' -v`
Expected: PASS。

- [ ] **Step 6: 全量回归**

Run: `go test ./... -count=1 && go vet ./...`
Expected: PASS。

- [ ] **Step 7: Commit**

```bash
git add internal/server/my_chains.go internal/server/my_chains_test.go internal/server/server.go internal/server/templates/my_chains.html internal/server/templates/my_chain_form.html internal/server/templates/_layout.html internal/server/templates/my_dashboard.html
git commit -m "feat(server): tenant relay chains over granted tunnels with quota/CIDR enforcement"
```

---

## Task 7: 集成验证 + 文档

**Files:**
- Modify: `README.md`（命令表面/协议参考附近补一段链路说明）
- 手动验证（docker fixture / 真机）

- [ ] **Step 1: gofmt + 全量测试 + vet**

Run: `gofmt -l internal/ && go vet ./... && go test ./... -count=1`
Expected: `gofmt -l` 无输出；vet/test 全 PASS。

- [ ] **Step 2: README 补充链路说明**

在 `README.md` 的「命令表面」或「配置与持久化」后补一小节（示意，按现有文风精简）：

```markdown
### 中继链路（Relay Chain）

面板「链路」页可把多台受管节点串成一条中继链：选有序的跳 + 填出口 `host:port`，
系统自动分配并对齐各跳监听端口、生成可复制的入口 `IP:端口`。每个进链路的节点需先在
节点详情页设置「中继地址」（数据面公网 IPv4/域名）。逐跳可选内核态(零拷贝)或用户态
(split-TCP，多跳推荐)。租户在「我的链路」里用自己被授权的通道串链，端口/CIDR/带宽/配额
沿用通道约束。
```

- [ ] **Step 3: 手动验证清单（在 docker fixture 或真机执行并记录结果）**

```
[ ] 两个 agent 节点设 relay_host，建一条 2 跳 admin 链路（userspace）→ 入口端点对 vless/裸 TCP 连通
[ ] 改顺序保存 → 未变节点端口不变；规则按新序重下发
[ ] 节点 tui 段先占一个端口 → 建经该节点链路，自动分配避开该端口（不触发 daemon 409）
[ ] 停一个 agent 后建/改链路 → 链路落库、该跳显示离线/待同步、agent 重连后规则补齐
[ ] 删除链路 → 两节点对应 forward 被清
[ ] 租户：授权 2 个不同节点的通道 → 「我的链路」串链成功；出口超 CIDR 被拒；超 max_forwards 被拒
[ ] 复刻 gomami→nnc-hk→nnc-tw→seednet（userspace 各跳），入口给客户端跑通
```

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs(readme): document relay chain feature"
```

---

## Self-Review（计划自检结论）

- **Spec 覆盖**：relay_host（T1/T5）、chains/chain_hops/forwards.chain_id（T1）、完整占用检查 + 回填手动建转发（T2）、链路 CRUD（T3）、RegenerateChain 分配/对齐/结构校验/稳定复用/udp→kernel/同节点禁重复（T4）、管理员 UI + 节点中继地址 + 一键重分配（T5）、租户 UI + 通道积木 + 出口 CIDR + 配额（T6）、离线容忍/下发（复用 dispatchAfterFanout，T5/T6）、集成与手动验证（T7）。
- **类型一致**：`DBTX`、`HopInput{NodeID,TunnelID,Mode}`、`RegenerateChain(tx,*Chain,[]HopInput,avoid)`、`OccupiedPortsOnNode(d,node,proto,excludeChainID)`、`Chain`/`ChainHop` 字段、`ChainPortMin/Max`、`chainView{Chain,Path,Entry}` 在各任务间保持一致。
- **占位符**：草稿里 `var _ = fmt.Sprintf`（T2 占位，T4 删除）与 `nodeNames`（T5 提示删除）均已显式标注移除时机，落地代码不得残留。
- **已知风险（spec 风险表）**：tui 快照过时 / userspace OS 端口冲突 → 由「换端口」(reallocate) + daemon 错误回传兜底；租户多跳 N× 配额 → UI 文案告知；部分下发失败 → 节点级状态 + resync。
