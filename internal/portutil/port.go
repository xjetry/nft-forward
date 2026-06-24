// Package portutil holds the dependency-free port picker shared by the panel
// (rule allocation) and the daemon (chain hops). It lives apart from internal/db
// so the daemon can pick ports without pulling in the sqlite-backed db package.
package portutil

import "math/rand"

// ChainPortMin/ChainPortMax bound the chain-hop port range used as a fallback
// when a node has no explicit port_range configured.
const (
	ChainPortMin = 10001
	ChainPortMax = 20000
)

// PickFreePort returns a port in [start,end] not present in used, or 0 when the
// range is exhausted. A random offset keeps assignment unpredictable so two
// near-simultaneous allocations don't keep colliding on the same port.
func PickFreePort(start, end int, used map[int]bool) int {
	span := end - start + 1
	if span <= 0 {
		return 0
	}
	offset := rand.Intn(span)
	for i := 0; i < span; i++ {
		p := start + ((offset + i) % span)
		if !used[p] {
			return p
		}
	}
	return 0
}
