package server

import "nft-forward/internal/nft"

// Hub is implemented in full in a later commit. This stub exists so
// selfnode.go compiles and the Dispatcher's remote-node branch can be
// exercised in tests without the WebSocket machinery.
type Hub struct{}

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {
	return nil
}
