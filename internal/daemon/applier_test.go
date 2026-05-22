package daemon

import (
	"testing"
)

func TestApplier_FakeAndDefaultSatisfyInterface(t *testing.T) {
	var _ Applier = (*fakeApplier)(nil)
	var _ Applier = nftApplier{}
}
