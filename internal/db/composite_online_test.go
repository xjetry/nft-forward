package db

import "testing"

func TestResolveCompositeOnline(t *testing.T) {
	mk := func(id int64, typ string, online int, disabled bool) *Node {
		return &Node{ID: id, NodeType: typ, Online: online, Disabled: disabled}
	}
	hop := func(comp, child int64) *NodeHop { return &NodeHop{NodeID: comp, HopNodeID: child} }

	tests := []struct {
		name  string
		nodes []*Node
		hops  []*NodeHop
		// compID -> expected online after resolution
		want map[int64]int
	}{
		{
			name:  "all children online => composite online",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 1, false), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]int{3: 1},
		},
		{
			name:  "one child offline => composite offline",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 0, false), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]int{3: 0},
		},
		{
			name:  "disabled child counts as offline",
			nodes: []*Node{mk(1, "remote", 1, false), mk(2, "remote", 1, true), mk(3, "composite", 0, false)},
			hops:  []*NodeHop{hop(3, 1), hop(3, 2)},
			want:  map[int64]int{3: 0},
		},
		{
			name:  "composite with no children => offline",
			nodes: []*Node{mk(3, "composite", 1, false)},
			hops:  nil,
			want:  map[int64]int{3: 0},
		},
		{
			name: "multiple composites resolved independently",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 0, false),
				mk(3, "composite", 0, false), mk(4, "composite", 0, false),
			},
			hops: []*NodeHop{hop(3, 1), hop(4, 1), hop(4, 2)},
			want: map[int64]int{3: 1, 4: 0},
		},
		{
			name: "nested composite online when every deep leaf online",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 1, false), mk(3, "remote", 1, false),
				mk(8, "composite", 0, false), mk(9, "composite", 0, false),
			},
			hops: []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			want: map[int64]int{8: 1, 9: 1},
		},
		{
			name: "nested composite offline when a deep leaf offline",
			nodes: []*Node{
				mk(1, "remote", 1, false), mk(2, "remote", 0, false), mk(3, "remote", 1, false),
				mk(8, "composite", 0, false), mk(9, "composite", 0, false),
			},
			hops: []*NodeHop{hop(9, 8), hop(9, 3), hop(8, 1), hop(8, 2)},
			want: map[int64]int{8: 0, 9: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveCompositeOnline(tt.nodes, tt.hops)
			for _, n := range tt.nodes {
				if exp, ok := tt.want[n.ID]; ok && n.Online != exp {
					t.Errorf("node %d online = %d, want %d", n.ID, n.Online, exp)
				}
			}
		})
	}
}
