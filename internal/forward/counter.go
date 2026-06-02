// Package forward is the data plane: it reconciles a resolved rule set onto
// the kernel (nftables DNAT + tc) and userspace (embedded TCP relay) backends
// behind one Dataplane surface. The forwarding layer is intentionally thin —
// the relay is a plain bidirectional copy, with only the minimum lifecycle
// machinery needed for correctness.
package forward

// Counter is the unified per-rule traffic counter across both backends. It is
// the data plane's public counter contract (the kernel backend maps
// nft.Counter into it; the userspace backend produces it directly). Bytes
// counts the inbound (client->target) direction only, matching nft prerouting
// counter semantics so both modes accrue tenant quota identically.
type Counter struct {
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	Bytes      int64  `json:"bytes"`
	Packets    int64  `json:"packets"`
}
