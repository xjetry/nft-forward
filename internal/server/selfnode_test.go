package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestEnsureSelfNodeCreatesOneRow(t *testing.T) {
	d := openDB(t)
	n, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n.NodeKind != "self" || n.Name != "self" {
		t.Fatalf("unexpected self node: %+v", n)
	}
	// Second call must not create a duplicate.
	n2, err := EnsureSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n2.ID != n.ID {
		t.Fatalf("EnsureSelfNode created a second row: %d vs %d", n.ID, n2.ID)
	}
}

func TestDispatchRoutesSelfToUnixSocketStub(t *testing.T) {
	d := openDB(t)
	self, _ := EnsureSelfNode(d)

	var called string
	disp := &Dispatcher{
		DB:  d,
		Hub: nil, // hub not needed for self route
		SendLocal: func(rules []nft.Rule) error {
			called = "local"
			return nil
		},
	}
	if err := disp.Dispatch(self.ID, []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}, "rev1"); err != nil {
		t.Fatal(err)
	}
	if called != "local" {
		t.Fatalf("expected SendLocal to fire, got %q", called)
	}
}
