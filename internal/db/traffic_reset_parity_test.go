package db

import (
	"database/sql"
	"testing"
)

// 一次重置把这个用户名下的全部账本一起归零：全局已用量、各授权的已用量、落地出口
// 账本、每条规则展示的累计流量。管理员手动重置与按周期自动重置是同一件事的两个
// 触发方式，归零的范围必须一致——否则周期重置之后规则上显示的流量会比用户的已用
// 流量大，两个数再也对不上。
func TestManualAndCycleResetZeroTheSameLedgers(t *testing.T) {
	resets := map[string]func(t *testing.T, d *sql.DB, uid int64){
		"manual": func(t *testing.T, d *sql.DB, uid int64) {
			if err := ResetAllUserTraffic(d, uid); err != nil {
				t.Fatal(err)
			}
		},
		"cycle": func(t *testing.T, d *sql.DB, uid int64) {
			if _, err := d.Exec(`UPDATE users SET traffic_reset_days=30, created_at=strftime('%s','now')-31*86400, last_traffic_reset_at=0 WHERE id=?`, uid); err != nil {
				t.Fatal(err)
			}
			u, _ := GetUserByID(d, uid)
			if reset, err := CheckAndResetTrafficCycle(d, u); err != nil || !reset {
				t.Fatalf("前置条件：周期已过应触发重置，reset=%v err=%v", reset, err)
			}
		},
	}
	for name, reset := range resets {
		t.Run(name, func(t *testing.T) {
			d := openTestDB(t)
			uid := createTestUser(t, d)
			other := createTestUser(t, d)
			nid := createTestNode(t, d, "n")
			grantNode(t, d, uid, nid)
			grantNode(t, d, other, nid)
			mine := seedRuleWithTraffic(t, d, uid, nid, 20001, 5000)
			theirs := seedRuleWithTraffic(t, d, other, nid, 20002, 6000)
			SyncUserLandingExits(d, uid, inputs("a.com"), "", "")
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`UPDATE users SET traffic_used_bytes=9000 WHERE id IN (?,?)`, []any{uid, other}},
				{`UPDATE user_nodes SET traffic_used_bytes=8000 WHERE user_id IN (?,?)`, []any{uid, other}},
				{`UPDATE user_landing_exits SET used_bytes=700, used_bytes_up=300, used_bytes_down=400 WHERE user_id=?`, []any{uid}},
			} {
				if _, err := d.Exec(q.sql, q.args...); err != nil {
					t.Fatal(err)
				}
			}

			reset(t, d, uid)

			one := func(q string, args ...any) int64 {
				t.Helper()
				var v int64
				if err := d.QueryRow(q, args...).Scan(&v); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return v
			}
			for _, c := range []struct {
				what string
				got  int64
				want int64
			}{
				{"用户全局已用量", one(`SELECT traffic_used_bytes FROM users WHERE id=?`, uid), 0},
				{"授权已用量", one(`SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, uid, nid), 0},
				{"落地出口账本", one(`SELECT used_bytes+used_bytes_up+used_bytes_down FROM user_landing_exits WHERE user_id=?`, uid), 0},
				{"规则累计流量", one(`SELECT total_bytes FROM rule_hops WHERE id=?`, mine), 0},
				{"别的用户的全局已用量", one(`SELECT traffic_used_bytes FROM users WHERE id=?`, other), 9000},
				{"别的用户的授权已用量", one(`SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, other, nid), 8000},
				{"别的用户的规则累计流量", one(`SELECT total_bytes FROM rule_hops WHERE id=?`, theirs), 6000},
			} {
				if c.got != c.want {
					t.Errorf("%s: 期望 %d，实际 %d", c.what, c.want, c.got)
				}
			}
		})
	}
}
