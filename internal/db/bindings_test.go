package db

import (
	"database/sql"
	"errors"
	"testing"
)

func TestNodeBindingsCRUD(t *testing.T) {
	d := openTestDB(t)
	up, _ := CreateNode(d, "entry-hk", "", "")
	mid, _ := CreateNode(d, "akari-hk", "", "")
	mid2, _ := CreateNode(d, "misaka", "", "")

	err := ReplaceBindingsForDownstream(d, mid.ID, []NodeBinding{
		{UpstreamNodeID: up.ID, DownstreamNodeID: mid.ID, Mode: "kernel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := GetNodeBinding(d, up.ID, mid.ID)
	if err != nil || b.Mode != "kernel" {
		t.Fatalf("want kernel edge, got %+v err=%v", b, err)
	}
	if _, err := GetNodeBinding(d, up.ID, mid2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing edge must be ErrNoRows, got %v", err)
	}

	// Replace is total for the downstream: dropping the old edge removes it.
	err = ReplaceBindingsForDownstream(d, mid.ID, []NodeBinding{
		{UpstreamNodeID: mid2.ID, DownstreamNodeID: mid.ID, Mode: "userspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetNodeBinding(d, up.ID, mid.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replaced-away edge must be gone, got %v", err)
	}
	all, _ := ListAllNodeBindings(d)
	if len(all) != 1 || all[0].UpstreamNodeID != mid2.ID {
		t.Fatalf("want 1 edge from mid2, got %+v", all)
	}
	ls, _ := ListBindingsForDownstream(d, mid.ID)
	if len(ls) != 1 {
		t.Fatalf("want 1 downstream edge, got %d", len(ls))
	}

	// Deleting a node cascades its edges away.
	if err := DeleteNode(d, mid2.ID); err != nil {
		t.Fatal(err)
	}
	all, _ = ListAllNodeBindings(d)
	if len(all) != 0 {
		t.Fatalf("cascade delete failed, edges left: %+v", all)
	}
}
