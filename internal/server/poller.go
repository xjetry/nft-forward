package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

type Poller struct {
	DB       *sql.DB
	Pusher   *Pusher
	Client   *http.Client
	Interval time.Duration
	stop     chan struct{}
}

func NewPoller(d *sql.DB, p *Pusher, interval time.Duration) *Poller {
	return &Poller{
		DB:       d,
		Pusher:   p,
		Client:   &http.Client{Timeout: 5 * time.Second},
		Interval: interval,
		stop:     make(chan struct{}),
	}
}

func (po *Poller) Run() {
	t := time.NewTicker(po.Interval)
	defer t.Stop()
	for {
		select {
		case <-po.stop:
			return
		case <-t.C:
			po.pollAll()
		}
	}
}

func (po *Poller) Stop() { close(po.stop) }

func (po *Poller) pollAll() {
	nodes, err := db.ListNodes(po.DB)
	if err != nil {
		log.Printf("poller: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		if n.Disabled {
			continue
		}
		po.pollOne(n)
	}
	po.enforceQuotas()
}

func (po *Poller) pollOne(n *db.Node) {
	var counters []nft.Counter
	if strings.HasPrefix(n.Address, LocalAddrPrefix) {
		if po.Pusher.Embedded == nil {
			return
		}
		got, err := po.Pusher.Embedded.GetCounters()
		if err != nil {
			log.Printf("poller node=%d (local): %v", n.ID, err)
			return
		}
		counters = got
	} else {
		url := strings.TrimRight(n.Address, "/") + "/v1/counters"
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+n.Secret)
		resp, err := po.Client.Do(req)
		if err != nil {
			log.Printf("poller node=%d: %v", n.ID, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			log.Printf("poller node=%d HTTP %d: %s", n.ID, resp.StatusCode, strings.TrimSpace(string(buf)))
			return
		}
		var body struct {
			Counters []nft.Counter `json:"counters"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			log.Printf("poller node=%d decode: %v", n.ID, err)
			return
		}
		counters = body.Counters
	}
	_ = db.MarkNodeSeen(po.DB, n.ID)

	for _, c := range counters {
		f, err := db.GetForwardByNodeProtoPort(po.DB, n.ID, c.Proto, c.ListenPort)
		if err != nil {
			continue
		}
		delta, err := db.UpdateForwardBytes(po.DB, f.ID, c.Bytes)
		if err != nil {
			log.Printf("poller: update forward %d: %v", f.ID, err)
			continue
		}
		if delta > 0 && f.TenantID.Valid {
			if err := db.AddTenantTraffic(po.DB, f.TenantID.Int64, delta); err != nil {
				log.Printf("poller: add tenant traffic: %v", err)
			}
		}
	}
}

// enforceQuotas disables tenants whose traffic exceeds their quota and asks
// the pusher to wipe their rules from each affected node. Tenants without a
// quota (quota_bytes == 0) are exempt.
func (po *Poller) enforceQuotas() {
	tenants, err := db.ListTenants(po.DB)
	if err != nil {
		return
	}
	for _, t := range tenants {
		if t.Disabled {
			continue
		}
		exceeded := t.TrafficQuotaBytes > 0 && t.TrafficUsedBytes >= t.TrafficQuotaBytes
		expired := t.ExpiresAt.Valid && t.ExpiresAt.Int64 > 0 && t.ExpiresAt.Int64 < time.Now().Unix()
		if !exceeded && !expired {
			continue
		}
		reason := ""
		if exceeded {
			reason = fmt.Sprintf("流量超额：%d / %d 字节", t.TrafficUsedBytes, t.TrafficQuotaBytes)
		} else {
			reason = "租户已过期"
		}
		if err := db.SetTenantDisabled(po.DB, t.ID, true, reason); err != nil {
			log.Printf("poller: disable tenant %d: %v", t.ID, err)
			continue
		}
		log.Printf("poller: tenant %d (%s) disabled: %s", t.ID, t.Name, reason)
		db.WriteAudit(po.DB, 0, "tenant.auto_disable", fmt.Sprintf("%d", t.ID), reason)
		nodes, _ := db.DistinctTenantNodes(po.DB, t.ID)
		for _, nid := range nodes {
			po.Pusher.Schedule(nid)
		}
	}
}
