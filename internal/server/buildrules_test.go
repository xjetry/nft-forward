package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func TestBuildRulesStampsChainMeta(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "hop1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	chainID, err := db.CreateChain(d, &db.Chain{
		Name: "seednet-vless", Proto: "tcp", ExitHost: "exit.example", ExitPort: 8443,
	})
	if err != nil {
		t.Fatal(err)
	}
	// One chain-owned forward and one standalone forward on the same node.
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "10.0.0.2", TargetPort: 20001,
		ChainID: sql.NullInt64{Int64: chainID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.9", TargetPort: 443,
	}); err != nil {
		t.Fatal(err)
	}

	forwards, err := db.ActiveForwardsForPush(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	rules := buildRules(d, forwards)

	var chained, standalone *nft.Rule
	for i := range rules {
		switch rules[i].SrcPort {
		case 20000:
			chained = &rules[i]
		case 30000:
			standalone = &rules[i]
		}
	}
	if chained == nil || standalone == nil {
		t.Fatalf("expected both forwards in rules, got %+v", rules)
	}
	if chained.ChainID != chainID || chained.ChainName != "seednet-vless" {
		t.Fatalf("chain forward should carry meta, got ChainID=%d ChainName=%q",
			chained.ChainID, chained.ChainName)
	}
	if standalone.ChainID != 0 || standalone.ChainName != "" {
		t.Fatalf("standalone forward must have no chain meta, got ChainID=%d ChainName=%q",
			standalone.ChainID, standalone.ChainName)
	}
}

func TestComputeRevIgnoresChainMeta(t *testing.T) {
	// Chain metadata must not change the revision hash: a chain rename must not
	// trigger a redundant re-apply when the data plane is unchanged.
	base := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001}}
	withMeta := []nft.Rule{{Proto: "tcp", SrcPort: 20000, DestIP: "10.0.0.2", DestPort: 20001,
		ChainID: 5, ChainName: "seednet-vless"}}
	if computeRev(base) != computeRev(withMeta) {
		t.Fatalf("chain metadata must not affect rev: %q vs %q", computeRev(base), computeRev(withMeta))
	}
}
