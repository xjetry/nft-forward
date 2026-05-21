package daemon

import (
	"testing"

	"nft-forward/internal/nft"
)

type fakeApplier struct {
	last []nft.Rule
	err  error
}

func (f *fakeApplier) Apply(rules []nft.Rule) error {
	f.last = append([]nft.Rule(nil), rules...)
	return f.err
}

func TestApplier_FakeAndDefaultSatisfyInterface(t *testing.T) {
	var _ Applier = (*fakeApplier)(nil)
	var _ Applier = nftApplier{}
}
