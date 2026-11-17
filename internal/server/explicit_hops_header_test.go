package server

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// 管理员直接给出物理跳时，规则表头里的逻辑路径由这些跳决定：跳按所属的逻辑节点
// （via_node_id，省略时是这一跳自己的节点）分成连续的段，第一段的逻辑节点是入口
// （node_id），其余各段依次是中转层（via_node_ids）。请求里另给的 node_id 或
// via_node_ids 与之不一致就是自相矛盾，回 400，不落库。改规则时给了 hops，表头跟着
// 改写。
func TestExplicitHopsDecideTheRuleHeader(t *testing.T) {
	d := openDB(t)
	node := func(name, relay string) int64 {
		t.Helper()
		n, err := db.CreateNode(d, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateNodeRelayHost(d, n.ID, relay); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	a, c1, c2, x := node("a", "10.0.0.1"), node("c1", "10.0.0.2"), node("c2", "10.0.0.3"), node("x", "10.0.0.9")
	comp := makeComposite(t, d, "comp", c1, c2).ID
	admin := loginAsAdmin(t, d)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}

	// hop(n, 0) 不写 via_node_id。
	hop := func(n, logical int64) map[string]any {
		h := map[string]any{"node_id": n, "mode": "kernel"}
		if logical != 0 {
			h["via_node_id"] = logical
		}
		return h
	}
	body := func(name string, extra map[string]any, hops ...map[string]any) map[string]any {
		b := map[string]any{"name": name, "proto": "tcp", "exit": "9.9.9.9:443", "hops": hops}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	type state struct {
		Entry int64
		Vias  []int64
		Chain []int64
	}
	stateOf := func(id int64) state {
		t.Helper()
		rl, err := db.GetRule(d, id)
		if err != nil {
			t.Fatal(err)
		}
		hops, err := db.ListRuleHops(d, id)
		if err != nil {
			t.Fatal(err)
		}
		st := state{Entry: rl.NodeID, Vias: append([]int64{}, rl.ViaNodeIDs...)}
		for _, h := range hops {
			st.Chain = append(st.Chain, h.NodeID)
		}
		return st
	}
	want := func(what string, id int64, w state) {
		t.Helper()
		if got := stateOf(id); !reflect.DeepEqual(got, w) {
			t.Errorf("%s：规则是 %+v，期望 %+v", what, got, w)
		}
	}
	ruleCount := func() int {
		t.Helper()
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// --- 建 ---
	whole := adminCreateRuleID(t, s, admin, body("whole", nil, hop(c1, comp), hop(c2, comp)))
	want("整条链属于一个组合", whole, state{comp, []int64{}, []int64{c1, c2}})

	layered := adminCreateRuleID(t, s, admin, body("layered", map[string]any{"node_id": a, "via_node_ids": []int64{comp}},
		hop(a, 0), hop(c1, comp), hop(c2, comp)))
	want("单点入口加一个组合，请求里的 node_id 与 via_node_ids 与跳一致", layered, state{a, []int64{comp}, []int64{a, c1, c2}})

	before := ruleCount()
	// why 是拒绝理由里应当出现的字样：同是 400，要是因为别的缘故被拒的就不算数。
	type refusal struct {
		name string
		body map[string]any
		why  string
	}
	refused := func(method, path string, r refusal) {
		t.Helper()
		rec := adminRequest(t, s, admin, method, path, r.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), r.why) {
			t.Errorf("%s %s，%s：回了 %d %s，应回 400 并说明 %q", method, path, r.name, rec.Code, rec.Body.String(), r.why)
		}
	}
	for _, bad := range []refusal{
		{"node_id 与第一跳所属的逻辑节点不一致", body("bad", map[string]any{"node_id": c1}, hop(c1, comp), hop(c2, comp)), "node_id 与 hops 不一致"},
		{"node_id 是链上没有的节点", body("bad", map[string]any{"node_id": x}, hop(a, 0), hop(c1, 0)), "node_id 与 hops 不一致"},
		{"via_node_ids 与跳不一致", body("bad", map[string]any{"via_node_ids": []int64{}}, hop(a, 0), hop(c1, 0)), "via_node_ids 与 hops 不一致"},
		{"via_node_ids 次序与跳不一致", body("bad", map[string]any{"via_node_ids": []int64{c2, c1}}, hop(a, 0), hop(c1, 0), hop(c2, 0)), "via_node_ids 与 hops 不一致"},
		{"跳所属的逻辑节点不存在", body("bad", nil, hop(a, 987654)), "节点 987654 不存在"},
		{"同一个逻辑节点的跳被别的段隔开", body("bad", nil, hop(c1, comp), hop(a, 0), hop(c2, comp)), "只能占一段"},
	} {
		refused("POST", "/api/rules", bad)
	}
	if after := ruleCount(); after != before {
		t.Errorf("被拒绝的建规则请求不应留下规则：之前 %d 条，之后 %d 条", before, after)
	}

	// --- 改 ---
	path := fmt.Sprintf("/api/rules/%d", whole)
	adminJSON(t, s, admin, "PUT", path, body("whole", nil, hop(a, 0), hop(c1, comp), hop(c2, comp)))
	want("改规则时给了 hops，表头跟着改写", whole, state{a, []int64{comp}, []int64{a, c1, c2}})

	adminJSON(t, s, admin, "PUT", path, body("whole", map[string]any{"node_id": comp, "via_node_ids": []int64{}}, hop(c1, comp), hop(c2, comp)))
	want("改规则时 hops 与一致的 node_id、via_node_ids 一起给", whole, state{comp, []int64{}, []int64{c1, c2}})

	for _, bad := range []refusal{
		{"node_id 与跳不一致", body("whole", map[string]any{"node_id": a}, hop(c1, comp), hop(c2, comp)), "node_id 与 hops 不一致"},
		{"via_node_ids 与跳不一致", body("whole", map[string]any{"via_node_ids": []int64{a}}, hop(c1, comp), hop(c2, comp)), "via_node_ids 与 hops 不一致"},
		{"跳所属的逻辑节点不存在", body("whole", nil, hop(c1, 987654)), "节点 987654 不存在"},
	} {
		refused("PUT", path, bad)
		want("改规则被拒绝后（"+bad.name+"）", whole, state{comp, []int64{}, []int64{c1, c2}})
	}

	// 页面保存规则时总是带上表头里的 node_id 与 via_node_ids，面板据此重新推导链。
	// 表头如实写着“入口是 comp”，推导出的就还是 c1 → c2。
	adminJSON(t, s, admin, "PUT", path, map[string]any{
		"node_id": comp, "via_node_ids": []int64{}, "name": "whole-renamed", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	want("按表头重新推导之后", whole, state{comp, []int64{}, []int64{c1, c2}})
}
