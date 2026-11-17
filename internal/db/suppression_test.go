package db

import (
	"database/sql"
	"testing"
	"time"
)

// seedChainRule 建一条两跳规则：入口跳在 entry 上，第二跳在 next 上；两跳都记在
// 逻辑节点 via 名下（rule_hops.via_node_id）。返回规则 id 与两跳的 id。
func seedChainRule(t *testing.T, d *sql.DB, uid, entry, next, via int64, port int) (ruleID, entryHop, nextHop int64) {
	t.Helper()
	res, err := d.Exec(`INSERT INTO rules(node_id, owner_id, name, proto, exit_host, exit_port, created_at) VALUES (?,?,?,?,?,?,0)`,
		via, uid, "r", "tcp", "203.0.113.9", 443)
	if err != nil {
		t.Fatal(err)
	}
	ruleID, _ = res.LastInsertId()
	hop := func(pos int, node int64, listen int) int64 {
		res, err := d.Exec(`INSERT INTO rule_hops(rule_id, position, node_id, proto, listen_port, target_host, target_port, via_node_id) VALUES (?,?,?,?,?,?,?,?)`,
			ruleID, pos, node, "tcp", listen, "203.0.113.9", 443, via)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	return ruleID, hop(0, entry, port), hop(1, next, port+1)
}

func pushedHopIDs(t *testing.T, d *sql.DB, nodeID int64) map[int64]bool {
	t.Helper()
	hops, err := ActiveRuleHopsForPush(d, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, h := range hops {
		out[h.ID] = true
	}
	return out
}

// “一条规则此刻转不转发”有两个读者：下发（ActiveRuleHopsForPush 决定哪些跳进节点）
// 和展示（RuleSuppressions 告诉页面哪条规则没生效、为什么）。两者必须是同一个
// 判定：每种压制条件各造一条只命中它的规则，它必须在两个节点上都不下发、并报出
// 这个原因；同节点上另一个用户的对照规则必须照常下发、不报原因。
func TestEachSuppressionClauseWithdrawsTheRuleEverywhereAndNamesTheReason(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	cases := []struct {
		reason SuppressReason
		// arrange 让规则命中这一条压制条件。via 是规则所记的逻辑节点（这里与入口
		// 节点是同一个），next 是第二跳所在的节点。
		arrange func(t *testing.T, d *sql.DB, uid, via, next int64)
	}{
		{SuppressUserDisabled, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `UPDATE users SET disabled=1 WHERE id=?`, uid)
		}},
		{SuppressUserExpired, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `UPDATE users SET expires_at=? WHERE id=?`, past, uid)
		}},
		{SuppressUserQuota, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `UPDATE users SET traffic_quota_bytes=1000, traffic_used_bytes=1000 WHERE id=?`, uid)
		}},
		{SuppressNodeDisabled, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `UPDATE nodes SET disabled=1 WHERE id=?`, next)
		}},
		{SuppressGrantQuota, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `UPDATE user_nodes SET traffic_quota_bytes=1000, traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, via)
		}},
		{SuppressExitQuota, func(t *testing.T, d *sql.DB, uid, via, next int64) {
			mustExec(t, d, `INSERT INTO user_landing_exits(user_id, host, port, present, quota_bytes, used_bytes) VALUES (?,?,?,1,1000,1000)`,
				uid, "203.0.113.9", 443)
		}},
	}
	for _, c := range cases {
		t.Run(string(c.reason), func(t *testing.T) {
			d := openTestDB(t)
			uid := createTestUser(t, d)
			other := createTestUser(t, d)
			entry := createTestNode(t, d, "entry")
			next := createTestNode(t, d, "next")
			grantNode(t, d, uid, entry)
			grantNode(t, d, other, entry)
			ruleID, entryHop, nextHop := seedChainRule(t, d, uid, entry, next, entry, 21001)
			// 对照规则只有一跳、只在入口节点上：命中“第二跳所在节点被停用”的是
			// 被测规则，不是它。
			controlHop := seedRuleWithTraffic(t, d, other, entry, 22001, 0)
			mustExec(t, d, `UPDATE rule_hops SET via_node_id=node_id WHERE id=?`, controlHop)

			if got := pushedHopIDs(t, d, entry); !got[entryHop] || !got[controlHop] {
				t.Fatalf("前置：压制条件成立之前两条规则都应下发到入口节点，实际 %v", got)
			}
			if got := pushedHopIDs(t, d, next); !got[nextHop] {
				t.Fatalf("前置：压制条件成立之前第二跳应下发，实际 %v", got)
			}

			c.arrange(t, d, uid, entry, next)

			onEntry, onNext := pushedHopIDs(t, d, entry), pushedHopIDs(t, d, next)
			if onEntry[entryHop] || onNext[nextHop] {
				t.Errorf("规则命中 %s 后应在每个节点上都不下发，实际 入口节点=%v 第二跳节点=%v", c.reason, onEntry[entryHop], onNext[nextHop])
			}
			if !onEntry[controlHop] {
				t.Errorf("另一个用户的对照规则不应受影响，实际没有下发")
			}
			reasons, err := RuleSuppressions(d)
			if err != nil {
				t.Fatal(err)
			}
			if reasons[ruleID] != c.reason {
				t.Errorf("规则的压制原因应为 %q，实际 %q", c.reason, reasons[ruleID])
			}
			if len(reasons) != 1 {
				t.Errorf("只有被测规则被压制，RuleSuppressions 应只有它一项，实际 %v", reasons)
			}
		})
	}
}

// 没命中任何压制条件的规则：下发，且不出现在 RuleSuppressions 里。
func TestUnsuppressedRuleIsPushedAndHasNoReason(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	entry := createTestNode(t, d, "entry")
	next := createTestNode(t, d, "next")
	grantNode(t, d, uid, entry)
	_, entryHop, nextHop := seedChainRule(t, d, uid, entry, next, entry, 21001)
	// 这些都不构成压制：未来才到期、配额没用完、不限量（配额 0）时已用再多。
	mustExec(t, d, `UPDATE users SET expires_at=?, traffic_quota_bytes=1000, traffic_used_bytes=999 WHERE id=?`, time.Now().Add(time.Hour).Unix(), uid)
	mustExec(t, d, `UPDATE user_nodes SET traffic_quota_bytes=0, traffic_used_bytes=999999 WHERE user_id=?`, uid)

	if !pushedHopIDs(t, d, entry)[entryHop] || !pushedHopIDs(t, d, next)[nextHop] {
		t.Fatal("没有命中任何压制条件的规则应下发到它的每个节点")
	}
	reasons, err := RuleSuppressions(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 0 {
		t.Fatalf("没有规则被压制，RuleSuppressions 应为空，实际 %v", reasons)
	}
}

// 一条规则同时命中多条压制条件时，报出的是最靠前的那一条（账号层面的先于节点与
// 配额层面的）：解除它之后，下一条才成为报出的原因。
func TestSuppressionReportsTheFirstApplicableReason(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	entry := createTestNode(t, d, "entry")
	next := createTestNode(t, d, "next")
	grantNode(t, d, uid, entry)
	ruleID, _, _ := seedChainRule(t, d, uid, entry, next, entry, 21001)
	mustExec(t, d, `UPDATE users SET disabled=1, traffic_quota_bytes=1000, traffic_used_bytes=2000 WHERE id=?`, uid)
	mustExec(t, d, `UPDATE nodes SET disabled=1 WHERE id=?`, next)

	want := []struct {
		reason SuppressReason
		lift   string
		arg    int64
	}{
		{SuppressUserDisabled, `UPDATE users SET disabled=0 WHERE id=?`, uid},
		{SuppressUserQuota, `UPDATE users SET traffic_used_bytes=0 WHERE id=?`, uid},
		{SuppressNodeDisabled, `UPDATE nodes SET disabled=0 WHERE id=?`, next},
	}
	for _, w := range want {
		reasons, err := RuleSuppressions(d)
		if err != nil {
			t.Fatal(err)
		}
		if reasons[ruleID] != w.reason {
			t.Fatalf("此时报出的原因应为 %q，实际 %q", w.reason, reasons[ruleID])
		}
		mustExec(t, d, w.lift, w.arg)
	}
	reasons, err := RuleSuppressions(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := reasons[ruleID]; still {
		t.Fatalf("三条压制条件都解除后规则不应再被压制，实际 %q", reasons[ruleID])
	}
}

// 规则记在某个逻辑节点名下（rule_hops.via_node_id，例如一个组合）而物理跳在别的
// 节点上时，停用这个逻辑节点同样压制规则：它是用户买到的那条线路。
func TestDisablingTheLogicalNodeSuppressesRulesBoughtThroughIt(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	logical := createTestNode(t, d, "line")
	m1 := createTestNode(t, d, "m1")
	m2 := createTestNode(t, d, "m2")
	grantNode(t, d, uid, logical)
	ruleID, h1, h2 := seedChainRule(t, d, uid, m1, m2, logical, 21001)

	mustExec(t, d, `UPDATE nodes SET disabled=1 WHERE id=?`, logical)

	if pushedHopIDs(t, d, m1)[h1] || pushedHopIDs(t, d, m2)[h2] {
		t.Fatal("停用规则所属的逻辑节点后，它在各物理节点上的跳都不应下发")
	}
	reasons, err := RuleSuppressions(d)
	if err != nil {
		t.Fatal(err)
	}
	if reasons[ruleID] != SuppressNodeDisabled {
		t.Fatalf("压制原因应为 %q，实际 %q", SuppressNodeDisabled, reasons[ruleID])
	}
}

// “账号已到期”有两处表达：Go 侧的 User.Expired（建规则、改规则时拒绝到期用户用它）
// 和压制条件里的 SQL。两处必须对同一个到期时间给出同一个答案。
//
// “永不到期”只有一种写法：NULL。0 不是它的另一种写法——写入方不写 0，旧库里的 0 由
// 迁移 0052 改成了 NULL；万一库里出现 0，两处都把它当成一个早已过去的时刻，账号
// 按已到期处理，而不是被悄悄放行。
func TestUserExpiredAgreesBetweenGoAndThePushPredicate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		expiresAt any
		expired   bool
	}{
		{"没有到期时间", nil, false},
		{"到期时间为 0", int64(0), true},
		{"一小时前到期", now.Add(-time.Hour).Unix(), true},
		{"一小时后到期", now.Add(time.Hour).Unix(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := openTestDB(t)
			uid := createTestUser(t, d)
			nid := createTestNode(t, d, "n")
			grantNode(t, d, uid, nid)
			hop := seedRuleWithTraffic(t, d, uid, nid, 20001, 0)
			mustExec(t, d, `UPDATE users SET expires_at=? WHERE id=?`, c.expiresAt, uid)

			u, err := GetUserByID(d, uid)
			if err != nil {
				t.Fatal(err)
			}
			expired := u.Expired(now)
			if expired != c.expired {
				t.Errorf("User.Expired=%v，期望 %v", expired, c.expired)
			}
			if pushed := pushedHopIDs(t, d, nid)[hop]; pushed == expired {
				t.Errorf("User.Expired=%v，但规则下发=%v（到期应不下发，未到期应下发）", expired, pushed)
			}
		})
	}
}

// rules 表没有“停用这条规则”的列：一条规则转不转发只由压制条件决定。迁移 0046 把
// 从未被写过的 rules.disabled 删掉；在带着这一列的旧库上重放它，列消失、规则行与
// 其余各列原样保留。
func TestMigrationDropsTheUnwrittenRuleDisabledColumn(t *testing.T) {
	const migration = "0046_drop_rule_disabled.sql"
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "n")
	hop := seedRuleWithTraffic(t, d, uid, nid, 20001, 0)
	disabledCols := func() int {
		t.Helper()
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('rules') WHERE name='disabled'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := disabledCols(); n != 0 {
		t.Fatalf("跑完全部迁移的库里 rules 表不应有 disabled 列，实际有 %d 个", n)
	}

	// 回到迁移之前的形状，再重放这一个迁移。
	mustExec(t, d, `ALTER TABLE rules ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`)
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}

	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	if n := disabledCols(); n != 0 {
		t.Fatalf("重放迁移后 rules 表不应再有 disabled 列，实际还有 %d 个", n)
	}
	rules, err := ListAllRules(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ExitHost != "8.8.8.8" || rules[0].ExitPort != 443 {
		t.Fatalf("规则行应原样保留，实际 %+v", rules)
	}
	if !pushedHopIDs(t, d, nid)[hop] {
		t.Fatal("规则应照常下发")
	}
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
