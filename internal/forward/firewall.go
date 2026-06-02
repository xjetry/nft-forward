package forward

import (
	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
)

// firewall drives the shim registry with both rule sets. Best-effort: a shim
// failure never fails a reconcile (the core nft table is already applied).
type firewall struct {
	shims *shim.Registry
}

func (f firewall) Sync(forwardRules []nft.Rule, listenPorts []shim.ListenPort) error {
	if f.shims == nil {
		return nil
	}
	return f.shims.SyncAll(shim.FirewallState{ForwardRules: forwardRules, ListenPorts: listenPorts})
}

func (f firewall) Cleanup() error {
	if f.shims == nil {
		return nil
	}
	return f.shims.CleanupAll()
}

func (f firewall) DetectedNames() []string {
	if f.shims == nil {
		return nil
	}
	return f.shims.DetectedNames()
}
