package db

import (
	"database/sql"
	"errors"
	"testing"
)

// UpdateRow's "no row changed means no such row" rests on the driver counting
// a matched row as changed even when it is written the values it already has.
// This asks the driver in use, rather than SQLite's documentation.
func TestUpdateRowTellsAMissingRowFromAnUnchangedOne(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "edge", "https://edge", "tok-edge")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateRow(d, `UPDATE nodes SET name=? WHERE id=?`, "edge", n.ID); err != nil {
		t.Errorf("writing a row the name it already has: %v, want nil", err)
	}
	if err := UpdateRow(d, `UPDATE nodes SET name=? WHERE id=?`, "renamed", n.ID); err != nil {
		t.Errorf("changing a row: %v, want nil", err)
	}
	if err := UpdateRow(d, `UPDATE nodes SET name=? WHERE id=?`, "nobody", n.ID+1000); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a key no row has: %v, want sql.ErrNoRows", err)
	}
	err = UpdateRow(d, `UPDATE no_such_table SET name=? WHERE id=?`, "x", n.ID)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a statement that fails: %v, want its own error", err)
	}
	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Errorf("the node is named %q, want %q", got.Name, "renamed")
	}
}
