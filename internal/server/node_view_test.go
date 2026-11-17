package server

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
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
// 都一样，并且都是按全量拓扑算出的值；用户面的 roles 一律是这条授权下的生效角色。
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
	// alice 对 edge-a 的授权把角色覆盖成只能做入口。
	check("用户", user, map[int64]float64{f.a: 1})
}
