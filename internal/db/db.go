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
	return OpenDriver("sqlite", path)
}

// OpenDriver is Open through the database/sql driver registered under
// driverName. The driver has to speak SQLite and accept the same DSN: the
// connection settings and the migrations below are SQLite's. It exists so a
// test can register a driver that wraps the real one and fails chosen
// statements, and still get a database set up exactly as Open sets it up.
func OpenDriver(driverName, path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	// synchronous=NORMAL is the recommended durability level under WAL: safe
	// across application crashes, only the last transaction is at risk on OS
	// power loss. It avoids an fsync per commit, which matters here because the
	// hot counters path commits frequently and MaxOpenConns(1) serializes every
	// writer behind those fsyncs.
	d, err := sql.Open(driverName, path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
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
	return d, nil
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
	if err := refuseNewerSchema(d, names); err != nil {
		return err
	}
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

// refuseNewerSchema 在库比本二进制新的时候报错。known 是本二进制带的迁移，已按名字
// 排序；迁移名以递增的四位序号开头，所以名字的字典序就是迁移的先后。
//
// 库里登记的最新迁移排在 known 的最后一个之后，说明这个库被更新的版本迁移过。本
// 二进制的 SQL 是按它自己那一版表结构写的，在更新的结构上不能照常运行：列被改名或
// 删掉时语句直接报错；只改了取值含义的（例如哪个值表示“永不到期”）语句照常执行，
// 写进去的值却会被新版读成另一个意思。所以这里拒绝打开，不让进程带着半数可用的
// 接口起来。
//
// 只比“最新”，不要求登记的每个名字都认识：0001_init.sql 自己就登记了一个没有对应
// 文件的名字（0004_simplify_schema.sql），所以每个库里都有本二进制不带的登记；历史
// 上被合并进 0001_init.sql 的那些迁移，在合并之前建的库里也还登记着。这些名字都排在
// known 的最后一个之前。
func refuseNewerSchema(d *sql.DB, known []string) error {
	if len(known) == 0 {
		return nil
	}
	var newest sql.NullString
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&newest); err != nil {
		return err
	}
	if newest.Valid && newest.String > known[len(known)-1] {
		return fmt.Errorf("database schema is newer than this binary: it has migration %s applied, and this binary knows none after %s; use a binary at least as new as the one that migrated it, or restore the database from before that upgrade",
			newest.String, known[len(known)-1])
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
