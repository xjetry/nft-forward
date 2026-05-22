package server

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
)

type Poller struct {
	DB       *sql.DB
	Pusher   *Pusher
	Interval time.Duration
	stop     chan struct{}
}

func NewPoller(d *sql.DB, p *Pusher, interval time.Duration) *Poller {
	return &Poller{
		DB:       d,
		Pusher:   p,
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
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		log.Printf("poller node=%d: %v", n.ID, err)
		return
	}
	counters, err := c.GetCounters()
	if err != nil {
		log.Printf("poller node=%d: %v", n.ID, err)
		return
	}
	_ = db.MarkNodeSeen(po.DB, n.ID)

	for _, ct := range counters {
		f, err := db.GetForwardByNodeProtoPort(po.DB, n.ID, ct.Proto, ct.ListenPort)
		if err != nil {
			continue
		}
		delta, err := db.UpdateForwardBytes(po.DB, f.ID, int64(ct.Bytes))
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
