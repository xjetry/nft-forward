package daemon

import (
	"testing"

	"nft-forward/internal/forward"
)

func TestDataplane_Implementations(t *testing.T) {
	var _ Dataplane = (*fakeDataplane)(nil)
	var _ Dataplane = (*forward.Dataplane)(nil)
}
