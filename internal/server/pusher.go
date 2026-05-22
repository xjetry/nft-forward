package server

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

type Pusher struct {
	DB      *sql.DB
	pending chan int64
	stop    chan struct{}
}

func NewPusher(d *sql.DB) *Pusher {
	return &Pusher{
		DB:      d,
		pending: make(chan int64, 256),
		stop:    make(chan struct{}),
	}
}

// Schedule asks the background worker to push the desired ruleset for nodeID.
// Non-blocking; if the channel is full the node is marked dirty so the
// periodic reconcile picks it up.
func (p *Pusher) Schedule(nodeID int64) {
	select {
	case p.pending <- nodeID:
	default:
		_ = db.MarkNodeError(p.DB, nodeID, "队列已满，等待重试")
	}
}

func (p *Pusher) Run() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case id := <-p.pending:
			p.pushOne(id)
		case <-tick.C:
			p.reconcile()
		}
	}
}

func (p *Pusher) Stop() { close(p.stop) }

func (p *Pusher) reconcile() {
	rows, err := p.DB.Query(`SELECT id FROM nodes WHERE dirty=1 AND disabled=0`)
	if err != nil {
		log.Printf("reconcile query: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		p.pushOne(id)
	}
}

func (p *Pusher) pushOne(nodeID int64) {
	n, err := db.GetNode(p.DB, nodeID)
	if err != nil {
		log.Printf("push: node %d: %v", nodeID, err)
		return
	}
	if n.Disabled {
		return
	}
	forwards, err := db.ActiveForwardsForPush(p.DB, nodeID)
	if err != nil {
		_ = db.MarkNodeError(p.DB, nodeID, err.Error())
		return
	}
	// Look up each forward's tunnel once so we can stamp the bandwidth on the
	// pushed rule. Forwards without a tunnel are unmetered (admin-mode).
	tunnels := map[int64]*db.Tunnel{}
	rules := make([]nft.Rule, 0, len(forwards))
	for _, f := range forwards {
		bw := 0
		if f.TunnelID.Valid {
			t, ok := tunnels[f.TunnelID.Int64]
			if !ok {
				t, _ = db.GetTunnel(p.DB, f.TunnelID.Int64)
				if t != nil {
					tunnels[f.TunnelID.Int64] = t
				}
			}
			if t != nil {
				bw = t.BandwidthMbps
			}
		}
		rule := nft.Rule{
			Proto:         f.Proto,
			SrcPort:       f.ListenPort,
			DestPort:      f.TargetPort,
			Comment:       f.Comment,
			BandwidthMbps: bw,
		}
		if resolver.IsHostname(f.TargetIP) {
			rule.DestHost = f.TargetIP
		} else {
			rule.DestIP = f.TargetIP
		}
		rules = append(rules, rule)
	}
	if err := p.send(n, rules); err != nil {
		log.Printf("push node=%d (%s): %v", n.ID, n.Name, err)
		_ = db.MarkNodeError(p.DB, n.ID, err.Error())
		return
	}
	_ = db.MarkNodeApplied(p.DB, n.ID)
	log.Printf("push node=%d (%s): applied %d rule(s)", n.ID, n.Name, len(rules))
}

func (p *Pusher) send(n *db.Node, rules []nft.Rule) error {
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		return fmt.Errorf("dial %s: %w", n.Address, err)
	}
	return c.PostRuleset("panel", rules)
}

// Probe verifies an agent is reachable. Useful for the "node detail" page so
// the operator can confirm the install command worked.
func (p *Pusher) Probe(n *db.Node) error {
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		return err
	}
	if err := c.Health(); err != nil {
		return err
	}
	_ = db.MarkNodeSeen(p.DB, n.ID)
	return nil
}
