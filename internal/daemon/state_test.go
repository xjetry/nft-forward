package daemon

import (
	"fmt"
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

func TestSaveLoad_RoundTrip(t *testing.T) {
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

// 状态文件只读当前版本。第 1–3 版（v0.5.0 之前的版本写下的）与没有版本号的文件都
// 报错，报错里写明读到的是第几版；不转换，不当成空状态，也不改动文件。
func TestLoadStateRefusesEarlierVersions(t *testing.T) {
	for version, content := range map[int]string{
		0: `{"owners":{"tui":[{"id":"a","proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]}}`,
		1: `{"version":1,"rules":[{"id":"x1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}]}`,
		2: `{"version":2,"owners":{"tui":[{"proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]}}`,
		3: `{"version":3,"owners":{"tui":[{"id":"a","proto":"tcp","src_port":80,"dest_ip":"10.0.0.1","dest_port":80}]},"agent_meta":{}}`,
	} {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		owners, _, err := LoadState(path)
		if err == nil {
			t.Errorf("第 %d 版的状态文件应被拒绝，实际读出了 %+v", version, owners)
			continue
		}
		if want := fmt.Sprintf("version %d", version); !strings.Contains(err.Error(), want) {
			t.Errorf("第 %d 版：报错应写明读到的版本（%q），实际 %q", version, want, err)
		}
		if owners != nil {
			t.Errorf("第 %d 版：被拒绝时不应返回规则集，实际 %+v", version, owners)
		}
		if after, rerr := os.ReadFile(path); rerr != nil || string(after) != content {
			t.Errorf("第 %d 版：文件不应被改动（读取错误 %v）", version, rerr)
		}
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

func TestSaveLoadStateKeepsAgentMeta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	owners := OwnerRuleset{"panel": {{Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.2", DestPort: 443}}}
	meta := AgentMeta{LastAppliedRev: "abc123"}
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
	if gotMeta != meta {
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

func TestSaveLoad_RoundTripUserspace(t *testing.T) {
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
// (priced by a grant), so it is dropped too; the rule's own per-rule cap survives
// because it is self-contained.
func TestDowngradePanelRuleStripsPanelState(t *testing.T) {
	in := nft.Rule{
		ID: "p1", Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 8443,
		RuleID: 7, RuleName: "r", HopCount: 2,
		ShapeGroup: 5, RateMBytes: 10, BandwidthMbps: 84,
	}
	out := downgradePanelRule(in)
	if out.ID == "" || out.ID == in.ID {
		t.Fatalf("downgrade must assign a fresh local id, got %q", out.ID)
	}
	if out.RuleID != 0 || out.RuleName != "" || out.HopCount != 0 {
		t.Fatalf("panel metadata must be cleared: %+v", out)
	}
	if out.ShapeGroup != 0 || out.RateMBytes != 0 {
		t.Fatalf("shape group fields must be cleared: %+v", out)
	}
	if out.BandwidthMbps != 84 {
		t.Fatalf("the rule's own cap must survive downgrade, got %d", out.BandwidthMbps)
	}
	if out.Proto != "tcp" || out.SrcPort != 443 || out.DestIP != "10.0.0.1" || out.DestPort != 8443 {
		t.Fatalf("forwarding fields must survive: %+v", out)
	}
	// 降级去掉的恰好是 nft.Rule.WithoutPanelBinding 定义的那些字段，此外只换了 ID；
	// “哪些字段是面板给的”不在降级这里另列一份。
	want := in.WithoutPanelBinding()
	want.ID = out.ID
	if out != want {
		t.Fatalf("降级的结果应等于去掉面板字段、换上新 ID 的规则:\n got %+v\nwant %+v", out, want)
	}
}

// 旧版 agent 写下的状态文件里，面板段的规则带着 owner_name。这个字段已经不在
// nft.Rule 里，带着它的状态文件必须照常读出来，rev 与其余字段不受影响。
func TestLoadStateReadsRulesThatStillCarryOwnerName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"version":4,"owners":{"panel":[{"id":"","proto":"tcp","src_port":12000,"dest_ip":"10.0.0.1","dest_port":443,"mode":"kernel","rule_id":7,"rule_name":"r","owner_name":"alice","hop_count":1,"shape_group":5,"rate_mbytes":10}]},"agent_meta":{"last_applied_rev":"abc"}}`
	if err := os.WriteFile(path, []byte(old), 0o640); err != nil {
		t.Fatal(err)
	}
	owners, meta, err := LoadState(path)
	if err != nil {
		t.Fatalf("带 owner_name 的状态文件应能读出: %v", err)
	}
	want := nft.Rule{
		Proto: "tcp", SrcPort: 12000, DestIP: "10.0.0.1", DestPort: 443, Mode: nft.ModeKernel,
		RuleID: 7, RuleName: "r", HopCount: 1, ShapeGroup: 5, RateMBytes: 10,
	}
	if got := owners[segPanel]; len(got) != 1 || got[0] != want {
		t.Fatalf("读出的面板段不对:\n got %+v\nwant %+v", got, want)
	}
	if meta.LastAppliedRev != "abc" {
		t.Fatalf("rev 应原样读出，实际 %q", meta.LastAppliedRev)
	}
}
