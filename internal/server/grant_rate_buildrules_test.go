package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

// 授权上的限速要到数据面：按这份授权限速的每条规则都带着分组与额度（MB/s）。面板
// 只用这两个字段表达限速，不填规则自己的 BandwidthMbps。没有属主的规则不限速。
func TestGrantRateLimitPropagatesToRules(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rl-node", "https://p", "s")
	db.GrantNode(d, uid, n.ID, 10, 0)
	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=10 WHERE user_id=? AND node_id=?`, uid, n.ID); err != nil {
		t.Fatal(err)
	}
	owned, _ := createStandaloneRuleHop(t, d, n.ID, "tcp", 0, "10.0.0.9", 9000, sql.NullInt64{Int64: uid, Valid: true})
	orphan, _ := createStandaloneRuleHop(t, d, n.ID, "tcp", 0, "10.0.0.9", 9001, sql.NullInt64{})

	ruleHops, _ := db.ActiveRuleHopsForPush(d, n.ID)
	rules, err := buildRules(d, ruleHops)
	if err != nil {
		t.Fatal(err)
	}

	var foundOwned, foundOrphan bool
	for _, r := range rules {
		switch r.RuleID {
		case owned:
			foundOwned = true
			if r.ShapeGroup <= 0 || r.RateMBytes != 10 {
				t.Errorf("owned rule shape = group %d rate %d, want positive group rate 10", r.ShapeGroup, r.RateMBytes)
			}
			if r.BandwidthMbps != 0 {
				t.Errorf("面板不应填规则自己的限速，实际 BandwidthMbps = %d", r.BandwidthMbps)
			}
		case orphan:
			foundOrphan = true
			if r.ShapeGroup != 0 || r.RateMBytes != 0 || r.BandwidthMbps != 0 {
				t.Errorf("ownerless rule must be unshaped, got %+v", r)
			}
		}
	}
	if !foundOwned || !foundOrphan {
		t.Fatalf("rules missing from built set: owned=%v orphan=%v", foundOwned, foundOrphan)
	}
}

// A chain rule's grant lives on the rule's panel node (the composite the
// rule was created on), not on the physical nodes the chain hops through —
// those intermediate nodes carry no grant of their own. buildRules must key
// GrantShapes off the rule's own node_id, not each rule_hop's node_id, or a
// composite chain silently loses its rate limit on every hop it pushes to.
func TestGrantRateLimitPropagatesToCompositeChainHops(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "https://p", "ta")
	b, _ := db.CreateNode(d, "jp", "https://p", "tb")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, _ := loginAsUser(t, d, 10)
	// Grant only the composite (the panel node the rule belongs to); the
	// physical hop nodes are never granted directly.
	_ = db.GrantNode(d, uid, comp.ID, 10, 0)
	if _, err := d.Exec(`UPDATE user_nodes SET rate_limit_mbytes=10 WHERE user_id=? AND node_id=?`, uid, comp.ID); err != nil {
		t.Fatal(err)
	}

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl := &db.Rule{NodeID: comp.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true}, Name: "chain-rl", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443}
	ruleID, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = ruleID
	hops := []db.HopInput{{NodeID: a.ID, Mode: "kernel"}, {NodeID: b.ID, Mode: "kernel"}}
	if _, _, _, err := db.RegenerateRule(tx, rl, hops, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, phys := range []*db.Node{a, b} {
		ruleHops, err := db.ActiveRuleHopsForPush(d, phys.ID)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := buildRules(d, ruleHops)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range rules {
			if r.RuleID != ruleID {
				continue
			}
			found = true
			if r.ShapeGroup <= 0 || r.RateMBytes != 10 {
				t.Errorf("node %d (%s): rule shape = group %d rate %d, want positive group rate 10", phys.ID, phys.Name, r.ShapeGroup, r.RateMBytes)
			}
		}
		if !found {
			t.Fatalf("node %d (%s): rule %d missing from pushed rule set", phys.ID, phys.Name, ruleID)
		}
	}
}

// Shape fields are data plane state: changing the rate must change the rev so
// reconnecting agents are not skipped by the rev short-circuit.
func TestComputeRevIncludesShapeFields(t *testing.T) {
	base := []nft.Rule{{Proto: "tcp", SrcPort: 1, DestIP: "1.1.1.1", DestPort: 1}}
	shaped := []nft.Rule{{Proto: "tcp", SrcPort: 1, DestIP: "1.1.1.1", DestPort: 1, ShapeGroup: 3, RateMBytes: 10}}
	if computeRev(base) == computeRev(shaped) {
		t.Fatal("rev must differ when shape fields differ")
	}
}
