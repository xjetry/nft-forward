# udp / tcp+udp 中继链支持 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 composite 中继链支持 `udp` 与 `tcp+udp` 协议,`tcp+udp` 在用户态模式下自动 TCP 走用户态 relay、UDP 走 nftables 内核 DNAT。

**Architecture:** 数据平面(`forward.Partition`)已支持 tcp+udp 拆分与逐跳级联 DNAT。本计划只补齐上层四处:服务端 API 放行 tcp+udp、端口占用跨协议互斥、计费按协议 fan-in、Web 表单放开 composite 协议选择。

**Tech Stack:** Go(`internal/server`、`internal/db`)、SQLite、React(`web/src`)。

---

## File Structure

- `internal/server/shared.go` — 新增 `validRuleProto` 协议校验 helper(供 api.go 三处复用)。
- `internal/server/api.go` — 三处规则创建/更新校验改用 helper。
- `internal/db/rules.go` — 新增 `overlappingProtos`,改 `OccupiedPortsOnNode` 跨协议占用。
- `internal/db/queries.go` — 新增 `hopCounterKeys`,改 `RuleHopMapByNode` 注册别名键。
- `internal/server/proto_compat_test.go` — 新建,覆盖端口占用与计数别名两个 db 函数。
- `web/src/pages/my/Rules.jsx` — 移除 composite 强制 tcp。
- `web/src/pages/rules/List.jsx` — 移除 composite 强制 tcp。

---

## Task 1: 服务端 API 放行 tcp+udp

**Files:**
- Modify: `internal/server/shared.go`(在 `parseExit` 附近新增 helper)
- Modify: `internal/server/api.go:689`、`:838`、`:1311`
- Test: `internal/server/proto_compat_test.go`(新建)

- [ ] **Step 1: 写失败测试**

新建 `internal/server/proto_compat_test.go`:

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

func TestCreateMyRuleAcceptsTCPUDP(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "edge", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5)

	s, _ := New(d)
	for _, tc := range []struct {
		proto string
		want  int
	}{
		{"tcp+udp", http.StatusOK},
		{"udp", http.StatusOK},
		{"sctp", http.StatusBadRequest},
	} {
		body, _ := json.Marshal(map[string]any{
			"node_id": g.ID, "name": "r-" + tc.proto, "proto": tc.proto, "exit": "9.9.9.9:8443",
		})
		req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("proto %s: status=%d want=%d body=%s", tc.proto, rec.Code, tc.want, rec.Body.String())
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestCreateMyRuleAcceptsTCPUDP -v`
Expected: FAIL —— `tcp+udp` 返回 400(当前校验只接受 tcp/udp)。

- [ ] **Step 3: 新增校验 helper**

在 `internal/server/shared.go` 的 `parseExit` 函数之前插入:

```go
// validRuleProto reports whether proto is an accepted forward protocol. tcp+udp
// is accepted: the data plane splits it into a udp kernel DNAT plus a tcp
// userspace relay when the hop runs in userspace mode (see forward.Partition).
func validRuleProto(proto string) bool {
	switch proto {
	case "tcp", "udp", "tcp+udp":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: 三处校验改用 helper**

`internal/server/api.go` 第 689、838、1311 行三处完全相同的块:

```go
	if name == "" || (proto != "tcp" && proto != "udp") {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp 或 udp")
		return
	}
```

每处替换为:

```go
	if name == "" || !validRuleProto(proto) {
		jsonErr(w, http.StatusBadRequest, "名称必填，协议须为 tcp、udp 或 tcp+udp")
		return
	}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestCreateMyRuleAcceptsTCPUDP -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/server/shared.go internal/server/api.go internal/server/proto_compat_test.go
git commit -m "feat: accept tcp+udp proto on relay-chain rule endpoints"
```

---

## Task 2: 端口占用跨协议互斥

**Files:**
- Modify: `internal/db/rules.go`(新增 `overlappingProtos`,改 `OccupiedPortsOnNode`)
- Test: `internal/server/proto_compat_test.go`(追加)

- [ ] **Step 1: 追加失败测试**

在 `internal/server/proto_compat_test.go` 追加:

```go
func TestOccupiedPortsCrossProto(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "edge", "https://p", "tok")
	rid, err := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(
		`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,?,?,?,?,?,?)`,
		rid, n.ID, "tcp+udp", 10001, "9.9.9.9", 443, "userspace", ""); err != nil {
		t.Fatal(err)
	}

	// A plain tcp rule must see the tcp+udp hop's port as occupied.
	occ, err := db.OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !occ[10001] {
		t.Fatalf("tcp query should see tcp+udp port 10001 as occupied, got %v", occ)
	}
	// A udp rule must see it too.
	occ, _ = db.OccupiedPortsOnNode(d, n.ID, "udp", 0)
	if !occ[10001] {
		t.Fatalf("udp query should see tcp+udp port 10001 as occupied, got %v", occ)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestOccupiedPortsCrossProto -v`
Expected: FAIL —— 当前 `OccupiedPortsOnNode` 按精确 proto 串匹配,`tcp` 查询看不到 `tcp+udp` 的端口。

- [ ] **Step 3: 新增 overlappingProtos**

在 `internal/db/rules.go` 的 `OccupiedPortsOnNode` 函数之前插入:

```go
// overlappingProtos returns every stored rule_hops.proto that competes for the
// same listen port as proto. tcp+udp occupies both the tcp and udp namespaces
// (forward.Partition splits it across both), so tcp+udp conflicts with tcp,
// udp, and tcp+udp; a plain tcp hop conflicts with tcp and tcp+udp; likewise
// for udp. Mirrors the overlap rule in forward.Partition so the server never
// hands out a port the daemon would later reject.
func overlappingProtos(proto string) []string {
	switch proto {
	case "tcp+udp":
		return []string{"tcp", "udp", "tcp+udp"}
	case "tcp":
		return []string{"tcp", "tcp+udp"}
	case "udp":
		return []string{"udp", "tcp+udp"}
	default:
		return []string{proto}
	}
}
```

- [ ] **Step 4: 改 OccupiedPortsOnNode 用 IN 匹配**

将现有函数体替换为:

```go
func OccupiedPortsOnNode(d DBTX, nodeID int64, proto string, excludeRuleID int64) (map[int]bool, error) {
	protos := overlappingProtos(proto)
	placeholders := make([]string, len(protos))
	args := make([]any, 0, len(protos)+2)
	args = append(args, nodeID)
	for i, p := range protos {
		placeholders[i] = "?"
		args = append(args, p)
	}
	args = append(args, excludeRuleID)
	q := `SELECT listen_port FROM rule_hops WHERE node_id=? AND proto IN (` +
		strings.Join(placeholders, ",") + `) AND (rule_id IS NULL OR rule_id<>?)`
	out := map[int]bool{}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
```

(`strings` 已在 `internal/db/rules.go` 导入,无需新增 import。)

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestOccupiedPortsCrossProto -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/db/rules.go internal/server/proto_compat_test.go
git commit -m "feat: treat tcp+udp as occupying both tcp and udp port namespaces"
```

---

## Task 3: 计费按协议 fan-in

**Files:**
- Modify: `internal/db/queries.go`(新增 `hopCounterKeys`,改 `RuleHopMapByNode`)
- Test: `internal/server/proto_compat_test.go`(追加)

- [ ] **Step 1: 追加失败测试**

在 `internal/server/proto_compat_test.go` 追加:

```go
func TestTCPUDPHopCounterFanIn(t *testing.T) {
	d := openDB(t)
	n, _ := db.CreateNode(d, "edge", "https://p", "tok")
	rid, _ := db.CreateRule(d, &db.Rule{NodeID: n.ID, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443})
	if _, err := d.Exec(
		`INSERT INTO rule_hops(rule_id,position,node_id,proto,listen_port,target_host,target_port,mode,comment) VALUES (?,0,?,?,?,?,?,?,?)`,
		rid, n.ID, "tcp+udp", 10001, "9.9.9.9", 443, "userspace", ""); err != nil {
		t.Fatal(err)
	}

	m, err := db.RuleHopMapByNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A tcp+udp hop reports as separate tcp and udp samples in userspace mode,
	// or as one tcp+udp sample in kernel mode; all must resolve to the same row.
	for _, key := range []string{"tcp/10001", "udp/10001", "tcp+udp/10001"} {
		if m[key] == nil {
			t.Fatalf("key %s should map to the tcp+udp hop, got nil", key)
		}
	}
	if m["tcp/10001"] != m["udp/10001"] {
		t.Fatalf("tcp and udp keys must point to the same hop row")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestTCPUDPHopCounterFanIn -v`
Expected: FAIL —— 当前只注册 `tcp+udp/10001`,`tcp/10001` 与 `udp/10001` 为 nil。

- [ ] **Step 3: 新增 hopCounterKeys 并改 RuleHopMapByNode**

在 `internal/db/queries.go` 中,把 `RuleHopMapByNode` 替换为:

```go
func RuleHopMapByNode(d *sql.DB, nodeID int64) (map[string]*RuleHop, error) {
	hops, err := queryAll(d, `SELECT `+ruleHopCols+` FROM rule_hops WHERE node_id=? ORDER BY listen_port`, scanRuleHop, nodeID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]*RuleHop, len(hops))
	for _, h := range hops {
		// A tcp+udp hop is reported by the daemon either as one tcp+udp sample
		// (kernel mode) or as separate tcp and udp samples (userspace mode, where
		// Partition splits it into a udp kernel DNAT + a tcp userspace relay).
		// Register every key that can carry this hop's bytes so applyCounters
		// sums them into the same row. Cross-proto port occupancy guarantees no
		// two hops on a node share an overlapping (proto, port), so keys are unique.
		for _, key := range hopCounterKeys(h.Proto, h.ListenPort) {
			m[key] = h
		}
	}
	return m, nil
}

// hopCounterKeys returns the proto/port counter keys a hop may receive samples
// under. A tcp+udp hop fans in to tcp, udp, and tcp+udp; anything else uses its
// own proto only.
func hopCounterKeys(proto string, port int) []string {
	protos := []string{proto}
	if proto == "tcp+udp" {
		protos = []string{"tcp+udp", "tcp", "udp"}
	}
	out := make([]string, len(protos))
	for i, p := range protos {
		out[i] = fmt.Sprintf("%s/%d", p, port)
	}
	return out
}
```

(`fmt` 已在 `internal/db/queries.go` 导入。)

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestTCPUDPHopCounterFanIn -v`
Expected: PASS

- [ ] **Step 5: 跑全包回归(确认计数/链路既有测试无回归)**

Run: `go test ./internal/server/ ./internal/db/ ./internal/forward/ ./internal/nft/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/db/queries.go internal/server/proto_compat_test.go
git commit -m "feat: fan tcp+udp hop counters into one rule_hop row across protos"
```

---

## Task 4: Web 表单放开 composite 协议选择

**Files:**
- Modify: `web/src/pages/my/Rules.jsx:73-74`、`:113-116`
- Modify: `web/src/pages/rules/List.jsx:118-119`、`:156-160`

- [ ] **Step 1: 改 my/Rules.jsx 的 set 逻辑**

`web/src/pages/my/Rules.jsx` 中删除 composite 强制 tcp 的分支。将:

```jsx
      if (k === 'node_id') {
        const n = (nodes || []).find(nd => String(nd.id) === v)
        if (n?.node_type === 'composite' && next.proto !== 'tcp') {
          next.proto = 'tcp'
        }
      }
```

改为(整段删除,只保留 set 的通用逻辑):

```jsx
      // composite nodes now support tcp / udp / tcp+udp like any other node;
      // tcp+udp is split TCP-userspace + UDP-kernel by the data plane.
```

- [ ] **Step 2: 改 my/Rules.jsx 的协议输入控件**

将:

```jsx
          {isComposite ? (
            <input className="input-field" value="TCP" disabled style={{ maxWidth: 200 }} />
          ) : (
            <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
              options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
          )}
```

改为:

```jsx
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
```

`isComposite` 若因此变为未使用变量,一并删除其声明(`const isComposite = selectedNode?.node_type === 'composite'`);若其他处仍用到则保留。

- [ ] **Step 3: 对 rules/List.jsx 做同样两处修改**

`web/src/pages/rules/List.jsx` 中删除:

```jsx
        if (n?.node_type === 'composite' && next.proto !== 'tcp') {
          next.proto = 'tcp'
        }
```

并把协议控件:

```jsx
          {isComposite ? (
            <input className="input-field" value="TCP" disabled style={{ maxWidth: 200 }} />
          ) : (
            <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
              options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
          )}
```

改为:

```jsx
          <Select value={form.proto} onChange={v => set('proto', v)} style={{ maxWidth: 200 }}
            options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'tcp+udp', label: 'TCP+UDP' }]} />
```

同样清理可能因此未使用的 `isComposite`。

- [ ] **Step 4: 构建前端确认无报错**

Run: `cd web && npm run build`
Expected: 构建成功,无未使用变量 lint 错误。

- [ ] **Step 5: 提交**

```bash
git add web/src/pages/my/Rules.jsx web/src/pages/rules/List.jsx
git commit -m "feat: allow udp/tcp+udp proto on composite-node rule forms"
```

---

## Task 5: 全量验证

**Files:** 无(仅验证)

- [ ] **Step 1: Go 全量测试**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 2: Go vet**

Run: `go vet ./...`
Expected: 无输出

- [ ] **Step 3: 前端构建**

Run: `cd web && npm run build`
Expected: 成功

- [ ] **Step 4: 端到端冒烟(可选,需 docker)**

Run: `cd docker && ./test.sh`
说明:若环境具备 docker,构建一条 tcp+udp 双跳链路,确认 TCP 与 UDP 同时连通、计数双协议累加。若无 docker 环境则跳过并在交付说明中注明未做端到端验证。

---

## Self-Review

- **Spec 覆盖**:API 放行(Task 1)、端口占用跨协议(Task 2)、计费 fan-in(Task 3)、Web 表单含纯 udp 放开(Task 4)——spec 四项改动点全部对应到任务。语义约定(tcp+udp+用户态拆分、计费按总字节、纯 udp 链路)由数据平面既有逻辑 + Task 3/4 共同满足。
- **占位符**:无 TBD/TODO,每个改动步骤均给出完整代码。
- **类型一致**:`validRuleProto`、`overlappingProtos`、`hopCounterKeys` 在定义任务内即被使用;`OccupiedPortsOnNode`/`RuleHopMapByNode` 签名保持不变,调用方无需改动。
- **非目标**:不实现 UDP 用户态转发、不改直连规则行为、不改 `nft/counters.go`(`th dport`→`tcp+udp` 已有)。
