package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// adminRequest 以管理员身份发一个 JSON 请求，不对状态码做断言。
func adminRequest(t *testing.T, s *Server, admin *http.Cookie, method, url string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return apiNodeAction(t, s, admin, method, url, mustJSON(body))
}

// 建组合时成员的转发模式认不出，请求应被当作格式错误拒绝，并且不留下任何东西：
// 建节点与写成员不在同一个事务里，校验必须发生在建节点之前。
func TestCreatingACompositeWithABadMemberModeLeavesNothingBehind(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	before, _ := db.ListNodes(d)

	rec := adminRequest(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "combo", "node_type": "composite",
		"hops": []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "relay"}},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "转发模式") {
		t.Fatalf("成员模式认不出应 400 并说明原因，实际 %d %s", rec.Code, rec.Body.String())
	}
	after, _ := db.ListNodes(d)
	if len(after) != len(before) {
		t.Fatalf("被拒绝的请求不应留下节点：之前 %d 个，之后 %d 个", len(before), len(after))
	}

	// 没填模式的成员按 userspace 记。
	rec = adminRequest(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "combo", "node_type": "composite",
		"hops": []map[string]any{{"node_id": a.ID}, {"node_id": b.ID, "mode": "kernel"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("建组合 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	members, _ := db.ListNodeHops(d, resp.Node.ID)
	if len(members) != 2 || members[0].Mode != "userspace" || members[1].Mode != "kernel" {
		t.Fatalf("成员模式应为 [userspace kernel]，实际 %+v", members)
	}
}

// 链尾不许直接转发是硬约束：管理员直接给出物理跳序列时不校验角色与绑定边，但这一条
// 在建规则和改规则两条路径上都必须拒绝，并且被拒绝的编辑不能改动已落库的链。
func TestExplicitHopsCannotEndOnANoDirectExitNodeOnCreateOrEdit(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	c, _ := db.CreateNode(d, "c", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, c.ID, "3.3.3.3")
	if err := db.UpdateNodeNoDirectExit(d, c.ID, true); err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	endingOnC := []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": c.ID, "mode": "kernel"}}
	rec := adminRequest(t, s, admin, "POST", "/api/rules", map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443", "hops": endingOnC,
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "禁止直接转发") {
		t.Fatalf("建规则：链尾是禁止直接转发的节点，应 400 并说明原因，实际 %d %s", rec.Code, rec.Body.String())
	}

	rid := adminCreateRuleID(t, s, admin, map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "kernel"}},
	})
	rec = adminRequest(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", rid), map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443", "hops": endingOnC,
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "禁止直接转发") {
		t.Fatalf("改规则：链尾是禁止直接转发的节点，应 400 并说明原因，实际 %d %s", rec.Code, rec.Body.String())
	}
	hops, _ := db.ListRuleHops(d, rid)
	if len(hops) != 2 || hops[1].NodeID != b.ID {
		t.Fatalf("被拒绝的编辑不应改动已落库的链，实际 %+v", hops)
	}

	// 禁止直接转发的节点不在链尾时可以用。
	rec = adminRequest(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", rid), map[string]any{
		"name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{{"node_id": c.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "kernel"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("禁止直接转发的节点不在链尾时应允许，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 不重新推导链的编辑（请求里没有入口、中转层、物理跳）沿用已落库的链，请求对出口模式
// 的指定仍然生效，取舍与重新推导时相同：给了 exit_mode 就用它，没给就保持现状；
// 动的只有最后一跳。
func TestHeaderOnlyEditAppliesTheSameExitModeChoice(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	comp := makeComposite(t, d, "comp", a.ID, b.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	header := func(extra map[string]any) map[string]any {
		body := map[string]any{"name": "r", "proto": "tcp", "exit": "9.9.9.9:443"}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	edit := func(rid int64, extra map[string]any) []string {
		t.Helper()
		adminJSON(t, s, admin, "PUT", fmt.Sprintf("/api/rules/%d", rid), header(extra))
		return chainModes(t, s, rid)
	}

	// 单点。
	single := adminCreateRuleID(t, s, admin, header(map[string]any{"node_id": a.ID}))
	wantModes(t, chainModes(t, s, single), "kernel")
	wantModes(t, edit(single, map[string]any{"exit_mode": "userspace"}), "userspace")
	wantModes(t, edit(single, nil), "userspace") // 没给：保持现状
	wantModes(t, edit(single, map[string]any{"exit_mode": "kernel"}), "kernel")

	// 两跳的显式链：没给就保持现状；给了只动最后一跳，中间那一跳不受影响。
	multi := adminCreateRuleID(t, s, admin, header(map[string]any{
		"hops": []map[string]any{{"node_id": a.ID, "mode": "userspace"}, {"node_id": b.ID, "mode": "kernel"}},
	}))
	wantModes(t, chainModes(t, s, multi), "userspace", "kernel")
	wantModes(t, edit(multi, nil), "userspace", "kernel")
	wantModes(t, edit(multi, map[string]any{"exit_mode": "userspace"}), "userspace", "userspace")

	// 只有一跳、这一跳记在组合名下的链也一样。
	oneUnderComposite := adminCreateRuleID(t, s, admin, header(map[string]any{
		"hops": []map[string]any{{"node_id": b.ID, "via_node_id": comp.ID, "mode": "kernel"}},
	}))
	wantModes(t, edit(oneUnderComposite, nil), "kernel")
	wantModes(t, edit(oneUnderComposite, map[string]any{"exit_mode": "userspace"}), "userspace")
}

// 用户被授权的是组合本身，它的成员（以及嵌套在里面的组合）通常不在授权之列。用户侧
// 接口给出的成员链必须仍是展开到物理节点的整条链、带节点名——规则表单的预览与
// “链尾能否直接转发”的判断都按它来。
func TestMyRulesShowsTheFullMemberChainOfAGrantedNestedComposite(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	x, _ := db.CreateNode(d, "x", "", "")
	for _, n := range []*db.Node{a, b, x} {
		_ = db.UpdateNodeRelayHost(d, n.ID, fmt.Sprintf("10.0.0.%d", n.ID))
	}
	inner := makeComposite(t, d, "inner", a.ID, b.ID)
	outer := makeComposite(t, d, "outer", inner.ID, x.ID)
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, outer.ID, 5, 0) // 只授权最外层的组合

	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, outer.ID, "r"); rec.Code != http.StatusOK {
		t.Fatalf("建规则 status=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("应有 1 条规则，实际 %d", len(rules))
	}

	want := []string{fmt.Sprintf("%d:a", a.ID), fmt.Sprintf("%d:b", b.ID), fmt.Sprintf("%d:x", x.ID)}
	// 规则列表与规则详情各自装配节点，两个面都要给出同一条成员链。
	for _, url := range []string{"/api/my/rules", fmt.Sprintf("/api/my/rules/%d", rules[0].ID)} {
		req := httptest.NewRequest("GET", url, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", url, rec.Code, rec.Body.String())
		}
		var resp struct {
			Nodes []struct {
				ID   int64 `json:"id"`
				Hops []struct {
					NodeID int64  `json:"node_id"`
					Name   string `json:"name"`
				} `json:"hops"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Nodes) != 1 || resp.Nodes[0].ID != outer.ID {
			t.Fatalf("%s：用户应只看到被授权的组合，实际 %+v", url, resp.Nodes)
		}
		var got []string
		for _, h := range resp.Nodes[0].Hops {
			got = append(got, fmt.Sprintf("%d:%s", h.NodeID, h.Name))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s 的成员链：得到 %v，期望 %v", url, got, want)
		}
	}
}
