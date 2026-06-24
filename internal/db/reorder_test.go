package db

import (
	"path/filepath"
	"testing"
)

func TestReorderNodes(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var ids []int64
	for _, name := range []string{"a", "b", "c"} {
		n, err := CreateNode(d, name, "https://p", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}

	// New nodes default to creation order (sort_order = max+1).
	nodes, _ := ListNodes(d)
	if got := names(nodes); got != "a,b,c" {
		t.Fatalf("initial order = %s, want a,b,c", got)
	}

	// Reorder to c, a, b.
	if err := ReorderNodes(d, []int64{ids[2], ids[0], ids[1]}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = ListNodes(d)
	if got := names(nodes); got != "c,a,b" {
		t.Fatalf("reordered = %s, want c,a,b", got)
	}
}

func names(nodes []*Node) string {
	out := ""
	for i, n := range nodes {
		if i > 0 {
			out += ","
		}
		out += n.Name
	}
	return out
}
