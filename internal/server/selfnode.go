package server

import (
	"database/sql"
	"fmt"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

// EnsureSelfNode upserts the panel's built-in self-node row. The panel
// always manages the daemon it runs alongside, so this row appears in
// every nodes list and the admin UI treats it like any remote agent —
// except dispatch shortcuts via the unix socket.
func EnsureSelfNode(d *sql.DB) (*db.Node, error) {
	return db.UpsertSelfNode(d)
}

// Dispatcher routes apply_ruleset deliveries. Remote nodes go through
// the WebSocket Hub; the self-node goes straight to the local daemon
// unix socket. Tests can substitute SendLocal to avoid touching the
// filesystem.
type Dispatcher struct {
	DB        *sql.DB
	Hub       *Hub
	SendLocal func(rules []nft.Rule) error // nil → use default unix socket
}

func (d *Dispatcher) Dispatch(nodeID int64, rules []nft.Rule, rev string) error {
	n, err := db.GetNode(d.DB, nodeID)
	if err != nil {
		return err
	}
	if n.NodeType == "self" {
		send := d.SendLocal
		if send == nil {
			send = sendLocalDefault
		}
		return send(rules)
	}
	if d.Hub == nil {
		return fmt.Errorf("hub not wired; cannot dispatch to remote node %d", nodeID)
	}
	return d.Hub.SendApplyRuleset(nodeID, rules, rev)
}

func sendLocalDefault(rules []nft.Rule) error {
	c, err := daemonclient.New(daemonclient.DefaultSocketPath)
	if err != nil {
		return err
	}
	return c.ApplyRuleset(rules)
}
