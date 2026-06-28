// Package forward is the data plane: it reconciles a resolved rule set onto
// the kernel (nftables DNAT + tc) and userspace (embedded TCP relay) backends
// behind one Dataplane surface. The forwarding layer is intentionally thin —
// the relay is a plain bidirectional copy, with only the minimum lifecycle
// machinery needed for correctness.
package forward

// Counter is the unified per-rule traffic counter across both backends. It is
// the data plane's public counter contract (the kernel backend maps
// nft.Counter into it; the userspace backend produces it directly). BytesUp
// tracks the client-to-target (original) direction and BytesDown tracks the
// target-to-client (reply) direction. Packets is the sum across both
// directions.
type Counter struct {
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	BytesUp    int64  `json:"bytes_up"`
	BytesDown  int64  `json:"bytes_down"`
	Packets    int64  `json:"packets"`
}
