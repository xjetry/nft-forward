package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"nft-forward/internal/nft"
)

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	owners, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
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
	if err := SaveState(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState(path)
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
	out, err := LoadState(path)
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
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected version error for v99")
	}
}

func TestSaveState_AtomicViaTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := OwnerRuleset{"tui": []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 1, DestPort: 1}}}
	if err := SaveState(path, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file leaked: stat err = %v", err)
	}
}

func TestSaveState_EmptyOwnersWritesValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, OwnerRuleset{}); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty owners, got %+v", out)
	}
}

func keysOf(m OwnerRuleset) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
