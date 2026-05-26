package server

import "nft-forward/internal/nft"

// Hub is the WebSocket fan-out for remote agents. The no-op default
// exists so Dispatcher can route to a non-nil Hub in tests, and so a
// self-node-only deployment does not pay any WS cost at startup.
type Hub struct{}

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {
	return nil
}
