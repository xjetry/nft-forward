package server

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
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
	if n.NodeType != "self" || n.Name != "self" {
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
