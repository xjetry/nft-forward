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

// rev 对规则的每个字段都与 nft.Rule.Dataplane 的归类一致：只改一个字段，rev 变当且
// 仅当数据面投影变了。字段从 nft.Rule 的定义里枚举（nfttest.FieldVariants），这里
// 不抄字段清单——daemon 判断数据面要不要重放用的是同一个投影（internal/daemon 的
// TestDataplaneIsReappliedExactlyWhenTheProjectionChanges），两边对“变没变”的回答
// 由此钉在一起。
func TestComputeRevChangesExactlyWhenTheDataplaneProjectionDoes(t *testing.T) {
	base := nft.Rule{
		RuleID: 5, RuleName: "seednet-vless", HopCount: 1, Comment: "c",
		Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001,
	}
	for field, variant := range nfttest.FieldVariants(base) {
		changed := computeRev([]nft.Rule{variant}) != computeRev([]nft.Rule{base})
		if want := variant.Dataplane() != base.Dataplane(); changed != want {
			t.Errorf("只改 %s：rev 变了=%v，按数据面投影应为 %v", field, changed, want)
		}
	}
}

// Mode 的空值与 "kernel" 在数据面上是同一回事，rev 不区分它们。
func TestComputeRevTreatsEmptyModeAsKernel(t *testing.T) {
	implicit := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}
	explicit := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, Mode: nft.ModeKernel}}
	if computeRev(implicit) != computeRev(explicit) {
		t.Fatal("空 Mode 与 kernel 的 rev 应相同")
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
	byRule := map[int64]nft.Rule{}
	for _, r := range buildRules(d, ruleHops) {
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
