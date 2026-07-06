package db

import "testing"

func TestResolveCompositeHops(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "hk-a", NodeType: "remote"},
		{ID: 2, Name: "hk-b", NodeType: "remote"},
		{ID: 9, Name: "combo", NodeType: "composite"},
		{ID: 7, Name: "solo", NodeType: "remote"},
	}
	hops := []*NodeHop{
		{NodeID: 9, Position: 0, HopNodeID: 1, Mode: "kernel"},
		{NodeID: 9, Position: 1, HopNodeID: 2, Mode: "userspace"},
	}

	resolveCompositeHops(nodes, hops)

	byID := map[int64]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}

	// The composite gets its ordered members, resolved to name/type/mode.
	got := byID[9].Hops
	if len(got) != 2 {
		t.Fatalf("composite members: got %d, want 2", len(got))
	}
	if got[0].NodeID != 1 || got[0].Name != "hk-a" || got[0].Mode != "kernel" {
		t.Errorf("member 0 = %+v, want {1 hk-a remote kernel}", got[0])
	}
	if got[1].NodeID != 2 || got[1].Name != "hk-b" || got[1].Mode != "userspace" {
		t.Errorf("member 1 = %+v, want {2 hk-b remote userspace}", got[1])
	}

	// A single node is left without members.
	if byID[7].Hops != nil {
		t.Errorf("single node solo should have no Hops, got %+v", byID[7].Hops)
	}
}

// A composite whose member is itself a composite flattens recursively to the
// physical leaf chain — inner composites are expanded away, never shown as
// members, and each leaf keeps the mode of the innermost composite that owns it.
func TestResolveCompositeHopsNested(t *testing.T) {
	nodes := []*Node{
		{ID: 1, Name: "a", NodeType: "remote"},
		{ID: 2, Name: "b", NodeType: "remote"},
		{ID: 3, Name: "x", NodeType: "remote"},
		{ID: 8, Name: "inner", NodeType: "composite"},
		{ID: 9, Name: "outer", NodeType: "composite"},
	}
	hops := []*NodeHop{
		{NodeID: 9, Position: 0, HopNodeID: 8, Mode: "kernel"},    // outer -> inner (mode dormant: inner is a black box)
		{NodeID: 9, Position: 1, HopNodeID: 3, Mode: "userspace"}, // outer -> x
		{NodeID: 8, Position: 0, HopNodeID: 1, Mode: "kernel"},    // inner -> a
		{NodeID: 8, Position: 1, HopNodeID: 2, Mode: "userspace"}, // inner -> b
	}

	resolveCompositeHops(nodes, hops)

	byID := map[int64]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	// inner resolves to its two physical children.
	if got := byID[8].Hops; len(got) != 2 || got[0].NodeID != 1 || got[1].NodeID != 2 {
		t.Fatalf("inner hops = %+v, want [a b]", got)
	}
	// outer flattens recursively to [a, b, x]; the inner composite is expanded away.
	got := byID[9].Hops
	if len(got) != 3 {
		t.Fatalf("outer hops: got %d, want 3 (%+v)", len(got), got)
	}
	if got[0].NodeID != 1 || got[0].Name != "a" || got[0].Mode != "kernel" {
		t.Errorf("leaf 0 = %+v, want {1 a kernel}", got[0])
	}
	if got[1].NodeID != 2 || got[1].Name != "b" || got[1].Mode != "userspace" {
		t.Errorf("leaf 1 = %+v, want {2 b userspace}", got[1])
	}
	if got[2].NodeID != 3 || got[2].Name != "x" || got[2].Mode != "userspace" {
		t.Errorf("leaf 2 = %+v, want {3 x userspace}", got[2])
	}
	for _, m := range got {
		if m.NodeType == "composite" {
			t.Errorf("flattened member must be physical, got composite %+v", m)
		}
	}
}
