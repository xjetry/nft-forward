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
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if d.socketPath != DefaultSocketPath {
		t.Errorf("socketPath default = %q, want %q", d.socketPath, DefaultSocketPath)
	}
	if d.statePath != DefaultStatePath {
		t.Errorf("statePath default = %q, want %q", d.statePath, DefaultStatePath)
	}
	if d.groupName != DefaultGroupName {
		t.Errorf("groupName default = %q, want %q", d.groupName, DefaultGroupName)
	}
	if d.dp == nil {
		t.Fatal("dp nil after New(Config{})")
	}
}

func TestNew_ExplicitOverrides(t *testing.T) {
	fa := &fakeDataplane{}
	d, err := New(Config{
		SocketPath: "/tmp/x.sock",
		StatePath:  "/tmp/x.json",
		GroupName:  "g",
		Dataplane:  fa,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.socketPath != "/tmp/x.sock" || d.statePath != "/tmp/x.json" || d.groupName != "g" {
		t.Fatalf("overrides not applied: %+v", d)
	}
	if d.dp != fa {
		t.Fatal("custom dataplane not used")
	}
}

func TestBootstrap_LoadsOwnerSegmentsAndAppliesMerged(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 8080}},
		"panel": []nft.Rule{
			{ID: "p1", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		},
	}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	fa := &fakeDataplane{}
	d, err := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Dataplane:  fa,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	d, err := New(Config{
		StatePath:  filepath.Join(t.TempDir(), "missing.json"),
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Dataplane:  &fakeDataplane{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap on empty state: %v", err)
	}
}

func TestRun_AcceptsSocketTrafficAndShutsDown(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
	statePath := filepath.Join(t.TempDir(), "state.json")
	fa := &fakeDataplane{}
	d, err := New(Config{
		SocketPath: sockPath,
		StatePath:  statePath,
		GroupName:  "",
		Dataplane:  fa,
	})
	if err != nil {
		t.Fatal(err)
	}

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
		t.Fatalf("data plane did not Reconcile the POSTed rule: %+v", fa.nftCalls)
	}
	saved, _, err := LoadState(statePath)
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

	fa := &fakeDataplane{}
	statePath := filepath.Join(root, "state.json")
	d, err := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Dataplane:  fa,
		LegacyPaths: LegacyMigrationPaths{
			TUI:           tuiPath,
			Agent:         filepath.Join(root, "no-such-agent.json"),
			EmbeddedAgent: filepath.Join(root, "no-such-embedded.json"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

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
	}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "rules.json")
	if err := os.WriteFile(legacyPath, []byte(`[{"id":"ghost","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]`), 0o640); err != nil {
		t.Fatal(err)
	}

	fa := &fakeDataplane{}
	d, err := New(Config{
		StatePath:   statePath,
		SocketPath:  filepath.Join(shortSockDir(t), "s.sock"),
		Dataplane:   fa,
		LegacyPaths: LegacyMigrationPaths{TUI: legacyPath},
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestBootstrap_ResolvesHostnamesBeforeApply(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(statePath, OwnerRuleset{
		"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 80, DestHost: "x.example.com", DestPort: 80}},
	}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}

	fa := &fakeDataplane{}
	d, err := New(Config{
		StatePath:  statePath,
		SocketPath: filepath.Join(shortSockDir(t), "s.sock"),
		Dataplane:  fa,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Override resolveFn to simulate hostname resolution
	d.resolveFn = func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
		resolved := make([]nft.Rule, len(rules))
		copy(resolved, rules)
		for i := range resolved {
			if resolved[i].DestHost == "x.example.com" {
				resolved[i].DestIP = "10.0.0.5"
			}
		}
		return resolved, true, nil
	}

	if err := d.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if len(fa.nftCalls) != 1 {
		t.Fatalf("expected 1 Apply call, got %d", len(fa.nftCalls))
	}
	if len(fa.nftCalls[0]) != 1 {
		t.Fatalf("expected 1 rule in Apply call, got %d", len(fa.nftCalls[0]))
	}
	if fa.nftCalls[0][0].DestIP != "10.0.0.5" {
		t.Fatalf("expected DestIP=10.0.0.5 after resolution, got %q", fa.nftCalls[0][0].DestIP)
	}
}

func TestDetectForwardDropNoShim_FalseWhenPolicyAccept(t *testing.T) {
	if detectForwardDropNoShim("Chain FORWARD (policy ACCEPT 0 packets)", []string{"docker-user"}) {
		t.Fatal("policy ACCEPT must not trigger warning")
	}
}

func TestDetectForwardDropNoShim_FalseWhenShimDetected(t *testing.T) {
	if detectForwardDropNoShim("Chain FORWARD (policy DROP 100 packets)", []string{"docker-user"}) {
		t.Fatal("known shim detected: no warning")
	}
}

func TestDetectForwardDropNoShim_TrueWhenDropAndNoShim(t *testing.T) {
	if !detectForwardDropNoShim("Chain FORWARD (policy DROP 100 packets)", nil) {
		t.Fatal("policy DROP + no shim must trigger warning")
	}
}

func TestDetectForwardDropNoShim_EmptyInput(t *testing.T) {
	if detectForwardDropNoShim("", nil) {
		t.Fatal("empty input (probe failed) must not trigger warning")
	}
}

func TestOnLocalMigratedClearsTuiSegmentAndSetsMeta(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Dataplane:  &fakeDataplane{},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.owners = OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}}
	if err := d.OnLocalMigrated(); err != nil {
		t.Fatal(err)
	}
	if len(d.owners["tui"]) != 0 {
		t.Fatalf("expected tui cleared, got %d", len(d.owners["tui"]))
	}
	if d.meta.MigratedAt.IsZero() {
		t.Fatalf("expected MigratedAt set")
	}
	_, meta, _ := LoadState(d.statePath)
	if meta.MigratedAt.IsZero() {
		t.Fatalf("MigratedAt not persisted")
	}
}

func TestDaemonRunCallsCleanupOnShutdown(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeDataplane{}
	d, err := New(Config{
		SocketPath: filepath.Join(shortSockDir(t), "sock"),
		StatePath:  filepath.Join(dir, "missing.json"),
		GroupName:  "",
		Dataplane:  fa,
		Iface:      "lo",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fa.cleanupCalls != 1 {
		t.Fatalf("expected Cleanup called once on shutdown, got %d", fa.cleanupCalls)
	}
}
