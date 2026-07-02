package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// fakeHub is a minimal test double that accepts a WS connection,
// reads frames, and exposes recorded frames via Recorder().
type fakeHub struct {
	mu       sync.Mutex
	frames   []wsproto.Envelope
	ackHooks map[string]func(env wsproto.Envelope) wsproto.Envelope // id-template -> response
	conn     *websocket.Conn                                        // most-recent accepted connection, for tests that drop it
}

func newFakeHub() *fakeHub {
	return &fakeHub{ackHooks: make(map[string]func(wsproto.Envelope) wsproto.Envelope)}
}

func (f *fakeHub) onAck(reqType string, respond func(wsproto.Envelope) wsproto.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackHooks[reqType] = respond
}

func (f *fakeHub) Frames() []wsproto.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]wsproto.Envelope, len(f.frames))
	copy(out, f.frames)
	return out
}

// closeConn drops the most-recently accepted connection from the server side,
// which surfaces to the dialer as a read error and triggers its disconnect
// path. Used by tests that need to simulate a mid-session drop.
func (f *fakeHub) closeConn() {
	f.mu.Lock()
	ws := f.conn
	f.mu.Unlock()
	if ws != nil {
		ws.Close(websocket.StatusNormalClosure, "")
	}
}

func (f *fakeHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		f.mu.Lock()
		f.conn = ws
		f.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				continue
			}
			f.mu.Lock()
			f.frames = append(f.frames, env)
			hook := f.ackHooks[env.Type]
			f.mu.Unlock()
			if hook != nil {
				resp := hook(env)
				rb, _ := json.Marshal(resp)
				_ = ws.Write(ctx, websocket.MessageText, rb)
			}
		}
	})
}

// waitConnected blocks until the dialer's serve loop is live (connected=true)
// so a command sent next won't hit the not-connected fast-fail before the
// session is up.
func waitConnected(t *testing.T, dl *Dialer) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(2 * time.Second)
	for !dl.connected.Load() {
		select {
		case <-deadline:
			t.Fatal("dialer never connected")
		case <-tick.C:
		}
	}
}

func TestDialerSendsHelloAndReceivesAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{}, AgentMeta{}
		},
		OnApply: func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
}

func TestDialerStoresNodeIdentityFromHelloAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 42, Name: "relay-1"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()
	waitConnected(t, dl)

	if dl.NodeName() != "relay-1" {
		t.Errorf("NodeName() = %q, want relay-1", dl.NodeName())
	}
	if dl.NodeID() != 42 {
		t.Errorf("NodeID() = %d, want 42", dl.NodeID())
	}
	if !dl.IsConnected() {
		t.Error("IsConnected() = false, want true")
	}
}

func TestDialerEditRuleHopRoundtripsAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeRuleHopEdit, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: "10.0.0.10:21000"})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	waitConnected(t, dl)
	ack, err := dl.EditRuleHop(ctx, wsproto.RuleHopEdit{RuleID: 5, ListenPort: 21000})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Entry != "10.0.0.10:21000" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func TestDialerCreateRuleRoundtripsAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeRuleCreate, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: "10.0.0.1:15000"})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	waitConnected(t, dl)
	ack, err := dl.CreateRule(ctx, wsproto.RuleCreate{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Entry != "10.0.0.1:15000" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func TestDialerUpdateRuleRoundtripsAck(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeRuleUpdate, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: "10.0.0.1:16000"})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	waitConnected(t, dl)
	ack, err := dl.UpdateRule(ctx, wsproto.RuleUpdate{RuleID: 5, ListenPort: 16000})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Entry != "10.0.0.1:16000" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func TestDialerSendCommandFailsWhenDisconnected(t *testing.T) {
	dl := NewDialer(DialerConfig{URL: "ws://127.0.0.1:1/"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dl.DeleteRule(ctx, 9); err == nil {
		t.Fatal("expected error when not connected")
	}
}

// A command waiting on its ack when the connection drops must be woken by
// runOnce's pending-drain, not left to spin until its own context deadline.
// The fakeHub here deliberately never replies to rule_hop_edit, so the only
// thing that can unblock EditRuleHop is the disconnect cleanup.
func TestDialerCommandWokenOnDisconnect(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	// Long ctx so a pass proves the wake came from disconnect cleanup, not
	// from the context deadline firing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()
	waitConnected(t, dl)

	type result struct {
		ack wsproto.RuleCmdAck
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		ack, err := dl.EditRuleHop(ctx, wsproto.RuleHopEdit{RuleID: 5, ListenPort: 21000})
		resCh <- result{ack, err}
	}()

	// Wait until the server has actually received the command so the dialer's
	// waiter is parked on its ack channel before we kill the connection.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	sent := time.After(2 * time.Second)
sendWait:
	for {
		select {
		case <-sent:
			t.Fatal("server never received rule_hop_edit")
		case <-tick.C:
			for _, f := range fh.Frames() {
				if f.Type == wsproto.TypeRuleHopEdit {
					break sendWait
				}
			}
		}
	}

	// Drop the connection from the server side; the dialer's reader sees a
	// read error, runOnce returns, and its defer drains pending waiters.
	fh.closeConn()

	select {
	case r := <-resCh:
		if r.err == nil && (r.ack.OK || !strings.Contains(r.ack.Error, "断开")) {
			t.Fatalf("expected disconnect signaled via err or ack.Error containing 断开, got ack=%+v err=%v", r.ack, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EditRuleHop was not woken by disconnect cleanup (would have hung to ctx deadline)")
	}
}

func TestDialerMigratesTuiRulesOnConnect(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeMigrateRules, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	migrated := make(chan struct{}, 1)
	tuiRules := []nft.Rule{{ID: "t1", Proto: "tcp", SrcPort: 12000, DestHost: "1.2.3.4", DestPort: 80}}
	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{"tui": tuiRules}, AgentMeta{}
		},
		OnApply:    func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
		OnMigrated: func() { migrated <- struct{}{} },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	select {
	case <-migrated:
		// Migration callback fired -- success.
	case <-time.After(2 * time.Second):
		t.Fatal("OnMigrated callback never called")
	}

	// Verify the migrate_rules frame was sent.
	found := false
	for _, f := range fh.Frames() {
		if f.Type == wsproto.TypeMigrateRules {
			found = true
			var mr wsproto.MigrateRules
			if err := json.Unmarshal(f.Payload, &mr); err != nil {
				t.Fatalf("unmarshal migrate_rules: %v", err)
			}
			if len(mr.Rules) != 1 || mr.Rules[0].ID != "t1" {
				t.Fatalf("unexpected migrate payload: %+v", mr.Rules)
			}
		}
	}
	if !found {
		t.Fatal("migrate_rules frame not found in server frames")
	}
}

func TestProbeOutboundIP(t *testing.T) {
	got := probeOutboundIP("udp4", "127.0.0.1:9")
	if got != "127.0.0.1" {
		t.Fatalf("probeOutboundIP(udp4, loopback) = %q, want 127.0.0.1", got)
	}
	if got := probeOutboundIP("udp4", "not-a-valid-target"); got != "" {
		t.Fatalf("probeOutboundIP with malformed target = %q, want empty", got)
	}
}

func TestDialerHelloIncludesProbedV4(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState:     func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:      func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(frames[0].Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ProbedV4 == "" {
		t.Error("expected ProbedV4 to be populated (host must have a default v4 route)")
	}
}

func TestDialerHelloIncludesDeclaredRelayHost(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	dl := NewDialer(DialerConfig{
		URL:                 "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:               "tok",
		AgentVersion:        "v1",
		DeclaredRelayHost:   "203.0.113.50",
		DeclaredRelayHostV6: "2001:db8::50",
		GetState:            func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:             func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dl.runOnce(ctx); err != nil && err != context.DeadlineExceeded {
		t.Logf("runOnce returned: %v (expected timeout)", err)
	}

	frames := fh.Frames()
	if len(frames) == 0 || frames[0].Type != wsproto.TypeHello {
		t.Fatalf("expected first frame to be hello, got %+v", frames)
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(frames[0].Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.DeclaredRelayHost != "203.0.113.50" {
		t.Errorf("DeclaredRelayHost = %q, want 203.0.113.50", hello.DeclaredRelayHost)
	}
	if hello.DeclaredRelayHostV6 != "2001:db8::50" {
		t.Errorf("DeclaredRelayHostV6 = %q, want 2001:db8::50", hello.DeclaredRelayHostV6)
	}
}
