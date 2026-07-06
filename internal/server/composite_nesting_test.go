package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// A rule over a nested composite flattens to the physical leaf chain: every hop
// records the top-level logical node as its via (inner composites are
// transparent sugar), each leaf keeps the mode configured in its innermost
// composite, and the rule's exit_mode owns the final hop.
func TestNestedCompositeRuleFlattens(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	x, _ := db.CreateNode(d, "x", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, x.ID, "3.3.3.3")
	inner := makeCompositeHopMode(t, d, "inner", "kernel", a.ID, b.ID)
	outer := makeComposite(t, d, "outer", inner.ID, x.ID) // outer's own hop modes default userspace

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, outer.ID, 5, 0)
	s, _ := New(d)

	userJSON(t, s, cookie, "POST", "/api/my/rules", map[string]any{
		"node_id": outer.ID, "name": "r", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "userspace",
	})
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	if len(hops) != 3 {
		t.Fatalf("flattened hops: got %d, want 3 (%+v)", len(hops), hops)
	}
	if hops[0].NodeID != a.ID || hops[1].NodeID != b.ID || hops[2].NodeID != x.ID {
		t.Fatalf("leaf order = %d/%d/%d, want a/b/x", hops[0].NodeID, hops[1].NodeID, hops[2].NodeID)
	}
	for i, h := range hops {
		if h.ViaNodeID != outer.ID {
			t.Errorf("hop %d via = %d, want top-level outer %d", i, h.ViaNodeID, outer.ID)
		}
	}
	// a,b keep inner's kernel; x (rule's exit hop) takes exit_mode userspace.
	wantModes(t, chainModes(t, s, rules[0].ID), "kernel", "kernel", "userspace")
}

// Recursive fan-out is bounded: an exponential nest (Cn=[Cn-1,Cn-1]) whose
// flattened length exceeds maxFlattenedHops is refused at rule-expansion time,
// so a hand-crafted deep composite can't blow up the physical chain.
func TestNestedCompositeHopCapEnforced(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	prev := makeComposite(t, d, "c1", a.ID, b.ID) // flattens to 2 physical hops
	// Double each level: c2=4, c3=8, c4=16, c5=32, c6=64 (> maxFlattenedHops=32).
	for i := 2; i <= 6; i++ {
		prev = makeComposite(t, d, fmt.Sprintf("c%d", i), prev.ID, prev.ID)
	}
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, prev.ID, 5, 0)
	s, _ := New(d)

	if rec := createMyRule(t, s, cookie, prev.ID, "r-huge"); rec.Code != http.StatusBadRequest {
		t.Fatalf("exponential nest exceeding hop cap: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// no_direct_exit is enforced on the true final physical hop, which for a nested
// composite is the deepest last leaf — not the logical composite node.
func TestNestedCompositeNoDirectExitDeepLeaf(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	x, _ := db.CreateNode(d, "x", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	_ = db.UpdateNodeRelayHost(d, x.ID, "3.3.3.3")
	inner := makeComposite(t, d, "inner", a.ID, b.ID)
	outer := makeComposite(t, d, "outer", x.ID, inner.ID) // flattens to [x, a, b]; tail = b
	_ = db.UpdateNodeNoDirectExit(d, b.ID, true)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, outer.ID, 5, 0)
	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, outer.ID, "r"); rec.Code != http.StatusBadRequest {
		t.Fatalf("nested composite whose deepest last leaf forbids direct exit: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// Role checks apply to the top-level logical node the rule references, not to
// flattened leaves: a nested composite lacking the entry role can't be an entry.
func TestNestedCompositeEntryRoleChecked(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	inner := makeComposite(t, d, "inner", a.ID, b.ID)
	outer := makeComposite(t, d, "outer", inner.ID, a.ID)
	_ = db.UpdateNodeRoles(d, outer.ID, db.NodeRoleVia) // via only, not entry

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, outer.ID, 5, 0)
	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, outer.ID, "r"); rec.Code != http.StatusBadRequest {
		t.Fatalf("nested composite without entry role used as entry: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// A nested composite sitting mid-chain owns how its own exit leg forwards: its
// flattened last leaf keeps the composite's configured mode (not the downstream
// binding edge's), inner segments keep their inner modes, and only the rule's
// final hop takes exit_mode. Provenance stays attributed to the logical nodes.
func TestNestedCompositeAsMiddleKeepsConfigMode(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	y, _ := db.CreateNode(d, "y", "", "")
	tailVia, _ := db.CreateNode(d, "tail-via", "", "")
	for _, n := range []*db.Node{entry, c1, c2, y, tailVia} {
		_ = db.UpdateNodeRelayHost(d, n.ID, "10.0.0.1")
	}
	inner := makeCompositeHopMode(t, d, "inner", "kernel", c1.ID, c2.ID)
	outer := makeComposite(t, d, "outer", inner.ID, y.ID) // outer's y hop mode defaults userspace
	bindVia(t, d, entry.ID, outer.ID, "userspace")
	bindVia(t, d, outer.ID, tailVia.ID, "userspace")

	uid, cookie := loginAsUser(t, d, 11)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, outer.ID, 5, 0)
	_ = db.GrantNode(d, uid, tailVia.ID, 5, 0)
	s, _ := New(d)

	userJSON(t, s, cookie, "POST", "/api/my/rules", map[string]any{
		"node_id": entry.ID, "via_node_ids": []int64{outer.ID, tailVia.ID},
		"name": "r-mid", "proto": "tcp", "exit": "9.9.9.9:8443", "exit_mode": "kernel",
	})
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	// entry: binding edge userspace; c1,c2: inner config kernel; y (outer's tail
	// while outer is a middle layer): outer config userspace, NOT the downstream
	// edge; tailVia (rule tail): exit_mode kernel.
	wantModes(t, chainModes(t, s, rules[0].ID), "userspace", "kernel", "kernel", "userspace", "kernel")
	hops, _ := db.ListRuleHops(d, rules[0].ID)
	wantVia := []int64{entry.ID, outer.ID, outer.ID, outer.ID, tailVia.ID}
	if len(hops) != len(wantVia) {
		t.Fatalf("hops = %d, want %d", len(hops), len(wantVia))
	}
	for i, h := range hops {
		if h.ViaNodeID != wantVia[i] {
			t.Fatalf("hop %d via = %d, want %d", i, h.ViaNodeID, wantVia[i])
		}
	}
}

// Deleting a node referenced by a composite first returns the affected
// composites for confirmation instead of silently cascading the member out;
// with confirm=1 the delete proceeds.
func TestDeleteNodeWarnsCompositeReferrers(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "a", "", "")
	b, _ := db.CreateNode(d, "b", "", "")
	x, _ := db.CreateNode(d, "x", "", "")
	inner := makeComposite(t, d, "inner", a.ID, b.ID)
	outer := makeComposite(t, d, "outer", inner.ID, x.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	// Deleting inner (referenced by outer) without confirm -> needs_confirm, not deleted.
	rec := apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d", inner.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete without confirm: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		NeedsConfirm       bool `json:"needs_confirm"`
		AffectedComposites []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"affected_composites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.NeedsConfirm {
		t.Fatalf("want needs_confirm, body=%s", rec.Body.String())
	}
	found := false
	for _, ac := range resp.AffectedComposites {
		if ac.ID == outer.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("affected composites %+v must include outer %d", resp.AffectedComposites, outer.ID)
	}
	if _, err := db.GetNode(d, inner.ID); err != nil {
		t.Fatalf("inner must NOT be deleted before confirm: %v", err)
	}

	// With confirm=1 the delete goes through.
	rec = apiNodeAction(t, s, admin, "DELETE", fmt.Sprintf("/api/nodes/%d?confirm=1", inner.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete with confirm: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := db.GetNode(d, inner.ID); err == nil {
		t.Fatalf("inner must be deleted after confirm")
	}
}
