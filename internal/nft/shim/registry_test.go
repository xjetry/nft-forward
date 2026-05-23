package shim

import (
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// stubShim is a hand-rolled fake satisfying ForwardShim. Captures every
// call so tests can assert ordering and arguments.
type stubShim struct {
	name       string
	detect     bool
	syncErr    error
	cleanErr   error
	syncCalls  int
	cleanCalls int
	lastRules  []nft.Rule
}

func (s *stubShim) Name() string { return s.name }
func (s *stubShim) Detect() bool { return s.detect }
func (s *stubShim) Sync(rules []nft.Rule) error {
	s.syncCalls++
	s.lastRules = rules
	return s.syncErr
}
func (s *stubShim) Cleanup() error {
	s.cleanCalls++
	return s.cleanErr
}

func TestRegistrySyncAllSkipsUndetected(t *testing.T) {
	a := &stubShim{name: "a", detect: false}
	b := &stubShim{name: "b", detect: true}
	r := &Registry{shims: []ForwardShim{a, b}}
	if err := r.SyncAll([]nft.Rule{{Proto: "tcp", DestIP: "1.1.1.1", DestPort: 80}}); err != nil {
		t.Fatal(err)
	}
	if a.syncCalls != 0 {
		t.Fatalf("a should not be synced (Detect false), syncCalls=%d", a.syncCalls)
	}
	if b.syncCalls != 1 {
		t.Fatalf("b should have synced once, got %d", b.syncCalls)
	}
}

func TestRegistrySyncAllAggregatesErrors(t *testing.T) {
	a := &stubShim{name: "a", detect: true, syncErr: errors.New("boom-a")}
	b := &stubShim{name: "b", detect: true, syncErr: errors.New("boom-b")}
	c := &stubShim{name: "c", detect: true}
	r := &Registry{shims: []ForwardShim{a, b, c}}
	err := r.SyncAll(nil)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if !strings.Contains(err.Error(), "boom-a") || !strings.Contains(err.Error(), "boom-b") {
		t.Fatalf("aggregate error must mention both, got %v", err)
	}
	if c.syncCalls != 1 {
		t.Fatal("third shim must still be called after earlier failures")
	}
}

func TestRegistryCleanupAllSkipsUndetected(t *testing.T) {
	a := &stubShim{name: "a", detect: false}
	b := &stubShim{name: "b", detect: true}
	r := &Registry{shims: []ForwardShim{a, b}}
	if err := r.CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if a.cleanCalls != 0 {
		t.Fatalf("a should not be cleaned (Detect false), got %d", a.cleanCalls)
	}
	if b.cleanCalls != 1 {
		t.Fatalf("b should clean once, got %d", b.cleanCalls)
	}
}

func TestDefaultRegistryListsKnownShims(t *testing.T) {
	r := DefaultRegistry()
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 default shims, got %d: %v", len(names), names)
	}
	want := map[string]bool{"docker-user": true, "ufw": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected shim %q in DefaultRegistry()", n)
		}
	}
}
