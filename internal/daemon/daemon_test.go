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

func TestBootstrap_LoadsOwnerSegmentsAndAppliesMerged(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 8080}},
		"panel": []nft.Rule{
			{ID: "p1", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		},
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
	if len(fa.nftCalls) != 1 || len(fa.nftCalls[0]) != 2 {
		t.Fatalf("Bootstrap should apply merged ruleset (2 rules), got nftCalls: %+v", fa.nftCalls)
	}
	if len(d.owners["tui"]) != 1 || len(d.owners["panel"]) != 1 {
		t.Fatalf("in-memory owners not populated: %+v", d.owners)
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

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	body := `{"rules":[{"id":"rZ","proto":"tcp","src_port":9090,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := client.Post("http://unix/v1/ruleset/tui", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if len(fa.nftCalls) != 1 || len(fa.nftCalls[0]) != 1 || fa.nftCalls[0][0].ID != "rZ" {
		t.Fatalf("applier did not see POSTed rule: %+v", fa.nftCalls)
	}
	saved, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved["tui"]) != 1 || saved["tui"][0].ID != "rZ" {
		t.Fatalf("state.json not persisted as expected: %+v", saved)
	}
}

func TestBootstrap_MigratesLegacyTuiFile(t *testing.T) {
	root := t.TempDir()
	tuiPath := filepath.Join(root, "etc", "nft-forward", "rules.json")
	if err := os.MkdirAll(filepath.Dir(tuiPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`[{"id":"legacy1","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]`)
	if err := os.WriteFile(tuiPath, legacy, 0o640); err != nil {
		t.Fatal(err)
	}

	fa := &fakeApplier{}
	statePath := filepath.Join(root, "state.json")
	d := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Applier:    fa,
		LegacyPaths: LegacyMigrationPaths{
			TUI:           tuiPath,
			Agent:         filepath.Join(root, "no-such-agent.json"),
			EmbeddedAgent: filepath.Join(root, "no-such-embedded.json"),
		},
	})

	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(d.owners["tui"]) != 1 || d.owners["tui"][0].ID != "legacy1" {
		t.Fatalf("legacy TUI rule not imported into tui segment: %+v", d.owners)
	}
	if len(fa.nftCalls) != 1 || len(fa.nftCalls[0]) != 1 {
		t.Fatalf("Apply should see merged ruleset (1 rule): nftCalls=%+v", fa.nftCalls)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json not created post-migration: %v", err)
	}
	if _, err := os.Stat(tuiPath + ".migrated"); err != nil {
		t.Fatalf("legacy file should be renamed: %v", err)
	}
}

func TestBootstrap_NoMigrationWhenStateAlreadyExists(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "from-v2", Proto: "tcp", SrcPort: 90, DestIP: "9.0.0.0", DestPort: 90}},
	}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "rules.json")
	if err := os.WriteFile(legacyPath, []byte(`[{"id":"ghost","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]`), 0o640); err != nil {
		t.Fatal(err)
	}

	fa := &fakeApplier{}
	d := New(Config{
		StatePath:   statePath,
		SocketPath:  filepath.Join(shortSockDir(t), "s.sock"),
		Applier:     fa,
		LegacyPaths: LegacyMigrationPaths{TUI: legacyPath},
	})
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if d.owners["tui"][0].ID != "from-v2" {
		t.Fatalf("expected v2 state, got: %+v", d.owners["tui"])
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file should still exist (not migrated): %v", err)
	}
	if _, err := os.Stat(legacyPath + ".migrated"); !os.IsNotExist(err) {
		t.Fatalf(".migrated marker should NOT exist when state already there: %v", err)
	}
}
