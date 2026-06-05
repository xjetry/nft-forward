package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

func newHubTestServer(t *testing.T) (*httptest.Server, *Hub, *db.Node) {
	t.Helper()
	d := openDB(t)
	n, err := db.CreateNode(d, "edge-1", "https://panel.example.com", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(d)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(srv.Close)
	return srv, hub, n
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// syncByPing sends a ping after a one-way notification frame and waits
// for the matching pong. readerLoop is a single goroutine that processes
// frames serially, so the pong's arrival proves the prior notification
// has finished its DB write — no fixed sleep needed.
func syncByPing(t *testing.T, c *websocket.Conn) {
	t.Helper()
	p, _ := json.Marshal(wsproto.Ping{TS: time.Now().UnixMilli()})
	id := "sync-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypePing, ID: id, Payload: p})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypePong {
		t.Fatalf("expected pong from sync ping, got %s", env.Type)
	}
}

func recvEnvelope(t *testing.T, c *websocket.Conn) wsproto.Envelope {
	t.Helper()
	_, b, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var e wsproto.Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, string(b))
	}
	return e
}

func TestHubRejectsBadToken(t *testing.T) {
	srv, _, _ := newHubTestServer(t)
	c := dialWS(t, srv)
	hello := wsproto.Hello{NodeToken: "tok-bad", AgentVersion: "v1", OS: "linux", Arch: "amd64"}
	hp, _ := json.Marshal(hello)
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", env.Type)
	}
	var ack wsproto.HelloAck
	json.Unmarshal(env.Payload, &ack)
	if ack.Error == "" {
		t.Fatalf("expected error in hello_ack for bad token, got %+v", ack)
	}
}

func TestHubAcceptsGoodToken(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1.0", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	env := recvEnvelope(t, c)
	var ack wsproto.HelloAck
	json.Unmarshal(env.Payload, &ack)
	if ack.NodeID != n.ID || ack.Error != "" {
		t.Fatalf("hello_ack mismatch: %+v", ack)
	}
	// hello_ack only ships after registerConn has put the conn in the hub
	// map, so the IsOnline check needs no wait.
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node %d online after hello_ack", n.ID)
	}
}

func TestHubSecondConnReplacesFirst(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c1 := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c1, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c1)
	c2 := dialWS(t, srv)
	sendJSON(t, c2, wsproto.Envelope{Type: wsproto.TypeHello, ID: "2", Payload: hp})
	_ = recvEnvelope(t, c2)
	// hello_ack arrives after the second conn has supplanted the first in
	// the hub map; no wait needed before IsOnline.
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node still online after replace")
	}
	// c1 should now read EOF / closed.
	_, _, err := c1.Read(context.Background())
	if err == nil {
		t.Fatalf("expected first conn to be closed after second hello")
	}
}

func TestHubSendApplyRulesetReturnsAck(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	// hello_ack arrives only after the conn is in the hub map, so the
	// lookup in SendApplyRuleset is guaranteed to find it without any
	// wait.
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node %d online immediately after hello_ack", n.ID)
	}

	// In a goroutine, server SendApplyRuleset and wait for ack.
	done := make(chan error, 1)
	go func() {
		done <- hub.SendApplyRuleset(n.ID, []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, "rev1")
	}()

	// Client reads the apply_ruleset frame.
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeApplyRuleset {
		t.Fatalf("expected apply_ruleset, got %s", env.Type)
	}
	// Client sends apply_ack.
	ackPayload, _ := json.Marshal(wsproto.ApplyAck{Rev: "rev1", OK: true})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: ackPayload})

	if err := <-done; err != nil {
		t.Fatalf("SendApplyRuleset error: %v", err)
	}
}

func TestHubCountersUpdatesForwardBytes(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	// Pre-create a forward in the DB matching what the agent will report.
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID:     n.ID,
		Proto:      "tcp",
		ListenPort: 9000,
		TargetIP:   "10.0.0.10",
		TargetPort: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}

	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	// First counters frame: total += 1024.
	cf, _ := json.Marshal(wsproto.Counters{Samples: []wsproto.CounterSample{
		{ListenPort: 9000, Proto: "tcp", BytesDelta: 1024},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cf})
	// Second frame: total += 512.
	cf2, _ := json.Marshal(wsproto.Counters{Samples: []wsproto.CounterSample{
		{ListenPort: 9000, Proto: "tcp", BytesDelta: 512},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cf2})
	syncByPing(t, c)

	got, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalBytes != 1536 {
		t.Fatalf("expected TotalBytes 1536 (1024 + 512), got %d", got.TotalBytes)
	}
	if got.LastBytes != 512 {
		t.Fatalf("expected LastBytes 512 (most recent delta), got %d", got.LastBytes)
	}
}

func TestHubCountersAccumulatesTenantTrafficAndNotifies(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	tid, err := db.CreateTenant(hub.DB, &db.Tenant{Name: "acme", TrafficQuotaBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID:     n.ID,
		TenantID:   sql.NullInt64{Int64: tid, Valid: true},
		Proto:      "tcp",
		ListenPort: 9100,
		TargetIP:   "10.0.0.20",
		TargetPort: 9100,
	})
	if err != nil {
		t.Fatal(err)
	}

	var notified []int64
	hub.OnTrafficUpdate = func(tenantID int64) { notified = append(notified, tenantID) }

	const delta = int64(4096)
	hub.applyCounters(n.ID, []wsproto.CounterSample{
		{ListenPort: 9100, Proto: "tcp", BytesDelta: delta},
	})

	gotFwd, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if gotFwd.TotalBytes != delta {
		t.Fatalf("forward total_bytes = %d, want %d", gotFwd.TotalBytes, delta)
	}
	gotTenant, err := db.GetTenant(hub.DB, tid)
	if err != nil {
		t.Fatal(err)
	}
	if gotTenant.TrafficUsedBytes != delta {
		t.Fatalf("tenant traffic_used_bytes = %d, want %d", gotTenant.TrafficUsedBytes, delta)
	}
	if len(notified) != 1 || notified[0] != tid {
		t.Fatalf("OnTrafficUpdate calls = %v, want [%d]", notified, tid)
	}
	_ = srv
}

func TestEnforceTenantQuotaDisablesOverQuotaTenant(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	// Route the tenant's forward through the self-node so the re-dispatch the
	// enforcer triggers hits the stubbed local sender instead of a real socket.
	self, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	var dispatched bool
	s.Dispatcher.SendLocal = func(rules []nft.Rule) error {
		dispatched = true
		return nil
	}

	tid, err := db.CreateTenant(d, &db.Tenant{Name: "over", TrafficQuotaBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddTenantTraffic(d, tid, 1500); err != nil { // push usage past the quota
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID:     self.ID,
		TenantID:   sql.NullInt64{Int64: tid, Valid: true},
		Proto:      "tcp",
		ListenPort: 9200,
		TargetIP:   "10.0.0.30",
		TargetPort: 9200,
	}); err != nil {
		t.Fatal(err)
	}

	s.enforceTenantQuota(tid)

	got, err := db.GetTenant(d, tid)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Fatalf("expected tenant %d disabled after exceeding quota", tid)
	}
	if !dispatched {
		t.Fatalf("expected re-dispatch to the tenant's node after disabling")
	}
}

func TestEnforceTenantQuotaLeavesUnderQuotaTenantEnabled(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	tid, err := db.CreateTenant(d, &db.Tenant{Name: "under", TrafficQuotaBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddTenantTraffic(d, tid, 200); err != nil {
		t.Fatal(err)
	}
	s.enforceTenantQuota(tid)
	got, err := db.GetTenant(d, tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disabled {
		t.Fatalf("tenant under quota should stay enabled, got disabled")
	}
}

func TestHubPanelSegmentEditUpdatesNonChainForward(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.1", TargetPort: 30000, Comment: "old", Mode: "kernel",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	pse, _ := json.Marshal(wsproto.PanelSegmentEdit{Forwards: []wsproto.Forward{
		{Proto: "tcp", ListenPort: 30000, TargetIP: "10.9.9.9", TargetPort: 8443, Comment: "new", Mode: "userspace"},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypePanelSegmentEdit, Payload: pse})
	syncByPing(t, c)

	got, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "10.9.9.9" || got.TargetPort != 8443 || got.Comment != "new" || got.Mode != "userspace" {
		t.Fatalf("panel edit not persisted: %+v", got)
	}
}

func TestHubPanelSegmentEditIgnoresChainForward(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	res, err := hub.DB.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	fid, err := db.CreateForward(hub.DB, &db.Forward{
		NodeID: n.ID, Proto: "tcp", ListenPort: 20001, TargetIP: "5.6.7.8", TargetPort: 20002,
		ChainID: sql.NullInt64{Int64: cid, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A chained hop reported with a tampered target must be rejected.
	hub.applyPanelEdits(n.ID, []wsproto.Forward{
		{Proto: "tcp", ListenPort: 20001, TargetIP: "1.1.1.1", TargetPort: 1, Comment: "hijack"},
	})

	got, err := db.GetForward(hub.DB, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "5.6.7.8" || got.TargetPort != 20002 {
		t.Fatalf("chain hop port/target must stay intact: %+v", got)
	}
	_ = srv
}

func TestHubPanelSegmentEditSkipsUnknownForward(t *testing.T) {
	_, hub, n := newHubTestServer(t)
	// No matching forward row exists; applyPanelEdits must not error/panic.
	hub.applyPanelEdits(n.ID, []wsproto.Forward{
		{Proto: "tcp", ListenPort: 65000, TargetIP: "10.0.0.1", TargetPort: 65000},
	})
}

func seedTwoHopChainDB(t *testing.T, d *sql.DB) (*db.Chain, int64, int64) {
	t.Helper()
	n0, _ := db.CreateNode(d, "chain-edge-0", "https://p0", "tok-chain-0")
	n1, _ := db.CreateNode(d, "chain-edge-1", "https://p1", "tok-chain-1")
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.10", n0.ID)
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.11", n1.ID)
	c := &db.Chain{Name: "wire", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	id, err := db.CreateChain(d, c)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id
	tx, _ := d.Begin()
	if _, _, err := db.RegenerateChain(tx, c, []db.HopInput{{NodeID: n0.ID}, {NodeID: n1.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	return c, n0.ID, n1.ID
}

func TestHubApplyChainHopEditSyncsUpstreamAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, n0, n1 := seedTwoHopChainDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	entry, err := hub.applyChainHopEdit(n1, c.ID, 21222, "kernel", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if entry == "" {
		t.Fatal("expected entry endpoint returned")
	}
	fwds, _ := db.ListForwardsByChain(hub.DB, c.ID)
	byNode := map[int64]*db.Forward{}
	for _, f := range fwds {
		byNode[f.NodeID] = f
	}
	if byNode[n1].ListenPort != 21222 {
		t.Fatalf("hop n1 listen_port = %d, want 21222", byNode[n1].ListenPort)
	}
	if byNode[n0].TargetPort != 21222 {
		t.Fatalf("upstream n0 target_port = %d, want 21222", byNode[n0].TargetPort)
	}
	if len(got) == 0 {
		t.Fatal("Redispatch was not called")
	}
}

func TestHubApplyChainHopEditRejectsForeignNode(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, _, _ := seedTwoHopChainDB(t, hub.DB)
	other, _ := db.CreateNode(hub.DB, "outsider", "https://x", "tokx")
	if _, err := hub.applyChainHopEdit(other.ID, c.ID, 21000, "kernel", ""); err == nil {
		t.Fatal("node not on chain must be rejected")
	}
}

func TestHubApplyChainDeleteRemovesChainAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	c, n0, n1 := seedTwoHopChainDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	if err := hub.applyChainDelete(n0, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetChain(hub.DB, c.ID); err == nil {
		t.Fatal("chain row should be gone")
	}
	fwds, _ := db.ListForwardsByChain(hub.DB, c.ID)
	if len(fwds) != 0 {
		t.Fatalf("chain forwards should be gone, got %d", len(fwds))
	}
	// Redispatch must receive the full set of nodes whose forwards were
	// removed — both the node that asked and the other hop — so every node
	// drops the deleted rules from its kernel.
	gotSet := map[int64]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet[n0] || !gotSet[n1] {
		t.Fatalf("Redispatch nodes = %v, want both n0=%d and n1=%d", got, n0, n1)
	}
}

func TestHubChainHopEditMalformedPayloadAcksError(t *testing.T) {
	srv, _, _ := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c) // hello_ack

	// A JSON string is a valid envelope payload but can't decode into the
	// ChainHopEdit object, so it drives the malformed-payload branch.
	const reqID = "edit-bad"
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeChainHopEdit, ID: reqID, Payload: json.RawMessage(`"not-an-object"`)})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeChainCmdAck {
		t.Fatalf("expected chain_cmd_ack, got %s", env.Type)
	}
	if env.ID != reqID {
		t.Fatalf("ack envelope ID = %q, want %q (must pair with request)", env.ID, reqID)
	}
	var ack wsproto.ChainCmdAck
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.OK || ack.Error == "" {
		t.Fatalf("expected failed ack with error message, got %+v", ack)
	}
}

func TestHubCloseSendsGoingAway(t *testing.T) {
	srv, hub, _ := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c) // hello_ack

	hub.Close()

	// The client's next read should observe a close frame with StatusGoingAway.
	_, _, err := c.Read(context.Background())
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected websocket.CloseError, got %v", err)
	}
	if ce.Code != websocket.StatusGoingAway {
		t.Fatalf("expected StatusGoingAway, got %v", ce.Code)
	}
}
