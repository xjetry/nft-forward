package db

import "testing"

func TestFillCompositeRawTraffic(t *testing.T) {
	mk := func(id int64, typ string) *Node { return &Node{ID: id, NodeType: typ} }
	hop := func(comp, child int64) *NodeHop { return &NodeHop{NodeID: comp, HopNodeID: child} }

	tests := []struct {
		name   string
		nodes  []*Node
		hops   []*NodeHop
		raw    map[int64]int64
		compID int64
		want   int64
	}{
		{
			name:   "composite takes entry physical child's raw",
			nodes:  []*Node{mk(1, "remote"), mk(2, "remote"), mk(9, "composite")},
			hops:   []*NodeHop{hop(9, 1), hop(9, 2)},
			raw:    map[int64]int64{1: 100, 2: 200},
			compID: 9,
			want:   100,
		},
		{
			name:   "entry never forwarded resolves to 0",
			nodes:  []*Node{mk(1, "remote"), mk(9, "composite")},
			hops:   []*NodeHop{hop(9, 1)},
			raw:    map[int64]int64{},
			compID: 9,
			want:   0,
		},
		{
			name:   "composite with no hops stays absent",
			nodes:  []*Node{mk(9, "composite")},
			hops:   nil,
			raw:    map[int64]int64{},
			compID: 9,
			want:   0,
		},
		{
			name: "nested composite takes deep first physical leaf",
			// outer(9) = [inner(8), x(3)]; inner(8) = [a(1), b(2)] => entry is a(1)
			nodes:  []*Node{mk(1, "remote"), mk(2, "remote"), mk(3, "remote"), mk(8, "composite"), mk(9, "composite")},
			hops:   []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			raw:    map[int64]int64{1: 42, 2: 7, 3: 999},
			compID: 9,
			want:   42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fillCompositeRawTraffic(tt.nodes, tt.hops, tt.raw)
			if got := tt.raw[tt.compID]; got != tt.want {
				t.Errorf("raw[%d] = %d, want %d", tt.compID, got, tt.want)
			}
		})
	}
}

// A physical node's own raw must never be rewritten by the composite fill.
func TestFillCompositeRawTrafficLeavesPhysicalUntouched(t *testing.T) {
	nodes := []*Node{{ID: 1, NodeType: "remote"}, {ID: 9, NodeType: "composite"}}
	hops := []*NodeHop{{NodeID: 9, HopNodeID: 1}}
	raw := map[int64]int64{1: 500}
	fillCompositeRawTraffic(nodes, hops, raw)
	if raw[1] != 500 {
		t.Errorf("physical raw[1] = %d, want 500", raw[1])
	}
	if raw[9] != 500 {
		t.Errorf("composite raw[9] = %d, want 500 (entry child)", raw[9])
	}
}
