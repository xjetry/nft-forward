package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// Setting a rule's bandwidth persists the cap and it flows into the generated
// nft.Rule the data plane shapes on.
func TestSetRuleBandwidthPersistsAndPropagates(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	n, _ := db.CreateNode(d, "bw-node", "https://p", "s")
	ruleID, _ := createStandaloneRuleHop(t, d, n.ID, "tcp", 0, "10.0.0.9", 9000, sql.NullInt64{})

	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/rules/%d/bandwidth", ruleID),
		strings.NewReader(`{"bandwidth_mbps":50}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set bandwidth status=%d body=%s", rec.Code, rec.Body.String())
	}

	rl, _ := db.GetRule(d, ruleID)
	if rl.BandwidthMbps != 50 {
		t.Fatalf("rule bandwidth = %d, want 50", rl.BandwidthMbps)
	}

	// buildRules must carry the cap into the data-plane rule set.
	ruleHops, _ := db.ActiveRuleHopsForPush(d, n.ID)
	rules := buildRules(d, ruleHops)
	found := false
	for _, r := range rules {
		if r.RuleID == ruleID {
			found = true
			if r.BandwidthMbps != 50 {
				t.Errorf("nft.Rule bandwidth = %d, want 50", r.BandwidthMbps)
			}
		}
	}
	if !found {
		t.Fatal("rule not present in built ruleset")
	}
}
