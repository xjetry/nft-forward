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
