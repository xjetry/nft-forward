package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

func TestNew_DefaultsApplied(t *testing.T) {
	d := New(Config{})
	if d.socketPath != DefaultSocketPath {
		t.Errorf("socketPath default = %q, want %q", d.socketPath, DefaultSocketPath)
	}
	if d.statePath != DefaultStatePath {
		t.Errorf("statePath default = %q, want %q", d.statePath, DefaultStatePath)
	}
	if d.groupName != DefaultGroupName {
		t.Errorf("groupName default = %q, want %q", d.groupName, DefaultGroupName)
	}
	if d.applier == nil {
		t.Fatal("applier nil after New(Config{})")
	}
}

func TestNew_ExplicitOverrides(t *testing.T) {
	fa := &fakeApplier{}
	d := New(Config{
		SocketPath: "/tmp/x.sock",
		StatePath:  "/tmp/x.json",
		GroupName:  "g",
		Applier:    fa,
	})
	if d.socketPath != "/tmp/x.sock" || d.statePath != "/tmp/x.json" || d.groupName != "g" {
		t.Fatalf("overrides not applied: %+v", d)
	}
	if d.applier != fa {
		t.Fatal("custom applier not used")
	}
}

func TestBootstrap_LoadsAndApplies(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 8080},
	}); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApplier{}
	d := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    fa,
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(fa.last) != 1 || fa.last[0].SrcPort != 80 {
		t.Fatalf("Bootstrap did not apply state: %+v", fa.last)
	}
	if len(d.rules) != 1 {
		t.Fatalf("in-memory rules not populated: %+v", d.rules)
	}
}

func TestBootstrap_EmptyStateIsFine(t *testing.T) {
	d := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "missing.json"),
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    &fakeApplier{},
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap on empty state: %v", err)
	}
}

func TestRun_AcceptsSocketTrafficAndShutsDown(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
	statePath := filepath.Join(t.TempDir(), "state.json")
	fa := &fakeApplier{}
	d := New(Config{
		SocketPath: sockPath,
		StatePath:  statePath,
		GroupName:  "",
		Applier:    fa,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Run did not exit within 3s after cancel")
		}
	})

	// 等 socket 出现，最多 1s
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket never appeared: %v", err)
	}

	// 通过 unix-socket dial 提交一条 ruleset
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	body := `{"rules":[{"id":"rZ","proto":"tcp","src_port":9090,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := client.Post("http://unix/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if len(fa.last) != 1 || fa.last[0].ID != "rZ" {
		t.Fatalf("applier did not see POSTed rule: %+v", fa.last)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].ID != "rZ" {
		t.Fatalf("state.json not persisted as expected: %+v", saved)
	}
}
