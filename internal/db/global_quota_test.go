package db

import (
	"testing"
)

// “全局配额用尽”有两处表达：Go 侧的 QuotaReached（页面状态与配额执行用它），和
// ActiveRuleHopsForPush 里的 SQL 条件（决定规则下不下发）。两处必须对同一组
// (配额, 已用) 给出同一个答案，否则页面说“已超额”而规则还在转发，或者反过来。
func TestGlobalQuotaPushPredicateMatchesQuotaReached(t *testing.T) {
	cases := []struct{ quota, used int64 }{
		{0, 0},      // 不限量
		{0, 999999}, // 不限量时已用多少都不算用尽
		{1000, 0},
		{1000, 999},
		{1000, 1000}, // 恰好用完算用尽
		{1000, 1001},
	}
	for _, c := range cases {
		d := openTestDB(t)
		uid := createTestUser(t, d)
		nid := createTestNode(t, d, "n")
		grantNode(t, d, uid, nid)
		hop := seedRuleWithTraffic(t, d, uid, nid, 20001, 0)
		if _, err := d.Exec(`UPDATE users SET traffic_quota_bytes=?, traffic_used_bytes=? WHERE id=?`, c.quota, c.used, uid); err != nil {
			t.Fatal(err)
		}

		want := QuotaReached(c.quota, c.used)

		u, err := GetUserByID(d, uid)
		if err != nil {
			t.Fatal(err)
		}
		if u.OverQuota != want {
			t.Errorf("quota=%d used=%d: User.OverQuota=%v，QuotaReached=%v", c.quota, c.used, u.OverQuota, want)
		}
		hops, err := ActiveRuleHopsForPush(d, nid)
		if err != nil {
			t.Fatal(err)
		}
		pushed := false
		for _, h := range hops {
			if h.ID == hop {
				pushed = true
			}
		}
		if pushed == want {
			t.Errorf("quota=%d used=%d: 规则下发=%v，但 QuotaReached=%v（用尽应不下发，未用尽应下发）", c.quota, c.used, pushed, want)
		}
	}
}

// 全局配额用尽不再用“停用账号”表达。迁移把此前因超额被停用的账号恢复为未停用
// （它们是否转发从此由配额状态决定），管理员手动停用的账号不动。
func TestMigrationLiftsOverQuotaAccountDisable(t *testing.T) {
	const migration = "0045_over_quota_no_longer_disables.sql"
	d := openTestDB(t)
	byQuota := createTestUser(t, d)
	byAdmin := createTestUser(t, d)
	if _, err := d.Exec(`UPDATE users SET disabled=1, disable_reason='流量超额' WHERE id=?`, byQuota); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET disabled=1, disable_reason='管理员手动禁用' WHERE id=?`, byAdmin); err != nil {
		t.Fatal(err)
	}
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

	if u, _ := GetUserByID(d, byQuota); u.Disabled || u.DisableReason.String != "" {
		t.Errorf("因超额被停用的账号应恢复：disabled=%v reason=%q", u.Disabled, u.DisableReason.String)
	}
	if u, _ := GetUserByID(d, byAdmin); !u.Disabled || u.DisableReason.String != "管理员手动禁用" {
		t.Errorf("管理员停用的账号不应被迁移改动：disabled=%v reason=%q", u.Disabled, u.DisableReason.String)
	}
}
