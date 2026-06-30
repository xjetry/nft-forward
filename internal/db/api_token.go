package db

import "database/sql"

type APIToken struct {
	ID         int64         `json:"id"`
	UserID     int64         `json:"user_id"`
	Token      string        `json:"-"`
	Disabled   bool          `json:"disabled"`
	CreatedAt  int64         `json:"created_at"`
	LastUsedAt sql.NullInt64 `json:"last_used_at"`
}

func CreateAPIToken(d *sql.DB, userID int64) (string, error) {
	token := RandToken(32)
	_, err := d.Exec(
		`INSERT INTO api_tokens(user_id, token, created_at) VALUES (?,?,?)`,
		userID, token, now())
	if err != nil {
		return "", err
	}
	return token, nil
}

func GetAPITokenByUser(d *sql.DB, userID int64) (*APIToken, error) {
	t := &APIToken{}
	var disabled int
	err := d.QueryRow(
		`SELECT id, user_id, token, disabled, created_at, last_used_at FROM api_tokens WHERE user_id=?`,
		userID).Scan(&t.ID, &t.UserID, &t.Token, &disabled, &t.CreatedAt, &t.LastUsedAt)
	if err != nil {
		return nil, err
	}
	t.Disabled = disabled == 1
	return t, nil
}

func GetUserByAPIToken(d *sql.DB, token string) (*User, *APIToken, error) {
	t := &APIToken{}
	var disabled int
	err := d.QueryRow(
		`SELECT t.id, t.user_id, t.token, t.disabled, t.created_at, t.last_used_at
		 FROM api_tokens t WHERE t.token=?`, token,
	).Scan(&t.ID, &t.UserID, &t.Token, &disabled, &t.CreatedAt, &t.LastUsedAt)
	if err != nil {
		return nil, nil, err
	}
	t.Disabled = disabled == 1
	u, err := GetUserByID(d, t.UserID)
	if err != nil {
		return nil, nil, err
	}
	return u, t, nil
}

func DeleteAPIToken(d *sql.DB, userID int64) error {
	_, err := d.Exec(`DELETE FROM api_tokens WHERE user_id=?`, userID)
	return err
}

func RefreshAPIToken(d *sql.DB, userID int64) (string, error) {
	token := RandToken(32)
	res, err := d.Exec(
		`UPDATE api_tokens SET token=?, created_at=?, last_used_at=NULL, disabled=0 WHERE user_id=?`,
		token, now(), userID)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", sql.ErrNoRows
	}
	return token, nil
}

func ToggleAPIToken(d *sql.DB, userID int64) (bool, error) {
	var disabled int
	err := d.QueryRow(`SELECT disabled FROM api_tokens WHERE user_id=?`, userID).Scan(&disabled)
	if err != nil {
		return false, err
	}
	newVal := 1 - disabled
	_, err = d.Exec(`UPDATE api_tokens SET disabled=? WHERE user_id=?`, newVal, userID)
	return newVal == 1, err
}

func TouchAPITokenUsage(d *sql.DB, tokenID int64) error {
	_, err := d.Exec(`UPDATE api_tokens SET last_used_at=? WHERE id=?`, now(), tokenID)
	return err
}
