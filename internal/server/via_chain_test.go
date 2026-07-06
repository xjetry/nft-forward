package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"nft-forward/internal/db"
)

func bindVia(t *testing.T, d *sql.DB, up, down int64, mode string) {
	t.Helper()
	if err := db.ReplaceBindingsForDownstream(d, down, []db.NodeBinding{
		{UpstreamNodeID: up, DownstreamNodeID: down, Mode: mode},
	}); err != nil {
		t.Fatal(err)
	}
	n, _ := db.GetNode(d, down)
	if err := db.UpdateNodeRoles(d, down, n.Roles|db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}
}

func createMyRuleVia(t *testing.T, s *Server, cookie *http.Cookie, nodeID int64, vias []int64, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"node_id": nodeID, "via_node_ids": vias, "name": name, "proto": "tcp",
		"exit": "9.9.9.9:8443", "exit_mode": "userspace",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// 入口(单点) + 组合中转：链 = entry ++ mid 的两个子节点；
// 衔接段模式取绑定边；末跳模式取规则 exit_mode。
func TestViaChainAssembly(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	m1, _ := db.CreateNode(d, "akari-1", "", "")
	m2, _ := db.CreateNode(d, "akari-2", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, m1.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, m2.ID, "3.3.3.3")
	mid := makeComposite(t, d, "akari", m1.ID, m2.ID)
	bindVia(t, d, entry.ID, mid.ID, "kernel")

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 3 {
		t.Fatalf("want 3 hops, got %d", len(hops))
	}
	// 段尾（entry，非末段）走绑定边 kernel；组合段内跳走配置 userspace；末跳走 exit_mode
	if hops[0].Mode != "kernel" || hops[1].Mode != "userspace" || hops[2].Mode != "userspace" {
		t.Fatalf("modes: %s/%s/%s", hops[0].Mode, hops[1].Mode, hops[2].Mode)
	}
	if hops[0].ViaNodeID != entry.ID || hops[1].ViaNodeID != mid.ID || hops[2].ViaNodeID != mid.ID {
		t.Fatalf("provenance: %d/%d/%d", hops[0].ViaNodeID, hops[1].ViaNodeID, hops[2].ViaNodeID)
	}
	if got := rules[0].ViaNodeIDs; len(got) != 1 || got[0] != mid.ID {
		t.Fatalf("rule via persisted wrong: %+v", got)
	}
}

// 组合作为中转（非末段）：组合的末跳保留组合自身配置的模式，而不是绑定边的
// 模式——组合被用作中转时，自己决定其出口段如何转发。作为末段时才由规则的
// exit_mode 覆盖（见 composite_exit_mode_test.go）。
func TestCompositeAsMiddleKeepsConfigMode(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	tailVia, _ := db.CreateNode(d, "tail-via", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, c1.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, c2.ID, "3.3.3.3")
	_ = db.UpdateNodeRelayHost(d, tailVia.ID, "4.4.4.4")
	// 组合内配置模式设为 kernel，好与两条 userspace 绑定边区分开。
	mid := makeCompositeHopMode(t, d, "mid", "kernel", c1.ID, c2.ID)
	bindVia(t, d, entry.ID, mid.ID, "userspace")
	bindVia(t, d, mid.ID, tailVia.ID, "userspace")

	uid, cookie := loginAsUser(t, d, 11)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	_ = db.GrantNode(d, uid, tailVia.ID, 5, 0)

	s, _ := New(d)
	body, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "via_node_ids": []int64{mid.ID, tailVia.ID},
		"name": "r-mid", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "kernel",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	// hop0 entry：绑定边 userspace；hop1 c1：组合配置 kernel；
	// hop2 c2（组合末跳，此处组合是中转）：组合配置 kernel，不取绑定边 userspace；
	// hop3 tailVia（规则末跳）：exit_mode kernel。
	wantModes(t, chainModes(t, s, rules[0].ID), "userspace", "kernel", "kernel", "kernel")
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if hops[0].ViaNodeID != entry.ID || hops[1].ViaNodeID != mid.ID || hops[2].ViaNodeID != mid.ID || hops[3].ViaNodeID != tailVia.ID {
		t.Fatalf("provenance: %d/%d/%d/%d", hops[0].ViaNodeID, hops[1].ViaNodeID, hops[2].ViaNodeID, hops[3].ViaNodeID)
	}
}

// 服务端权威校验：无绑定边 / 无 via 角色 / 无授权 都必须拒绝。
func TestViaChainValidation(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "mid", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, mid.ID, "2.2.2.2")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	s, _ := New(d)

	// 有授权、有角色，但无绑定边 → 400
	n, _ := db.GetNode(d, mid.ID)
	_ = db.UpdateNodeRoles(d, mid.ID, n.Roles|db.NodeRoleVia)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-noedge"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no edge: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	// 有边但摘掉角色 → 400
	bindVia(t, d, entry.ID, mid.ID, "userspace")
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-norole"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no role: want 400, got %d", rec.Code)
	}
	// 有边有角色但撤销授权 → 403
	_ = db.UpdateNodeRoles(d, mid.ID, db.NodeRoleEntry|db.NodeRoleVia)
	_ = db.RevokeNode(d, uid, mid.ID)
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-nogrant"); rec.Code != http.StatusForbidden {
		t.Fatalf("no grant: want 403, got %d", rec.Code)
	}
}

// 组合入口的子节点与 via 引用同一物理节点时，链路会展开出重复的物理跳；
// 这是配置冲突而非请求格式错误，必须是 409。
func TestViaChainDuplicatePhysicalNodeConflict(t *testing.T) {
	d := openDB(t)
	x, _ := db.CreateNode(d, "x", "", "")
	y, _ := db.CreateNode(d, "y", "", "")
	_ = db.UpdateNodeRelayHost(d, x.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, y.ID, "2.2.2.2")
	entry := makeComposite(t, d, "entry-comp", x.ID, y.ID)
	bindVia(t, d, entry.ID, x.ID, "userspace")

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, x.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{x.ID}, "dup-node")
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

// 服务端权威校验：仅持有 via 角色（无 entry 角色）的节点不能被当作入口，
// 即便用户已获得该节点的授权——UI 的入口选择器过滤不能替代服务端校验。
func TestEntryRoleEnforced(t *testing.T) {
	d := openDB(t)
	viaOnly, _ := db.CreateNode(d, "via-only", "", "")
	_ = db.UpdateNodeRelayHost(d, viaOnly.ID, "1.1.1.1")
	_ = db.UpdateNodeRoles(d, viaOnly.ID, db.NodeRoleVia)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, viaOnly.ID, 5, 0)

	s, _ := New(d)
	rec := createMyRuleVia(t, s, cookie, viaOnly.ID, nil, "r-entry-role")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("via-only as entry: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// 编辑防降级：不带 via_node_ids 的编辑保留原路径。
func TestEditWithoutViaFieldKeepsPath(t *testing.T) {
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
	rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)

	body, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "name": "r1-renamed", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req := httptest.NewRequest("PUT", "/api/my/rules/"+strconv.FormatInt(rules[0].ID, 10), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 2 {
		t.Fatalf("via silently dropped on edit: %d hops", len(hops))
	}
	// 显式清空 via（送空数组）则回到单段
	body2, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "via_node_ids": []int64{}, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req2 := httptest.NewRequest("PUT", "/api/my/rules/"+strconv.FormatInt(rules[0].ID, 10), bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	s.Router().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("clear via: %d %s", w2.Code, w2.Body.String())
	}
	if hops, _ = db.ListRuleHops(d, rules[0].ID); len(hops) != 1 {
		t.Fatalf("explicit empty via must clear layers: %d hops", len(hops))
	}
}
