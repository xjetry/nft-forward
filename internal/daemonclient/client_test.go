package daemonclient

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// shortSockDir mirrors internal/daemon's helper: macOS sun_path is capped
// at 104 bytes and t.TempDir under /var/folders is often too long.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nftc-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mockServer starts an HTTP server bound to a unix socket inside a short
// temp dir and serves handlerFn. Returns the socket path so the test can
// build a Client. The server is closed automatically via t.Cleanup.
func mockServer(t *testing.T, handlerFn http.HandlerFunc) string {
	t.Helper()
	sockPath := filepath.Join(shortSockDir(t), "mock.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handlerFn}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); l.Close() })
	return sockPath
}

func TestHealth_OK(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c := New(sock)
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHealth_FailsWhenSocketMissing(t *testing.T) {
	c := New(filepath.Join(shortSockDir(t), "nope.sock"))
	if err := c.Health(); err == nil {
		t.Fatal("expected error when socket does not exist")
	}
}

func TestHealth_FailsOnNon200(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c := New(sock)
	err := c.Health()
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestHealth_FailsOnOkFalse(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	})
	c := New(sock)
	err := c.Health()
	if err == nil || !strings.Contains(err.Error(), "ok=false") {
		t.Fatalf("expected ok=false error, got %v", err)
	}
}

func TestGetRuleset_RoundTrip(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owners":{"tui":[{"id":"r1","proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80}]}}`))
	})
	c := New(sock)
	got, err := c.GetRuleset()
	if err != nil {
		t.Fatalf("GetRuleset: %v", err)
	}
	if len(got["tui"]) != 1 || got["tui"][0].ID != "r1" {
		t.Fatalf("unexpected ruleset: %+v", got)
	}
}

func TestGetRuleset_EmptyOwnersReturnsNonNilMap(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owners":{}}`))
	})
	c := New(sock)
	got, err := c.GetRuleset()
	if err != nil {
		t.Fatalf("GetRuleset: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil OwnerRuleset")
	}
	if len(got) != 0 {
		t.Fatalf("expected zero owners, got %+v", got)
	}
}

func TestPostRuleset_SendsBodyAndOwnerInPath(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"count":1}`))
	})
	c := New(sock)
	err := c.PostRuleset("tui", []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 80},
	})
	if err != nil {
		t.Fatalf("PostRuleset: %v", err)
	}
	if capturedPath != "/v1/ruleset/tui" {
		t.Errorf("path = %q, want /v1/ruleset/tui", capturedPath)
	}
	var got segmentPayload
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" {
		t.Fatalf("body did not carry rule: %+v", got)
	}
}

func TestPostRuleset_NilRulesNormalizesToEmpty(t *testing.T) {
	var capturedBody []byte
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"count":0}`))
	})
	c := New(sock)
	if err := c.PostRuleset("tui", nil); err != nil {
		t.Fatalf("PostRuleset: %v", err)
	}
	if !strings.Contains(string(capturedBody), `"rules":[]`) {
		t.Fatalf("expected empty rules array in body, got %s", capturedBody)
	}
}

func TestPostRuleset_PropagatesConflict(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "port tcp/80 already claimed by owner \"panel\"", http.StatusConflict)
	})
	c := New(sock)
	err := c.PostRuleset("tui", []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "tcp/80") {
		t.Fatalf("error should mention 409 and conflicting port; got: %v", err)
	}
}

func TestPostRuleset_EmptyOwnerRejectedClientSide(t *testing.T) {
	c := New("/tmp/not-used.sock")
	err := c.PostRuleset("", []nft.Rule{{ID: "x"}})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("expected owner-empty error, got %v", err)
	}
}
