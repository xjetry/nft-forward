package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
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
	rules := buildRules(d, ruleHops)

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

func TestComputeRevIgnoresRuleMeta(t *testing.T) {
	base := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001}}
	withMeta := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001,
		RuleID: 5, RuleName: "seednet-vless"}}
	if computeRev(base) != computeRev(withMeta) {
		t.Fatalf("rule metadata must not affect rev: %q vs %q", computeRev(base), computeRev(withMeta))
	}
}

func TestBuildRules_FillsOwnerName(t *testing.T) {
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

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl := &db.Rule{NodeID: n.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true}, Name: "owned", Proto: "tcp", ExitHost: "72.234.229.145", ExitPort: 17171}
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

	tx2, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl2 := &db.Rule{NodeID: n.ID, Name: "no-owner", Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 18000}
	ruleID2, err := db.CreateRule(tx2, rl2)
	if err != nil {
		tx2.Rollback()
		t.Fatal(err)
	}
	rl2.ID = ruleID2
	_, _, _, err = db.RegenerateRule(tx2, rl2, []db.HopInput{{NodeID: n.ID}}, nil)
	if err != nil {
		tx2.Rollback()
		t.Fatal(err)
	}
	tx2.Commit()

	ruleHops, _ := db.ActiveRuleHopsForPush(d, n.ID)
	rules := buildRules(d, ruleHops)

	var owned, unowned *nft.Rule
	for i := range rules {
		if rules[i].RuleID == ruleID {
			owned = &rules[i]
		}
		if rules[i].RuleID == ruleID2 {
			unowned = &rules[i]
		}
	}
	if owned == nil || unowned == nil {
		t.Fatalf("expected both rules, got %+v", rules)
	}
	if owned.OwnerName != "qqpw" {
		t.Fatalf("owner rule should carry owner name, got %q", owned.OwnerName)
	}
	if unowned.OwnerName != "" {
		t.Fatalf("ownerless rule should leave OwnerName empty, got %q", unowned.OwnerName)
	}
}

func TestComputeRev_ExcludesOwnerName(t *testing.T) {
	base := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, OwnerName: "alpha"}}
	renamed := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, OwnerName: "beta"}}
	if computeRev(base) != computeRev(renamed) {
		t.Fatal("owner rename must not change rev (display-only metadata)")
	}
}
