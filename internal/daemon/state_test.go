package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	owners, _, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadState missing: %v", err)
	}
	if len(owners) != 0 {
		t.Fatalf("expected empty, got %d owners", len(owners))
	}
}

func TestSaveLoad_RoundTrip_V2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "t1", Proto: "tcp", SrcPort: 8080, DestIP: "1.2.3.4", DestPort: 80, Comment: "demo"},
		},
		"panel": []nft.Rule{
			{ID: "p1", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
			{ID: "p2", Proto: "tcp+udp", SrcPort: 443, DestHost: "example.com", DestIP: "203.0.113.5", DestPort: 8443, BandwidthMbps: 100, Comment: "with bandwidth"},
		},
	}
	if err := SaveState(path, in, AgentMeta{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, _, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\nin = %+v\nout = %+v", in, out)
	}
}

func TestLoadState_V1CompatibilityReadsAsTuiSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v1 := []byte(`{
		"version": 1,
		"rules": [
			{"id":"x1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}
		]
	}`)
	if err := os.WriteFile(path, v1, 0o640); err != nil {
		t.Fatal(err)
	}
	out, _, err := LoadState(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 owner segment, got %d: %+v", len(out), out)
	}
	tui, ok := out["tui"]
	if !ok {
		t.Fatalf("expected 'tui' owner from v1 migration, got owners: %v", keysOf(out))
	}
	if len(tui) != 1 || tui[0].ID != "x1" {
		t.Fatalf("tui segment after v1 read: %+v", tui)
	}
}

func TestLoadState_UnknownVersionErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"owners":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadState(path); err == nil {
		t.Fatal("expected version error for v99")
	}
}

func TestSaveState_AtomicViaTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := OwnerRuleset{"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 1, DestPort: 1}}}
	if err := SaveState(path, in, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file leaked: stat err = %v", err)
	}
}

func TestSaveState_EmptyOwnersWritesValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, OwnerRuleset{}, AgentMeta{}); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	out, _, err := LoadState(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty owners, got %+v", out)
	}
}

func TestLoadStateV2UpgradesToV3WithZeroAgentMeta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	v2 := `{"version":2,"owners":{"tui":[{"proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]}}`
	if err := os.WriteFile(p, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	owners, meta, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners["tui"]) != 1 {
		t.Fatalf("expected 1 tui rule, got %d", len(owners["tui"]))
	}
	if meta.LastAppliedRev != "" {
		t.Fatalf("expected empty LastAppliedRev, got %q", meta.LastAppliedRev)
	}
}

func TestSaveLoadStateV3Roundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	owners := OwnerRuleset{"panel": {{Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.2", DestPort: 443}}}
	meta := AgentMeta{
		LastAppliedRev: "abc123",
		PanelURL:       "wss://panel/v1/agents",
	}
	if err := SaveState(p, owners, meta); err != nil {
		t.Fatal(err)
	}
	got, gotMeta, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["panel"]) != 1 {
		t.Fatalf("expected 1 panel rule, got %d", len(got["panel"]))
	}
	if gotMeta.LastAppliedRev != "abc123" || gotMeta.PanelURL != "wss://panel/v1/agents" {
		t.Fatalf("meta roundtrip mismatch: %+v", gotMeta)
	}
}

func TestSaveStateAlwaysSerializesAgentMetaBlock(t *testing.T) {
	// Operators reading state.json by hand must see a canonical layout:
	// the agent_meta block is always present, even when empty.
	p := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(p, OwnerRuleset{}, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, `"agent_meta"`) {
		t.Fatalf("agent_meta key missing from on-disk state: %s", body)
	}
}

func TestLoadState_V3FillsKernelMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	v3 := `{"version":3,"owners":{"tui":[{"id":"a","proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]},"agent_meta":{}}`
	if err := os.WriteFile(path, []byte(v3), 0o600); err != nil {
		t.Fatal(err)
	}
	owners, _, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	r := owners["tui"][0]
	if r.Mode != nft.ModeKernel {
		t.Fatalf("v3 rule should be normalized to kernel, got %q", r.Mode)
	}
}

func TestSaveLoad_V4RoundTripUserspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := OwnerRuleset{"tui": {{ID: "a", Proto: "tcp", SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeUserspace}}}
	if err := SaveState(path, in, AgentMeta{}); err != nil {
		t.Fatal(err)
	}
	out, _, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if out["tui"][0].Mode != nft.ModeUserspace {
		t.Fatalf("userspace mode lost on round trip: %+v", out["tui"][0])
	}
}

// Standalone downgrade strips panel bindings. Group shaping is panel policy
// (priced by a grant), so it is dropped too; the legacy per-rule cap survives
// because it is self-contained.
func TestDowngradePanelRuleStripsPanelState(t *testing.T) {
	in := nft.Rule{
		ID: "p1", Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 8443,
		RuleID: 7, RuleName: "r", OwnerName: "alice", HopCount: 2,
		ShapeGroup: 5, RateMBytes: 10, BandwidthMbps: 84,
	}
	out := downgradePanelRule(in)
	if out.ID == "" || out.ID == in.ID {
		t.Fatalf("downgrade must assign a fresh local id, got %q", out.ID)
	}
	if out.RuleID != 0 || out.RuleName != "" || out.OwnerName != "" || out.HopCount != 0 {
		t.Fatalf("panel metadata must be cleared: %+v", out)
	}
	if out.ShapeGroup != 0 || out.RateMBytes != 0 {
		t.Fatalf("shape group fields must be cleared: %+v", out)
	}
	if out.BandwidthMbps != 84 {
		t.Fatalf("legacy cap must survive downgrade, got %d", out.BandwidthMbps)
	}
	if out.Proto != "tcp" || out.SrcPort != 443 || out.DestIP != "10.0.0.1" || out.DestPort != 8443 {
		t.Fatalf("forwarding fields must survive: %+v", out)
	}
}

func keysOf(m OwnerRuleset) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
