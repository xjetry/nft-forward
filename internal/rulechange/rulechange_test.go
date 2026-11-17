package rulechange

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"nft-forward/internal/chain"
	"nft-forward/internal/db"
)

// 本文件钉的是“谁、对哪条规则、能做什么”的细则，直接调 Changes。各入口（HTTP、agent
// 通道）是否都走到这里，由 internal/server 的测试从入口钉。

type fixture struct {
	t *testing.T
	d *sql.DB
	c *Changes
	n int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &fixture{t: t, d: d, c: New(d)}
}

func (f *fixture) user(role string, maxRules int) *db.User {
	f.t.Helper()
	f.n++
	id, err := db.CreateUser(f.d, fmt.Sprintf("u%d", f.n), "hash", role)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.d.Exec(`UPDATE users SET max_rules=? WHERE id=?`, maxRules, id); err != nil {
		f.t.Fatal(err)
	}
	return f.reload(id)
}

func (f *fixture) reload(id int64) *db.User {
	f.t.Helper()
	u, err := db.GetUserByID(f.d, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// node 建一台有中继地址的物理节点。
func (f *fixture) node(name string) int64 {
	f.t.Helper()
	f.n++
	n, err := db.CreateNode(f.d, name, "", "")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := db.UpdateNodeRelayHost(f.d, n.ID, fmt.Sprintf("10.0.0.%d", f.n)); err != nil {
		f.t.Fatal(err)
	}
	return n.ID
}

func (f *fixture) grant(u *db.User, nodeID int64, maxRules int) {
	f.t.Helper()
	if err := db.GrantNode(f.d, u.ID, nodeID, maxRules, 0); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.d.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func fields(name string) Fields {
	return Fields{Name: name, Proto: "tcp", Exit: "198.51.100.9:443"}
}

// refused 断言 cerr 是类别为 kind 的拒绝。
func refused(t *testing.T, what string, cerr *Error, kind Kind) {
	t.Helper()
	if cerr == nil {
		t.Fatalf("%s：应被拒绝，实际成功", what)
	}
	if cerr.Kind != kind {
		t.Fatalf("%s：拒绝类别应为 %d，实际 %d（%s）", what, kind, cerr.Kind, cerr.Msg)
	}
}

func mustCreate(t *testing.T, c *Changes, actor Actor, name string, entry int64) *db.Rule {
	t.Helper()
	res, cerr := c.Create(actor, fields(name), Path{Entry: entry}, false)
	if cerr != nil {
		t.Fatalf("建规则 %s 被拒: %s", name, cerr.Msg)
	}
	return res.Rule
}

// 预演把整条路径走完但什么都不留下，并且受与真建相同的上限约束。
func TestDryRunLeavesNothingBehindAndObeysCaps(t *testing.T) {
	f := newFixture(t)
	u := f.user("user", 1)
	n := f.node("edge")
	f.grant(u, n, 5)
	actor := Actor{User: u}

	res, cerr := f.c.Create(actor, fields("r"), Path{Entry: n}, true)
	if cerr != nil {
		t.Fatalf("预演被拒: %s", cerr.Msg)
	}
	if res.Rule != nil || res.Entry == "" || len(res.Hops) != 1 {
		t.Fatalf("预演应给出入口与物理链、不给规则，实际 %+v", res)
	}
	if n := f.count(`SELECT COUNT(*) FROM rules`) + f.count(`SELECT COUNT(*) FROM rule_hops`) + f.count(`SELECT COUNT(*) FROM audit_logs`); n != 0 {
		t.Fatalf("预演不应留下规则、跳或审计，实际共 %d 行", n)
	}

	mustCreate(t, f.c, actor, "real", n)
	_, cerr = f.c.Create(actor, fields("over"), Path{Entry: n}, true)
	refused(t, "到了上限之后的预演", cerr, Conflict)
}

// 两个上限数的都是规则条数；一批接管的规则合起来算，超了就整批不落库；管理员没有
// 上限。
func TestCapsCountRulesAndABatchCountsAsAWhole(t *testing.T) {
	f := newFixture(t)
	n := f.node("edge")
	locals := func(k int) []Local {
		out := make([]Local, k)
		for i := range out {
			out[i] = Local{Fields: Fields{Proto: "tcp", Exit: fmt.Sprintf("198.51.100.9:%d", 80+i)}, ListenPort: 12000 + i}
		}
		return out
	}

	// 账号上限 2，授权上限充足。
	byAccount := f.user("user", 2)
	f.grant(byAccount, n, 50)
	_, cerr := f.c.Adopt(Actor{User: byAccount, Node: n}, locals(3))
	refused(t, "一批 3 条、账号上限 2", cerr, Conflict)
	if got := f.count(`SELECT COUNT(*) FROM rules WHERE owner_id=?`, byAccount.ID); got != 0 {
		t.Fatalf("被拒的批不应落库任何一条，实际 %d 条", got)
	}
	if _, cerr := f.c.Adopt(Actor{User: byAccount, Node: n}, locals(2)); cerr != nil {
		t.Fatalf("一批 2 条在账号上限之内，应成功: %s", cerr.Msg)
	}

	// 账号上限充足，授权上限 2。
	byGrant := f.user("user", 50)
	f.grant(byGrant, n, 2)
	_, cerr = f.c.Adopt(Actor{User: byGrant, Node: n}, []Local{
		{Fields: Fields{Proto: "tcp", Exit: "198.51.100.9:90"}, ListenPort: 12100},
		{Fields: Fields{Proto: "tcp", Exit: "198.51.100.9:91"}, ListenPort: 12101},
		{Fields: Fields{Proto: "tcp", Exit: "198.51.100.9:92"}, ListenPort: 12102},
	})
	refused(t, "一批 3 条、授权上限 2", cerr, Conflict)

	// 管理员：账号的 max_rules 是 0，也没有授权，照样能建。
	admin := f.user("admin", 0)
	for i := 0; i < 3; i++ {
		mustCreate(t, f.c, Actor{User: admin}, fmt.Sprintf("a%d", i), n)
	}
}

// 改规则换入口时，这条规则开始占新入口那条授权的名额；不换入口不受上限影响，哪怕
// 上限后来被调到了现有条数以下。
func TestMovingARuleCountsAgainstTheNewEntrysGrant(t *testing.T) {
	f := newFixture(t)
	u := f.user("user", 10)
	n1, n2 := f.node("n1"), f.node("n2")
	f.grant(u, n1, 5)
	f.grant(u, n2, 1)
	actor := Actor{User: u}
	onN1 := mustCreate(t, f.c, actor, "on-n1", n1)
	mustCreate(t, f.c, actor, "on-n2", n2)

	_, cerr := f.c.Update(actor, onN1.ID, fields("on-n1"), Path{Entry: n2})
	refused(t, "换到名额已满的入口", cerr, Conflict)

	if _, err := f.d.Exec(`UPDATE users SET max_rules=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	f.grant(u, n1, 0)
	actor.User = f.reload(u.ID)
	if _, cerr := f.c.Update(actor, onN1.ID, fields("renamed"), Path{}); cerr != nil {
		t.Fatalf("不换入口的编辑不应被上限拦住: %s", cerr.Msg)
	}
}

// 直接指定物理跳、重新分配端口只属于面板上的管理员；改一跳、交出本地规则只属于节点。
func TestOperationsReservedForOneKindOfActor(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin", 0)
	u := f.user("user", 10)
	n1, n2 := f.node("n1"), f.node("n2")
	f.grant(u, n1, 5)
	hops := []chain.Hop{{NodeID: n1, Mode: "kernel"}, {NodeID: n2, Mode: "kernel"}}
	rl := mustCreate(t, f.c, Actor{User: u}, "r", n1)

	_, cerr := f.c.Create(Actor{User: u}, fields("x"), Path{Hops: hops}, false)
	refused(t, "普通用户直接指定物理跳建规则", cerr, Forbidden)
	_, cerr = f.c.Update(Actor{User: u}, rl.ID, fields("r"), Path{Hops: hops})
	refused(t, "普通用户直接指定物理跳改规则", cerr, Forbidden)
	_, cerr = f.c.Create(Actor{User: admin, Node: n1}, fields("x"), Path{Entry: n1, Hops: hops}, false)
	refused(t, "节点以管理员属主的名义直接指定物理跳", cerr, Forbidden)
	if _, cerr := f.c.Create(Actor{User: admin}, fields("explicit"), Path{Hops: hops}, false); cerr != nil {
		t.Fatalf("面板上的管理员直接指定物理跳应成功: %s", cerr.Msg)
	}

	_, cerr = f.c.ReallocateHop(Actor{User: u}, rl.ID, 0, 0)
	refused(t, "普通用户重新分配端口", cerr, Forbidden)
	_, cerr = f.c.ReallocateHop(Actor{User: admin, Node: n1}, rl.ID, 0, 0)
	refused(t, "节点重新分配端口", cerr, Forbidden)
	if _, cerr := f.c.ReallocateHop(Actor{User: admin}, rl.ID, 0, 0); cerr != nil {
		t.Fatalf("面板上的管理员重新分配端口应成功: %s", cerr.Msg)
	}
	_, cerr = f.c.ReallocateHop(Actor{User: admin}, rl.ID, 3, 0)
	refused(t, "跳序号越界", cerr, Invalid)

	_, cerr = f.c.EditHop(Actor{User: admin}, rl.ID, HopEdit{ListenPort: 12345})
	refused(t, "不是节点的发起方改一跳", cerr, Forbidden)
	_, cerr = f.c.Adopt(Actor{User: admin}, []Local{{Fields: fields("x")}})
	refused(t, "不是节点的发起方交出本地规则", cerr, Forbidden)

	// 节点的规则只住在它自己身上。属主对 n2 也有授权，所以下面拒绝的只能是这一条。
	f.grant(u, n2, 5)
	_, cerr = f.c.Create(Actor{User: admin, Node: n1}, fields("x"), Path{Entry: n2}, false)
	refused(t, "节点在别的节点上建规则", cerr, Forbidden)
	_, cerr = f.c.Update(Actor{User: u, Node: n1}, rl.ID, fields("r"), Path{Entry: n2})
	refused(t, "节点改规则时换入口", cerr, Forbidden)
}

// 被停用或已到期的账号不能建规则、改规则，但随时可以删；面板上的管理员不受这一条
// 约束。
func TestAccountStateGatesChangesButNotDeletion(t *testing.T) {
	for _, state := range []string{"disabled", "expired"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			n := f.node("edge")
			lapse := func(u *db.User) *db.User {
				if state == "disabled" {
					if _, err := f.d.Exec(`UPDATE users SET disabled=1 WHERE id=?`, u.ID); err != nil {
						t.Fatal(err)
					}
				} else if _, err := f.d.Exec(`UPDATE users SET expires_at=? WHERE id=?`, time.Now().Add(-time.Hour).Unix(), u.ID); err != nil {
					t.Fatal(err)
				}
				return f.reload(u.ID)
			}

			u := f.user("user", 10)
			f.grant(u, n, 5)
			rl := mustCreate(t, f.c, Actor{User: u}, "r", n)
			u = lapse(u)
			_, cerr := f.c.Create(Actor{User: u}, fields("x"), Path{Entry: n}, false)
			refused(t, "建规则", cerr, Forbidden)
			_, cerr = f.c.Update(Actor{User: u}, rl.ID, fields("r2"), Path{})
			refused(t, "改规则", cerr, Forbidden)
			if _, cerr := f.c.Delete(Actor{User: u}, rl.ID); cerr != nil {
				t.Fatalf("删规则任何时候都允许: %s", cerr.Msg)
			}

			admin := lapse(f.user("admin", 0))
			if _, cerr := f.c.Create(Actor{User: admin}, fields("a"), Path{Entry: n}, false); cerr != nil {
				t.Fatalf("面板上的管理员不受账号状态约束: %s", cerr.Msg)
			}
			// 同一个管理员账号，命令来自节点时受约束。
			_, cerr = f.c.Create(Actor{User: admin, Node: n}, fields("b"), Path{Entry: n}, false)
			refused(t, "节点以这个账号的名义建规则", cerr, Forbidden)
		})
	}
}

// 没有属主的节点不能建规则、改规则、交出本地规则，但仍能改自己那一跳、删经过自己
// 的规则；这两种变更的审计不记在任何账号名下。
func TestOwnerlessNodeCanStillEditItsHopAndDelete(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin", 0)
	n := f.node("edge")
	rl := mustCreate(t, f.c, Actor{User: admin}, "r", n)

	actor, cerr := f.c.NodeActor(n)
	if cerr != nil {
		t.Fatal(cerr.Msg)
	}
	if actor.User != nil || actor.Node != n {
		t.Fatalf("没有属主的节点：发起方应只有节点，实际 %+v", actor)
	}
	_, cerr = f.c.Create(actor, fields("x"), Path{Entry: n}, false)
	refused(t, "建规则", cerr, Forbidden)
	_, cerr = f.c.Update(actor, rl.ID, fields("x"), Path{})
	refused(t, "改规则", cerr, Forbidden)
	_, cerr = f.c.Adopt(actor, []Local{{Fields: fields("x")}})
	refused(t, "交出本地规则", cerr, Forbidden)

	res, cerr := f.c.EditHop(actor, rl.ID, HopEdit{ListenPort: 12345, Mode: "kernel", Comment: "c"})
	if cerr != nil {
		t.Fatalf("改自己那一跳应成功: %s", cerr.Msg)
	}
	if res.Entry == "" || len(res.Affected) == 0 {
		t.Fatalf("改一跳应给出入口地址与要重推的节点，实际 %+v", res)
	}
	if _, cerr := f.c.Delete(actor, rl.ID); cerr != nil {
		t.Fatalf("删经过自己的规则应成功: %s", cerr.Msg)
	}
	for _, action := range []string{"rule.node_hop_edit", "rule.node_delete"} {
		if got := f.count(`SELECT COUNT(*) FROM audit_logs WHERE action=? AND user_id IS NULL AND payload=?`, action, fmt.Sprintf("node=%d", n)); got != 1 {
			t.Errorf("%s 应有 1 条不记账号、注明节点的审计，实际 %d", action, got)
		}
	}

	if _, cerr := f.c.NodeActor(n + 100); cerr == nil || cerr.Kind != NotFound {
		t.Fatalf("不存在的节点应报不存在，实际 %+v", cerr)
	}
}

// 审计动作名按发起方分三组。
func TestAuditActionNamesTheKindOfActor(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin", 0)
	u := f.user("user", 10)
	n := f.node("edge")
	f.grant(u, n, 5)
	if err := db.UpdateNodeOwner(f.d, n, &u.ID); err != nil {
		t.Fatal(err)
	}
	node, cerr := f.c.NodeActor(n)
	if cerr != nil {
		t.Fatal(cerr.Msg)
	}
	if node.User == nil || node.User.ID != u.ID {
		t.Fatalf("节点的发起方应以属主 %d 的名义，实际 %+v", u.ID, node.User)
	}

	byAdmin := mustCreate(t, f.c, Actor{User: admin}, "a", n)
	byUser := mustCreate(t, f.c, Actor{User: u}, "u", n)
	byNode := mustCreate(t, f.c, node, "", n)
	for action, want := range map[string][2]int64{
		"rule.create":      {admin.ID, byAdmin.ID},
		"rule.user_create": {u.ID, byUser.ID},
		"rule.node_create": {u.ID, byNode.ID},
	} {
		if got := f.count(`SELECT COUNT(*) FROM audit_logs WHERE action=? AND user_id=? AND target=?`, action, want[0], fmt.Sprint(want[1])); got != 1 {
			t.Errorf("%s 应有 1 条记在账号 %d 名下、指向规则 %d 的审计，实际 %d", action, want[0], want[1], got)
		}
	}
	if got := f.count(`SELECT COUNT(*) FROM audit_logs`); got != 3 {
		t.Errorf("三次建规则应恰有 3 条审计，实际 %d", got)
	}
}
