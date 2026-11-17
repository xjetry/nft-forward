package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// 规则的写入有三种发起方（管理员、普通用户、节点），经四个入口进来（/api、/api/my、
// /api/v1、agent 通道）。本文件从这些入口钉住“谁、对哪条规则、能做什么”在各入口
// 上是同一套判定；判定本身的细则见 internal/rulechange 的测试。

func firstAdminID(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`SELECT id FROM users WHERE role='admin' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ownedNode 建一台属主为 ownerID 的远程节点并让它的 agent 接入。v4 是它上报的出口
// 地址，面板据此回填数据面地址；不同节点要给不同的值。
func ownedNode(t *testing.T, e *localAgentEnv, name string, ownerID int64, v4 string) (*db.Node, *fakeAgent) {
	t.Helper()
	n, err := db.CreateNode(e.d, name, "", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeOwner(e.d, n.ID, &ownerID); err != nil {
		t.Fatal(err)
	}
	a := connectFakeAgent(t, wsURL(e.public), wsproto.Hello{NodeToken: "tok-" + name, OS: "linux", Arch: "amd64", ProbedV4: v4}, nil)
	return n, a
}

func auditCount(t *testing.T, d *sql.DB, action string, ruleID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action=? AND target=?`, action, fmt.Sprint(ruleID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func onlyRuleOf(t *testing.T, d *sql.DB, ownerID int64) *db.Rule {
	t.Helper()
	rules, err := db.ListRulesByUser(d, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("应恰有 1 条规则，实际 %d 条", len(rules))
	}
	return rules[0]
}

// users.max_forwards 数的是规则条数：一条规则占一个名额，不论它的链有几跳。
func TestMaxForwardsCountsRulesNotHops(t *testing.T) {
	d := openDB(t)
	var members []int64
	for i, name := range []string{"a", "b", "c"} {
		n, _ := db.CreateNode(d, name, "", "")
		_ = db.UpdateNodeRelayHost(d, n.ID, fmt.Sprintf("10.0.0.%d", i+1))
		members = append(members, n.ID)
	}
	comp := makeComposite(t, d, "comp", members...)
	single, _ := db.CreateNode(d, "single", "", "")
	_ = db.UpdateNodeRelayHost(d, single.ID, "10.0.0.9")
	uid, cookie := loginAsUser(t, d, 2)
	_ = db.GrantNode(d, uid, comp.ID, 10, 0)
	_ = db.GrantNode(d, uid, single.ID, 10, 0)
	s, _ := New(d)

	// 三跳的链也只占一个名额：上限 2 时能建两条。
	if rec := createMyRule(t, s, cookie, comp.ID, "r1"); rec.Code != http.StatusOK {
		t.Fatalf("第一条（三跳）应能建，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := createMyRule(t, s, cookie, single.ID, "r2"); rec.Code != http.StatusOK {
		t.Fatalf("第二条应能建，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec := createMyRule(t, s, cookie, single.ID, "r3")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "超出用户最大转发数") {
		t.Fatalf("第三条应因条数上限被拒，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 已到上限时把一条单跳规则换到三跳的组合上：条数没变，不受上限影响。
	rules, _ := db.ListRulesByUser(d, uid)
	var r2 int64
	for _, rl := range rules {
		if rl.Name == "r2" {
			r2 = rl.ID
		}
	}
	rec = userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", r2), map[string]any{
		"node_id": comp.ID, "name": "r2", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("换到更长的链不增加条数，应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 不点名入口与中转层的编辑沿用规则落库的链，对普通用户与管理员是同一个规则：组合
// 的成员后来变了，改个名字不会把规则悄悄换到新成员上。
func TestHeaderOnlyEditByAUserKeepsTheStoredChain(t *testing.T) {
	d := openDB(t)
	ids := map[string]int64{}
	for i, name := range []string{"a", "b", "c"} {
		n, _ := db.CreateNode(d, name, "", "")
		_ = db.UpdateNodeRelayHost(d, n.ID, fmt.Sprintf("10.0.0.%d", i+1))
		ids[name] = n.ID
	}
	comp := makeComposite(t, d, "comp", ids["a"], ids["b"])
	uid, cookie := loginAsUser(t, d, 5)
	_ = db.GrantNode(d, uid, comp.ID, 5, 0)
	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, comp.ID, "r"); rec.Code != http.StatusOK {
		t.Fatalf("建规则 %d %s", rec.Code, rec.Body.String())
	}
	rl := onlyRuleOf(t, d, uid)

	// 组合的成员换成 a → c。
	if _, err := d.Exec(`DELETE FROM node_hops WHERE node_id=?`, comp.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNodeHops(d, comp.ID, []db.NodeHop{
		{NodeID: comp.ID, Position: 0, HopNodeID: ids["a"], Mode: "userspace"},
		{NodeID: comp.ID, Position: 1, HopNodeID: ids["c"], Mode: "userspace"},
	}); err != nil {
		t.Fatal(err)
	}

	chainOf := func() []int64 {
		hops, _ := db.ListRuleHops(d, rl.ID)
		var out []int64
		for _, h := range hops {
			out = append(out, h.NodeID)
		}
		return out
	}
	rec := userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", rl.ID), map[string]any{
		"name": "renamed", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("改名 %d %s", rec.Code, rec.Body.String())
	}
	if got := chainOf(); len(got) != 2 || got[1] != ids["b"] {
		t.Fatalf("只改表头应沿用落库的链 a → b，实际 %v", got)
	}

	// 点名了入口才按当前的组合重新推导。
	rec = userJSON(t, s, cookie, "PUT", fmt.Sprintf("/api/my/rules/%d", rl.ID), map[string]any{
		"node_id": comp.ID, "name": "renamed", "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("重新推导 %d %s", rec.Code, rec.Body.String())
	}
	if got := chainOf(); len(got) != 2 || got[1] != ids["c"] {
		t.Fatalf("点名入口后应按当前成员推导成 a → c，实际 %v", got)
	}
}

// 节点侧建规则与面板上建规则过同一套校验，并留下审计记录；节点的表单没有规则名这
// 一栏，规则名由面板补上，不留空。
func TestNodeSideCreateIsValidatedNamedAndAudited(t *testing.T) {
	e := newLocalAgentEnv(t)
	adminID := firstAdminID(t, e.d)
	_, a := ownedNode(t, e, "edge", adminID, "203.0.113.8")

	for _, bad := range []wsproto.RuleCreate{
		{Proto: "tcp", ExitHost: "bad host!", ExitPort: 80, ListenPort: 12011},
		{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 0, ListenPort: 12011},
		{Proto: "icmp", ExitHost: "198.51.100.9", ExitPort: 80, ListenPort: 12011},
	} {
		if ack := a.ruleCmd(t, wsproto.TypeRuleCreate, bad); ack.OK || ack.Error == "" {
			t.Fatalf("不合法的规则 %+v 应被拒绝并说明原因，实际 %+v", bad, ack)
		}
	}
	if n, _ := db.CountRulesForUser(e.d, adminID); n != 0 {
		t.Fatalf("被拒的请求不应落库，实际 %d 条", n)
	}

	ack := a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 80, ListenPort: 12011})
	if !ack.OK {
		t.Fatalf("合法的规则应能建，实际被拒: %s", ack.Error)
	}
	rl := onlyRuleOf(t, e.d, adminID)
	if rl.Name != "198.51.100.9:80" {
		t.Errorf("没给名字也没给备注时规则名取出口地址，实际 %q", rl.Name)
	}
	if n := auditCount(t, e.d, "rule.node_create", rl.ID); n != 1 {
		t.Errorf("节点侧建规则应留下 1 条审计记录，实际 %d", n)
	}

	ack = a.ruleCmd(t, wsproto.TypeRuleUpdate, wsproto.RuleUpdate{RuleID: rl.ID, Proto: "udp", ExitHost: "198.51.100.9", ExitPort: 53, ListenPort: 12011})
	if !ack.OK {
		t.Fatalf("节点侧改自己的规则应成功，实际被拒: %s", ack.Error)
	}
	if n := auditCount(t, e.d, "rule.node_save", rl.ID); n != 1 {
		t.Errorf("节点侧改规则应留下 1 条审计记录，实际 %d", n)
	}
	if ack = a.ruleCmd(t, wsproto.TypeRuleUpdate, wsproto.RuleUpdate{RuleID: rl.ID, Proto: "tcp", ExitHost: "", ExitPort: 53}); ack.OK {
		t.Fatal("节点侧改规则时出口地址为空应被拒绝")
	}

	ack = a.ruleCmd(t, wsproto.TypeRuleDelete, wsproto.RuleDelete{RuleID: rl.ID})
	if !ack.OK {
		t.Fatalf("节点侧删规则应成功，实际被拒: %s", ack.Error)
	}
	if n := auditCount(t, e.d, "rule.node_delete", rl.ID); n != 1 {
		t.Errorf("节点侧删规则应留下 1 条审计记录，实际 %d", n)
	}
}

// 节点交出本地规则时，每条都过建规则的校验、都有规则名、都留审计；一条不合法则整批
// 不落库。
func TestAdoptedLocalRulesAreValidatedNamedAndAudited(t *testing.T) {
	e := newLocalAgentEnv(t)
	adminID := firstAdminID(t, e.d)
	_, a := ownedNode(t, e, "edge", adminID, "203.0.113.8")

	ack := a.ruleCmd(t, wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: []nft.Rule{
		{ID: "a1", Proto: "tcp", SrcPort: 12011, DestIP: "198.51.100.9", DestPort: 80},
		{ID: "a2", Proto: "tcp", SrcPort: 12012, DestIP: "198.51.100.9", DestPort: 0},
	}})
	if ack.OK {
		t.Fatal("批里有一条目标端口非法，整批应被拒绝")
	}
	if n, _ := db.CountRulesForUser(e.d, adminID); n != 0 {
		t.Fatalf("被拒的批不应落库任何规则，实际 %d 条", n)
	}

	ack = a.ruleCmd(t, wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: []nft.Rule{
		{ID: "a1", Proto: "tcp", SrcPort: 12011, DestIP: "198.51.100.9", DestPort: 80},
		{ID: "a2", Proto: "udp", SrcPort: 12012, DestHost: "dns.example.com", DestPort: 53, Comment: "解析"},
	}})
	if !ack.OK {
		t.Fatalf("合法的批应被接管，实际被拒: %s", ack.Error)
	}
	rules, _ := db.ListRulesByUser(e.d, adminID)
	if len(rules) != 2 {
		t.Fatalf("应接管 2 条，实际 %d 条", len(rules))
	}
	names := map[string]bool{}
	for _, rl := range rules {
		names[rl.Name] = true
		if n := auditCount(t, e.d, "rule.node_adopt", rl.ID); n != 1 {
			t.Errorf("接管规则 %d 应留下 1 条审计记录，实际 %d", rl.ID, n)
		}
	}
	if !names["198.51.100.9:80"] || !names["解析"] {
		t.Errorf("规则名应取备注、没有备注取出口地址，实际 %v", names)
	}
}

// 节点属主只说明节点侧的命令以谁的名义执行，不代替授权：属主是普通用户时，他得持有
// 这台节点的授权，并受这条授权的条数上限约束——与他在面板上建规则完全一样。
func TestNodeSideCommandsNeedTheOwnersGrant(t *testing.T) {
	e := newLocalAgentEnv(t)
	uid, _ := loginAsUser(t, e.d, 5)
	node, a := ownedNode(t, e, "user-edge", uid, "203.0.113.8")
	create := func(port int) wsproto.RuleCmdAck {
		return a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: port, ListenPort: 12000 + port})
	}

	if ack := create(80); ack.OK || !strings.Contains(ack.Error, "无权使用该节点") {
		t.Fatalf("属主没有授权时应被拒绝并说明原因，实际 %+v", ack)
	}
	if err := db.GrantNode(e.d, uid, node.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	if ack := create(80); !ack.OK {
		t.Fatalf("有授权后应能建，实际被拒: %s", ack.Error)
	}
	if ack := create(81); ack.OK || !strings.Contains(ack.Error, "超出该节点的转发上限") {
		t.Fatalf("超出授权的条数上限应被拒绝，实际 %+v", ack)
	}
}

// 节点只能改住在自己身上的规则：同一个属主名下另一台节点的规则，它动不了，被拒绝的
// 命令也不会把规则挪到它身上。
func TestNodeSideUpdateOnlyReachesRulesOnThatNode(t *testing.T) {
	e := newLocalAgentEnv(t)
	adminID := firstAdminID(t, e.d)
	_, a1 := ownedNode(t, e, "edge-1", adminID, "203.0.113.8")
	n2, a2 := ownedNode(t, e, "edge-2", adminID, "203.0.113.9")

	if ack := a2.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 80, ListenPort: 12011}); !ack.OK {
		t.Fatalf("在 edge-2 上建规则被拒: %s", ack.Error)
	}
	rl := onlyRuleOf(t, e.d, adminID)

	ack := a1.ruleCmd(t, wsproto.TypeRuleUpdate, wsproto.RuleUpdate{RuleID: rl.ID, Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 81, ListenPort: 12011})
	if ack.OK || !strings.Contains(ack.Error, "节点不在该规则上") {
		t.Fatalf("edge-1 改 edge-2 上的规则应被拒绝，实际 %+v", ack)
	}
	hops, _ := db.ListRuleHops(e.d, rl.ID)
	if len(hops) != 1 || hops[0].NodeID != n2.ID || hops[0].TargetPort != 80 {
		t.Fatalf("被拒的命令不应改动规则，实际 %+v", hops[0])
	}
}

// 节点侧建规则走与面板相同的链路推导：管理员禁止直接转发的节点、不是入口的节点，
// 从节点侧同样建不出规则。
func TestNodeSideCreateObeysTheNodesSeats(t *testing.T) {
	e := newLocalAgentEnv(t)
	adminID := firstAdminID(t, e.d)
	node, a := ownedNode(t, e, "edge", adminID, "203.0.113.8")
	create := func() wsproto.RuleCmdAck {
		return a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 80, ListenPort: 12011})
	}

	if err := db.UpdateNodeNoDirectExit(e.d, node.ID, true); err != nil {
		t.Fatal(err)
	}
	if ack := create(); ack.OK || !strings.Contains(ack.Error, "禁止直接转发") {
		t.Fatalf("禁止直接转发的节点从节点侧也不能建单跳规则，实际 %+v", ack)
	}
	_ = db.UpdateNodeNoDirectExit(e.d, node.ID, false)
	if err := db.UpdateNodeRoles(e.d, node.ID, db.NodeRoleVia); err != nil {
		t.Fatal(err)
	}
	if ack := create(); ack.OK || !strings.Contains(ack.Error, "不是入口") {
		t.Fatalf("不是入口的节点从节点侧也不能建规则，实际 %+v", ack)
	}
	_ = db.UpdateNodeRoles(e.d, node.ID, db.NodeRoleEntry|db.NodeRoleVia)
	if ack := create(); !ack.OK {
		t.Fatalf("恢复之后应能建，实际被拒: %s", ack.Error)
	}
}

// 经某个节点探测与把它用进规则是同一个问题：管理员可以，其余人要有授权；只是节点的
// 属主不算。
func TestProbeThroughANodeNeedsAGrant(t *testing.T) {
	e := newLocalAgentEnv(t)
	uid, cookie := loginAsUser(t, e.d, 5)
	node, _ := ownedNode(t, e, "user-edge", uid, "203.0.113.8")
	status := func() int {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/probe?target=9.9.9.9:80&node=%d", e.public.URL, node.ID), nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(); got != http.StatusForbidden {
		t.Fatalf("只是属主、没有授权时探测应 403，实际 %d", got)
	}
	if err := db.GrantNode(e.d, uid, node.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != http.StatusOK {
		t.Fatalf("有授权后探测应放行，实际 %d", got)
	}
}
