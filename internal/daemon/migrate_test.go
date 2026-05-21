package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"nft-forward/internal/nft"
)

// writeLegacyRules dumps rules to path in the old `[]nft.Rule` schema used
// by both store.Save and agent.saveState.
func writeLegacyRules(t *testing.T, path string, rules []nft.Rule) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate_NoLegacyFilesIsNoOp(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "etc", "nft-forward", "rules.json"),
		Agent:         filepath.Join(root, "var", "lib", "nft-forward", "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "var", "lib", "nft-forward", "embedded-agent-state.json"),
	}
	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners) != 0 {
		t.Fatalf("expected empty migration result, got %+v", owners)
	}
}

func TestMigrate_TuiFileOnly(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{
		{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["tui"]) != 1 || owners["tui"][0].ID != "t1" {
		t.Fatalf("tui segment after migration: %+v", owners)
	}
	if _, exists := owners["panel"]; exists {
		t.Fatalf("panel should be absent when no agent file: %+v", owners)
	}
	if _, err := os.Stat(cfg.TUI); !os.IsNotExist(err) {
		t.Errorf("legacy TUI file still present after migration: %v", err)
	}
	if _, err := os.Stat(cfg.TUI + ".migrated"); err != nil {
		t.Errorf("expected %s.migrated to exist: %v", cfg.TUI, err)
	}
}

func TestMigrate_AgentFileOnly(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "a1", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "a1" {
		t.Fatalf("panel segment after migration: %+v", owners)
	}
	if _, err := os.Stat(cfg.Agent + ".migrated"); err != nil {
		t.Errorf("agent file should be renamed: %v", err)
	}
}

func TestMigrate_EmbeddedAgentWinsOverAgent(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "from-agent", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})
	writeLegacyRules(t, cfg.EmbeddedAgent, []nft.Rule{
		{ID: "from-embedded", Proto: "tcp", SrcPort: 100, DestIP: "3.0.0.0", DestPort: 100},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "from-embedded" {
		t.Fatalf("panel should equal embedded rules: %+v", owners["panel"])
	}
}

func TestMigrate_AllThreeFiles(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent-state.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{
		{ID: "tui-1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
	})
	writeLegacyRules(t, cfg.Agent, []nft.Rule{
		{ID: "agent-1", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90},
	})
	writeLegacyRules(t, cfg.EmbeddedAgent, []nft.Rule{
		{ID: "embedded-1", Proto: "tcp", SrcPort: 100, DestIP: "3.0.0.0", DestPort: 100},
	})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(owners["tui"]) != 1 || owners["tui"][0].ID != "tui-1" {
		t.Errorf("tui segment: %+v", owners["tui"])
	}
	if len(owners["panel"]) != 1 || owners["panel"][0].ID != "embedded-1" {
		t.Errorf("panel segment should equal embedded: %+v", owners["panel"])
	}
	for _, p := range []string{cfg.TUI, cfg.Agent, cfg.EmbeddedAgent} {
		if _, err := os.Stat(p + ".migrated"); err != nil {
			t.Errorf("expected %s.migrated to exist: %v", p, err)
		}
	}
}

func TestMigrate_EmptyLegacyFilesProduceNoSegment(t *testing.T) {
	root := t.TempDir()
	cfg := LegacyMigrationPaths{
		TUI:           filepath.Join(root, "rules.json"),
		Agent:         filepath.Join(root, "agent.json"),
		EmbeddedAgent: filepath.Join(root, "embedded.json"),
	}
	writeLegacyRules(t, cfg.TUI, []nft.Rule{})

	owners, err := migrateLegacyState(cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, exists := owners["tui"]; exists {
		t.Fatalf("empty legacy file should not create owner key: %+v", owners)
	}
	if _, err := os.Stat(cfg.TUI + ".migrated"); err != nil {
		t.Errorf("expected migration marker on empty file too: %v", err)
	}
}
