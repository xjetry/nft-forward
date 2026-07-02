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
// for the matching pong. readerLoop processes frames serially, so the
// pong's arrival proves the prior notification has finished its DB write.
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
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node still online after replace")
	}
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
	if !hub.IsOnline(n.ID) {
		t.Fatalf("expected node %d online immediately after hello_ack", n.ID)
	}

	done := make(chan error, 1)
	go func() {
		_, e := hub.SendApplyRuleset(n.ID, []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, "rev1")
		done <- e
	}()

	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeApplyRuleset {
		t.Fatalf("expected apply_ruleset, got %s", env.Type)
	}
	ackPayload, _ := json.Marshal(wsproto.ApplyAck{Rev: "rev1", OK: true})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: ackPayload})

	if err := <-done; err != nil {
		t.Fatalf("SendApplyRuleset error: %v", err)
	}
}

// createStandaloneRuleHop creates a single-hop rule on a node and returns the
// rule ID and hop ID for testing counters/panel edits.
func createStandaloneRuleHop(t *testing.T, d *sql.DB, nodeID int64, proto string, listenPort int, targetHost string, targetPort int, ownerID sql.NullInt64) (int64, int64) {
	t.Helper()
	_ = db.UpdateNodeRelayHost(d, nodeID, "127.0.0.1")
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl := &db.Rule{NodeID: nodeID, OwnerID: ownerID, Name: "test", Proto: proto, ExitHost: targetHost, ExitPort: targetPort}
	ruleID, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = ruleID
	_, _, err = db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: nodeID, DesiredPort: listenPort}}, nil)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	hops, _ := db.ListRuleHops(d, ruleID)
	if len(hops) != 1 {
		t.Fatalf("expected 1 hop, got %d", len(hops))
	}
	return ruleID, hops[0].ID
}

func TestHubCountersUpdatesRuleHopBytes(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	_, hopID := createStandaloneRuleHop(t, hub.DB, n.ID, "tcp", 0, "10.0.0.10", 9000, sql.NullInt64{})

	// Get the actual listen port allocated
	hops, _ := db.ListRuleHopsByNode(hub.DB, n.ID)
	var listenPort int
	for _, h := range hops {
		if h.ID == hopID {
			listenPort = h.ListenPort
			break
		}
	}

	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	cf, _ := json.Marshal(wsproto.Counters{Samples: []wsproto.CounterSample{
		{ListenPort: listenPort, Proto: "tcp", BytesUp: 512, BytesDown: 512},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cf})
	cf2, _ := json.Marshal(wsproto.Counters{Samples: []wsproto.CounterSample{
		{ListenPort: listenPort, Proto: "tcp", BytesUp: 256, BytesDown: 256},
	}})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeCounters, Payload: cf2})
	syncByPing(t, c)

	hopMap, _ := db.RuleHopMapByNode(hub.DB, n.ID)
	key := "tcp/" + strconv.Itoa(listenPort)
	got := hopMap[key]
	if got == nil {
		t.Fatalf("rule hop not found for %s", key)
	}
	if got.TotalBytes != 1536 {
		t.Fatalf("expected TotalBytes 1536 (1024 + 512), got %d", got.TotalBytes)
	}
	if got.LastBytes != 512 {
		t.Fatalf("expected LastBytes 512 (most recent delta), got %d", got.LastBytes)
	}
}

func TestHubCountersAccumulatesUserTrafficAndNotifies(t *testing.T) {
	_, hub, n := newHubTestServer(t)
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(hub.DB, "acme", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.DB.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, 1000, uid); err != nil {
		t.Fatal(err)
	}
	_, hopID := createStandaloneRuleHop(t, hub.DB, n.ID, "tcp", 0, "10.0.0.20", 9100, sql.NullInt64{Int64: uid, Valid: true})

	hops, _ := db.ListRuleHopsByNode(hub.DB, n.ID)
	var listenPort int
	for _, h := range hops {
		if h.ID == hopID {
			listenPort = h.ListenPort
			break
		}
	}

	var notified []int64
	hub.OnTrafficUpdate = func(userID int64, nodeID int64) { notified = append(notified, userID) }

	const delta = int64(4096)
	hub.applyCounters(n.ID, []wsproto.CounterSample{
		{ListenPort: listenPort, Proto: "tcp", BytesUp: delta / 2, BytesDown: delta / 2},
	})

	gotUser, err := db.GetUserByID(hub.DB, uid)
	if err != nil {
		t.Fatal(err)
	}
	if gotUser.TrafficUsedBytes != delta {
		t.Fatalf("user traffic_used_bytes = %d, want %d", gotUser.TrafficUsedBytes, delta)
	}
	if len(notified) != 1 || notified[0] != uid {
		t.Fatalf("OnTrafficUpdate calls = %v, want [%d]", notified, uid)
	}
}

func TestEnforceUserQuotaDisablesOverQuotaUser(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	self, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	var dispatched bool
	s.Dispatcher.SendLocal = func(rules []nft.Rule) (string, error) {
		dispatched = true
		return "", nil
	}

	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, "over", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, 1000, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUserTraffic(d, uid, 1500); err != nil {
		t.Fatal(err)
	}
	createStandaloneRuleHop(t, d, self.ID, "tcp", 0, "10.0.0.30", 9200, sql.NullInt64{Int64: uid, Valid: true})

	s.enforceUserQuota(uid)

	got, err := db.GetUserByID(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Fatalf("expected user %d disabled after exceeding quota", uid)
	}
	if !dispatched {
		t.Fatalf("expected re-dispatch to the user's node after disabling")
	}
}

func TestEnforceUserQuotaLeavesUnderQuotaUserEnabled(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, "under", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET traffic_quota_bytes=? WHERE id=?`, 1000, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUserTraffic(d, uid, 200); err != nil {
		t.Fatal(err)
	}
	s.enforceUserQuota(uid)
	got, err := db.GetUserByID(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disabled {
		t.Fatalf("user under quota should stay enabled, got disabled")
	}
}

func seedTwoHopRuleDB(t *testing.T, d *sql.DB) (*db.Rule, int64, int64) {
	t.Helper()
	n0, _ := db.CreateNode(d, "rule-edge-0", "https://p0", "tok-rule-0")
	n1, _ := db.CreateNode(d, "rule-edge-1", "https://p1", "tok-rule-1")
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.10", n0.ID)
	d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.11", n1.ID)
	rl := &db.Rule{NodeID: n0.ID, Name: "wire", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	tx, _ := d.Begin()
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	rl.ID = id
	if _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{{NodeID: n0.ID}, {NodeID: n1.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	return rl, n0.ID, n1.ID
}

func TestHubApplyRuleHopEditSyncsUpstreamAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	rl, n0, n1 := seedTwoHopRuleDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	entry, err := hub.applyRuleHopEdit(n1, rl.ID, 11222, "kernel", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if entry == "" {
		t.Fatal("expected entry endpoint returned")
	}
	hops, _ := db.ListRuleHops(hub.DB, rl.ID)
	byNode := map[int64]*db.RuleHop{}
	for _, h := range hops {
		byNode[h.NodeID] = h
	}
	if byNode[n1].ListenPort != 11222 {
		t.Fatalf("hop n1 listen_port = %d, want 11222", byNode[n1].ListenPort)
	}
	if byNode[n0].TargetPort != 11222 {
		t.Fatalf("upstream n0 target_port = %d, want 11222", byNode[n0].TargetPort)
	}
	if len(got) == 0 {
		t.Fatal("Redispatch was not called")
	}
}

func TestHubApplyRuleHopEditRejectsForeignNode(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	rl, _, _ := seedTwoHopRuleDB(t, hub.DB)
	other, _ := db.CreateNode(hub.DB, "outsider", "https://x", "tokx")
	if _, err := hub.applyRuleHopEdit(other.ID, rl.ID, 21000, "kernel", ""); err == nil {
		t.Fatal("node not on rule must be rejected")
	}
}

func TestHubApplyRuleDeleteRemovesRuleAndRedispatches(t *testing.T) {
	_, hub, _ := newHubTestServer(t)
	rl, n0, n1 := seedTwoHopRuleDB(t, hub.DB)
	var got []int64
	hub.Redispatch = func(nodes []int64) { got = append(got, nodes...) }

	if err := hub.applyRuleDelete(n0, rl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetRule(hub.DB, rl.ID); err == nil {
		t.Fatal("rule row should be gone")
	}
	hops, _ := db.ListRuleHops(hub.DB, rl.ID)
	if len(hops) != 0 {
		t.Fatalf("rule hops should be gone, got %d", len(hops))
	}
	gotSet := map[int64]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet[n0] || !gotSet[n1] {
		t.Fatalf("Redispatch nodes = %v, want both n0=%d and n1=%d", got, n0, n1)
	}
}

func TestHubRuleHopEditMalformedPayloadAcksError(t *testing.T) {
	srv, _, _ := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	const reqID = "edit-bad"
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRuleHopEdit, ID: reqID, Payload: json.RawMessage(`"not-an-object"`)})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeRuleCmdAck {
		t.Fatalf("expected rule_cmd_ack, got %s", env.Type)
	}
	if env.ID != reqID {
		t.Fatalf("ack envelope ID = %q, want %q (must pair with request)", env.ID, reqID)
	}
	var ack wsproto.RuleCmdAck
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
	_ = recvEnvelope(t, c)

	hub.Close()

	_, _, err := c.Read(context.Background())
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected websocket.CloseError, got %v", err)
	}
	if ce.Code != websocket.StatusGoingAway {
		t.Fatalf("expected StatusGoingAway, got %v", ce.Code)
	}
}

func TestHubFillsRelayHostByFamily(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		ProbedV6: "2001:db8::1",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	// Hello handling (including fillNodeRelayHosts) runs synchronously in the
	// same goroutine before readerLoop starts, so a pong to a ping sent after
	// hello_ack proves the relay-host writes have already landed.
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "127.0.0.1" {
		t.Errorf("RelayHost = %q, want 127.0.0.1 (from connectIP, this conn is v4)", got.RelayHost)
	}
	if got.RelayHostV6 != "2001:db8::1" {
		t.Errorf("RelayHostV6 = %q, want 2001:db8::1 (from agent self-probe, connectIP didn't cover v6)", got.RelayHostV6)
	}
}

func TestHubReconcilesDirtyV6RelayHostOnConnect(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	// Simulate historical data written before relay_host was family-aware:
	// a pure-v6 node's connect IP was once stuffed straight into relay_host.
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, "2001:db8::dead"); err != nil {
		t.Fatal(err)
	}
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		ProbedV4: "198.51.100.1",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::dead" {
		t.Errorf("RelayHostV6 = %q, want 2001:db8::dead (migrated from the stale relay_host value)", got.RelayHostV6)
	}
	if got.RelayHost != "127.0.0.1" {
		t.Errorf("RelayHost = %q, want 127.0.0.1 (re-seeded from connectIP after the dirty v6 value was evicted)", got.RelayHost)
	}
}

// dialWSWithForwardedFor dials like dialWS but sets X-Forwarded-For on the
// handshake request, letting a test simulate a v6 connectIP without needing
// an actual v6-reachable listener (httptest.Server only binds v4 loopback).
func dialWSWithForwardedFor(t *testing.T, srv *httptest.Server, forwardedFor string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"X-Forwarded-For": []string{forwardedFor}}}
	c, _, err := websocket.Dial(context.Background(), url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// TestHubPrefersFreshConnectIPOverStaleV6RelayHost covers a node whose
// relay_host still holds pre-split v6 data (see fillNodeRelayHosts) that
// reconnects with a v6 connectIP different from the stale value. The
// connectIP observed on THIS connection must win relay_host_v6, since the
// agent's real v6 address may have moved on since the stale data was
// written; the old value must not be allowed to claim the field first and
// block the fresher one from ever landing.
func TestHubPrefersFreshConnectIPOverStaleV6RelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	const staleV6 = "2001:db8::dead" // pre-split leftover, sitting in relay_host
	const freshV6 = "2001:db8::cafe" // this connection's observed connectIP
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, staleV6); err != nil {
		t.Fatal(err)
	}
	c := dialWSWithForwardedFor(t, srv, freshV6)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != freshV6 {
		t.Errorf("RelayHostV6 = %q, want %s (this connection's connectIP, not the stale relay_host value)", got.RelayHostV6, freshV6)
	}
	if got.RelayHost != "" {
		t.Errorf("RelayHost = %q, want empty (stale v6 literal evicted, no v4 source available to re-fill it)", got.RelayHost)
	}
}

func TestHubNeverOverwritesManualRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		ProbedV4: "198.51.100.1",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.9" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.9 (manual value must not be overwritten)", got.RelayHost)
	}
}

func TestHubAppliesDeclaredRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want 203.0.113.50 (declared value)", got.RelayHost)
	}
	if !got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be true after a hello carrying DeclaredRelayHost")
	}
}

func TestHubDeclaredRelayHostOverridesExistingValue(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	if err := db.UpdateNodeRelayHost(hub.DB, n.ID, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want 203.0.113.50 (declared value must override a pre-existing one)", got.RelayHost)
	}
}

func TestHubIgnoresInvalidDeclaredRelayHost(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "2001:db8::1", // v6 literal is invalid for the v4 field
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "" {
		t.Errorf("RelayHost = %q, want empty (invalid declared value must be ignored)", got.RelayHost)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should stay false when the declared value was rejected")
	}
}

func TestHubClearingDeclaredRelayHostUnlocksValue(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHost: "203.0.113.50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	// Reconnect without a declared value, as if the operator removed the
	// --relay-host flag and restarted the daemon.
	c2 := dialWS(t, srv)
	hp2, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c2, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp2})
	_ = recvEnvelope(t, c2)
	syncByPing(t, c2)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be false after a hello with no declared value")
	}
	if got.RelayHost != "203.0.113.50" {
		t.Errorf("RelayHost = %q, want unchanged 203.0.113.50 (unlocking must not blank the field)", got.RelayHost)
	}
}

func TestHubAppliesDeclaredRelayHostV6(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{
		NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64",
		DeclaredRelayHostV6: "2001:db8::50",
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)
	syncByPing(t, c)

	got, err := db.GetNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::50" {
		t.Errorf("RelayHostV6 = %q, want 2001:db8::50 (declared value)", got.RelayHostV6)
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should be true after a hello carrying DeclaredRelayHostV6")
	}
}
