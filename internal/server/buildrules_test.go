package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/nft/nfttest"
)

func TestBuildRulesStampsRuleMeta(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "hop1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl := &db.Rule{NodeID: n.ID, Name: "seednet-vless", Proto: "tcp", ExitHost: "exit.example", ExitPort: 8443}
	ruleID, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = ruleID
	_, _, _, err = db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: n.ID}}, nil)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	ruleHops, err := db.ActiveRuleHopsForPush(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := buildRules(d, ruleHops)
	if err != nil {
		t.Fatal(err)
	}

	if len(rules) == 0 {
		t.Fatal("expected at least 1 rule")
	}
	found := false
	for _, r := range rules {
		if r.RuleID == ruleID {
			found = true
			if r.RuleName != "seednet-vless" {
				t.Fatalf("rule should carry name, got RuleName=%q", r.RuleName)
			}
		}
	}
	if !found {
		t.Fatalf("expected rule with RuleID=%d in results", ruleID)
	}
}

// rev 标识整份下发载荷：同一份载荷算出同一个 rev，任何一个字段变了 rev 都变——
// 包括数据面不读的规则名、备注、跳数，节点要靠 rev 不同才会在重连后收到它们的新值。
// 字段从 nft.Rule 的定义里枚举（nfttest.FieldVariants），加了字段不用改这里。
func TestComputeRevIdentifiesTheWholePayload(t *testing.T) {
	base := nft.Rule{
		RuleID: 5, RuleName: "seednet-vless", HopCount: 1, Comment: "c",
		Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001,
	}
	if computeRev([]nft.Rule{base}) != computeRev([]nft.Rule{base}) {
		t.Fatal("同一份载荷两次算出的 rev 不同")
	}
	// 没有规则的载荷也是一份载荷：nil 与空切片说的是同一件事。
	if computeRev(nil) != computeRev([]nft.Rule{}) {
		t.Fatal("空规则集的 rev 随它是 nil 还是空切片而不同")
	}
	variants := nfttest.FieldVariants(base)
	if len(variants) == 0 {
		t.Fatal("前置条件：FieldVariants 没有给出任何字段")
	}
	for field, variant := range variants {
		if computeRev([]nft.Rule{variant}) == computeRev([]nft.Rule{base}) {
			t.Errorf("只改 %s：rev 没变，节点重连后收不到这个字段的新值", field)
		}
	}
}

// 面板下发的规则带着所属规则的 id、名字与整条链的跳数；没有属主的规则同样下发。
func TestBuildRulesCarriesRuleIdentityForOwnedAndOwnerlessRules(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1")
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, "qqpw", hash, "user")
	if err != nil {
		t.Fatal(err)
	}

	create := func(rl *db.Rule) int64 {
		t.Helper()
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.CreateRule(tx, rl)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		rl.ID = id
		if _, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: n.ID}}, nil); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ownedID := create(&db.Rule{NodeID: n.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true}, Name: "owned", Proto: "tcp", ExitHost: "72.234.229.145", ExitPort: 17171})
	ownerlessID := create(&db.Rule{NodeID: n.ID, Name: "no-owner", Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 18000})

	ruleHops, err := db.ActiveRuleHopsForPush(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := buildRules(d, ruleHops)
	if err != nil {
		t.Fatal(err)
	}
	byRule := map[int64]nft.Rule{}
	for _, r := range rules {
		byRule[r.RuleID] = r
	}
	for id, name := range map[int64]string{ownedID: "owned", ownerlessID: "no-owner"} {
		r, ok := byRule[id]
		if !ok {
			t.Fatalf("规则 %d（%s）应在下发的规则集里，实际 %+v", id, name, byRule)
		}
		if r.RuleName != name || r.HopCount != 1 {
			t.Errorf("规则 %d 应带名字 %q 与跳数 1，实际 %+v", id, name, r)
		}
	}
}
