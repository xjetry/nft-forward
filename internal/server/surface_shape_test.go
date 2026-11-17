package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// 节点与规则的各个对外面（/api、/api/my、/api/v1）的 JSON 钉成基准文件，比较的是解码
// 之后的值（键的顺序不计）。基准文件在 testdata/surfaces 下，改动对外形状时用
// -update-surfaces 重新生成，基准文件的 diff 就是这次改动对外可见的全部变化。
var updateSurfaces = flag.Bool("update-surfaces", false, "rewrite testdata/surfaces from the current responses")

type surfaceFixture struct {
	s                *Server
	d                *sql.DB
	adminCookie      *http.Cookie
	aliceCookie      *http.Cookie
	adminToken       string
	aliceToken       string
	alice            int64
	a, b, c, dd      int64 // 物理节点
	k, n, m, loop    int64 // 组合
	r1, r2, r3, r4   int64 // 规则
	createdPhysical  any   // 建物理节点的响应
	createdComposite any   // 建组合的响应
}

// newSurfaceFixture 建一份所有时间戳、端口、凭据都固定的数据：几台物理节点（在线、
// 离线、停用各有）、能展开的组合、嵌套组合、含停用成员的组合、成环展不开的组合，
// 管理员与用户各自的规则（含中转层与落地出口），授权上的角色覆盖、配额与限速。
func newSurfaceFixture(t *testing.T) *surfaceFixture {
	t.Helper()
	d := openDB(t)
	f := &surfaceFixture{d: d}
	hash, _ := HashPassword("pw")
	adminID, err := db.CreateUser(d, "root", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	f.alice, err = db.CreateUser(d, "alice", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, `UPDATE users SET max_forwards=20, billing_rate=1.5, expires_at=2000000000,
		traffic_quota_bytes=1073741824, traffic_used_bytes=4096, traffic_reset_days=30,
		last_traffic_reset_at=1700000500, admin_note='vip', created_at=1700000001 WHERE id=?`, f.alice)
	mustExec(t, d, `UPDATE users SET created_at=1700000000, last_traffic_reset_at=1700000400 WHERE id=?`, adminID)
	for _, p := range []struct {
		uid    int64
		cookie **http.Cookie
		token  *string
	}{{adminID, &f.adminCookie, &f.adminToken}, {f.alice, &f.aliceCookie, &f.aliceToken}} {
		sess, err := db.CreateSession(d, p.uid, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		*p.cookie = &http.Cookie{Name: sessionCookie, Value: sess}
		if *p.token, err = db.CreateAPIToken(d, p.uid, db.TokenScopeReadWrite); err != nil {
			t.Fatal(err)
		}
	}
	f.s, err = New(d)
	if err != nil {
		t.Fatal(err)
	}

	physical := func(body map[string]any) (int64, any) {
		t.Helper()
		rec := adminRequest(t, f.s, f.adminCookie, "POST", "/api/nodes", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("建节点 %v：%d %s", body["name"], rec.Code, rec.Body.String())
		}
		var resp struct {
			Node struct {
				ID int64 `json:"id"`
			} `json:"node"`
		}
		decodeInto(t, rec, &resp)
		created := decodeAny(t, rec)
		// 建节点的响应是在下面统一改写时间戳之前拿到的，created_at 是当时的时钟。
		if node, ok := created.(map[string]any)["node"].(map[string]any); ok {
			node["created_at"] = float64(0)
		}
		return resp.Node.ID, created
	}
	f.a, f.createdPhysical = physical(map[string]any{"name": "edge-a", "secret": "secret-a", "rate_multiplier": 2, "port_range": "20000-20100"})
	f.b, _ = physical(map[string]any{"name": "edge-b", "secret": "secret-b"})
	f.c, _ = physical(map[string]any{"name": "tail-c", "secret": "secret-c", "unidirectional": true})
	f.dd, _ = physical(map[string]any{"name": "spare-d", "secret": "secret-d"})
	mustExec(t, d, `UPDATE nodes SET online=1, address='203.0.113.1', agent_version='v0.69.0', agent_sha='sha-a',
		relay_host='10.0.0.1', relay_host_v6='2001:db8::a', last_seen=1700000900 WHERE id=?`, f.a)
	mustExec(t, d, `UPDATE nodes SET online=1, address='203.0.113.2', agent_version='v0.68.0', relay_host='10.0.0.2',
		relay_host_v6='2001:db8::b', relay_host_declared=1, last_seen=1700000901 WHERE id=?`, f.b)
	mustExec(t, d, `UPDATE nodes SET online=0, relay_host='10.0.0.3', last_seen=1700000902 WHERE id=?`, f.c)
	mustExec(t, d, `UPDATE nodes SET online=1, disabled=1, relay_host='10.0.0.4' WHERE id=?`, f.dd)
	if rec := adminRequest(t, f.s, f.adminCookie, "POST", fmt.Sprintf("/api/nodes/%d/roles", f.c), map[string]any{"roles": 2}); rec.Code != http.StatusOK {
		t.Fatalf("设中转角色：%d %s", rec.Code, rec.Body.String())
	}
	if rec := adminRequest(t, f.s, f.adminCookie, "POST", fmt.Sprintf("/api/nodes/%d/bindings", f.c), map[string]any{
		"bindings": []map[string]any{{"upstream_node_id": f.a, "mode": "userspace"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("绑定上游：%d %s", rec.Code, rec.Body.String())
	}

	composite := func(name string, rate float64, members ...int64) (int64, any) {
		t.Helper()
		hops := make([]map[string]any, len(members))
		for i, m := range members {
			hops[i] = map[string]any{"node_id": m, "mode": []string{"kernel", "userspace"}[i%2]}
		}
		return physical(map[string]any{"name": name, "node_type": "composite", "rate_multiplier": rate, "hops": hops})
	}
	f.k, f.createdComposite = composite("combo-k", 3, f.a, f.b)
	f.n, _ = composite("nested-n", 0, f.k, f.c)
	f.m, _ = composite("with-disabled-m", 0, f.a, f.dd)
	f.loop, _ = composite("loop-l", 0, f.a, f.b)
	// 写入时成环会被拒绝，这里直接改库，模拟展不开的陈旧数据。
	mustExec(t, d, `UPDATE node_hops SET hop_node_id=? WHERE node_id=? AND position=1`, f.loop, f.loop)

	if err := db.GrantNode(d, f.alice, f.a, 10, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGrantRoles(d, f.alice, f.a, db.NodeRoleEntry); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantNode(d, f.alice, f.k, 5, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantNode(d, f.alice, f.c, 3, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, `UPDATE user_nodes SET rate_limit_mbytes=8, traffic_used_bytes=2048 WHERE user_id=? AND node_id=?`, f.alice, f.k)
	mustExec(t, d, `UPDATE user_nodes SET granted_at=1700000100`)

	// 落地出口先物化，后面的规则才能按它归类出口。
	db.SetUserLandingSource(d, f.alice, "", "vless://u@6.6.6.6:8388#HK")
	if rec := surfaceGet(t, f.s, f.adminCookie, "", fmt.Sprintf("/api/users/%d", f.alice)); rec.Code != http.StatusOK {
		t.Fatalf("预热落地出口：%d %s", rec.Code, rec.Body.String())
	}

	rule := func(cookie *http.Cookie, url string, body map[string]any) int64 {
		t.Helper()
		rec := adminRequest(t, f.s, cookie, "POST", url, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("建规则 %v：%d %s", body["name"], rec.Code, rec.Body.String())
		}
		var resp struct {
			RuleID int64 `json:"rule_id"`
			Rule   struct {
				ID int64 `json:"id"`
			} `json:"rule"`
		}
		decodeInto(t, rec, &resp)
		if resp.RuleID != 0 {
			return resp.RuleID
		}
		return resp.Rule.ID
	}
	f.r1 = rule(f.adminCookie, "/api/rules", map[string]any{"node_id": f.a, "name": "adm-a", "proto": "tcp", "exit": "9.9.9.9:443", "entry_port": 20001})
	f.r2 = rule(f.adminCookie, "/api/rules", map[string]any{"node_id": f.n, "name": "adm-n", "proto": "tcp+udp", "exit": "8.8.8.8:53", "entry_port": 20002, "comment": "nested"})
	f.r3 = rule(f.aliceCookie, "/api/my/rules", map[string]any{"node_id": f.a, "via_node_ids": []int64{f.c}, "name": "alice-a", "proto": "tcp", "exit": "6.6.6.6:8388", "entry_port": 20003})
	f.r4 = rule(f.aliceCookie, "/api/my/rules", map[string]any{"node_id": f.k, "name": "alice-k", "proto": "tcp", "exit": "7.7.7.7:7", "entry_port": 20004, "exit_mode": "userspace"})

	// 中间跳的监听端口是随机分配的，改成由跳 id 决定，并让上一跳的目标端口跟着改。
	mustExec(t, d, `UPDATE rule_hops SET listen_port = 40000 + id WHERE position > 0`)
	mustExec(t, d, `UPDATE rule_hops SET target_port = (SELECT nx.listen_port FROM rule_hops nx
		WHERE nx.rule_id = rule_hops.rule_id AND nx.position = rule_hops.position + 1)
		WHERE EXISTS (SELECT 1 FROM rule_hops nx WHERE nx.rule_id = rule_hops.rule_id AND nx.position = rule_hops.position + 1)`)
	mustExec(t, d, `UPDATE rules SET created_at = 1700001000 + id`)
	// 两本账各造一个可辨认的数：规则的加权流量在规则行上，逐跳的折算流量在跳行上。
	mustExec(t, d, `UPDATE rules SET weighted_bytes = 1000 * id`)
	mustExec(t, d, `UPDATE rule_hops SET total_bytes = 10 * id`)
	mustExec(t, d, `UPDATE nodes SET created_at = 1700000000 + id, last_apply_at = NULL, last_error = NULL, last_warning = ''`)
	mustExec(t, d, `UPDATE nodes SET last_error = 'dial timeout', last_warning = '' WHERE id=?`, f.c)
	mustExec(t, d, `UPDATE nodes SET last_apply_at = 1700000950, last_warning = 'skipped 1' WHERE id=?`, f.a)
	for id, raw := range map[int64]int64{f.a: 1000, f.b: 2000, f.c: 500} {
		mustExec(t, d, `INSERT INTO node_raw_traffic(node_id, raw_bytes) VALUES(?,?)`, id, raw)
	}
	return f
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func surfaceGet(t *testing.T, s *Server, cookie *http.Cookie, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("解码响应：%v（%s）", err, rec.Body.String())
	}
}

func decodeAny(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	decodeInto(t, rec, &v)
	return normalizeSurface(v)
}

// normalizeSurface 只抹掉两类本身不确定的值：节点凭据（建节点时随机生成）记成它
// 是否为空，快照时间记成 0。其余值都由 fixture 固定。
func normalizeSurface(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch k {
			case "secret":
				if s, ok := val.(string); ok && s != "" {
					x[k] = "<set>"
				}
			case "generated_at":
				x[k] = float64(0)
			default:
				x[k] = normalizeSurface(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = normalizeSurface(x[i])
		}
	}
	return v
}

func TestNodeAndRuleSurfacesMatchTheirBaselines(t *testing.T) {
	f := newSurfaceFixture(t)
	type surface struct {
		name   string
		cookie *http.Cookie
		token  string
		path   string
	}
	surfaces := []surface{
		{"admin_dashboard", f.adminCookie, "", "/api/dashboard"},
		{"admin_nodes", f.adminCookie, "", "/api/nodes"},
		{"admin_node_physical", f.adminCookie, "", fmt.Sprintf("/api/nodes/%d", f.a)},
		{"admin_node_composite", f.adminCookie, "", fmt.Sprintf("/api/nodes/%d", f.k)},
		{"admin_node_nested", f.adminCookie, "", fmt.Sprintf("/api/nodes/%d", f.n)},
		{"admin_node_unflattenable", f.adminCookie, "", fmt.Sprintf("/api/nodes/%d", f.loop)},
		{"admin_rules", f.adminCookie, "", "/api/rules"},
		{"admin_rules_by_owner", f.adminCookie, "", fmt.Sprintf("/api/rules?owner_ids=%d", f.alice)},
		{"admin_rule_nested", f.adminCookie, "", fmt.Sprintf("/api/rules/%d", f.r2)},
		{"admin_rule_landing", f.adminCookie, "", fmt.Sprintf("/api/rules/%d", f.r3)},
		{"admin_user", f.adminCookie, "", fmt.Sprintf("/api/users/%d", f.alice)},
		{"my_dashboard", f.aliceCookie, "", "/api/my"},
		{"my_rules", f.aliceCookie, "", "/api/my/rules"},
		{"my_rule_via", f.aliceCookie, "", fmt.Sprintf("/api/my/rules/%d", f.r3)},
		{"my_rule_composite", f.aliceCookie, "", fmt.Sprintf("/api/my/rules/%d", f.r4)},
		{"v1_my_nodes", nil, f.aliceToken, "/api/v1/my/nodes"},
		{"v1_my_rules", nil, f.aliceToken, "/api/v1/my/rules"},
		{"v1_my_rule", nil, f.aliceToken, fmt.Sprintf("/api/v1/my/rules/%d", f.r4)},
		{"v1_info", nil, f.aliceToken, "/api/v1/info"},
		{"v1_nodes", nil, f.adminToken, "/api/v1/nodes"},
		{"v1_dashboard", nil, f.adminToken, "/api/v1/dashboard"},
		{"v1_usage", nil, f.adminToken, "/api/v1/usage"},
	}
	got := map[string]any{
		"admin_create_physical":  f.createdPhysical,
		"admin_create_composite": f.createdComposite,
	}
	for _, sf := range surfaces {
		rec := surfaceGet(t, f.s, sf.cookie, sf.token, sf.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s：%d %s", sf.name, sf.path, rec.Code, rec.Body.String())
		}
		got[sf.name] = decodeAny(t, rec)
	}

	dir := filepath.Join("testdata", "surfaces")
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := filepath.Join(dir, name+".json")
		if *updateSurfaces {
			b, err := json.MarshalIndent(got[name], "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s：缺少基准文件（用 -update-surfaces 生成）：%v", name, err)
		}
		var want any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("%s：基准文件不是 JSON：%v", name, err)
		}
		if diffs := surfaceDiff("", want, got[name]); len(diffs) > 0 {
			t.Errorf("%s 与基准不一致：\n  %s", name, strings.Join(diffs, "\n  "))
		}
	}
}

// surfaceDiff 列出两份解码后的 JSON 不一致的路径，失败信息直接指到哪个字段变了。
func surfaceDiff(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s：期望对象，得到 %s", path, compactJSON(got))}
		}
		var out []string
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !gok:
				out = append(out, fmt.Sprintf("%s.%s：缺少（期望 %s）", path, k, compactJSON(wv)))
			case !wok:
				out = append(out, fmt.Sprintf("%s.%s：多出（得到 %s）", path, k, compactJSON(gv)))
			default:
				out = append(out, surfaceDiff(path+"."+k, wv, gv)...)
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s：期望 %s\n    得到 %s", path, compactJSON(want), compactJSON(got))}
		}
		var out []string
		for i := range w {
			out = append(out, surfaceDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return out
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s：期望 %s，得到 %s", path, compactJSON(want), compactJSON(got))}
		}
		return nil
	}
}

func compactJSON(v any) string {
	var buf bytes.Buffer
	b, _ := json.Marshal(v)
	_ = json.Compact(&buf, b)
	return buf.String()
}
