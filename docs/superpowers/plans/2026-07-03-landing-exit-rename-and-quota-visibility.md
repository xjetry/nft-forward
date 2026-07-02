# 落地出口改名 + 用户侧配额可见性 + 重置按钮样式 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** admin 可给解析出的落地节点改名（订阅刷新后存活、用户复制到的 URI 同步改名）；用户侧落地节点页配额列不再因本地 URI 覆盖而显示"—"；重置按钮改为 pill 标签样式。

**Architecture:** 改名存为 `user_landing_exits.name_override`（同步只覆盖 `name` 列，override 天然存活）；服务端 `internal/landing` 新增与 `RewriteEndpoint` 对称的 `RewriteName`，在 `/api/my/landing-nodes` 出口处统一应用（显示名 + URI 重写），所有消费方（用户复制、规则选择器、stale 快照）一次覆盖。用户侧配额列改为按 host:port 查 serverNodes 账本，与合并胜负解耦。

**Tech Stack:** Go 1.x + SQLite（嵌入式迁移）、React 19 + Vite 6（web/，无 JS 单测基建，靠 Go 测试 + `npm run build` 兜底）。

**Spec:** `docs/superpowers/specs/2026-07-03-landing-exit-rename-and-quota-visibility-design.md`

## Global Constraints

- 代码注释、KDoc、commit message 中**绝对禁止**出现过程元信息（任务/步骤编号、方案代号、审阅轮次等）；注释只解释 WHY 与不变量。
- 版本号严格三段 vX.Y.Z（本计划不含发版）。
- 流量单位全链路字节；前端展示用现有 `fmtTrafficGB`，不引入 Mbps。
- 所有 Go 测试命令在仓库根目录 `/Users/xjetry/work/vibe/nft-forward` 运行。

---

### Task 1: `landing.RewriteName`

**Files:**
- Modify: `internal/landing/parse.go`（在 `RewriteEndpoint` 一族后新增）
- Test: `internal/landing/parse_test.go`（package `landing`，内部测试包）

**Interfaces:**
- Consumes: 已有 `b64Decode`、`errInvalid`、`ParseURIs`。
- Produces: `func RewriteName(uri, name string) (string, error)` — Task 4 的服务端调用方依赖此签名。

- [ ] **Step 1: 写失败测试**

在 `internal/landing/parse_test.go` 末尾追加（该文件已 import `encoding/base64`、`strings`、`testing`）：

```go
// RewriteName must round-trip through the parser: the rewritten URI parses
// back to the new name with the endpoint untouched, for every protocol shape
// the parser accepts.
func TestRewriteNameRoundTrip(t *testing.T) {
	uris := []string{
		"vless://u@1.2.3.4:443?type=tcp#Old",
		"trojan://u@5.6.7.8:8443#Old",
		"vless://u@1.2.3.4:443",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"9.9.9.9","port":"443","ps":"Old"}`)),
		"ss://YWVzLTEyOC1nY206cGFzcw==@1.2.3.4:8388#Old",
		"Old = snell, 1.2.3.4, 443, psk = xxx, version = 5",
	}
	const newName = "香港 01"
	for _, uri := range uris {
		got, err := RewriteName(uri, newName)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		nodes := ParseURIs([]string{got})
		if len(nodes) != 1 || nodes[0].Name != newName {
			t.Fatalf("%s -> %s: parsed %+v", uri, got, nodes)
		}
		orig := ParseURIs([]string{uri})[0]
		if nodes[0].Host != orig.Host || nodes[0].Port != orig.Port || nodes[0].Protocol != orig.Protocol {
			t.Fatalf("%s: endpoint changed, %+v vs %+v", uri, nodes[0], orig)
		}
	}
}

func TestRewriteNameMalformed(t *testing.T) {
	if _, err := RewriteName("not a proxy uri", "x"); err == nil {
		t.Fatal("malformed uri must error")
	}
	if _, err := RewriteName("vmess://!!!", "x"); err == nil {
		t.Fatal("undecodable vmess must error")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/landing/ -run TestRewriteName -v`
Expected: 编译错误 `undefined: RewriteName`

- [ ] **Step 3: 实现**

在 `internal/landing/parse.go` 的 `rewriteSS` 之后（`DecodeSubscription` 之前）加入：

```go
// RewriteName returns uri with its display name replaced, leaving the
// connection endpoint and credentials untouched — the mirror of
// RewriteEndpoint. The name lives in the URL fragment for authority-style
// URIs and ss, in the "ps" field for vmess, and in the "Name =" prefix for
// snell lines.
func RewriteName(uri, name string) (string, error) {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return renameSnell(uri, name)
	}
	switch strings.ToLower(uri[:idx]) {
	case "vmess":
		return renameVMess(uri, name)
	default:
		return renameFragment(uri, name)
	}
}

// renameFragment swaps (or appends) the URL fragment. Percent-encoding via
// EscapedFragment keeps spaces and non-ASCII round-trippable through both the
// Go parser and the browser's decodeURIComponent.
func renameFragment(uri, name string) (string, error) {
	if h := strings.Index(uri, "#"); h >= 0 {
		uri = uri[:h]
	}
	u := url.URL{Fragment: name}
	return uri + "#" + u.EscapedFragment(), nil
}

func renameVMess(uri, name string) (string, error) {
	dec, ok := b64Decode(uri[len("vmess://"):])
	if !ok {
		return "", errInvalid
	}
	var m map[string]any
	if err := json.Unmarshal(dec, &m); err != nil {
		return "", errInvalid
	}
	m["ps"] = name
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(b), nil
}

// renameSnell replaces the name before the first '=' — later '=' signs belong
// to key=value params (psk, version) and must stay put.
func renameSnell(line, name string) (string, error) {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return "", errInvalid
	}
	rest := strings.TrimSpace(line[eqIdx+1:])
	parts := strings.SplitN(rest, ",", -1)
	if len(parts) < 3 || strings.TrimSpace(strings.ToLower(parts[0])) != "snell" {
		return "", errInvalid
	}
	return name + " =" + line[eqIdx+1:], nil
}
```

`net/url` 已在该文件 import（`parseAuthority` 使用），无需新增 import。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/landing/ -v`
Expected: 全部 PASS（含既有测试）

- [ ] **Step 5: Commit**

```bash
git add internal/landing/parse.go internal/landing/parse_test.go
git commit -m "feat(landing): RewriteName rewrites a proxy URI's display name per protocol"
```

---

### Task 2: `name_override` 列 + `SetUserLandingExitName`

**Files:**
- Create: `internal/db/migrations/0030_landing_exit_name_override.sql`
- Modify: `internal/db/landing_exits.go`
- Test: `internal/db/landing_exits_test.go`（helpers：`openTestDB(t)`、`createTestUser(t, d)`、`inputs(hosts...)`）

**Interfaces:**
- Consumes: 已有 `exitRowPresent`、`now()`。
- Produces: `LandingExit.NameOverride string`（json `name_override`）；`func SetUserLandingExitName(d *sql.DB, userID int64, host string, port int, name string) (updated bool, err error)` — Task 3/4 依赖。

- [ ] **Step 1: 写失败测试**

在 `internal/db/landing_exits_test.go` 末尾追加：

```go
func TestLandingExitNameOverride(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	if _, _, err := SyncUserLandingExits(d, uid, inputs("a.com"), "", ""); err != nil {
		t.Fatal(err)
	}

	// unknown row: no update, no error
	updated, err := SetUserLandingExitName(d, uid, "nope.com", 443, "x")
	if err != nil || updated {
		t.Fatalf("unknown row: updated=%v err=%v", updated, err)
	}

	updated, err = SetUserLandingExitName(d, uid, "a.com", 443, "香港 01")
	if err != nil || !updated {
		t.Fatalf("set: updated=%v err=%v", updated, err)
	}

	// the override must survive a re-sync that overwrites the parsed name
	if _, _, err := SyncUserLandingExits(d, uid, inputs("a.com"), "", ""); err != nil {
		t.Fatal(err)
	}
	exits, _ := ListUserLandingExits(d, uid)
	if len(exits) != 1 || exits[0].NameOverride != "香港 01" || exits[0].Name != "n-a.com" {
		t.Fatalf("override lost or name corrupted: %+v", exits[0])
	}

	// empty name clears the override
	if _, err := SetUserLandingExitName(d, uid, "a.com", 443, ""); err != nil {
		t.Fatal(err)
	}
	exits, _ = ListUserLandingExits(d, uid)
	if exits[0].NameOverride != "" {
		t.Fatalf("override not cleared: %+v", exits[0])
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/db/ -run TestLandingExitNameOverride -v`
Expected: 编译错误 `undefined: SetUserLandingExitName`

- [ ] **Step 3: 实现**

新建 `internal/db/migrations/0030_landing_exit_name_override.sql`：

```sql
ALTER TABLE user_landing_exits ADD COLUMN name_override TEXT NOT NULL DEFAULT '';
```

`internal/db/landing_exits.go` 三处修改：

结构体 `LandingExit` 的 `Name` 字段后加一行：

```go
	NameOverride string `json:"name_override"`
```

（`Name string \`json:"name"\`` 与 `Protocol string \`json:"protocol"\`` 之间。）

`landingExitCols` 改为：

```go
const landingExitCols = `user_id, host, port, name, name_override, protocol, uri, present, quota_bytes, used_bytes, updated_at`
```

`scanLandingExit` 的 Scan 调用改为：

```go
	if err := r.Scan(&e.UserID, &e.Host, &e.Port, &e.Name, &e.NameOverride, &e.Protocol, &e.URI, &present, &e.QuotaBytes, &e.UsedBytes, &e.UpdatedAt); err != nil {
```

文件末尾（`NodesForUserExit` 之后）新增：

```go
// SetUserLandingExitName sets or clears (name == "") one exit's display-name
// override. The override lives outside SyncUserLandingExits so a subscription
// refresh cannot undo an admin rename; the parsed name column stays intact so
// clearing the override restores it. Renames never change push exclusion, so
// no re-dispatch hint is returned.
func SetUserLandingExitName(d *sql.DB, userID int64, host string, port int, name string) (updated bool, err error) {
	found, _, err := exitRowPresent(d, userID, host, port)
	if err != nil || !found {
		return false, err
	}
	_, err = d.Exec(`UPDATE user_landing_exits SET name_override=?, updated_at=? WHERE user_id=? AND host=? AND port=?`,
		name, now(), userID, host, port)
	return err == nil, err
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/db/ -v -run "TestLandingExit|TestSyncUserLandingExits"`
Expected: 全部 PASS

- [ ] **Step 5: 全量 db + server 测试（scan 列序回归）**

Run: `go test ./internal/db/ ./internal/server/`
Expected: PASS（`landingExitCols` 列序改动会波及所有查询路径，必须全量过一遍）

- [ ] **Step 6: Commit**

```bash
git add internal/db/migrations/0030_landing_exit_name_override.sql internal/db/landing_exits.go internal/db/landing_exits_test.go
git commit -m "feat(db): landing-exit display-name override that survives subscription sync"
```

---

### Task 3: admin rename API

**Files:**
- Modify: `internal/server/landing.go`（`apiDeleteLandingExit` 之后新增 handler）
- Modify: `internal/server/server.go:497`（route 注册，紧跟 delete 路由）
- Test: `internal/server/landing_exit_api_test.go`（helpers：`openDB`、`loginAsUser`、`loginAsAdmin`、`adminPost`、`itoa`）

**Interfaces:**
- Consumes: Task 2 的 `db.SetUserLandingExitName`。
- Produces: `POST /api/users/{id}/landing-exits/rename`，body `{host string, port int, name string}`，`name` 为空串清除改名；404 = 无账本行。Task 5 前端依赖。

- [ ] **Step 1: 写失败测试**

在 `internal/server/landing_exit_api_test.go` 末尾追加：

```go
func TestAPILandingExitRename(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	db.SyncUserLandingExits(d, uid, []db.LandingExitInput{
		{Host: "1.2.3.4", Port: 443, Name: "HK", Protocol: "vless", URI: "vless://u@1.2.3.4:443#HK"},
	}, "", "")
	s, _ := New(d)
	admin := loginAsAdmin(t, d)

	rec := adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/rename",
		map[string]any{"host": "1.2.3.4", "port": 443, "name": "香港 01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	exits, _ := db.ListUserLandingExits(d, uid)
	if exits[0].NameOverride != "香港 01" {
		t.Fatalf("override not stored: %+v", exits[0])
	}

	// unknown exit 404
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/rename",
		map[string]any{"host": "nope", "port": 1, "name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown exit: %d", rec.Code)
	}

	// empty (or blank) name clears the override
	if rec = adminPost(t, s, admin, "/api/users/"+itoa(uid)+"/landing-exits/rename",
		map[string]any{"host": "1.2.3.4", "port": 443, "name": "  "}); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if exits, _ = db.ListUserLandingExits(d, uid); exits[0].NameOverride != "" {
		t.Fatalf("override not cleared: %+v", exits[0])
	}
}

func TestAPILandingExitRenameRequiresAdmin(t *testing.T) {
	d := openDB(t)
	uid, userCookie := loginAsUser(t, d, 10)
	s, _ := New(d)
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/landing-exits/rename",
		bytes.NewReader([]byte(`{"host":"1.2.3.4","port":443,"name":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(userCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("non-admin must be rejected")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run TestAPILandingExitRename -v`
Expected: FAIL（rename 路由不存在 → 404/405，或 handler 未定义的编译错误）

- [ ] **Step 3: 实现**

`internal/server/landing.go` 的 `apiDeleteLandingExit` 之后追加：

```go
// apiRenameLandingExit sets or clears a display-name override on one exit.
// The override outlives subscription syncs and is what the user-facing list
// and copied URIs show; an empty name falls back to the parsed one.
func (s *Server) apiRenameLandingExit(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	updated, err := db.SetUserLandingExitName(s.DB, id, body.Host, body.Port, name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		jsonErr(w, http.StatusNotFound, "出口不存在")
		return
	}
	db.WriteAudit(s.DB, u.ID, "user.rename_exit", strconv.FormatInt(id, 10),
		fmt.Sprintf("%s:%d name=%q", body.Host, body.Port, name))
	jsonOK(w, map[string]any{"ok": true})
}
```

（`strings`、`strconv`、`fmt` 均已在 landing.go import。）

`internal/server/server.go` 在 `/users/{id}/landing-exits/delete` 行（:497）之后加：

```go
			r.Post("/users/{id}/landing-exits/rename", s.apiRenameLandingExit)
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/server/ -run "TestAPILandingExit" -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/landing.go internal/server/server.go internal/server/landing_exit_api_test.go
git commit -m "feat(server): admin endpoint to rename a landing exit's display name"
```

---

### Task 4: `/api/my/landing-nodes` 应用 override（显示名 + URI 重写）

**Files:**
- Modify: `internal/server/landing.go`（`apiMyLandingNodes` 视图组装循环，约 :238-246）
- Test: `internal/server/landing_sync_test.go`

**Interfaces:**
- Consumes: Task 1 `landing.RewriteName`、Task 2 `LandingExit.NameOverride`。
- Produces: `/api/my/landing-nodes` 响应中 `nodes[].name` 为生效名、`nodes[].uri` 为改名后 URI。stale 快照路径经同一账本 join 生效，无需单独处理。

- [ ] **Step 1: 写失败测试**

在 `internal/server/landing_sync_test.go` 末尾追加（文件已 import `httptest`、`json`、`db`；若 `nft-forward/internal/landing` 未 import 则补上）：

```go
func TestMyLandingNodesAppliesNameOverride(t *testing.T) {
	d := openDB(t)
	uid, cookie := loginAsUser(t, d, 10)
	db.SetUserLandingSource(d, uid, "", "vless://u@1.2.3.4:443#HK")
	s, _ := New(d)

	// first request materializes the exit set the rename targets
	req := httptest.NewRequest("GET", "/api/my/landing-nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := db.SetUserLandingExitName(d, uid, "1.2.3.4", 443, "香港 01"); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var resp struct {
		Nodes []struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"nodes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Nodes) != 1 || resp.Nodes[0].Name != "香港 01" {
		t.Fatalf("override not applied: %+v", resp.Nodes)
	}
	parsed := landing.ParseURIs([]string{resp.Nodes[0].URI})
	if len(parsed) != 1 || parsed[0].Name != "香港 01" || parsed[0].Host != "1.2.3.4" || parsed[0].Port != 443 {
		t.Fatalf("uri not rewritten or endpoint changed: %q -> %+v", resp.Nodes[0].URI, parsed)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run TestMyLandingNodesAppliesNameOverride -v`
Expected: FAIL，`override not applied`（name 仍为 "HK"）

- [ ] **Step 3: 实现**

`internal/server/landing.go` 的 `apiMyLandingNodes` 中，把视图组装循环：

```go
	for _, n := range nodes {
		v := myLandingNodeView{Node: n}
		if e := ledger[net.JoinHostPort(n.Host, strconv.Itoa(n.Port))]; e != nil {
			v.QuotaBytes = e.QuotaBytes
			v.UsedBytes = e.UsedBytes
			v.Exceeded = e.QuotaBytes > 0 && e.UsedBytes >= e.QuotaBytes
		}
		views = append(views, v)
	}
```

改为：

```go
	for _, n := range nodes {
		v := myLandingNodeView{Node: n}
		if e := ledger[net.JoinHostPort(n.Host, strconv.Itoa(n.Port))]; e != nil {
			v.QuotaBytes = e.QuotaBytes
			v.UsedBytes = e.UsedBytes
			v.Exceeded = e.QuotaBytes > 0 && e.UsedBytes >= e.QuotaBytes
			// An admin rename must reach the client the user actually
			// imports, not just this list — rewrite the URI's display name
			// too. A URI the rewriter can't handle is served unchanged so
			// copying keeps working.
			if e.NameOverride != "" {
				v.Node.Name = e.NameOverride
				if rewritten, err := landing.RewriteName(v.Node.URI, e.NameOverride); err == nil {
					v.Node.URI = rewritten
				}
			}
		}
		views = append(views, v)
	}
```

（`nft-forward/internal/landing` 已在该文件 import——`landing.Node` 在 stale 回退路径使用。）

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/server/ -run "TestMyLandingNodes|TestLandingSync" -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/landing.go internal/server/landing_sync_test.go
git commit -m "feat(server): my landing-nodes serves renamed exits with rewritten URI names"
```

---

### Task 5: 管理端 UI — 名称行内编辑 + 重置按钮 pill 样式

**Files:**
- Modify: `web/src/pages/users/Detail.jsx`（`LandingSourceForm` 表格 :457-505、`ExitQuotaForm` 之后新增组件）

**Interfaces:**
- Consumes: Task 3 的 `POST /users/{id}/landing-exits/rename`；`apiListUserLandingExits` 响应中的 `name_override`（Task 2 结构体序列化自带）。
- Produces: 仅 UI，无下游依赖。

- [ ] **Step 1: 新增 `ExitNameCell` 组件**

在 `Detail.jsx` 的 `ExitQuotaForm`（:516-536）之后加入：

```jsx
/* Inline-editable display name for a parsed node. The override lives on the
   exit-ledger row so it survives subscription refreshes; nodes without a
   ledger row (not yet synced) stay read-only. Saving an empty value restores
   the parsed name. */
function ExitNameCell({ userId, name, exit, onDone }) {
  const [editing, setEditing] = useState(false)
  const [val, setVal] = useState('')
  const toast = useToast()
  const effective = (exit?.name_override || name) || '(未命名)'
  if (!exit) return <span className="font-semibold">{effective}</span>
  const start = () => { setVal(exit.name_override || name || ''); setEditing(true) }
  const save = async () => {
    try {
      await api.post(`/users/${userId}/landing-exits/rename`,
        { host: exit.host, port: exit.port, name: val.trim() })
      toast(val.trim() ? '已改名' : '已恢复原名')
      setEditing(false)
      onDone()
    } catch (err) { toast(err.message, 'error') }
  }
  if (!editing) return (
    <button type="button" onClick={start}
      title={exit.name_override ? `原名称: ${name || '(未命名)'}` : '点击改名'}
      className="font-semibold text-left hover:text-blue-600 transition-colors">
      {effective}
      {exit.name_override && <span className="text-blue-500 ml-1">*</span>}
    </button>
  )
  return (
    <form onSubmit={e => { e.preventDefault(); save() }} className="inline-flex items-center gap-1.5">
      <input autoFocus className="input-field" value={val} onChange={e => setVal(e.target.value)}
        onKeyDown={e => { if (e.key === 'Escape') setEditing(false) }}
        placeholder="留空恢复原名" style={{ width: 140 }} />
      <button type="submit" className="btn-secondary text-xs">保存</button>
    </form>
  )
}
```

- [ ] **Step 2: 接入两处名称单元格**

preview 行（原 :464）：

```jsx
<td className="font-semibold">{n.name || '(未命名)'}</td>
```

改为：

```jsx
<td><ExitNameCell userId={userId} name={n.name}
  exit={exitByAddr[`${n.host}:${n.port}`]} onDone={loadExits} /></td>
```

residual 行（原 :488-491）：

```jsx
<td className="font-semibold">
  {ex.name || '(未命名)'}
  <Badge color="gray">已不在来源</Badge>
</td>
```

改为：

```jsx
<td>
  <ExitNameCell userId={userId} name={ex.name} exit={ex} onDone={loadExits} />
  <Badge color="gray">已不在来源</Badge>
</td>
```

- [ ] **Step 3: 重置按钮改 pill 样式（两处）**

preview 行（原 :473）与 residual 行（原 :498）的

```jsx
<button onClick={() => resetExit(ex)} className="text-blue-600 text-xs font-semibold ml-2">重置</button>
```

均改为：

```jsx
<button onClick={() => resetExit(ex)}
  className="ml-2 px-2 py-0.5 text-[11px] font-semibold rounded-md border transition-colors bg-blue-50 text-blue-700 border-blue-200 hover:bg-blue-100 dark:bg-blue-900/30 dark:text-blue-400 dark:border-blue-700">
  重置
</button>
```

- [ ] **Step 4: 构建验证**

Run: `cd /Users/xjetry/work/vibe/nft-forward/web && npm run build`
Expected: vite build 成功，无报错

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/users/Detail.jsx
git commit -m "feat(web): inline rename for parsed landing nodes; reset button as pill"
```

---

### Task 6: 用户侧配额列按 host:port 查账本

**Files:**
- Modify: `web/src/pages/my/LandingNodes.jsx`

**Interfaces:**
- Consumes: `/api/my/landing-nodes` 响应节点上的 `quota_bytes`/`used_bytes`/`exceeded`（含 Task 4 的改名生效）。
- Produces: 仅 UI，无下游依赖。

- [ ] **Step 1: 建账本查询表并改配额单元格**

`LandingNodes.jsx` 中，在 `const allNodes = mergeLanding(localNodes, serverNodes)`（:51）之前加：

```jsx
  /* Quotas are enforced per host:port regardless of which URI won the merge,
     so the ledger lookup must not depend on the row's source: a user pasting
     their own copy of an admin-assigned node keeps seeing its quota. */
  const ledger = new Map((serverNodes || []).map(n => [`${n.host}:${n.port}`, n]))
```

"已用/总量"单元格（:87-94）：

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

改为：

```jsx
<td className="font-mono text-xs">
  {(() => {
    const ex = ledger.get(`${n.host}:${n.port}`)
    if (!ex) return '—'
    return (
      <>
        {fmtTrafficGB(ex.used_bytes, ex.quota_bytes)}
        {ex.exceeded && <Badge color="red">已超额</Badge>}
      </>
    )
  })()}
</td>
```

- [ ] **Step 2: 构建验证**

Run: `cd /Users/xjetry/work/vibe/nft-forward/web && npm run build`
Expected: vite build 成功

- [ ] **Step 3: 全量回归**

Run: `cd /Users/xjetry/work/vibe/nft-forward && go test ./...`
Expected: 全部 PASS

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/my/LandingNodes.jsx
git commit -m "fix(web): landing quota column reads the exit ledger by host:port, surviving local-URI merge wins"
```
