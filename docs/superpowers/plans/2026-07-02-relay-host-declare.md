# daemon 显式声明 relay_host（v4/v6）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 daemon 能在启动参数里显式声明自己的数据面地址（relay_host / relay_host_v6），随 Hello 上报给 server，server 端把声明值当作权威来源（覆盖优先级高于自动识别与手动 UI 编辑），解决双出口中转机（如 hep-ix）relay_host 被自动填成出口 IP 的问题。

**Architecture:** 协议层给 `Hello` 加两个可选字段；server 在 `fillNodeRelayHosts`（自动识别，仅填空字段）之前新增 `applyDeclaredRelayHosts`（声明值，无条件覆盖并落一个 `*_declared` 标记位）；`nodes` 表加两个布尔列记录"这个字段当前是否被声明值锁定"，UI 和手动覆盖 API 据此拒绝对锁定字段的编辑；daemon 侧新增 CLI flag 把值一路透传进 Hello。

**Tech Stack:** Go（server + daemon 共用 `internal/db`、`internal/wsproto`）、SQLite 迁移（`internal/db/migrations`）、bash（`install.sh`）、React（`web/src/pages/nodes/Detail.jsx`）。

## Global Constraints

- 代码注释、commit message 里禁止出现任务编号、方案代号、审阅轮次等过程性信息（如"Task 3"、"Phase 2"）；只写 WHY 和不变量。
- Go 代码一律 tab 缩进，改完跑 `gofmt -w` 落盘格式化（`queries.go`/`grants.go`/`api.go`/`messages.go`/`daemon.go` 在改动前就已经不是 gofmt 干净状态，是历史遗留问题，不是本次改动引入的；`-w` 会顺手把它们格式化，这是预期行为，不用纠结这几个文件为什么有非本次改动的格式变化）。
- 每个 `nodes` 表加列，必须同时改齐三处：`internal/db/queries.go` 的 `nodeCols` 常量、`scanNode`、`internal/db/grants.go` 的 `ListNodesForUser` 内联 scan——漏掉第三处会在授权节点列表接口里静默清空该列。
- 声明值路径（`applyDeclaredRelayHosts`）是自动化的按连接同步，不写 `db.WriteAudit`，跟现有 `fillNodeRelayHosts` 的处理方式保持一致。
- 声明值校验失败时只记日志、不能中断 WebSocket 握手。

---

## Task 1: DB 层——relay_host_declared / relay_host_v6_declared 列

**Files:**
- Create: `internal/db/migrations/0024_node_relay_host_declared.sql`
- Modify: `internal/db/queries.go:37-73`（`Node` struct）、`internal/db/queries.go:265`（`nodeCols`）、`internal/db/queries.go:274-313`（`scanNode`）、`internal/db/queries.go:460-470`（新增两个 Set 函数）
- Modify: `internal/db/grants.go:94-146`（`ListNodesForUser` 内联 scan）
- Test: `internal/db/queries_test.go`

**Interfaces:**
- Produces: `db.Node.RelayHostDeclared bool`、`db.Node.RelayHostV6Declared bool`；`db.SetNodeRelayHostDeclared(d *sql.DB, id int64, declared bool) error`；`db.SetNodeRelayHostV6Declared(d *sql.DB, id int64, declared bool) error`

- [ ] **Step 1: 写迁移文件**

创建 `internal/db/migrations/0024_node_relay_host_declared.sql`：

```sql
ALTER TABLE nodes ADD COLUMN relay_host_declared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN relay_host_v6_declared INTEGER NOT NULL DEFAULT 0;
```

- [ ] **Step 2: 写失败的 round-trip 测试**

在 `internal/db/queries_test.go` 末尾追加：

```go
func TestNodeRelayHostDeclaredRoundTrip(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "n1", "https://p", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHostDeclared || n.RelayHostV6Declared {
		t.Fatalf("new node should start undeclared, got %+v", n)
	}

	if err := UpdateNodeRelayHost(d, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostDeclared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostV6Declared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be true after SetNodeRelayHostDeclared(true)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should be true after SetNodeRelayHostV6Declared(true)")
	}

	if err := SetNodeRelayHostDeclared(d, n.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be false after SetNodeRelayHostDeclared(false)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should remain true (only the v4 flag was cleared)")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/db/... -run TestNodeRelayHostDeclaredRoundTrip -v`
Expected: FAIL — `n.RelayHostDeclared`/`SetNodeRelayHostDeclared` 未定义（编译错误）。

- [ ] **Step 4: Node struct 加字段**

`internal/db/queries.go:44-45` 现状：

```go
	RelayHost   string       `json:"relay_host"`
	RelayHostV6 string       `json:"relay_host_v6"`
```

改成：

```go
	RelayHost           string `json:"relay_host"`
	RelayHostV6         string `json:"relay_host_v6"`
	RelayHostDeclared   bool   `json:"relay_host_declared"`
	RelayHostV6Declared bool   `json:"relay_host_v6_declared"`
```

- [ ] **Step 5: nodeCols 加列**

`internal/db/queries.go:265` 现状：

```go
const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,relay_host_v6,online,agent_version,agent_sha,last_seen,last_apply_at,last_error,last_warning,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error,hidden,sort_order,rate_multiplier,unidirectional`
```

改成（末尾追加两列）：

```go
const nodeCols = `id,name,node_type,owner_id,address,secret,relay_host,relay_host_v6,online,agent_version,agent_sha,last_seen,last_apply_at,last_error,last_warning,disabled,local_migrated_at,port_range,created_at,last_upgrade_at,last_upgrade_version,last_upgrade_status,last_upgrade_error,hidden,sort_order,rate_multiplier,unidirectional,relay_host_declared,relay_host_v6_declared`
```

- [ ] **Step 6: scanNode 加列**

`internal/db/queries.go:274-313` 现状：

```go
func scanNode(r rowScanner) (*Node, error) {
	n := &Node{}
	var disabled, hidden, unidirectional int
	var localMigratedAt, lastSeen sql.NullInt64
	var agentVersion sql.NullString
	var ownerID sql.NullInt64
	var luVersion, luStatus, luError sql.NullString
	if err := r.Scan(
		&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
		&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
		&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
		&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
		&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
		&hidden, &n.SortOrder, &n.RateMultiplier, &unidirectional,
	); err != nil {
		return nil, err
	}
	n.Disabled = disabled == 1
	n.Hidden = hidden == 1
	n.Unidirectional = unidirectional == 1
```

改成：

```go
func scanNode(r rowScanner) (*Node, error) {
	n := &Node{}
	var disabled, hidden, unidirectional, relayHostDeclared, relayHostV6Declared int
	var localMigratedAt, lastSeen sql.NullInt64
	var agentVersion sql.NullString
	var ownerID sql.NullInt64
	var luVersion, luStatus, luError sql.NullString
	if err := r.Scan(
		&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
		&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
		&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
		&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
		&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
		&hidden, &n.SortOrder, &n.RateMultiplier, &unidirectional,
		&relayHostDeclared, &relayHostV6Declared,
	); err != nil {
		return nil, err
	}
	n.Disabled = disabled == 1
	n.Hidden = hidden == 1
	n.Unidirectional = unidirectional == 1
	n.RelayHostDeclared = relayHostDeclared == 1
	n.RelayHostV6Declared = relayHostV6Declared == 1
```

（函数其余部分不变。）

- [ ] **Step 7: grants.go 的 ListNodesForUser 内联 scan 对齐**

`internal/db/grants.go:94-124` 现状：

```go
func ListNodesForUser(d *sql.DB, userID int64) ([]*Node, []*UserNode, error) {
	rows, err := d.Query(`
		SELECT `+nodeCols+`,
		       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []*Node
	var grants []*UserNode
	for rows.Next() {
		n := &Node{}
		g := &UserNode{UserID: userID}
		var disabled, hidden, unidirectional int
		var localMigratedAt, lastSeen sql.NullInt64
		var agentVersion sql.NullString
		var ownerID sql.NullInt64
		var luVersion, luStatus, luError sql.NullString
		if err := rows.Scan(
			&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
			&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
			&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
			&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
			&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
			&hidden, &n.SortOrder, &n.RateMultiplier, &unidirectional,
			&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.GrantedAt,
		); err != nil {
			return nil, nil, err
		}
		n.LastUpgradeVersion = luVersion.String
		n.LastUpgradeStatus = luStatus.String
		n.LastUpgradeError = luError.String
		n.Disabled = disabled == 1
		n.Hidden = hidden == 1
		n.Unidirectional = unidirectional == 1
```

改成：

```go
func ListNodesForUser(d *sql.DB, userID int64) ([]*Node, []*UserNode, error) {
	rows, err := d.Query(`
		SELECT `+nodeCols+`,
		       g.max_forwards, g.traffic_quota_bytes, g.traffic_used_bytes, g.granted_at
		FROM nodes n JOIN user_nodes g ON g.node_id = n.id
		WHERE g.user_id = ? ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []*Node
	var grants []*UserNode
	for rows.Next() {
		n := &Node{}
		g := &UserNode{UserID: userID}
		var disabled, hidden, unidirectional, relayHostDeclared, relayHostV6Declared int
		var localMigratedAt, lastSeen sql.NullInt64
		var agentVersion sql.NullString
		var ownerID sql.NullInt64
		var luVersion, luStatus, luError sql.NullString
		if err := rows.Scan(
			&n.ID, &n.Name, &n.NodeType, &ownerID, &n.Address, &n.Secret,
			&n.RelayHost, &n.RelayHostV6, &n.Online, &agentVersion, &n.AgentSHA,
			&lastSeen, &n.LastApplyAt, &n.LastError, &n.LastWarning,
			&disabled, &localMigratedAt, &n.PortRange, &n.CreatedAt,
			&n.LastUpgradeAt, &luVersion, &luStatus, &luError,
			&hidden, &n.SortOrder, &n.RateMultiplier, &unidirectional,
			&relayHostDeclared, &relayHostV6Declared,
			&g.MaxForwards, &g.TrafficQuotaBytes, &g.TrafficUsedBytes, &g.GrantedAt,
		); err != nil {
			return nil, nil, err
		}
		n.LastUpgradeVersion = luVersion.String
		n.LastUpgradeStatus = luStatus.String
		n.LastUpgradeError = luError.String
		n.Disabled = disabled == 1
		n.Hidden = hidden == 1
		n.Unidirectional = unidirectional == 1
		n.RelayHostDeclared = relayHostDeclared == 1
		n.RelayHostV6Declared = relayHostV6Declared == 1
```

（函数其余部分不变。）

- [ ] **Step 8: 新增 Set 函数**

`internal/db/queries.go:467-470` 后追加：

```go
func SetNodeRelayHostDeclared(d *sql.DB, id int64, declared bool) error {
	v := 0
	if declared {
		v = 1
	}
	_, err := d.Exec(`UPDATE nodes SET relay_host_declared=? WHERE id=?`, v, id)
	return err
}

func SetNodeRelayHostV6Declared(d *sql.DB, id int64, declared bool) error {
	v := 0
	if declared {
		v = 1
	}
	_, err := d.Exec(`UPDATE nodes SET relay_host_v6_declared=? WHERE id=?`, v, id)
	return err
}
```

- [ ] **Step 9: 跑新测试 + 既有的三处对齐守护测试**

Run: `go test ./internal/db/... -run TestNodeRelayHostDeclaredRoundTrip -v`
Expected: PASS

Run: `go test ./internal/server/... -run TestListNodesForUserAfterGrant -v`
Expected: PASS（这条已有测试专门守护 `nodeCols`/scan 数量对齐，如果 grants.go 没改到位，这里会因 scan 参数数量不匹配而报错）

- [ ] **Step 10: gofmt + 提交**

Run: `gofmt -w internal/db/queries.go internal/db/grants.go internal/db/queries_test.go`
Expected: 无输出（`-w` 直接落盘格式化；这两个源文件在本次改动前就不是 gofmt 干净状态，格式化后 `git diff` 里可能看到少量本次未触及的历史字段对齐变化，属预期行为）

```bash
git add internal/db/migrations/0024_node_relay_host_declared.sql internal/db/queries.go internal/db/grants.go internal/db/queries_test.go
git commit -m "feat(db): add relay_host_declared/relay_host_v6_declared columns"
```

---

## Task 2: server 端——applyDeclaredRelayHosts

**Files:**
- Modify: `internal/wsproto/messages.go:73-90`（`Hello` struct）
- Modify: `internal/server/api.go:2521-2526`（提取 `isValidRelayHostV6`）、`internal/server/api.go:648-680`（`apiSetNodeRelayHostV6` 改用它）
- Modify: `internal/server/hub.go:149-153`（`ServeWS` 接入）、新增 `applyDeclaredRelayHosts` 函数
- Test: `internal/server/hub_test.go`

**Interfaces:**
- Consumes: `db.Node.RelayHostDeclared/RelayHostV6Declared`、`db.SetNodeRelayHostDeclared/SetNodeRelayHostV6Declared`、`db.UpdateNodeRelayHost/UpdateNodeRelayHostV6`（均来自 Task 1）；已有的 `isValidRelayHost`
- Produces: `wsproto.Hello.DeclaredRelayHost/DeclaredRelayHostV6 string`；`isValidRelayHostV6(host string) bool`；`applyDeclaredRelayHosts(d *sql.DB, node *db.Node, declaredV4, declaredV6 string)`

- [ ] **Step 1: Hello 协议加字段**

`internal/wsproto/messages.go:83-90` 现状：

```go
	PortRange      string `json:"port_range,omitempty"`
	// ProbedV4/ProbedV6 are this agent's own best-guess outbound address per
	// family, re-probed fresh on every hello. The panel only uses these to
	// seed the family its own connection-observed address didn't cover —
	// see hub.go's fillNodeRelayHosts. Empty from agents that predate this probe.
	ProbedV4 string `json:"probed_v4,omitempty"`
	ProbedV6 string `json:"probed_v6,omitempty"`
}
```

改成：

```go
	PortRange      string `json:"port_range,omitempty"`
	// ProbedV4/ProbedV6 are this agent's own best-guess outbound address per
	// family, re-probed fresh on every hello. The panel only uses these to
	// seed the family its own connection-observed address didn't cover —
	// see hub.go's fillNodeRelayHosts. Empty from agents that predate this probe.
	ProbedV4 string `json:"probed_v4,omitempty"`
	ProbedV6 string `json:"probed_v6,omitempty"`
	// DeclaredRelayHost/DeclaredRelayHostV6 are explicit operator-provided
	// addresses (daemon --relay-host/--relay-host-v6), distinct from
	// ProbedV4/ProbedV6 which are the agent's own outbound-route guesses.
	// When present they are authoritative and override whatever is in the
	// DB, unlike ProbedV4/ProbedV6 which only ever seed an empty field —
	// see hub.go's applyDeclaredRelayHosts.
	DeclaredRelayHost   string `json:"declared_relay_host,omitempty"`
	DeclaredRelayHostV6 string `json:"declared_relay_host_v6,omitempty"`
}
```

- [ ] **Step 2: 提取 isValidRelayHostV6**

`internal/server/api.go:2515-2526` 现状：

```go
// isValidRelayHost checks that a string is a valid IPv4 literal or hostname.
// relay_host (the data-plane v4 address) and relay_host_v6 are family-typed
// fields; an IPv6 literal here would look identical to a v4-only node's
// address to anything reading relay_host without also checking family,
// silently reintroducing the mixed-family bug this field split was meant to
// fix. IPv6 belongs exclusively in relay_host_v6.
func isValidRelayHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	return resolver.IsHostname(host)
}
```

改成（追加一个函数）：

```go
// isValidRelayHost checks that a string is a valid IPv4 literal or hostname.
// relay_host (the data-plane v4 address) and relay_host_v6 are family-typed
// fields; an IPv6 literal here would look identical to a v4-only node's
// address to anything reading relay_host without also checking family,
// silently reintroducing the mixed-family bug this field split was meant to
// fix. IPv6 belongs exclusively in relay_host_v6.
func isValidRelayHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	return resolver.IsHostname(host)
}

// isValidRelayHostV6 checks that a string is a valid IPv6 literal (not a
// v4-mapped one, which belongs in relay_host instead).
func isValidRelayHostV6(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}
```

然后把 `apiSetNodeRelayHostV6` 里的内联校验（`internal/server/api.go:666-673`）：

```go
	host := strings.TrimSpace(body.RelayHostV6)
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() != nil {
			jsonErr(w, http.StatusBadRequest, "IPv6 中继地址须为有效的 IPv6 地址")
			return
		}
	}
```

改成：

```go
	host := strings.TrimSpace(body.RelayHostV6)
	if host != "" && !isValidRelayHostV6(host) {
		jsonErr(w, http.StatusBadRequest, "IPv6 中继地址须为有效的 IPv6 地址")
		return
	}
```

- [ ] **Step 3: 写失败的 hub 测试**

在 `internal/server/hub_test.go` 末尾追加：

```go
func TestHubAppliesDeclaredRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want 203.0.113.50 (declared value)", got.RelayHost)
	}
	if !got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be true after a hello carrying DeclaredRelayHost")
	}
}

func TestHubDeclaredRelayHostOverridesExistingValue(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want 203.0.113.50 (declared value must override a pre-existing one)", got.RelayHost)
	}
}

func TestHubIgnoresInvalidDeclaredRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "2001:db8::1", // v6 literal is invalid for the v4 field
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "" {
		t.Errorf("RelayHost = %q, want empty (invalid declared value must be ignored)", got.RelayHost)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should stay false when the declared value was rejected")
	}
}

func TestHubClearingDeclaredRelayHostUnlocksValue(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	// Reconnect without a declared value, as if the operator removed the
	// --relay-host flag and restarted the daemon.
	c2 := dialWS(t, srv)
	hp2, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c2, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp2})
	_ = recvEnvelope(t, c2)
	syncByPing(t, c2)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be false after a hello with no declared value")
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.50 (unlocking must not blank the field)", got.RelayHost)
	}
}

func TestHubAppliesDeclaredRelayHostV6(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHostV6: "2001:db8::50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::50" {
		t.Errorf("RelayHostV6 = %q, want 2001:db8::50 (declared value)", got.RelayHostV6)
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should be true after a hello carrying DeclaredRelayHostV6")
	}
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `go test ./internal/server/... -run TestHubAppliesDeclaredRelayHost -v`
Expected: FAIL — `wsproto.Hello` 还没有 `DeclaredRelayHost` 字段（编译错误）。

- [ ] **Step 5: 实现 applyDeclaredRelayHosts**

`internal/server/hub.go:456` 后（`fillNodeRelayHosts` 结束之后）追加：

```go

// applyDeclaredRelayHosts handles operator-declared relay_host/relay_host_v6
// values sent via Hello.DeclaredRelayHost/DeclaredRelayHostV6 (see
// cmd/nft-agent's --relay-host/--relay-host-v6 flags). Unlike
// fillNodeRelayHosts, which only ever seeds an empty field once, a declared
// value is authoritative: it overwrites whatever is in the DB on every
// hello where it's present, so config drift self-heals. When the daemon
// stops declaring a value (flag removed, daemon restarted), the DB field
// unlocks but keeps its last value rather than going blank, so a live route
// doesn't disappear out from under the running link.
func applyDeclaredRelayHosts(d *sql.DB, node *db.Node, declaredV4, declaredV6 string) {
	if declaredV4 != "" {
		if isValidRelayHost(declaredV4) {
			if node.RelayHost != declaredV4 || !node.RelayHostDeclared {
				_ = db.UpdateNodeRelayHost(d, node.ID, declaredV4)
				_ = db.SetNodeRelayHostDeclared(d, node.ID, true)
				node.RelayHost, node.RelayHostDeclared = declaredV4, true
			}
		} else {
			log.Printf("hub: node %d declared invalid relay_host %q, ignoring", node.ID, declaredV4)
		}
	} else if node.RelayHostDeclared {
		_ = db.SetNodeRelayHostDeclared(d, node.ID, false)
		node.RelayHostDeclared = false
	}

	if declaredV6 != "" {
		if isValidRelayHostV6(declaredV6) {
			if node.RelayHostV6 != declaredV6 || !node.RelayHostV6Declared {
				_ = db.UpdateNodeRelayHostV6(d, node.ID, declaredV6)
				_ = db.SetNodeRelayHostV6Declared(d, node.ID, true)
				node.RelayHostV6, node.RelayHostV6Declared = declaredV6, true
			}
		} else {
			log.Printf("hub: node %d declared invalid relay_host_v6 %q, ignoring", node.ID, declaredV6)
		}
	} else if node.RelayHostV6Declared {
		_ = db.SetNodeRelayHostV6Declared(d, node.ID, false)
		node.RelayHostV6Declared = false
	}
}
```

- [ ] **Step 6: 接入 ServeWS**

`internal/server/hub.go:149-153` 现状：

```go
	connectIP := extractIP(r)
	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, connectIP); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	fillNodeRelayHosts(h.DB, node, connectIP, hello.ProbedV4, hello.ProbedV6)
```

改成：

```go
	connectIP := extractIP(r)
	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, connectIP); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	applyDeclaredRelayHosts(h.DB, node, hello.DeclaredRelayHost, hello.DeclaredRelayHostV6)
	fillNodeRelayHosts(h.DB, node, connectIP, hello.ProbedV4, hello.ProbedV6)
```

- [ ] **Step 7: 跑测试确认通过**

Run: `go test ./internal/server/... -run 'TestHubAppliesDeclaredRelayHost|TestHubDeclaredRelayHostOverridesExistingValue|TestHubIgnoresInvalidDeclaredRelayHost|TestHubClearingDeclaredRelayHostUnlocksValue|TestHubAppliesDeclaredRelayHostV6' -v`
Expected: PASS（5 项全过）

Run: `go test ./internal/server/... ./internal/wsproto/...`
Expected: PASS（确认没有破坏既有的 `fillNodeRelayHosts`/hub 测试）

- [ ] **Step 8: gofmt + 提交**

Run: `gofmt -w internal/wsproto/messages.go internal/server/hub.go internal/server/api.go internal/server/hub_test.go`
Expected: 无输出（`messages.go`/`api.go` 在本次改动前就不是 gofmt 干净状态，格式化后可能看到少量本次未触及的历史对齐变化，属预期行为）

```bash
git add internal/wsproto/messages.go internal/server/hub.go internal/server/api.go internal/server/hub_test.go
git commit -m "feat(server): apply operator-declared relay_host from Hello"
```

---

## Task 3: server API——锁定 declared 字段不给手动改

**Files:**
- Modify: `internal/server/api.go:613-680`（`apiSetNodeRelayHost`、`apiSetNodeRelayHostV6`）
- Test: `internal/server/handlers_admin_test.go`

**Interfaces:**
- Consumes: `db.Node.RelayHostDeclared/RelayHostV6Declared`（Task 1）；`db.SetNodeRelayHostDeclared`（Task 1，测试里用来模拟"已声明"状态）

- [ ] **Step 1: 写失败的测试**

在 `internal/server/handlers_admin_test.go` 末尾追加：

```go
func TestSetNodeRelayHostRejectsWhenDeclared(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	if err := db.UpdateNodeRelayHost(d, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNodeRelayHostDeclared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	if code := setNodeRelayHost(t, s, admin, n.ID, "198.51.100.1"); code != http.StatusConflict {
		t.Fatalf("relay-host on a declared field: status = %d, want 409", code)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.9" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.9 (declared field must reject manual edits)", got.RelayHost)
	}
}

func TestSetNodeRelayHostV6RejectsWhenDeclared(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "n1", "https://p", "t1")
	if err := db.UpdateNodeRelayHostV6(d, n.ID, "2001:db8::9"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNodeRelayHostV6Declared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]string{"relay_host_v6": "2001:db8::99"})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/nodes/%d/relay-host-v6", n.ID), bytes.NewReader(body))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("relay-host-v6 on a declared field: status = %d, want 409", rec.Code)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::9" {
		t.Errorf("RelayHostV6 = %q, want unchanged 2001:db8::9 (declared field must reject manual edits)", got.RelayHostV6)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/... -run 'TestSetNodeRelayHostRejectsWhenDeclared|TestSetNodeRelayHostV6RejectsWhenDeclared' -v`
Expected: FAIL — 当前两个 handler 都会返回 200。

- [ ] **Step 3: apiSetNodeRelayHost 加拦截**

`internal/server/api.go:613-623` 现状：

```go
func (s *Server) apiSetNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
```

改成：

```go
func (s *Server) apiSetNodeRelayHost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if node.RelayHostDeclared {
		jsonErr(w, http.StatusConflict, "该字段由节点 daemon 的 --relay-host 参数管理，如需修改请更新节点配置后重启 daemon")
		return
	}
```

- [ ] **Step 4: apiSetNodeRelayHostV6 加拦截**

`internal/server/api.go:648-658`（Step 3 之后行号会下移，按函数名定位）现状：

```go
func (s *Server) apiSetNodeRelayHostV6(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := db.GetNode(s.DB, id); err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
```

改成：

```go
func (s *Server) apiSetNodeRelayHostV6(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	node, err := db.GetNode(s.DB, id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	if node.RelayHostV6Declared {
		jsonErr(w, http.StatusConflict, "该字段由节点 daemon 的 --relay-host-v6 参数管理，如需修改请更新节点配置后重启 daemon")
		return
	}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/... -run 'TestSetNodeRelayHostRejectsWhenDeclared|TestSetNodeRelayHostV6RejectsWhenDeclared|TestSetNodeRelayHostRejectsIPv6Literal|TestSetNodeRelayHostAcceptsIPv4AndHostname' -v`
Expected: PASS（新旧测试都过，确认没改坏未声明节点的正常手动编辑路径）

- [ ] **Step 6: gofmt + 提交**

Run: `gofmt -w internal/server/api.go internal/server/handlers_admin_test.go`
Expected: 无输出（`api.go` 在本次改动前就不是 gofmt 干净状态，格式化后可能看到少量本次未触及的历史对齐变化，属预期行为）

```bash
git add internal/server/api.go internal/server/handlers_admin_test.go
git commit -m "feat(server): reject manual relay_host edits on daemon-declared fields"
```

---

## Task 4: daemon 端——把声明值透传进 Hello

**Files:**
- Modify: `internal/daemon/daemon.go:31-97`（`Config`、`New`）、`internal/daemon/daemon.go:216-232`（`DialerConfig` 构造）
- Modify: `internal/daemon/handlers.go:23-46`（`Daemon` struct）
- Modify: `internal/daemon/dialer.go:41-61`（`DialerConfig`）、`internal/daemon/dialer.go:280-292`（`runOnce` 组装 Hello）
- Test: `internal/daemon/dialer_test.go`

**Interfaces:**
- Consumes: `wsproto.Hello.DeclaredRelayHost/DeclaredRelayHostV6`（Task 2）
- Produces: `daemon.Config.DeclaredRelayHost/DeclaredRelayHostV6 string`；`DialerConfig.DeclaredRelayHost/DeclaredRelayHostV6 string`

- [ ] **Step 1: 写失败的测试**

在 `internal/daemon/dialer_test.go` 末尾追加：

```go
func TestDialerHelloIncludesDeclaredRelayHost(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:                 "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:               "tok",
		AgentVersion:        "v1",
		DeclaredRelayHost:   "203.0.113.50",
		DeclaredRelayHostV6: "2001:db8::50",
		GetState:            func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:             func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(frames[0].Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.DeclaredRelayHost != "203.0.113.50" {
		t.Errorf("DeclaredRelayHost = %q, want 203.0.113.50", hello.DeclaredRelayHost)
	}
	if hello.DeclaredRelayHostV6 != "2001:db8::50" {
		t.Errorf("DeclaredRelayHostV6 = %q, want 2001:db8::50", hello.DeclaredRelayHostV6)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/... -run TestDialerHelloIncludesDeclaredRelayHost -v`
Expected: FAIL — `DialerConfig` 还没有 `DeclaredRelayHost` 字段（编译错误）。

- [ ] **Step 3: DialerConfig 加字段并透传进 Hello**

`internal/daemon/dialer.go:41-47` 现状：

```go
type DialerConfig struct {
	URL          string
	Token        string
	AgentVersion string
	AgentSHA     string
	PortRange    string

```

改成：

```go
type DialerConfig struct {
	URL          string
	Token        string
	AgentVersion string
	AgentSHA     string
	PortRange    string

	// DeclaredRelayHost/DeclaredRelayHostV6 come from the daemon's
	// --relay-host/--relay-host-v6 flags. Non-empty values are sent with
	// every hello so the panel treats them as authoritative — see
	// hub.go's applyDeclaredRelayHosts.
	DeclaredRelayHost   string
	DeclaredRelayHostV6 string

```

`internal/daemon/dialer.go:282-292` 现状：

```go
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:      d.cfg.Token,
		AgentVersion:   d.cfg.AgentVersion,
		AgentSHA:       d.cfg.AgentSHA,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		LastAppliedRev: currentMeta.LastAppliedRev,
		PortRange:      d.cfg.PortRange,
		ProbedV4:       probedV4,
		ProbedV6:       probedV6,
	})
```

改成：

```go
	helloPayload, err := json.Marshal(wsproto.Hello{
		NodeToken:           d.cfg.Token,
		AgentVersion:        d.cfg.AgentVersion,
		AgentSHA:            d.cfg.AgentSHA,
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		LastAppliedRev:      currentMeta.LastAppliedRev,
		PortRange:           d.cfg.PortRange,
		ProbedV4:            probedV4,
		ProbedV6:            probedV6,
		DeclaredRelayHost:   d.cfg.DeclaredRelayHost,
		DeclaredRelayHostV6: d.cfg.DeclaredRelayHostV6,
	})
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/daemon/... -run TestDialerHelloIncludesDeclaredRelayHost -v`
Expected: PASS

- [ ] **Step 5: 从 daemon.Config 一路透传到 DialerConfig**

`internal/daemon/handlers.go:39-46` 现状：

```go
	connectURL string
	connectTok string
	portRange  string
	dialer     atomic.Pointer[Dialer]
```

改成：

```go
	connectURL          string
	connectTok          string
	portRange           string
	declaredRelayHost   string
	declaredRelayHostV6 string
	dialer              atomic.Pointer[Dialer]
```

`internal/daemon/daemon.go:31-54`（`Config` struct）现状：

```go
	ConnectURL   string
	ConnectToken string
	PortRange    string
}
```

改成：

```go
	ConnectURL   string
	ConnectToken string
	PortRange    string

	// DeclaredRelayHost/DeclaredRelayHostV6, when set, are sent with every
	// hello as the authoritative data-plane address for this node — see
	// cmd/nft-agent's --relay-host/--relay-host-v6 flags.
	DeclaredRelayHost   string
	DeclaredRelayHostV6 string
}
```

`internal/daemon/daemon.go:86-97`（`New` 里构造 `Daemon`）现状：

```go
	return &Daemon{
		socketPath:  cfg.SocketPath,
		statePath:   cfg.StatePath,
		groupName:   cfg.GroupName,
		dp:          cfg.Dataplane,
		legacyPaths: cfg.LegacyPaths,
		countersFn:  cfg.Dataplane.Counters,
		resolveFn:   defaultResolver(resolver.New()),
		connectURL:  cfg.ConnectURL,
		connectTok:  cfg.ConnectToken,
		portRange:   cfg.PortRange,
	}, nil
```

改成：

```go
	return &Daemon{
		socketPath:          cfg.SocketPath,
		statePath:           cfg.StatePath,
		groupName:           cfg.GroupName,
		dp:                  cfg.Dataplane,
		legacyPaths:         cfg.LegacyPaths,
		countersFn:          cfg.Dataplane.Counters,
		resolveFn:           defaultResolver(resolver.New()),
		connectURL:          cfg.ConnectURL,
		connectTok:          cfg.ConnectToken,
		portRange:           cfg.PortRange,
		declaredRelayHost:   cfg.DeclaredRelayHost,
		declaredRelayHostV6: cfg.DeclaredRelayHostV6,
	}, nil
```

`internal/daemon/daemon.go:216-232`（`DialerConfig` 构造）现状：

```go
		dl := NewDialer(DialerConfig{
			URL:          d.connectURL,
			Token:        d.connectTok,
			AgentVersion: agentVersion(),
			AgentSHA:     agentSHA(),
			PortRange:    d.portRange,
			GetState:     d.SnapshotForDialer,
			OnApply:      d.SetPanelRuleset,
			OnMigrated:   d.clearTuiSegment,
			CountersFn:   d.counterSamples,
```

改成：

```go
		dl := NewDialer(DialerConfig{
			URL:                 d.connectURL,
			Token:               d.connectTok,
			AgentVersion:        agentVersion(),
			AgentSHA:            agentSHA(),
			PortRange:           d.portRange,
			DeclaredRelayHost:   d.declaredRelayHost,
			DeclaredRelayHostV6: d.declaredRelayHostV6,
			GetState:            d.SnapshotForDialer,
			OnApply:             d.SetPanelRuleset,
			OnMigrated:          d.clearTuiSegment,
			CountersFn:          d.counterSamples,
```

（`OnConfigUpdate` 字段紧随其后，保持不变。）

- [ ] **Step 6: 跑全量 daemon 测试确认没改坏别的**

Run: `go test ./internal/daemon/...`
Expected: PASS

- [ ] **Step 7: gofmt + 提交**

Run: `gofmt -w internal/daemon/daemon.go internal/daemon/handlers.go internal/daemon/dialer.go internal/daemon/dialer_test.go`
Expected: 无输出（`daemon.go` 在本次改动前就不是 gofmt 干净状态，格式化后可能看到少量本次未触及的历史对齐变化，属预期行为）

```bash
git add internal/daemon/daemon.go internal/daemon/handlers.go internal/daemon/dialer.go internal/daemon/dialer_test.go
git commit -m "feat(daemon): thread declared relay_host through to Hello"
```

---

## Task 5: cmd/nft-agent——CLI flag

**Files:**
- Modify: `cmd/nft-agent/main.go:32-86`

**Interfaces:**
- Consumes: `daemon.Config.DeclaredRelayHost/DeclaredRelayHostV6`（Task 4）

- [ ] **Step 1: 加 flag 变量与定义**

`cmd/nft-agent/main.go:32-48` 现状：

```go
	var (
		socketPath     string
		statePath      string
		groupName      string
		iface          string
		connectURL     string
		panelTokenFile string
		portRange      string
	)
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
	fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/standalone mode")
	fs.StringVar(&panelTokenFile, "panel-token-file", "/etc/nft-forward/panel.token", "bearer token file (required when --connect is set)")
	fs.StringVar(&portRange, "port-range", "", "端口范围（如 10001-20000），上报给面板")
```

改成：

```go
	var (
		socketPath     string
		statePath      string
		groupName      string
		iface          string
		connectURL     string
		panelTokenFile string
		portRange      string
		relayHost      string
		relayHostV6    string
	)
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
	fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/standalone mode")
	fs.StringVar(&panelTokenFile, "panel-token-file", "/etc/nft-forward/panel.token", "bearer token file (required when --connect is set)")
	fs.StringVar(&portRange, "port-range", "", "端口范围（如 10001-20000），上报给面板")
	fs.StringVar(&relayHost, "relay-host", "", "显式声明数据面 IPv4 地址/域名，覆盖面板的自动识别（用于双出口等场景）")
	fs.StringVar(&relayHostV6, "relay-host-v6", "", "显式声明数据面 IPv6 地址，覆盖面板的自动识别")
```

- [ ] **Step 2: 透传进 daemon.Config**

`cmd/nft-agent/main.go:79-86` 现状：

```go
	cfg := daemon.Config{
		SocketPath: socketPath,
		StatePath:  statePath,
		GroupName:  groupName,
		Iface:      iface,
		ConnectURL: connectURL,
		PortRange:  portRange,
	}
```

改成：

```go
	cfg := daemon.Config{
		SocketPath:          socketPath,
		StatePath:           statePath,
		GroupName:           groupName,
		Iface:               iface,
		ConnectURL:          connectURL,
		PortRange:           portRange,
		DeclaredRelayHost:   relayHost,
		DeclaredRelayHostV6: relayHostV6,
	}
```

- [ ] **Step 3: 构建确认**

Run: `go build ./...`
Expected: 无报错

Run: `gofmt -l cmd/nft-agent/main.go`
Expected: 无输出（若因手工对齐产生 gofmt 差异，跑 `gofmt -w cmd/nft-agent/main.go` 修一下）

Run: `go run ./cmd/nft-agent daemon --help 2>&1 | grep relay-host`
Expected: 输出包含新增的两行 `-relay-host`/`-relay-host-v6` 说明文字

- [ ] **Step 4: 提交**

```bash
git add cmd/nft-agent/main.go
git commit -m "feat(agent): add --relay-host/--relay-host-v6 flags"
```

---

## Task 6: install.sh——装机参数

**Files:**
- Modify: `install.sh:260-269`（帮助文本）、`install.sh:546-585`（参数解析）、`install.sh:799-819`（agent 模式拼 systemd unit）

**Interfaces:**
- Consumes: `cmd/nft-agent daemon --relay-host/--relay-host-v6`（Task 5）

- [ ] **Step 1: 帮助文本加两行**

`install.sh:263` 后追加：

```
  --port-range R                   agent 占用的中继端口范围，格式 START-END（默认 10001-20000）
  --relay-host HOST                agent 数据面 IPv4 地址/域名声明，覆盖面板自动识别（双出口中转机场景）
  --relay-host-v6 HOST             agent 数据面 IPv6 地址声明，覆盖面板自动识别
```

（即在现有 `--port-range` 那一行后面插入两行新说明，其余帮助文本不变。）

- [ ] **Step 2: 参数解析加变量与 case 分支**

`install.sh:548-550` 现状：

```sh
mode=""
panel_url=""
token=""
port_range=""
addr=""
```

改成：

```sh
mode=""
panel_url=""
token=""
port_range=""
relay_host=""
relay_host_v6=""
addr=""
```

`install.sh:572-573` 现状：

```sh
    --port-range) require_val --port-range "${2:-}"; port_range="$2"; shift 2 ;;
    --port-range=*) port_range="${1#*=}"; shift ;;
```

改成（紧随其后插入两组新 case）：

```sh
    --port-range) require_val --port-range "${2:-}"; port_range="$2"; shift 2 ;;
    --port-range=*) port_range="${1#*=}"; shift ;;
    --relay-host) require_val --relay-host "${2:-}"; relay_host="$2"; shift 2 ;;
    --relay-host=*) relay_host="${1#*=}"; shift ;;
    --relay-host-v6) require_val --relay-host-v6 "${2:-}"; relay_host_v6="$2"; shift 2 ;;
    --relay-host-v6=*) relay_host_v6="${1#*=}"; shift ;;
```

- [ ] **Step 3: agent 模式拼进 systemd unit**

`install.sh:817-819` 现状：

```sh
    range_arg=""
    [[ -n "$port_range" ]] && range_arg=" --port-range $port_range"
    write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token${range_arg}"
```

改成：

```sh
    range_arg=""
    [[ -n "$port_range" ]] && range_arg=" --port-range $port_range"
    relay_arg=""
    [[ -n "$relay_host" ]] && relay_arg+=" --relay-host $relay_host"
    [[ -n "$relay_host_v6" ]] && relay_arg+=" --relay-host-v6 $relay_host_v6"
    write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token${range_arg}${relay_arg}"
```

- [ ] **Step 4: 语法检查**

Run: `bash -n install.sh`
Expected: 无输出（语法通过）

Run: `grep -n "relay-host" install.sh`
Expected: 能看到帮助文本、参数解析、`write_daemon_unit` 拼接这三处新增内容

- [ ] **Step 5: 提交**

```bash
git add install.sh
git commit -m "feat(install): add --relay-host/--relay-host-v6 agent install flags"
```

---

## Task 7: 前端——declared 字段禁用编辑

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx:26-73`（state + save 逻辑不变，读取新字段）、`web/src/pages/nodes/Detail.jsx:260-272`（`ConfigField` 渲染）

**Interfaces:**
- Consumes: 节点详情接口返回的 `node.relay_host_declared`/`node.relay_host_v6_declared`（Task 1 的 `Node.RelayHostDeclared`/`RelayHostV6Declared` 会被现有的 `json:"relay_host_declared"` tag 自动带出，节点详情 GET 接口本就是直接序列化 `db.Node`，不需要额外后端改动）

- [ ] **Step 1: ConfigField 渲染加锁定态**

`web/src/pages/nodes/Detail.jsx:260-272` 现状：

```jsx
            <ConfigField label="中继地址（数据面）" hint="中继链路用它作为上一跳打向本节点的目标地址">
              <form onSubmit={saveRelay} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHost} onChange={e => setRelayHost(e.target.value)} placeholder="数据面公网 IP 或域名" />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>

            <ConfigField label="IPv6 中继地址" hint="设置后该节点可转发 IPv6 目标，留空表示不支持 IPv6">
              <form onSubmit={saveRelayV6} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHostV6} onChange={e => setRelayHostV6(e.target.value)} placeholder="数据面公网 IPv6 地址" />
                <button type="submit" className="btn-primary flex-none px-5">保存</button>
              </form>
            </ConfigField>
```

改成：

```jsx
            <ConfigField
              label="中继地址（数据面）"
              hint={node.relay_host_declared ? '由 daemon 启动参数 --relay-host 管理，UI 不可修改；如需变更请更新节点配置后重启 daemon' : '中继链路用它作为上一跳打向本节点的目标地址'}
            >
              <form onSubmit={saveRelay} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHost} onChange={e => setRelayHost(e.target.value)} placeholder="数据面公网 IP 或域名" disabled={node.relay_host_declared} />
                <button type="submit" className="btn-primary flex-none px-5" disabled={node.relay_host_declared}>保存</button>
              </form>
            </ConfigField>

            <ConfigField
              label="IPv6 中继地址"
              hint={node.relay_host_v6_declared ? '由 daemon 启动参数 --relay-host-v6 管理，UI 不可修改；如需变更请更新节点配置后重启 daemon' : '设置后该节点可转发 IPv6 目标，留空表示不支持 IPv6'}
            >
              <form onSubmit={saveRelayV6} className="flex gap-2">
                <input className="input-field font-mono flex-1" value={relayHostV6} onChange={e => setRelayHostV6(e.target.value)} placeholder="数据面公网 IPv6 地址" disabled={node.relay_host_v6_declared} />
                <button type="submit" className="btn-primary flex-none px-5" disabled={node.relay_host_v6_declared}>保存</button>
              </form>
            </ConfigField>
```

- [ ] **Step 2: 构建确认**

Run: `cd web && npm run build`
Expected: 构建成功，无 JSX/lint 报错

- [ ] **Step 3: 浏览器手动验证**

启动本地开发服务器（`npm run dev`），打开一个节点的详情页：
1. 未声明的节点：确认两个中继地址输入框可正常编辑、保存
2. 手动把某节点的 `relay_host_declared` 置 1（可以先跑通 Task 1-4，用真实 daemon 带 `--relay-host` 连一次），刷新详情页，确认输入框变灰不可编辑，hint 文案变成"由 daemon 启动参数管理"

- [ ] **Step 4: 提交**

```bash
git add web/src/pages/nodes/Detail.jsx
git commit -m "feat(web): disable relay-host fields locked by daemon declaration"
```

---

## 端到端验证（全部 Task 完成后）

- [ ] Run: `go build ./... && go test ./...`
  Expected: 全部 PASS
- [ ] hep-ix 上用 `nft-agent daemon --connect <panel> --panel-token-file ... --relay-host <国内入口IP>` 重启 daemon（或 `install.sh agent --relay-host <国内入口IP> ...` 重装），确认：
  - server 端节点详情页 `relay_host` 立即变为声明的入口 IP
  - 页面上"中继地址（数据面）"输入框变为禁用态
  - 引用该节点的规则链路的 entry 地址随之更新为入口 IP（`internal/server/shared.go` 的 `buildRuleView`）
