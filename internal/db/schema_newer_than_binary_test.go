package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// 库里登记的最新迁移比本二进制带的最新迁移还新，说明这个库被更新的版本迁移过。
// 这时不能照常打开：旧代码的 SQL 是按旧表结构写的，在新结构上有的语句直接报错，
// 有的照旧写入、含义却已经变了。Open 必须报错并点名那个迁移，库本身不动。
func TestOpenRefusesADatabaseMigratedByANewerBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const newer = "9999_from_a_newer_release.sql"
	mustExec(t, d, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 0)`, newer)
	registered := func() []string {
		t.Helper()
		rows, err := d.Query(`SELECT version FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	before := registered()

	if err := migrate(d); err == nil {
		t.Fatal("库比二进制新时 migrate 应报错，实际没有")
	} else if !strings.Contains(err.Error(), newer) {
		t.Errorf("报错应点名库里那个更新的迁移 %s，实际 %q", newer, err)
	}
	if after := registered(); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("被拒绝时迁移登记不应有变化\n之前 %v\n之后 %v", before, after)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("库比二进制新时 Open 应报错，实际打开了")
	}
	if !strings.Contains(err.Error(), newer) {
		t.Errorf("Open 的报错应点名 %s，实际 %q", newer, err)
	}
}

// 只有“比本二进制最新的迁移还新”才算。历史上有迁移文件被合并、删除过，老库里登记着
// 现在已经不存在的名字（都排在最新的迁移之前），这样的库必须照常打开。
func TestOpenAcceptsRegistrationsOfMigrationsThatNoLongerExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"0002_tunnel_combos.sql", "0003_merge_tenants.sql"} {
		mustExec(t, d, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 0)`, gone)
	}
	if err := migrate(d); err != nil {
		t.Fatalf("登记着已不存在的旧迁移的库应照常通过，实际 %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("登记着已不存在的旧迁移的库应能打开，实际 %v", err)
	}
	reopened.Close()
}
