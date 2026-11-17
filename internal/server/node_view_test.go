package server

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// 管理员仪表盘给的是全部节点的整行（连接地址、错误、属主……），只能给管理员。
func TestAdminDashboardRefusesUsers(t *testing.T) {
	f := newSurfaceFixture(t)
	if rec := surfaceGet(t, f.s, f.aliceCookie, "", "/api/dashboard"); rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户取 /api/dashboard 应被拒绝，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := surfaceGet(t, f.s, f.adminCookie, "", "/api/dashboard"); rec.Code != http.StatusOK {
		t.Fatalf("管理员取 /api/dashboard 应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// machineOnlyKeys 是节点上只对管理员有意义的字段：机器的接入与运行状态、属主、
// 行的排序与创建时间。用户面的节点不带这些字段。
var machineOnlyKeys = []string{
	"owner_id", "address", "secret", "relay_host_declared", "relay_host_v6_declared",
	"agent_version", "agent_sha", "last_seen", "last_apply_at", "last_error", "last_warning",
	"port_range", "sort_order", "created_at",
	"last_upgrade_at", "last_upgrade_version", "last_upgrade_status", "last_upgrade_error",
}

// surfaceNodes 从一个响应里按 id 取出 nodes 列表与 node_by_id 里的每个节点对象，
// 键是“字段名#节点 id”，便于失败时指出是哪个面的哪个节点。
func surfaceNodes(t *testing.T, f *surfaceFixture, cookie *http.Cookie, path string, keys ...string) map[string]map[string]any {
	t.Helper()
	rec := surfaceGet(t, f.s, cookie, "", path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s：%d %s", path, rec.Code, rec.Body.String())
	}
	body, ok := decodeAny(t, rec).(map[string]any)
	if !ok {
		t.Fatalf("%s：响应不是对象", path)
	}
	out := map[string]map[string]any{}
	add := func(where string, v any) {
		n, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s %s：节点不是对象：%v", path, where, v)
		}
		out[fmt.Sprintf("%s %s#%v", path, where, n["id"])] = n
	}
	for _, key := range keys {
		switch v := body[key].(type) {
		case []any:
			for _, n := range v {
				add(key, n)
			}
		case map[string]any:
			if _, isNode := v["node_type"]; isNode {
				add(key, v)
				continue
			}
			for _, n := range v {
				add(key, n)
			}
		default:
			t.Fatalf("%s：没有 %s（%T）", path, key, body[key])
		}
	}
	return out
}

func TestUserSurfacesCarryNoMachineFields(t *testing.T) {
	f := newSurfaceFixture(t)
	nodes := map[string]map[string]any{}
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/api/my", []string{"nodes"}},
		{"/api/my/rules", []string{"nodes", "node_by_id"}},
		{fmt.Sprintf("/api/my/rules/%d", f.r4), []string{"nodes", "node_by_id"}},
	} {
		for k, v := range surfaceNodes(t, f, f.aliceCookie, p.path, p.keys...) {
			nodes[k] = v
		}
	}
	if len(nodes) == 0 {
		t.Fatal("用户面一个节点都没取到")
	}
	for where, n := range nodes {
		var leaked []string
		for _, k := range machineOnlyKeys {
			if _, ok := n[k]; ok {
				leaked = append(leaked, k)
			}
		}
		if len(leaked) > 0 {
			sort.Strings(leaked)
			t.Errorf("%s 带了只给管理员的字段：%s", where, strings.Join(leaked, ", "))
		}
	}
}

// derivedOf 取一个节点对象上由别的行推出的那几个属性。
func derivedOf(n map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"online", "entry_relay_host", "entry_relay_host_v6", "exit_relay_host_v6", "hops", "roles"} {
		out[k] = n[k]
	}
	return out
}

// 同一个节点，不论出现在哪个面、哪个字段里，派生属性（在线、出入口地址、成员链）
// 都一样，并且都是按全量拓扑算出的值；用户面的 roles 一律是这条授权下生效的入口/中转资格。
func TestEveryNodeSurfaceDerivesTheSameAttributes(t *testing.T) {
	f := newSurfaceFixture(t)
	admin := map[string]map[string]any{}
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/api/dashboard", []string{"nodes"}},
		{"/api/nodes", []string{"nodes"}},
		{fmt.Sprintf("/api/nodes/%d", f.k), []string{"node"}},
		{fmt.Sprintf("/api/nodes/%d", f.n), []string{"node", "single_nodes"}},
		{fmt.Sprintf("/api/nodes/%d", f.loop), []string{"node"}},
		{"/api/rules", []string{"nodes"}},
		{fmt.Sprintf("/api/rules/%d", f.r2), []string{"nodes", "node_by_id"}},
		{fmt.Sprintf("/api/users/%d", f.alice), []string{"nodes", "all_nodes"}},
	} {
		for k, v := range surfaceNodes(t, f, f.adminCookie, p.path, p.keys...) {
			admin[k] = v
		}
	}
	user := map[string]map[string]any{}
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/api/my", []string{"nodes"}},
		{"/api/my/rules", []string{"nodes", "node_by_id"}},
		{fmt.Sprintf("/api/my/rules/%d", f.r3), []string{"nodes", "node_by_id"}},
	} {
		for k, v := range surfaceNodes(t, f, f.aliceCookie, p.path, p.keys...) {
			user[k] = v
		}
	}

	hop := func(id int64, name, mode string) map[string]any {
		return map[string]any{"node_id": float64(id), "name": name, "node_type": "remote", "mode": mode}
	}
	want := map[int64]map[string]any{
		// 两个成员都在线：组合在线；入口取第一个成员、出口 v6 取最后一个成员。
		f.k: {"online": float64(1), "entry_relay_host": "10.0.0.1", "entry_relay_host_v6": "2001:db8::a",
			"exit_relay_host_v6": "2001:db8::b", "roles": float64(3),
			"hops": []any{hop(f.a, "edge-a", "kernel"), hop(f.b, "edge-b", "userspace")}},
		// 嵌套展开到三台机器，最后一台离线：组合离线；最后一台没有 v6，出口 v6 为空。
		f.n: {"online": float64(0), "entry_relay_host": "10.0.0.1", "entry_relay_host_v6": "2001:db8::a",
			"exit_relay_host_v6": nil, "roles": float64(3),
			"hops": []any{hop(f.a, "edge-a", "kernel"), hop(f.b, "edge-b", "userspace"), hop(f.c, "tail-c", "userspace")}},
		// 成员成环、展不开：没有任何派生属性。
		f.loop: {"online": float64(0), "entry_relay_host": nil, "entry_relay_host_v6": nil,
			"exit_relay_host_v6": nil, "roles": float64(3), "hops": nil},
		// 物理节点：在线取它自己的行，没有组合才有的派生字段。
		f.a: {"online": float64(1), "entry_relay_host": nil, "entry_relay_host_v6": nil,
			"exit_relay_host_v6": nil, "roles": float64(3), "hops": nil},
	}
	check := func(side string, got map[string]map[string]any, roleOverride map[int64]float64) {
		seen := map[int64]int{}
		for where, n := range got {
			id := int64(n["id"].(float64))
			exp, ok := want[id]
			if !ok {
				continue
			}
			seen[id]++
			expected := map[string]any{}
			for k, v := range exp {
				expected[k] = v
			}
			if r, ok := roleOverride[id]; ok {
				expected["roles"] = r
			}
			if d := derivedOf(n); !reflect.DeepEqual(d, expected) {
				t.Errorf("%s %s：派生属性\n  得到 %s\n  期望 %s", side, where, compactJSON(d), compactJSON(expected))
			}
		}
		if len(seen) == 0 {
			t.Errorf("%s 面一个被检查的节点都没取到", side)
		}
	}
	check("管理员", admin, nil)
	// alice 对 edge-a 的授权把资格覆盖成只能做入口。
	check("用户", user, map[int64]float64{f.a: 1})
}

// nodeObjects 在一份解码后的响应里找出所有节点对象：带 id 与 unidirectional 两个键的
// 对象（规则、成员链、授权都不带 unidirectional）。
func nodeObjects(v any, visit func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		_, hasID := x["id"]
		if _, hasUni := x["unidirectional"]; hasID && hasUni {
			visit(x)
		}
		for _, c := range x {
			nodeObjects(c, visit)
		}
	case []any:
		for _, c := range x {
			nodeObjects(c, visit)
		}
	}
}

// 组合的计费方向与链尾资格不在它自己那一行上，要从成员推出：计费方向取第一个物理成员
// 的（经过组合的流量在段首那一跳入账，按上报那台机器的方向折算），链尾资格取最后一个
// 物理成员的（发起出口那条腿的是它）。物理节点取自己的列。节点不论出现在哪个面，给的
// 都是这个值——用户拿不到组合的成员，只能靠组合自己带着。
func TestCompositeBillingDirectionAndExitEligibilityComeFromItsMembers(t *testing.T) {
	f := newSurfaceFixture(t)
	if err := db.UpdateNodeUnidirectional(f.d, f.a, true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeNoDirectExit(f.d, f.b, true); err != nil {
		t.Fatal(err)
	}
	// 夹具里的组合都以 a 打头；再建一个次序反过来的，“取第一个/最后一个成员”才与
	// “任何一个成员有就算”分得开。
	rev, err := db.CreateCompositeNode(f.d, "reversed", db.LineSpec{}, []db.NodeHop{
		{HopNodeID: f.b, Mode: "kernel"}, {HopNodeID: f.a, Mode: "userspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	type flags struct{ unidirectional, noDirectExit bool }
	want := map[int64]flags{
		f.a:    {true, false},
		f.b:    {false, true},
		f.c:    {true, false},  // 夹具建它时就是单向计费
		f.k:    {true, true},   // 成员 a、b
		rev.ID: {false, false}, // 成员 b、a
		f.n:    {true, false},  // 展开为 a、b、c
		f.loop: {false, false}, // 展不开
	}
	type side struct {
		name   string
		cookie *http.Cookie
		token  string
		paths  []string
		// 这一侧必须给出的节点：管理员面是全部，用户面是 alice 被授权的。
		must []int64
	}
	all := []int64{f.a, f.b, f.c, f.k, rev.ID, f.n, f.loop}
	granted := []int64{f.a, f.c, f.k}
	sides := []side{
		{"管理员", f.adminCookie, "", []string{
			"/api/dashboard", "/api/nodes", fmt.Sprintf("/api/nodes/%d", f.k), fmt.Sprintf("/api/nodes/%d", f.n),
			"/api/rules", fmt.Sprintf("/api/rules/%d", f.r2), fmt.Sprintf("/api/users/%d", f.alice),
		}, all},
		{"用户", f.aliceCookie, "", []string{
			"/api/my", "/api/my/rules", fmt.Sprintf("/api/my/rules/%d", f.r3),
		}, granted},
		{"v1 管理员", nil, f.adminToken, []string{"/api/v1/nodes"}, all},
		{"v1 用户", nil, f.aliceToken, []string{"/api/v1/my/nodes", "/api/v1/info"}, granted},
	}
	for _, sd := range sides {
		seen := map[int64]int{}
		for _, path := range sd.paths {
			rec := surfaceGet(t, f.s, sd.cookie, sd.token, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s：%d %s", sd.name, path, rec.Code, rec.Body.String())
			}
			found := 0
			nodeObjects(decodeAny(t, rec), func(n map[string]any) {
				found++
				id := int64(n["id"].(float64))
				exp, ok := want[id]
				if !ok {
					return
				}
				seen[id]++
				if got := n["unidirectional"]; got != exp.unidirectional {
					t.Errorf("%s %s 节点 %d（%v）：unidirectional = %v，应为 %v", sd.name, path, id, n["name"], got, exp.unidirectional)
				}
				// /api/v1/info 的节点只带计费方向。
				if got, has := n["no_direct_exit"]; has && got != exp.noDirectExit {
					t.Errorf("%s %s 节点 %d（%v）：no_direct_exit = %v，应为 %v", sd.name, path, id, n["name"], got, exp.noDirectExit)
				}
			})
			if found == 0 {
				t.Errorf("%s %s：响应里一个节点对象都没找到", sd.name, path)
			}
		}
		for _, id := range sd.must {
			if seen[id] == 0 {
				t.Errorf("%s面没有给出节点 %d", sd.name, id)
			}
		}
	}
}
