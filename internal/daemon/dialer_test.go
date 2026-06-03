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
	ackHooks map[string]func(env wsproto.Envelope) wsproto.Envelope // id-template → response
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

func (f *fakeHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
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
		OnRegister: func(forwards []wsproto.Forward) {},
		OnApply:    func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
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

func TestDialerSendsRegisterLocalWhenTuiPresentAndNotMigrated(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	fh.onAck(wsproto.TypeRegisterLocal, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RegisterLocalAck{Imported: []wsproto.ImportedForward{{ListenPort: 80, Proto: "tcp", RuleID: 1}}})
		return wsproto.Envelope{Type: wsproto.TypeRegisterLocalAck, ID: env.ID, Payload: ack}
	})
	srv := httptest.NewServer(fh.handler(t))
	defer srv.Close()

	registered := make(chan []wsproto.Forward, 1)
	dl := NewDialer(DialerConfig{
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
		Token:        "tok",
		AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) {
			return OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}}, AgentMeta{}
		},
		OnRegister: func(forwards []wsproto.Forward) {
			registered <- forwards
		},
		OnApply: func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	select {
	case got := <-registered:
		if len(got) != 1 || got[0].ListenPort != 80 {
			t.Fatalf("unexpected registered forwards: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("OnRegister never called")
	}
}

func TestRulesToForwardsCarriesMode(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, Mode: nft.ModeUserspace},
		{Proto: "tcp", SrcPort: 81, DestIP: "10.0.0.2", DestPort: 81, Mode: nft.ModeKernel},
		{Proto: "tcp", SrcPort: 82, DestIP: "10.0.0.3", DestPort: 82}, // empty mode
	}
	fwds := rulesToForwards(rules)
	if len(fwds) != 3 {
		t.Fatalf("want 3 forwards, got %d", len(fwds))
	}
	if fwds[0].Mode != nft.ModeUserspace {
		t.Errorf("userspace rule lost mode: got %q want %q", fwds[0].Mode, nft.ModeUserspace)
	}
	if fwds[1].Mode != nft.ModeKernel {
		t.Errorf("kernel rule lost mode: got %q want %q", fwds[1].Mode, nft.ModeKernel)
	}
	if fwds[2].Mode != "" {
		t.Errorf("empty-mode rule should round-trip empty, got %q", fwds[2].Mode)
	}
}

func TestDialerSendsPanelSegmentEditOnNotify(t *testing.T) {
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
		OnApply:       func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
		OnPanelNotice: func(_ []wsproto.Forward) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()

	dl.NotifyPanelEdited([]nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}})

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("never received panel_segment_edit frame; frames=%+v", fh.Frames())
		case <-tick.C:
			for _, f := range fh.Frames() {
				if f.Type != wsproto.TypePanelSegmentEdit {
					continue
				}
				var pse wsproto.PanelSegmentEdit
				if err := json.Unmarshal(f.Payload, &pse); err != nil {
					t.Fatalf("unmarshal panel_segment_edit: %v", err)
				}
				if len(pse.Forwards) == 1 && pse.Forwards[0].ListenPort == 30000 && pse.Forwards[0].TargetIP == "10.0.0.9" {
					return
				}
				t.Fatalf("unexpected panel_segment_edit payload: %+v", pse)
			}
		}
	}
}

func TestDialerSkipsRegisterWhenMigratedAtIsNonzero(t *testing.T) {
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
			return OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}},
				AgentMeta{MigratedAt: time.Now().UTC()}
		},
		OnRegister: func(forwards []wsproto.Forward) {
			t.Errorf("OnRegister called despite MigratedAt set")
		},
		OnApply: func(_ context.Context, rev string, rules []nft.Rule) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _ = dl.runOnce(ctx)

	frames := fh.Frames()
	for _, f := range frames {
		if f.Type == wsproto.TypeRegisterLocal {
			t.Fatalf("dialer sent register_local despite MigratedAt set")
		}
	}
}
