package db

import "database/sql"

type TunnelCombo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

type TunnelComboHop struct {
	ComboID  int64  `json:"combo_id"`
	Position int    `json:"position"`
	TunnelID int64  `json:"tunnel_id"`
	Mode     string `json:"mode"`
}

type UserTunnelCombo struct {
	UserID      int64 `json:"user_id"`
	ComboID     int64 `json:"combo_id"`
	MaxForwards int   `json:"max_forwards"`
	GrantedAt   int64 `json:"granted_at"`
}

func CreateTunnelCombo(d DBTX, name string, hops []TunnelComboHop) (int64, error) {
	res, err := d.Exec(`INSERT INTO tunnel_combos(name, created_at) VALUES (?,?)`, name, now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for i, h := range hops {
		if _, err := d.Exec(`INSERT INTO tunnel_combo_hops(combo_id, position, tunnel_id, mode) VALUES (?,?,?,?)`,
			id, i, h.TunnelID, h.Mode); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func GetTunnelCombo(d *sql.DB, id int64) (*TunnelCombo, error) {
	row := d.QueryRow(`SELECT id, name, created_at FROM tunnel_combos WHERE id=?`, id)
	c := &TunnelCombo{}
	if err := row.Scan(&c.ID, &c.Name, &c.CreatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

func ListTunnelCombos(d *sql.DB) ([]*TunnelCombo, error) {
	rows, err := d.Query(`SELECT id, name, created_at FROM tunnel_combos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TunnelCombo
	for rows.Next() {
		c := &TunnelCombo{}
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func ListComboHops(d *sql.DB, comboID int64) ([]*TunnelComboHop, error) {
	rows, err := d.Query(`SELECT combo_id, position, tunnel_id, mode FROM tunnel_combo_hops WHERE combo_id=? ORDER BY position`, comboID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TunnelComboHop
	for rows.Next() {
		h := &TunnelComboHop{}
		if err := rows.Scan(&h.ComboID, &h.Position, &h.TunnelID, &h.Mode); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func UpdateTunnelCombo(d DBTX, id int64, name string, hops []TunnelComboHop) error {
	if _, err := d.Exec(`UPDATE tunnel_combos SET name=? WHERE id=?`, name, id); err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM tunnel_combo_hops WHERE combo_id=?`, id); err != nil {
		return err
	}
	for i, h := range hops {
		if _, err := d.Exec(`INSERT INTO tunnel_combo_hops(combo_id, position, tunnel_id, mode) VALUES (?,?,?,?)`,
			id, i, h.TunnelID, h.Mode); err != nil {
			return err
		}
	}
	return nil
}

func DeleteTunnelCombo(d *sql.DB, id int64) error {
	_, err := d.Exec(`DELETE FROM tunnel_combos WHERE id=?`, id)
	return err
}

func GrantCombo(d *sql.DB, userID, comboID int64, maxForwards int) error {
	_, err := d.Exec(`INSERT INTO user_tunnel_combos(user_id, combo_id, max_forwards, granted_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id, combo_id) DO UPDATE SET max_forwards=excluded.max_forwards`,
		userID, comboID, maxForwards, now())
	return err
}

func RevokeCombo(d *sql.DB, userID, comboID int64) error {
	_, err := d.Exec(`DELETE FROM user_tunnel_combos WHERE user_id=? AND combo_id=?`, userID, comboID)
	return err
}

func ListCombosForUser(d *sql.DB, userID int64) ([]*TunnelCombo, []*UserTunnelCombo, error) {
	rows, err := d.Query(`
		SELECT c.id, c.name, c.created_at, g.max_forwards, g.granted_at
		FROM tunnel_combos c JOIN user_tunnel_combos g ON g.combo_id = c.id
		WHERE g.user_id = ? ORDER BY c.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var combos []*TunnelCombo
	var grants []*UserTunnelCombo
	for rows.Next() {
		c := &TunnelCombo{}
		g := &UserTunnelCombo{UserID: userID}
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt, &g.MaxForwards, &g.GrantedAt); err != nil {
			return nil, nil, err
		}
		g.ComboID = c.ID
		combos = append(combos, c)
		grants = append(grants, g)
	}
	return combos, grants, rows.Err()
}

func GetComboGrant(d *sql.DB, userID, comboID int64) (*UserTunnelCombo, error) {
	row := d.QueryRow(`SELECT user_id, combo_id, max_forwards, granted_at FROM user_tunnel_combos WHERE user_id=? AND combo_id=?`, userID, comboID)
	g := &UserTunnelCombo{}
	if err := row.Scan(&g.UserID, &g.ComboID, &g.MaxForwards, &g.GrantedAt); err != nil {
		return nil, err
	}
	return g, nil
}
