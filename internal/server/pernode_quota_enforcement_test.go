package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestPerNodeQuotaExclusion(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "pn1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "pn2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")

	db.GrantNode(d, uid, n1.ID, 10, 1000) // 1000 byte quota on n1
	db.GrantNode(d, uid, n2.ID, 10, 0)    // no per-node quota on n2

	r1 := createTestRuleDirectNode(t, d, uid, n1.ID)
	r2 := createTestRuleDirectNode(t, d, uid, n2.ID)

	// before exceeding: both rules should be pushed
	hops1, _ := db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ := db.ActiveRuleHopsForPush(d, n2.ID)
	if len(hops1) == 0 {
		t.Fatal("r1 hops should be active before exceeding quota")
	}
	if len(hops2) == 0 {
		t.Fatal("r2 hops should be active")
	}

	// exceed n1 quota
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=1000 WHERE user_id=? AND node_id=?`, uid, n1.ID)

	// n1 rules excluded, n2 rules still active
	hops1, _ = db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ = db.ActiveRuleHopsForPush(d, n2.ID)
	if len(hops1) != 0 {
		t.Fatalf("r1 hops should be excluded after n1 quota exceeded, got %d", len(hops1))
	}
	if len(hops2) == 0 {
		t.Fatal("r2 hops should still be active — n2 has no per-node quota")
	}
	_ = r1
	_ = r2
}

func TestChainExcludedWhenOneHopExceedsQuota(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "ch1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	n2, _ := db.CreateNode(d, "ch2", "", "")
	db.UpdateNodeRelayHost(d, n2.ID, "2.2.2.2")

	db.GrantNode(d, uid, n1.ID, 10, 0)
	db.GrantNode(d, uid, n2.ID, 10, 500) // quota on n2

	// chain rule: n1 → n2
	ruleID := createTestRuleWithHops(t, d, uid, n1.ID, n2.ID)

	// exceed n2 quota
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=500 WHERE user_id=? AND node_id=?`, uid, n2.ID)

	// both n1 and n2 hops for this rule should be excluded
	hops1, _ := db.ActiveRuleHopsForPush(d, n1.ID)
	hops2, _ := db.ActiveRuleHopsForPush(d, n2.ID)
	for _, h := range hops1 {
		if h.RuleID == ruleID {
			t.Fatal("chain rule hop on n1 should be excluded because n2 exceeded quota")
		}
	}
	for _, h := range hops2 {
		if h.RuleID == ruleID {
			t.Fatal("chain rule hop on n2 should be excluded")
		}
	}
}

func TestGlobalQuotaStillDisablesUser(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 100)

	n1, _ := db.CreateNode(d, "gq1", "", "")
	db.UpdateNodeRelayHost(d, n1.ID, "1.1.1.1")
	db.GrantNode(d, uid, n1.ID, 10, 0)

	// set global quota
	d.Exec(`UPDATE users SET traffic_quota_bytes=2000, traffic_used_bytes=2000 WHERE id=?`, uid)

	s, _ := New(d)
	s.enforceUserQuota(uid)

	u, _ := db.GetUserByID(d, uid)
	if !u.Disabled {
		t.Fatal("user should be disabled when global quota exceeded")
	}
}
