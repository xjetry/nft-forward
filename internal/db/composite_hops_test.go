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
