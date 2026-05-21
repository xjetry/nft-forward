package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"nft-forward/internal/nft"
)

func TestLoadState_MissingFileReturnsEmpty(t *testing.T) {
	rules, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadState missing: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected empty, got %d", len(rules))
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	in := []nft.Rule{
		{ID: "r1", Proto: "tcp", SrcPort: 8080, DestIP: "1.2.3.4", DestPort: 80, Comment: "demo"},
		{ID: "r2", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		{ID: "r3", Proto: "tcp+udp", SrcPort: 443, DestHost: "example.com", DestIP: "203.0.113.5", DestPort: 8443, BandwidthMbps: 100, Comment: "with host + bandwidth"},
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

func TestLoadState_WrongVersionErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"rules":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("expected version error")
	}
}

func TestSaveState_AtomicViaTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := SaveState(path, []nft.Rule{{ID: "r1", Proto: "tcp", SrcPort: 1, DestPort: 1}}); err != nil {
		t.Fatal(err)
	}
	// 临时文件不应该残留
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file leaked: stat err = %v", err)
	}
}
