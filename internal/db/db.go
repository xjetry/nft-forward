package db

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// best-effort; if MkdirAll fails the Open below surfaces a real error
		_ = ensureDir(dir)
	}
	// synchronous=NORMAL is the recommended durability level under WAL: safe
	// across application crashes, only the last transaction is at risk on OS
	// power loss. It avoids an fsync per commit, which matters here because the
	// hot counters path commits frequently and MaxOpenConns(1) serializes every
	// writer behind those fsyncs.
	d, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(d); err != nil {
		d.Close()
		return nil, err
	}
	if err := hashLegacyAPITokens(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// hashLegacyAPITokens converts pre-0025 plaintext api_tokens rows to the
// hash-at-rest form in place, preserving each token so existing integrations
// keep working. token_prefix='' marks a not-yet-migrated row; after conversion
// the prefix is set, so this is idempotent and a no-op on already-hashed DBs.
// SHA-256 can't be computed in modernc SQLite, so the backfill runs in Go.
func hashLegacyAPITokens(d *sql.DB) error {
	rows, err := d.Query(`SELECT id, token FROM api_tokens WHERE token_prefix = ''`)
	if err != nil {
		return err
	}
	type legacy struct {
		id    int64
		token string
	}
	var pending []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.token); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, l := range pending {
		if _, err := d.Exec(`UPDATE api_tokens SET token=?, token_prefix=? WHERE id=?`,
			HashToken(l.token), tokenPrefix(l.token), l.id); err != nil {
			return err
		}
	}
	return nil
}

func migrate(d *sql.DB) error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var dummy string
		if err := d.QueryRow(`SELECT version FROM schema_migrations WHERE version = ?`, name).Scan(&dummy); err == nil {
			continue
		} else if err != sql.ErrNoRows {
			return err
		}
		if err := applyMigration(d, name); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs a single migration file inside a transaction.
// Extracted so defer tx.Rollback() scopes to one migration, not the outer loop.
func applyMigration(d *sql.DB, name string) error {
	body, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, strftime('%s','now'))`, name); err != nil {
		return err
	}
	return tx.Commit()
}
