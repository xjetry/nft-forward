package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backup writes a consistent, compacted snapshot of the database to destPath
// using SQLite's VACUUM INTO. It captures a correct snapshot even under WAL and
// in a single file, so recovery is just copying the file back over panel.db.
// destPath must not already exist (VACUUM INTO refuses to overwrite).
func Backup(d *sql.DB, destPath string) error {
	// destPath is operator-controlled (derived from --db), not user input, but
	// escape quotes anyway since VACUUM INTO takes the path inline, not as a bind.
	esc := strings.ReplaceAll(destPath, "'", "''")
	if _, err := d.Exec("VACUUM INTO '" + esc + "'"); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}
	return nil
}

// StartBackups runs a periodic local backup of the panel DB into a "backups"
// directory next to it, retaining the most recent `keep` snapshots. It takes one
// backup immediately, then every `interval`. interval<=0 or keep<=0 disables it.
// The returned func stops the loop. Backups live on the same host/disk, so they
// guard against DB corruption, a bad migration or accidental deletion — offsite
// copies remain the operator's job.
func StartBackups(d *sql.DB, dbPath string, interval time.Duration, keep int) func() {
	if interval <= 0 || keep <= 0 {
		return func() {}
	}
	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	stop := make(chan struct{})
	run := func() {
		if err := ensureDir(dir); err != nil {
			log.Printf("backup: ensure dir %s: %v", dir, err)
			return
		}
		dest := filepath.Join(dir, "panel-"+time.Now().Format("20060102-150405")+".db")
		// VACUUM INTO refuses to overwrite. A backup for this second already
		// existing means a near-simultaneous run (e.g. two processes overlapping
		// during an upgrade restart) already captured it — skip rather than log a
		// spurious error.
		if _, err := os.Stat(dest); err == nil {
			return
		}
		if err := Backup(d, dest); err != nil {
			log.Printf("backup: %v", err)
			return
		}
		pruneBackups(dir, keep)
	}
	go func() {
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				run()
			}
		}
	}()
	return func() { close(stop) }
}

// pruneBackups keeps only the newest `keep` panel-*.db files in dir. Names embed
// a sortable timestamp, so lexical order is chronological.
func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "panel-") && strings.HasSuffix(e.Name(), ".db") {
			files = append(files, e.Name())
		}
	}
	if len(files) <= keep {
		return
	}
	sort.Strings(files)
	for _, name := range files[:len(files)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("backup: prune %s: %v", name, err)
		}
	}
}
