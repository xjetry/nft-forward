package db

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// chainRule creates a rule over the nodes, in order, to 9.9.9.9:443.
func chainRule(t *testing.T, d *sql.DB, name string, nodes ...*Node) int64 {
	t.Helper()
	r := &Rule{NodeID: nodes[0].ID, Name: name, Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if r.ID, err = CreateRule(tx, r); err != nil {
		t.Fatal(err)
	}
	hops := make([]HopInput, len(nodes))
	for i, n := range nodes {
		hops[i] = HopInput{NodeID: n.ID, Mode: "kernel", LogicalNodeID: n.ID}
	}
	if _, _, _, err := RegenerateRule(tx, r, hops, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// relayNode creates a node with the given relay address.
func relayNode(t *testing.T, d *sql.DB, name, relayHost string) *Node {
	t.Helper()
	n, err := CreateNode(d, name, "", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeRelayHost(d, n.ID, relayHost); err != nil {
		t.Fatal(err)
	}
	return n
}

// hopLines is a rule's hops as comparable text: node, listen port, target.
func hopLines(t *testing.T, d *sql.DB, ruleID int64) []string {
	t.Helper()
	hops, err := ListRuleHops(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, len(hops))
	for i, h := range hops {
		lines[i] = fmt.Sprintf("#%d node %d :%d -> %s:%d", h.Position, h.NodeID, h.ListenPort, h.TargetHost, h.TargetPort)
	}
	return lines
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64{}, ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestRulesFollowANodesStoredRelayAddress: the hop before one on a node
// targets the node's relay address. When the address is written without the
// rules being re-generated, StaleRulesIntoNode finds exactly the rules that
// run into the node, and RewireRulesIntoNode brings them in line.
func TestRulesFollowANodesStoredRelayAddress(t *testing.T) {
	d := openTestDB(t)
	a := relayNode(t, d, "a", "10.0.0.1")
	b := relayNode(t, d, "b", "10.0.0.2")
	c := relayNode(t, d, "c", "10.0.0.3")
	into := chainRule(t, d, "a-b", a, b)         // b is the second hop
	through := chainRule(t, d, "a-b-c", a, b, c) // b is in the middle
	from := chainRule(t, d, "b-c", b, c)         // b is the entry: nothing targets it
	apart := chainRule(t, d, "a-c", a, c)        // not on b at all

	for _, n := range []*Node{a, b, c} {
		if stale, err := StaleRulesIntoNode(d, n.ID); err != nil || len(stale) != 0 {
			t.Fatalf("rules just generated: stale into node %s = %v (%v), want none", n.Name, stale, err)
		}
	}
	before := map[int64][]string{}
	for _, id := range []int64{into, through, from, apart} {
		before[id] = hopLines(t, d, id)
	}

	if err := UpdateNodeRelayHost(d, b.ID, "10.0.0.22"); err != nil {
		t.Fatal(err)
	}
	stale, err := StaleRulesIntoNode(d, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{into, through}; !reflect.DeepEqual(stale, want) {
		t.Fatalf("stale into b after its address changed = %v, want %v (the rules with a hop before b's)", stale, want)
	}
	for _, n := range []*Node{a, c} {
		if stale, err := StaleRulesIntoNode(d, n.ID); err != nil || len(stale) != 0 {
			t.Errorf("stale into node %s = %v (%v), want none: its address did not change", n.Name, stale, err)
		}
	}

	nodes, err := RewireRulesIntoNode(d, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedIDs(nodes), []int64{a.ID, b.ID, c.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes to re-push = %v, want %v (every node of the two re-generated rules, once)", got, want)
	}
	for _, id := range []int64{into, through} {
		hops, err := ListRuleHops(d, id)
		if err != nil {
			t.Fatal(err)
		}
		if hops[0].TargetHost != "10.0.0.22" {
			t.Errorf("rule %d: the hop before b targets %s, want b's address 10.0.0.22", id, hops[0].TargetHost)
		}
	}
	// The address is all that changed: the ports the hops listen on stay.
	if got, want := hopLines(t, d, into)[1], before[into][1]; got != want {
		t.Errorf("b's own hop changed: %s, was %s", got, want)
	}
	for _, id := range []int64{from, apart} {
		if got := hopLines(t, d, id); !reflect.DeepEqual(got, before[id]) {
			t.Errorf("rule %d does not run into b and was touched:\n%v\nwas\n%v", id, got, before[id])
		}
	}
	if stale, err := StaleRulesIntoNode(d, b.ID); err != nil || len(stale) != 0 {
		t.Errorf("stale into b after the rewire = %v (%v), want none", stale, err)
	}
	if nodes, err := RewireRulesIntoNode(d, b.ID); err != nil || len(nodes) != 0 {
		t.Errorf("a second rewire re-pushes %v (%v), want nothing: no rule is stale", nodes, err)
	}
}

// TestARuleThatCannotBeRewiredDoesNotHoldBackTheOthers: the address is the
// node's own report and is stored already, so a rule that cannot follow it is
// reported and left exactly as it was, while the rules that can follow do.
// It stays stale, which is what makes the next call try it again.
func TestARuleThatCannotBeRewiredDoesNotHoldBackTheOthers(t *testing.T) {
	d := openTestDB(t)
	a := relayNode(t, d, "a", "10.0.0.1")
	b := relayNode(t, d, "b", "10.0.0.2")
	c := relayNode(t, d, "c", "10.0.0.3")
	stuck := chainRule(t, d, "a-b-c", a, b, c)
	fine := chainRule(t, d, "a-b", a, b)

	// c loses its address: a rule over c cannot be generated any more.
	if err := UpdateNodeRelayHost(d, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeRelayHost(d, b.ID, "10.0.0.22"); err != nil {
		t.Fatal(err)
	}
	stuckBefore := hopLines(t, d, stuck)

	nodes, err := RewireRulesIntoNode(d, b.ID)
	var rewire *RewireError
	if !errors.As(err, &rewire) || rewire.RuleID != stuck || !IsChainError(err) {
		t.Fatalf("error = %v, want a RewireError for rule %d carrying why the chain cannot be built", err, stuck)
	}
	if got, want := sortedIDs(nodes), []int64{a.ID, b.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes to re-push = %v, want %v (the nodes of the rule that was re-generated)", got, want)
	}
	if hops, _ := ListRuleHops(d, fine); len(hops) != 2 || hops[0].TargetHost != "10.0.0.22" {
		t.Errorf("the rule that can follow was not re-generated: %v", hopLines(t, d, fine))
	}
	if got := hopLines(t, d, stuck); !reflect.DeepEqual(got, stuckBefore) {
		t.Errorf("the rule that cannot follow was changed:\n%v\nwas\n%v", got, stuckBefore)
	}
	if stale, err := StaleRulesIntoNode(d, b.ID); err != nil || !reflect.DeepEqual(stale, []int64{stuck}) {
		t.Errorf("stale into b = %v (%v), want [%d]: the rule that failed is still to be done", stale, err, stuck)
	}

	// Once the chain can be built again, the same call finishes the job.
	if err := UpdateNodeRelayHost(d, c.ID, "10.0.0.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := RewireRulesIntoNode(d, b.ID); err != nil {
		t.Fatal(err)
	}
	if hops, _ := ListRuleHops(d, stuck); len(hops) != 3 || hops[0].TargetHost != "10.0.0.22" {
		t.Errorf("the rule was not re-generated once it could be: %v", hopLines(t, d, stuck))
	}
}
