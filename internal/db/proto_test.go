package db

import (
	"strings"
	"testing"
)

// The counter-key fan-out and the port-occupancy overlap are both derived from
// protoNamespaces, but the invariant that keeps per-hop counter keys unique
// across a node is subtle: every proto a hop produces a counter key for must be
// one the occupancy check would have reserved. If that ever stops holding, two
// distinct hops on the same node could share a key and bytes would be
// misattributed. Lock it.
func TestHopCounterKeysWithinOccupancy(t *testing.T) {
	for _, proto := range storedProtos {
		occ := map[string]bool{}
		for _, p := range overlappingProtos(proto) {
			occ[p] = true
		}
		for _, key := range hopCounterKeys(proto, 1) {
			kp := strings.SplitN(key, "/", 2)[0]
			if !occ[kp] {
				t.Errorf("proto %s: counter key proto %q is not in the occupancy set %v; keys could collide across hops", proto, kp, occ)
			}
		}
	}
}

func TestProtoNamespaces(t *testing.T) {
	cases := map[string][]string{
		"tcp":     {"tcp"},
		"udp":     {"udp"},
		"tcp+udp": {"tcp", "udp"},
	}
	for proto, want := range cases {
		got := protoNamespaces(proto)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("protoNamespaces(%q) = %v, want %v", proto, got, want)
		}
	}
}
