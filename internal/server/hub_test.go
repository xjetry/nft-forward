package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// Wait briefly for register goroutine to run.
	time.Sleep(50 * time.Millisecond)
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
	time.Sleep(50 * time.Millisecond)
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

func TestHubRegisterLocalInsertsAndIsIdempotent(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	// First register_local: should INSERT.
	rl, _ := json.Marshal(wsproto.RegisterLocal{
		Forwards: []wsproto.Forward{{Proto: "tcp", ListenPort: 80, TargetIP: "10.0.0.1", TargetPort: 80}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "r1", Payload: rl})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeRegisterLocalAck {
		t.Fatalf("expected register_local_ack, got %s", env.Type)
	}
	var ack wsproto.RegisterLocalAck
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if len(ack.Imported) != 1 {
		t.Fatalf("expected 1 imported, got %d", len(ack.Imported))
	}

	// Second register_local with different forwards: must be no-op
	// (the local_migrated_at stamp from the first call is the idempotency
	// anchor; a retried frame still gets an ack so the agent can clear
	// its tui segment).
	rl2, _ := json.Marshal(wsproto.RegisterLocal{
		Forwards: []wsproto.Forward{{Proto: "udp", ListenPort: 53, TargetIP: "10.0.0.2", TargetPort: 53}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeRegisterLocal, ID: "r2", Payload: rl2})
	env = recvEnvelope(t, c)
	var ack2 wsproto.RegisterLocalAck
	if err := json.Unmarshal(env.Payload, &ack2); err != nil {
		t.Fatal(err)
	}
	if len(ack2.Imported) != 0 {
		t.Fatalf("expected idempotent empty imported on 2nd call, got %d", len(ack2.Imported))
	}

	// Verify only the first forward made it to DB.
	fws, err := db.ListForwardsByNode(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fws) != 1 || fws[0].ListenPort != 80 {
		t.Fatalf("DB should have exactly 1 forward (listen 80), got %+v", fws)
	}
}

func TestHubTuiSegmentChangedUpsertsSnapshot(t *testing.T) {
	srv, hub, n := newHubTestServer(t)
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	_ = recvEnvelope(t, c)

	tsc, _ := json.Marshal(wsproto.TuiSegmentChanged{
		Forwards: []wsproto.Forward{{Proto: "tcp", ListenPort: 443, TargetIP: "10.0.0.3", TargetPort: 443}},
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeTuiSegmentChanged, Payload: tsc})
	time.Sleep(100 * time.Millisecond)

	got, _, err := db.GetTuiSnapshot(hub.DB, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "10.0.0.3") {
		t.Fatalf("expected snapshot to contain target IP, got %q", got)
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
	time.Sleep(100 * time.Millisecond)

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
