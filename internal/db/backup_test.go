package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupProducesUsableCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "panel.db")
	d, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := CreateUser(d, "backup-me", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Reopen (Open ran migrations) and back up to a fresh path.
	d, err = Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dest := filepath.Join(dir, "copy.db")
	if err := Backup(d, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The copy must open and contain the row.
	cp, err := Open(dest)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer cp.Close()
	u, err := GetUserByID(cp, uid)
	if err != nil || u.Username != "backup-me" {
		t.Fatalf("backup missing user: %v", err)
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"panel-20260101-000000.db",
		"panel-20260102-000000.db",
		"panel-20260103-000000.db",
		"panel-20260104-000000.db",
		"unrelated.txt",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneBackups(dir, 2)

	got := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		got[e.Name()] = true
	}
	if got["panel-20260101-000000.db"] || got["panel-20260102-000000.db"] {
		t.Error("old backups should have been pruned")
	}
	if !got["panel-20260103-000000.db"] || !got["panel-20260104-000000.db"] {
		t.Error("newest backups should be kept")
	}
	if !got["unrelated.txt"] {
		t.Error("non-backup files must not be touched")
	}
}
