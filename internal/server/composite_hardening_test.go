package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// 1: 组合作为链尾时，真正发起出口段的是组合的末位子节点，no_direct_exit 校验必须
// 穿透到该物理子节点，而不是只看逻辑组合节点。
func TestNoDirectExitCompositeLastChild(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	_ = db.UpdateNodeNoDirectExit(d, c2.ID, true) // 末位子节点禁止直出

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)
	s, _ := New(d)

	if rec := createMyRule(t, s, cookie, comp.ID, "r-comp-tail"); rec.Code != http.StatusBadRequest {
		t.Fatalf("composite whose last child forbids direct exit: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// 2: 撤销「中转」节点的授权，即便该节点不是规则入口，也必须清掉以它为 via 的
// 既有链，否则规则继续跑（继续计费）却已无授权。
func TestRevokeViaNodeStopsChain(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "mid", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, mid.ID, "2.2.2.2")
	bindVia(t, d, entry.ID, mid.ID, "userspace")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	s, _ := New(d)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r1"); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	nodes, err := db.DeleteRulesForUserNode(d, uid, mid.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 0 {
		t.Fatalf("revoking a via must delete the chain, still have %d rules", len(rules))
	}
	if !containsID(nodes, entry.ID) || !containsID(nodes, mid.ID) {
		t.Fatalf("re-dispatch nodes %v missing entry(%d)/mid(%d)", nodes, entry.ID, mid.ID)
	}
}

// 3: 删除组合节点必须删掉跑在它上面的规则，并返回其物理子节点以便重下发——组合
// id 从不出现在 rule_hops.node_id，仅靠 FK 级联会留下子节点的内核残留。
func TestDeleteRulesUsingNodeReturnsCompositeChildren(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)
	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, comp.ID, "r-comp"); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	nodes, err := db.DeleteRulesUsingNode(d, comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 0 {
		t.Fatalf("composite delete must remove its rules, still %d", len(rules))
	}
	if !containsID(nodes, c1.ID) || !containsID(nodes, c2.ID) {
		t.Fatalf("nodes %v must include physical children c1(%d)/c2(%d)", nodes, c1.ID, c2.ID)
	}
	if containsID(nodes, comp.ID) {
		t.Fatalf("composite id %d must be excluded from re-dispatch set %v", comp.ID, nodes)
	}
}

// 3 (endpoint): 通过删除节点端点删组合，其规则应被移除（且不报错）。
func TestDeleteCompositeEndpointRemovesRules(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)
	s, _ := New(d)
	createMyRule(t, s, cookie, comp.ID, "r-comp")

	admin := loginAsAdmin(t, d)
	rec := apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d", comp.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete composite: %d %s", rec.Code, rec.Body.String())
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 0 {
		t.Fatalf("composite delete endpoint must remove its rules, still %d", len(rules))
	}
}

// 4: explicit-hops 是 admin 的任意链逃生通道，角色/绑定边有意不校验，但 no_direct_exit
// 是硬安全约束——禁止直出的节点即便经此路径也不能坐在出口位。
func TestExplicitHopsRejectsNoDirectExitTail(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeNoDirectExit(d, b.ID, true)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	body, _ := json.Marshal(map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "kernel"}},
	})
	req := httptest.NewRequest("POST", "/api/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("explicit hops ending at no_direct_exit node: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// 5: 组合成员可以是另一个组合(嵌套),保存时递归展平到物理单点;只有环引用与
// 展平后跳数超限会被拒(否则展平无法终止)。建组合与改跳序两条写入路径都放开嵌套、
// 都挡环。
func TestCompositeNestingAllowedCyclesRejected(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	c, _ := db.CreateNode(d, "c", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, c.ID, "3.3.3.3")
	inner := makeComposite(t, d, "inner", a.ID, b.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	// 建组合时以组合为子节点 → 现在允许(200)
	rec := apiNodeAction(t, s, admin, "POST", "/api/nodes", mustJSON(map[string]any{
		"name": "outer", "node_type": "composite",
		"hops": []map[string]any{{"node_id": inner.ID, "mode": "kernel"}, {"node_id": c.ID, "mode": "kernel"}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("nesting a composite on create should be allowed: %d %s", rec.Code, rec.Body.String())
	}

	// 改跳序把子节点换成组合 → 允许
	outer2 := makeComposite(t, d, "outer2", a.ID, c.ID)
	rec = apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", outer2.ID), mustJSON(map[string]any{
		"hops": []map[string]any{{"node_id": inner.ID, "mode": "kernel"}, {"node_id": c.ID, "mode": "kernel"}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("nesting a composite on reorder should be allowed: %d %s", rec.Code, rec.Body.String())
	}

	// 直接自引用 → 400
	selfRef := makeComposite(t, d, "selfref", a.ID, b.ID)
	rec = apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", selfRef.ID), mustJSON(map[string]any{
		"hops": []map[string]any{{"node_id": selfRef.ID, "mode": "kernel"}, {"node_id": a.ID, "mode": "kernel"}},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-referential composite: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// 双向环 A→B→A:闭合边写入时被拒
	ca := makeComposite(t, d, "cyc-a", a.ID, b.ID)
	cb := makeComposite(t, d, "cyc-b", a.ID, b.ID)
	rec = apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", ca.ID), mustJSON(map[string]any{
		"hops": []map[string]any{{"node_id": cb.ID, "mode": "kernel"}, {"node_id": a.ID, "mode": "kernel"}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("ca->cb (no cycle yet) should be allowed: %d %s", rec.Code, rec.Body.String())
	}
	rec = apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", cb.ID), mustJSON(map[string]any{
		"hops": []map[string]any{{"node_id": ca.ID, "mode": "kernel"}, {"node_id": a.ID, "mode": "kernel"}},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cycle cb->ca->cb: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// 三节点环 x→y→z→x:闭合边写入时经多层 DFS 被拒(覆盖环检测的递归深入)
	tx := makeComposite(t, d, "tri-x", a.ID, b.ID)
	ty := makeComposite(t, d, "tri-y", a.ID, b.ID)
	tz := makeComposite(t, d, "tri-z", a.ID, b.ID)
	setEdge := func(from, to int64) *httptest.ResponseRecorder {
		return apiNodeAction(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", from), mustJSON(map[string]any{
			"hops": []map[string]any{{"node_id": to, "mode": "kernel"}, {"node_id": a.ID, "mode": "kernel"}},
		}))
	}
	if rec := setEdge(tx.ID, ty.ID); rec.Code != http.StatusOK {
		t.Fatalf("tri x->y should be allowed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := setEdge(ty.ID, tz.ID); rec.Code != http.StatusOK {
		t.Fatalf("tri y->z should be allowed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := setEdge(tz.ID, tx.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("three-node cycle z->x->y->z: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// 6: 组合探测要覆盖每个子节点，而不是只测末跳→目标；中间子节点离线时整体结果不能
// 报 OK（数据面子链路无端口不可独立探测，故以每个子节点的存活折入结果）。
func TestCompositeProbeReportsEveryChild(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/probe?target=9.9.9.9:80&node=%d", comp.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var res probeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(res.Hops) != 2 {
		t.Fatalf("composite probe must report every child, got %d hops: %s", len(res.Hops), rec.Body.String())
	}
	if res.OK {
		t.Fatalf("no agents connected → composite probe must not report OK")
	}
}
