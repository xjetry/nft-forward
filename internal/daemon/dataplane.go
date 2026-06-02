package daemon

import (
	"context"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
)

// Dataplane is the data-plane seam the daemon depends on. Production wires
// *forward.Dataplane; tests substitute a fake. The daemon owns this
// (consumer-defined) interface so the dependency points daemon -> forward.
type Dataplane interface {
	Reconcile(ctx context.Context, rules []nft.Rule) error
	Counters() ([]forward.Counter, error)
	Close(ctx context.Context) error
}
