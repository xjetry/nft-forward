package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"nft-forward/internal/agent"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// LocalAddrPrefix marks a node whose data plane lives in the panel process.
// We bypass HTTP entirely for these and call the embedded agent directly.
const LocalAddrPrefix = "local://"

type Pusher struct {
	DB       *sql.DB
	Client   *http.Client
	Embedded *agent.Agent
	pending  chan int64
	stop     chan struct{}
}

func NewPusher(d *sql.DB, embedded *agent.Agent) *Pusher {
	return &Pusher{
		DB:       d,
		Embedded: embedded,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
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
	if strings.HasPrefix(n.Address, LocalAddrPrefix) {
		if p.Embedded == nil {
			return fmt.Errorf("节点 %s 标记为本地，但 panel 未注册内嵌 agent", n.Name)
		}
		return p.Embedded.ApplyRules(rules)
	}
	body, err := json.Marshal(map[string]any{"rules": rules})
	if err != nil {
		return err
	}
	url := strings.TrimRight(n.Address, "/") + "/v1/apply"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("agent HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}

// Probe verifies an agent is reachable. Useful for the "node detail" page so
// the operator can confirm the install command worked.
func (p *Pusher) Probe(n *db.Node) error {
	if strings.HasPrefix(n.Address, LocalAddrPrefix) {
		_ = db.MarkNodeSeen(p.DB, n.ID)
		return nil
	}
	url := strings.TrimRight(n.Address, "/") + "/v1/status"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Secret)
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	_ = db.MarkNodeSeen(p.DB, n.ID)
	return nil
}
