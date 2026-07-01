package db

import "testing"

func TestResolveCompositeRelayStack(t *testing.T) {
	mk := func(id int64, typ, v4, v6 string) *Node {
		return &Node{ID: id, NodeType: typ, RelayHost: v4, RelayHostV6: v6}
	}
	hop := func(comp, child int64) *NodeHop { return &NodeHop{NodeID: comp, HopNodeID: child} }

	tests := []struct {
		name        string
		nodes       []*Node
		hops        []*NodeHop
		compID      int64
		wantEntry   string
		wantEntryV6 string
		wantExitV6  string
	}{
		{
			name:      "single hop composite mirrors that node",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1"), mk(9, "composite", "", "")},
			hops:      []*NodeHop{hop(9, 1)},
			compID:    9,
			wantEntry: "10.0.0.1", wantEntryV6: "2001:db8::1", wantExitV6: "2001:db8::1",
		},
		{
			name: "multi-hop: entry from first, exit v6 from last",
			nodes: []*Node{
				mk(1, "remote", "10.0.0.1", ""),
				mk(2, "remote", "10.0.0.2", "2001:db8::2"),
				mk(9, "composite", "", ""),
			},
			hops:      []*NodeHop{hop(9, 1), hop(9, 2)},
			compID:    9,
			wantEntry: "10.0.0.1", wantEntryV6: "", wantExitV6: "2001:db8::2",
		},
		{
			name:      "composite with no hops stays empty",
			nodes:     []*Node{mk(9, "composite", "", "")},
			hops:      nil,
			compID:    9,
			wantEntry: "", wantEntryV6: "", wantExitV6: "",
		},
		{
			name:      "non-composite node is left untouched",
			nodes:     []*Node{mk(1, "remote", "10.0.0.1", "2001:db8::1")},
			hops:      nil,
			compID:    1,
			wantEntry: "", wantEntryV6: "", wantExitV6: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveCompositeRelayStack(tt.nodes, tt.hops)
			var got *Node
			for _, n := range tt.nodes {
				if n.ID == tt.compID {
					got = n
				}
			}
			if got.EntryRelayHost != tt.wantEntry {
				t.Errorf("EntryRelayHost = %q, want %q", got.EntryRelayHost, tt.wantEntry)
			}
			if got.EntryRelayHostV6 != tt.wantEntryV6 {
				t.Errorf("EntryRelayHostV6 = %q, want %q", got.EntryRelayHostV6, tt.wantEntryV6)
			}
			if got.ExitRelayHostV6 != tt.wantExitV6 {
				t.Errorf("ExitRelayHostV6 = %q, want %q", got.ExitRelayHostV6, tt.wantExitV6)
			}
		})
	}
}
