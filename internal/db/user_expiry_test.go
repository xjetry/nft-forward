package db

import (
	"encoding/json"
	"testing"
)

// 迁移 0052 把 users.expires_at 里当作“永不到期”存着的 0 改成 NULL。此前的判定是
// “NULL 或不大于 0 都算永不到期”，所以负数与 0 一样改；正的到期时刻（已过去的也
// 一样）与本来就是 NULL 的行不动。
func TestMigrationStoresNoExpiryAsNull(t *testing.T) {
	const migration = "0052_user_expiry_null_means_never.sql"
	d := openTestDB(t)

	rows := []struct {
		name     string
		before   any
		wantType string
		want     int64
	}{
		{"0", int64(0), "null", 0},
		{"负数", int64(-5), "null", 0},
		{"本来就是 NULL", nil, "null", 0},
		{"已经过去的时刻", int64(1000), "integer", 1000},
		{"将来的时刻", int64(4102444800), "integer", 4102444800},
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = createTestUser(t, d)
		mustExec(t, d, `UPDATE users SET expires_at=? WHERE id=?`, r.before, ids[i])
	}
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}

	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	for i, r := range rows {
		var typ string
		var val int64
		if err := d.QueryRow(`SELECT typeof(expires_at), COALESCE(expires_at, 0) FROM users WHERE id=?`, ids[i]).Scan(&typ, &val); err != nil {
			t.Fatal(err)
		}
		if typ != r.wantType || val != r.want {
			t.Errorf("%s：迁移后是 %s %d，期望 %s %d", r.name, typ, val, r.wantType, r.want)
		}
	}
}

// 用户行不能直接写进响应：单独一行、放在切片里、放在 map 里都一样。对外的形状由
// server 包里的用户视图给出。
func TestUserRowRefusesDirectSerialization(t *testing.T) {
	u := &User{ID: 1, Username: "u"}
	for name, v := range map[string]any{
		"一行":      u,
		"一行的值":    *u,
		"切片":      []*User{u},
		"map 里的行": map[string]any{"user": u},
	} {
		if b, err := json.Marshal(v); err == nil {
			t.Errorf("%s：序列化成功了（%s），应当失败", name, b)
		}
	}
}
