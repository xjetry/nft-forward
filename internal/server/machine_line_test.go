package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

func createCompositeOverHTTP(t *testing.T, s *Server, admin *http.Cookie, name string, members ...int64) int64 {
	t.Helper()
	hops := make([]map[string]any, len(members))
	for i, m := range members {
		hops[i] = map[string]any{"node_id": m, "mode": "kernel"}
	}
	rec := adminRequest(t, s, admin, "POST", "/api/nodes", map[string]any{"name": name, "node_type": "composite", "hops": hops})
	if rec.Code != http.StatusOK {
		t.Fatalf("建组合 %s：%d %s", name, rec.Code, rec.Body.String())
	}
	var resp struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	decodeInto(t, rec, &resp)
	return resp.Node.ID
}

func nodeCount(t *testing.T, d *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 建组合是一个整体：节点行、它的类型、它的成员要么都写进去，要么都没有。这里让写成员
// 那一步在库里必然失败（成员行一插入就中止），请求失败之后不应留下没有成员的组合。
func TestCreatingACompositeWritesNodeAndMembersTogether(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	mustExec(t, d, `CREATE TRIGGER refuse_members BEFORE INSERT ON node_hops BEGIN SELECT RAISE(ABORT, 'members refused'); END`)

	rec := adminRequest(t, s, admin, "POST", "/api/nodes", map[string]any{
		"name": "combo", "node_type": "composite",
		"hops": []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "userspace"}},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("写成员失败时请求不应成功：%s", rec.Body.String())
	}
	if n := nodeCount(t, d, "combo"); n != 0 {
		t.Fatalf("写成员失败时不应留下组合节点行，实际 %d 行", n)
	}
}

// 组合不跑 agent：它的行上没有接入凭据、没有端口段、没有属主。
func TestCompositeRowsCarryNoMachineAttributes(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	id := createCompositeOverHTTP(t, s, admin, "combo", a.ID, b.ID)
	n, err := db.GetNode(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if n.NodeType != "composite" || n.Secret != "" || n.PortRange != "" || n.OwnerID != nil {
		t.Fatalf("组合行不应带机器属性：type=%q secret=%q port_range=%q owner=%v", n.NodeType, n.Secret, n.PortRange, n.OwnerID)
	}
}

// 管理员建的物理节点，属主是建它的管理员：节点侧命令以属主的名义执行。
func TestCreatedMachineIsOwnedByItsCreator(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	adminUser, err := db.GetUserByUsername(d, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	rec := adminRequest(t, s, admin, "POST", "/api/nodes", map[string]any{"name": "edge", "secret": "tok-edge"})
	if rec.Code != http.StatusOK {
		t.Fatalf("建节点：%d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Node struct {
			ID      int64  `json:"id"`
			OwnerID *int64 `json:"owner_id"`
		} `json:"node"`
	}
	decodeInto(t, rec, &resp)
	if resp.Node.OwnerID == nil || *resp.Node.OwnerID != adminUser.ID {
		t.Fatalf("响应里的属主应是建它的管理员 %d，实际 %v", adminUser.ID, resp.Node.OwnerID)
	}
	n, _ := db.GetNode(d, resp.Node.ID)
	if n.OwnerID == nil || *n.OwnerID != adminUser.ID {
		t.Fatalf("库里的属主应是建它的管理员 %d，实际 %v", adminUser.ID, n.OwnerID)
	}
}

// 建节点请求里只对另一种节点有意义的字段被拒绝，而不是悄悄丢掉；被拒绝的请求不留下
// 节点行。
func TestNodeCreationRefusesFieldsOfTheOtherKind(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	members := []map[string]any{{"node_id": a.ID, "mode": "kernel"}, {"node_id": b.ID, "mode": "kernel"}}
	cases := []struct {
		name string
		body map[string]any
	}{
		{"组合带端口段", map[string]any{"node_type": "composite", "hops": members, "port_range": "30000-30010"}},
		{"组合带凭据", map[string]any{"node_type": "composite", "hops": members, "secret": "tok"}},
		{"组合带单向计费", map[string]any{"node_type": "composite", "hops": members, "unidirectional": true}},
		{"物理节点带成员", map[string]any{"hops": members}},
		{"认不出的节点类型", map[string]any{"node_type": "self"}},
		{"物理节点端口段非法", map[string]any{"port_range": "not-a-range"}},
	}
	for i, c := range cases {
		name := fmt.Sprintf("refused-%d", i)
		c.body["name"] = name
		rec := adminRequest(t, s, admin, "POST", "/api/nodes", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：应 400，实际 %d %s", c.name, rec.Code, rec.Body.String())
		}
		if n := nodeCount(t, d, name); n != 0 {
			t.Errorf("%s：被拒绝的请求不应留下节点行，实际 %d 行", c.name, n)
		}
	}
}

// 只对机器有意义的写接口拒绝组合，组合行上的机器列一个都不被写入。
func TestMachineOnlyWritesRefuseComposites(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	adminUser, _ := db.GetUserByUsername(d, "admin-test")
	comp := createCompositeOverHTTP(t, s, admin, "combo", a.ID, b.ID)

	writes := []struct {
		path string
		body map[string]any
	}{
		{"relay-host", map[string]any{"relay_host": "1.2.3.4"}},
		{"relay-host-v6", map[string]any{"relay_host_v6": "2001:db8::9"}},
		{"port-range", map[string]any{"port_range": "30000-30010"}},
		{"owner", map[string]any{"owner_id": adminUser.ID}},
		{"unidirectional", map[string]any{"unidirectional": true}},
		{"no-direct-exit", map[string]any{"no_direct_exit": true}},
		{"resync", map[string]any{}},
		{"upgrade", map[string]any{}},
	}
	for _, w := range writes {
		rec := adminRequest(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/%s", comp, w.path), w.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /api/nodes/{组合}/%s：应 400，实际 %d %s", w.path, rec.Code, rec.Body.String())
		}
	}
	_, tok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	if rec := v1Do(t, s, "POST", fmt.Sprintf("/api/v1/nodes/%d/resync", comp), tok, nil); rec.Code != http.StatusBadRequest || v1ErrCode(t, rec) != codeValidation {
		t.Errorf("POST /api/v1/nodes/{组合}/resync：应 400 validation，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 重新下发的共享判定也分清“节点不存在”。
	if rec := v1Do(t, s, "POST", "/api/v1/nodes/999999/resync", tok, nil); rec.Code != http.StatusNotFound || v1ErrCode(t, rec) != codeNotFound {
		t.Errorf("POST /api/v1/nodes/{不存在}/resync：应 404 not_found，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminRequest(t, s, admin, "POST", "/api/nodes/999999/resync", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/nodes/{不存在}/resync：应 404，实际 %d %s", rec.Code, rec.Body.String())
	}

	n, err := db.GetNode(d, comp)
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHost != "" || n.RelayHostV6 != "" || n.PortRange != "" || n.OwnerID != nil || n.Unidirectional || n.NoDirectExit ||
		n.LastError.Valid || n.LastUpgradeAt.Valid || n.LastApplyAt.Valid {
		t.Errorf("组合行上的机器列不应被写入：relay=%q/%q port_range=%q owner=%v uni=%v nde=%v last_error=%v upgrade=%v apply=%v",
			n.RelayHost, n.RelayHostV6, n.PortRange, n.OwnerID, n.Unidirectional, n.NoDirectExit, n.LastError, n.LastUpgradeAt, n.LastApplyAt)
	}
}

// 反方向：成员只属于组合，给机器写成员被拒绝，什么也不写入。成员不足两个时报的也是
// “非组合节点”，而不是成员个数。
func TestMemberWritesRefuseMachines(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	for _, members := range [][]int64{{b.ID, b.ID}, {b.ID}} {
		hops := make([]map[string]any, len(members))
		for i, m := range members {
			hops[i] = map[string]any{"node_id": m}
		}
		rec := adminRequest(t, s, admin, "POST", fmt.Sprintf("/api/nodes/%d/hops", a.ID), map[string]any{"hops": hops})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "非组合节点") {
			t.Errorf("给机器写 %d 个成员：应 400 非组合节点，实际 %d %s", len(members), rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_hops WHERE node_id=?`, a.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("机器上不应写入成员，实际 %d 条", n)
	}
}

// 只有机器能以 agent 身份接入：即使组合行上残留着一个凭据（旧版本建组合时随机生成，
// 迁移会清掉），拿它来接入也认不出节点。
func TestOnlyMachinesAuthenticateAsAgents(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "tok-a")
	b, _ := db.CreateNode(d, "b", "", "tok-b")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	comp := createCompositeOverHTTP(t, s, admin, "combo", a.ID, b.ID)
	mustExec(t, d, `UPDATE nodes SET secret='stale-composite-token' WHERE id=?`, comp)

	if n, err := lookupNodeBySecret(d, "stale-composite-token"); err != nil || n != nil {
		t.Fatalf("组合的残留凭据不应认出节点，实际 %+v err=%v", n, err)
	}
	if n, err := lookupNodeBySecret(d, "tok-a"); err != nil || n == nil || n.ID != a.ID {
		t.Fatalf("机器的凭据应认出它自己，实际 %+v err=%v", n, err)
	}
}
