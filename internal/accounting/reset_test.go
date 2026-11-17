package accounting

import (
	"database/sql"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// userLedgers 在 nid 上给 uid 建一条单跳规则和一个落地出口，并把他名下每本账都
// 记上非零值，返回规则 id 与跳 id。
func userLedgers(t *testing.T, d *sql.DB, uid, nid int64, exitHost string, ruleBytes int64) (ruleID, hopID int64) {
	t.Helper()
	if err := db.GrantNode(d, uid, nid, 10, 0); err != nil {
		t.Fatal(err)
	}
	ruleID, hops := newRule(t, d, &db.Rule{NodeID: nid, OwnerID: owned(uid), Name: "r", Proto: "tcp", ExitHost: exitHost, ExitPort: 443},
		[]db.HopInput{{NodeID: nid, Mode: "kernel"}})
	seedExit(t, d, uid, exitHost, 443)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE users SET traffic_used_bytes=9000 WHERE id=?`, []any{uid}},
		{`UPDATE user_nodes SET traffic_used_bytes=8000 WHERE user_id=?`, []any{uid}},
		{`UPDATE user_landing_exits SET used_bytes=700, used_bytes_up=300, used_bytes_down=400 WHERE user_id=?`, []any{uid}},
		{`UPDATE rules SET weighted_bytes=? WHERE id=?`, []any{ruleBytes, ruleID}},
		{`UPDATE rule_hops SET total_bytes=? WHERE id=?`, []any{ruleBytes, hops[0].ID}},
	} {
		if _, err := d.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return ruleID, hops[0].ID
}

// 一次用户重置把这个用户名下的全部账本一起归零：全局已用量、各授权的已用量、落地
// 出口账本、每条规则的加权流量与逐跳流量。管理员手动重置与按周期自动重置是同一件事
// 的两个触发方式，归零的范围必须一致——否则周期重置之后规则上显示的流量会比用户的
// 已用流量大，两个数再也对不上。别的用户的账与节点原始流量不动。
func TestManualAndCycleResetZeroTheSameLedgers(t *testing.T) {
	resets := map[string]func(t *testing.T, d *sql.DB, uid int64){
		"manual": func(t *testing.T, d *sql.DB, uid int64) {
			if err := ResetUser(d, uid); err != nil {
				t.Fatal(err)
			}
		},
		"cycle": func(t *testing.T, d *sql.DB, uid int64) {
			if _, err := d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=strftime('%s','now')-31*86400, last_traffic_reset_at=0 WHERE id=?`, uid); err != nil {
				t.Fatal(err)
			}
			u, _ := db.GetUserByID(d, uid)
			if reset, err := ResetCycleIfDue(d, u); err != nil || !reset {
				t.Fatalf("前置条件：周期已过应触发重置，reset=%v err=%v", reset, err)
			}
		},
	}
	for name, reset := range resets {
		t.Run(name, func(t *testing.T) {
			d := openDB(t)
			uid, other := newUser(t, d), newUser(t, d)
			nid := newNode(t, d, "n", "10.0.0.1")
			mineRule, mineHop := userLedgers(t, d, uid, nid, "a.com", 5000)
			theirRule, theirHop := userLedgers(t, d, other, nid, "b.com", 6000)
			if _, err := d.Exec(`INSERT INTO node_raw_traffic(node_id, raw_bytes) VALUES (?, 4096)`, nid); err != nil {
				t.Fatal(err)
			}

			reset(t, d, uid)

			for _, c := range []struct {
				what      string
				got, want int64
			}{
				{"用户全局已用量", one(t, d, `SELECT traffic_used_bytes FROM users WHERE id=?`, uid), 0},
				{"授权已用量", one(t, d, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, uid, nid), 0},
				{"落地出口账本", one(t, d, `SELECT used_bytes+used_bytes_up+used_bytes_down FROM user_landing_exits WHERE user_id=?`, uid), 0},
				{"规则的加权流量", one(t, d, `SELECT weighted_bytes FROM rules WHERE id=?`, mineRule), 0},
				{"逐跳流量", one(t, d, `SELECT total_bytes FROM rule_hops WHERE id=?`, mineHop), 0},
				{"别的用户的全局已用量", one(t, d, `SELECT traffic_used_bytes FROM users WHERE id=?`, other), 9000},
				{"别的用户的授权已用量", one(t, d, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, other, nid), 8000},
				{"别的用户的落地出口账本", one(t, d, `SELECT used_bytes FROM user_landing_exits WHERE user_id=?`, other), 700},
				{"别的用户的规则的加权流量", one(t, d, `SELECT weighted_bytes FROM rules WHERE id=?`, theirRule), 6000},
				{"别的用户的逐跳流量", one(t, d, `SELECT total_bytes FROM rule_hops WHERE id=?`, theirHop), 6000},
				{"节点原始流量", one(t, d, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, nid), 4096},
			} {
				if c.got != c.want {
					t.Errorf("%s: 期望 %d，实际 %d", c.what, c.want, c.got)
				}
			}
		})
	}
}

// 周期从开户时刻起算：traffic_reset_days=0 从不重置；周期翻过时重置一次并记下时间，
// 同一个周期里再查不再重置；手动重置不动这个时间。
func TestResetCycleIfDue(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes=9999 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByID(d, uid)
	if reset, err := ResetCycleIfDue(d, u); err != nil || reset {
		t.Fatalf("traffic_reset_days=0 不应重置：reset=%v err=%v", reset, err)
	}

	past := time.Now().Unix() - 31*86400
	if _, err := d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=? WHERE id=?`, past, uid); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUserByID(d, uid)
	if reset, err := ResetCycleIfDue(d, u); err != nil || !reset {
		t.Fatalf("开户 31 天、周期 30 天应重置：reset=%v err=%v", reset, err)
	}
	u, _ = db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 || u.LastTrafficResetAt == 0 {
		t.Fatalf("重置后已用量应为 0、应记下重置时间：used=%d at=%d", u.TrafficUsedBytes, u.LastTrafficResetAt)
	}
	stamped := u.LastTrafficResetAt

	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes=100 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUserByID(d, uid)
	if reset, err := ResetCycleIfDue(d, u); err != nil || reset {
		t.Fatalf("同一个周期里不应再重置：reset=%v err=%v", reset, err)
	}
	if err := ResetUser(d, uid); err != nil {
		t.Fatal(err)
	}
	if got := one(t, d, `SELECT last_traffic_reset_at FROM users WHERE id=?`, uid); got != stamped {
		t.Fatalf("手动重置不应改动周期重置的时间：%d → %d", stamped, got)
	}
}

// 周期重置要么整体发生、要么整体不发生：各本账的归零与 last_traffic_reset_at 的记录
// 在同一个事务里（ResetCycleIfDue）。两个方向各有后果：只清了前面几本账，规则上显示
// 的流量就与用户的已用流量对不上；账清了而时间没记下，下一次调用仍判定“本周期还没
// 重置过”，会把这之间新记的用量再清掉一次。
func TestCycleResetIsAllOrNothing(t *testing.T) {
	for name, trigger := range map[string]string{
		"最后一本账写不进去": `CREATE TRIGGER refuse BEFORE UPDATE OF total_bytes ON rule_hops BEGIN SELECT RAISE(ABORT, 'refused'); END`,
		"重置时间写不进去":  `CREATE TRIGGER refuse BEFORE UPDATE OF last_traffic_reset_at ON users BEGIN SELECT RAISE(ABORT, 'refused'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			d := openDB(t)
			uid := newUser(t, d)
			nid := newNode(t, d, "n", "10.0.0.1")
			ruleID, hopID := userLedgers(t, d, uid, nid, "a.com", 5000)
			if _, err := d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=? WHERE id=?`, time.Now().Unix()-31*86400, uid); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			u, err := db.GetUserByID(d, uid)
			if err != nil {
				t.Fatal(err)
			}
			if reset, err := ResetCycleIfDue(d, u); err == nil || reset {
				t.Fatalf("期望报错且未重置，实际 reset=%v err=%v", reset, err)
			}
			for _, c := range []struct {
				what      string
				got, want int64
			}{
				{"用户全局已用量", one(t, d, `SELECT traffic_used_bytes FROM users WHERE id=?`, uid), 9000},
				{"授权已用量", one(t, d, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, uid, nid), 8000},
				{"落地出口账本", one(t, d, `SELECT used_bytes+used_bytes_up+used_bytes_down FROM user_landing_exits WHERE user_id=?`, uid), 1400},
				{"规则的加权流量", one(t, d, `SELECT weighted_bytes FROM rules WHERE id=?`, ruleID), 5000},
				{"逐跳流量", one(t, d, `SELECT total_bytes FROM rule_hops WHERE id=?`, hopID), 5000},
				{"记下的重置时间", one(t, d, `SELECT last_traffic_reset_at FROM users WHERE id=?`, uid), 0},
			} {
				if c.got != c.want {
					t.Errorf("%s: 重置失败后应保持 %d，实际 %d", c.what, c.want, c.got)
				}
			}
		})
	}
}

// 重置一条授权只清这一条授权的已用量。
func TestResetGrantClearsOnlyThatGrant(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	a, b := newNode(t, d, "a", "10.0.0.1"), newNode(t, d, "b", "10.0.0.2")
	for _, n := range []int64{a, b} {
		if err := db.GrantNode(d, uid, n, 10, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes=500 WHERE user_id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET traffic_used_bytes=900 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if err := ResetGrant(d, uid, a); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what      string
		got, want int64
	}{
		{"重置的授权", one(t, d, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, uid, a), 0},
		{"另一条授权", one(t, d, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, uid, b), 500},
		{"全局已用量", one(t, d, `SELECT traffic_used_bytes FROM users WHERE id=?`, uid), 900},
	} {
		if c.got != c.want {
			t.Errorf("%s: 期望 %d，实际 %d", c.what, c.want, c.got)
		}
	}
}

// 重置一个落地出口账：合计与上下行三列一起清零，超额随之解除；返回值说明这一行
// 在不在、在不在册，不存在的出口什么也不动。
func TestResetExit(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	seedExit(t, d, uid, "a.com", 443)
	seedExit(t, d, uid, "b.com", 443)
	if _, err := d.Exec(`UPDATE user_landing_exits SET quota_bytes=100, used_bytes=150, used_bytes_up=50, used_bytes_down=100 WHERE user_id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=? AND host='b.com'`, uid); err != nil {
		t.Fatal(err)
	}

	if updated, present, err := ResetExit(d, uid, "a.com", 443); err != nil || !updated || !present {
		t.Fatalf("在册的出口：updated=%v present=%v err=%v", updated, present, err)
	}
	if got := one(t, d, `SELECT used_bytes + used_bytes_up + used_bytes_down FROM user_landing_exits WHERE user_id=? AND host='a.com'`, uid); got != 0 {
		t.Fatalf("三列都应清零，剩 %d", got)
	}
	if keys, err := db.ExitsExceedingQuota(d, uid); err != nil || len(keys) != 0 {
		t.Fatalf("重置后不应再超额：%v err=%v", keys, err)
	}
	if updated, present, err := ResetExit(d, uid, "b.com", 443); err != nil || !updated || present {
		t.Fatalf("不在册的残留行：updated=%v present=%v err=%v", updated, present, err)
	}
	if updated, present, err := ResetExit(d, uid, "nope.com", 443); err != nil || updated || present {
		t.Fatalf("不存在的出口：updated=%v present=%v err=%v", updated, present, err)
	}
}
