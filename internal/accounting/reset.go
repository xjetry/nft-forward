package accounting

import (
	"database/sql"
	"time"

	"nft-forward/internal/db"
)

// zeroUser 把一个用户名下的全部用量账本归零：全局已用量、各授权的已用量、落地出口
// 账本、名下每条规则的加权流量与逐跳流量。节点原始流量是运维指标，不属于任何用户，
// 不在其中。
//
// 这几样在页面上是同一个周期的用量，必须一起归零：只清一部分，规则上显示的流量就
// 会与用户的已用流量对不上。“用户重置”只有这一份定义，管理员手动重置（ResetUser）
// 与按周期自动重置（ResetCycleIfDue）都走这里；新增账本时加在这里，两种重置同时
// 生效。
func zeroUser(tx *sql.Tx, userID int64) error {
	for _, q := range []string{
		`UPDATE users SET traffic_used_bytes = 0 WHERE id=?`,
		`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=?`,
		`UPDATE user_landing_exits SET used_bytes = 0, used_bytes_up = 0, used_bytes_down = 0 WHERE user_id=?`,
		`UPDATE rules SET weighted_bytes = 0 WHERE owner_id = ?`,
		`UPDATE rule_hops SET total_bytes = 0 WHERE rule_id IN (SELECT id FROM rules WHERE owner_id = ?)`,
	} {
		if _, err := tx.Exec(q, userID); err != nil {
			return err
		}
	}
	return nil
}

// ResetUser 是管理员手动重置：把该用户的全部用量账本归零（范围见 zeroUser）。不动
// last_traffic_reset_at，周期重置的节拍不受手动重置影响。
func ResetUser(d *sql.DB, userID int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := zeroUser(tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetCycleIfDue 在用户的流量周期已经翻过时把他的全部用量账本归零（与手动重置同一
// 范围，见 zeroUser），并记下这次重置的时间；返回是否重置了。
//
// traffic_reset_days ≤ 0 表示从不自动重置。周期从账号创建时刻起算，边界可以预期
// （例如“从开户日起每 30 天”）。
func ResetCycleIfDue(d *sql.DB, u *db.User) (bool, error) {
	if u.TrafficResetDays <= 0 {
		return false, nil
	}
	nowTs := time.Now().Unix()
	period := int64(u.TrafficResetDays) * 86400
	var createdAt int64
	if err := d.QueryRow(`SELECT created_at FROM users WHERE id=?`, u.ID).Scan(&createdAt); err != nil {
		return false, err
	}
	elapsed := nowTs - createdAt
	if elapsed < 0 {
		return false, nil
	}
	cycleStart := createdAt + (elapsed/period)*period
	if u.LastTrafficResetAt >= cycleStart {
		return false, nil
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := zeroUser(tx, u.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE users SET last_traffic_reset_at = ? WHERE id=?`, nowTs, u.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ResetGrant 把用户在一个节点上的授权已用量归零，只动这一本账。
func ResetGrant(d *sql.DB, userID, nodeID int64) error {
	_, err := d.Exec(`UPDATE user_nodes SET traffic_used_bytes = 0 WHERE user_id=? AND node_id=?`, userID, nodeID)
	return err
}

// ResetExit 把用户的一个落地出口账归零（合计与上下行三列一起），只动这一本账。
// updated 表示有这一行；present 表示它在册——不在册的残留行不在下发排除的范围里，
// 调用方据此决定要不要重推。
func ResetExit(d *sql.DB, userID int64, host string, port int) (updated, present bool, err error) {
	var p int
	err = d.QueryRow(`UPDATE user_landing_exits SET used_bytes = 0, used_bytes_up = 0, used_bytes_down = 0, updated_at = ?
		WHERE user_id=? AND host=? AND port=? RETURNING present`,
		time.Now().Unix(), userID, host, port).Scan(&p)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, p == 1, nil
}
